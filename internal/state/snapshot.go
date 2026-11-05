package state

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
)

// Snapshot file layout (all integers are varints unless noted):
//
//	magic "AFGOSNAP" | version uint32 LE
//	watermark seq | watermark eventId | watermark occurredAt | clock
//	methods: 256 x (count, count x id)
//	devices: 256 x (count, count x device)
//	players: 256 x (count, count x player)
//	pending voids: count, count x (betId, t), sorted by betId
//	CRC-32C uint32 LE of everything before it
//
// Entities are written in index order, so a restored state assigns every ID
// the same index. Encoding is canonical: equal states give equal bytes.
const (
	snapshotMagic   = "AFGOSNAP"
	SnapshotVersion = 2
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// ErrBadSnapshot is wrapped by every decoding error.
var ErrBadSnapshot = errors.New("bad snapshot")

// WriteSnapshot encodes the state to w, one shard at a time under that
// shard's read lock. The caller must make sure the writer goroutine does not
// apply events meanwhile, so that the snapshot is a consistent cut at the
// watermark. Decisions are never blocked: they only take read locks.
func (s *State) WriteSnapshot(w io.Writer) error {
	crc := crc32.New(castagnoli)
	out := io.MultiWriter(w, crc)
	buf := make([]byte, 0, 1<<16)

	buf = append(buf, snapshotMagic...)
	buf = binary.LittleEndian.AppendUint32(buf, SnapshotVersion)
	buf = binary.AppendVarint(buf, s.mark.Seq)
	buf = appendString(buf, s.mark.EventID)
	buf = binary.AppendVarint(buf, s.mark.OccurredAt)
	buf = binary.AppendVarint(buf, s.clock)
	var err error
	if buf, err = flushBuf(out, buf); err != nil {
		return err
	}

	if buf, err = writeTable(out, buf, s.methods, func(b []byte, _ *struct{}) []byte { return b }); err != nil {
		return err
	}
	if buf, err = writeTable(out, buf, s.devices, appendDevice); err != nil {
		return err
	}
	if buf, err = writeTable(out, buf, s.players, appendPlayer); err != nil {
		return err
	}

	ids := slices.Sorted(maps.Keys(s.Pending.m))
	buf = binary.AppendUvarint(buf, uint64(len(ids)))
	for _, id := range ids {
		buf = appendString(buf, id)
		buf = binary.AppendVarint(buf, s.Pending.m[id])
	}
	if _, err = flushBuf(out, buf); err != nil {
		return err
	}
	_, err = w.Write(binary.LittleEndian.AppendUint32(nil, crc.Sum32()))
	return err
}

func flushBuf(w io.Writer, buf []byte) ([]byte, error) {
	_, err := w.Write(buf)
	return buf[:0], err
}

// writeTable encodes each shard of t under its read lock. Shards are flushed
// to w as they are encoded, so the buffer stays around one shard in size.
func writeTable[T any](w io.Writer, buf []byte, t *table[T], enc func([]byte, *T) []byte) ([]byte, error) {
	for i := range t.shards {
		sh := &t.shards[i]
		sh.mu.RLock()
		buf = binary.AppendUvarint(buf, uint64(len(sh.item)))
		for pos := range sh.item {
			buf = appendString(buf, sh.rev[pos])
			buf = enc(buf, &sh.item[pos])
		}
		sh.mu.RUnlock()
		var err error
		if buf, err = flushBuf(w, buf); err != nil {
			return buf, err
		}
	}
	return buf, nil
}

func appendString(b []byte, s string) []byte {
	b = binary.AppendUvarint(b, uint64(len(s)))
	return append(b, s...)
}

func appendBool(b []byte, v bool) []byte {
	if v {
		return append(b, 1)
	}
	return append(b, 0)
}

func appendDeclines(b []byte, ds []Decline) []byte {
	b = binary.AppendUvarint(b, uint64(len(ds)))
	for _, d := range ds {
		b = binary.AppendVarint(b, d.T)
		b = binary.AppendUvarint(b, uint64(d.PM))
	}
	return b
}

func appendDevice(b []byte, d *Device) []byte {
	b = appendDeclines(b, d.Declines)
	b = binary.AppendUvarint(b, uint64(len(d.Players)))
	for _, e := range d.Players {
		b = binary.AppendUvarint(b, uint64(e.Player))
		b = binary.AppendVarint(b, e.LastSeenT)
	}
	return b
}

func appendPlayer(b []byte, p *Player) []byte {
	for _, v := range [...]int64{p.LastDepositT, p.LastDepositEUR, p.LastDepositSeq, p.TurnoverEUR,
		p.LastPasswordResetT, p.LastPayoutMethodAddedT} {
		b = binary.AppendVarint(b, v)
	}
	b = binary.AppendUvarint(b, uint64(len(p.RecentBets)))
	for _, bet := range p.RecentBets {
		b = binary.LittleEndian.AppendUint64(b, bet.IDHash)
		b = binary.AppendVarint(b, bet.T)
		b = binary.AppendVarint(b, bet.AmountEUR)
		b = appendBool(b, bet.Voided)
	}
	b = appendDeclines(b, p.Declines)
	b = binary.AppendUvarint(b, uint64(len(p.Devices)))
	for _, d := range p.Devices {
		b = binary.AppendUvarint(b, uint64(d.Device))
		b = binary.AppendVarint(b, d.FirstSeenT)
		b = appendBool(b, d.FirstSeenIsLogin)
	}
	b = binary.AppendUvarint(b, uint64(len(p.ApprovedPMs)))
	for _, pm := range p.ApprovedPMs {
		b = binary.AppendUvarint(b, uint64(pm))
	}
	return b
}

// ReadSnapshot decodes a snapshot written by WriteSnapshot into a new state.
// It verifies the checksum and version before decoding anything.
func ReadSnapshot(data []byte) (*State, error) {
	hdr := len(snapshotMagic) + 4
	if len(data) < hdr+4 || string(data[:len(snapshotMagic)]) != snapshotMagic {
		return nil, fmt.Errorf("%w: not a snapshot file", ErrBadSnapshot)
	}
	if v := binary.LittleEndian.Uint32(data[len(snapshotMagic):]); v != SnapshotVersion {
		return nil, fmt.Errorf("%w: version %d, want %d", ErrBadSnapshot, v, SnapshotVersion)
	}
	body, sum := data[:len(data)-4], binary.LittleEndian.Uint32(data[len(data)-4:])
	if crc32.Checksum(body, castagnoli) != sum {
		return nil, fmt.Errorf("%w: checksum mismatch", ErrBadSnapshot)
	}

	d := decoder{b: body[hdr:]}
	s := New()
	s.mark.Seq = d.varint()
	s.mark.EventID = d.string()
	s.mark.OccurredAt = d.varint()
	s.clock = d.varint()
	s.prunedBucket = pendingBucket(s.clock)

	readTable(&d, s.methods, func(*decoder, *struct{}) {})
	readTable(&d, s.devices, decodeDevice)
	readTable(&d, s.players, decodePlayer)
	n := d.count(2)
	for range n {
		id := d.string()
		s.Pending.m[id] = d.varint()
	}
	if d.err == nil && len(d.b) != 0 {
		d.err = fmt.Errorf("%w: %d trailing bytes", ErrBadSnapshot, len(d.b))
	}
	if d.err != nil {
		return nil, d.err
	}
	return s, nil
}

func readTable[T any](d *decoder, t *table[T], dec func(*decoder, *T)) {
	for i := range t.shards {
		sh := &t.shards[i]
		n := d.count(1)
		sh.ids = make(map[string]uint32, n)
		sh.rev = make([]string, n)
		sh.item = make([]T, n)
		for pos := range n {
			id := d.string()
			sh.ids[id] = uint32(pos)
			sh.rev[pos] = id
			dec(d, &sh.item[pos])
		}
		if d.err != nil {
			return
		}
	}
}

func decodeDeclines(d *decoder) []Decline {
	n := d.count(2)
	if n == 0 {
		return nil
	}
	out := make([]Decline, n)
	for i := range out {
		out[i] = Decline{T: d.varint(), PM: d.uint32()}
	}
	return out
}

func decodeDevice(d *decoder, dev *Device) {
	dev.Declines = decodeDeclines(d)
	if n := d.count(2); n > 0 {
		dev.Players = make([]DevicePlayer, n)
		for i := range dev.Players {
			dev.Players[i] = DevicePlayer{Player: d.uint32(), LastSeenT: d.varint()}
		}
	}
}

func decodePlayer(d *decoder, p *Player) {
	p.LastDepositT = d.varint()
	p.LastDepositEUR = d.varint()
	p.LastDepositSeq = d.varint()
	p.TurnoverEUR = d.varint()
	p.LastPasswordResetT = d.varint()
	p.LastPayoutMethodAddedT = d.varint()
	if n := d.count(11); n > 0 {
		p.RecentBets = make([]Bet, n)
		for i := range p.RecentBets {
			p.RecentBets[i] = Bet{IDHash: d.uint64(), T: d.varint(), AmountEUR: d.varint(), Voided: d.bool()}
		}
	}
	p.Declines = decodeDeclines(d)
	if n := d.count(3); n > 0 {
		p.Devices = make([]PlayerDevice, n)
		for i := range p.Devices {
			p.Devices[i] = PlayerDevice{Device: d.uint32(), FirstSeenT: d.varint(), FirstSeenIsLogin: d.bool()}
		}
	}
	if n := d.count(1); n > 0 {
		p.ApprovedPMs = make([]uint32, n)
		for i := range p.ApprovedPMs {
			p.ApprovedPMs[i] = d.uint32()
		}
	}
}

// decoder reads from b; after the first error every read returns zero.
type decoder struct {
	b   []byte
	err error
}

func (d *decoder) fail(what string) {
	if d.err == nil {
		d.err = fmt.Errorf("%w: truncated or malformed %s", ErrBadSnapshot, what)
	}
	d.b = nil
}

func (d *decoder) uvarint() uint64 {
	v, n := binary.Uvarint(d.b)
	if n <= 0 {
		d.fail("varint")
		return 0
	}
	d.b = d.b[n:]
	return v
}

func (d *decoder) varint() int64 {
	v, n := binary.Varint(d.b)
	if n <= 0 {
		d.fail("varint")
		return 0
	}
	d.b = d.b[n:]
	return v
}

func (d *decoder) uint32() uint32 {
	v := d.uvarint()
	if v > 1<<32-1 {
		d.fail("index")
		return 0
	}
	return uint32(v)
}

func (d *decoder) uint64() uint64 {
	if len(d.b) < 8 {
		d.fail("hash")
		return 0
	}
	v := binary.LittleEndian.Uint64(d.b)
	d.b = d.b[8:]
	return v
}

func (d *decoder) bool() bool {
	if len(d.b) < 1 || d.b[0] > 1 {
		d.fail("bool")
		return false
	}
	v := d.b[0] == 1
	d.b = d.b[1:]
	return v
}

// count reads an element count. Each element takes at least minSize bytes, so
// a count larger than the remaining input is rejected before allocating.
func (d *decoder) count(minSize int) int {
	n := d.uvarint()
	if n > uint64(len(d.b)/minSize) {
		d.fail("count")
		return 0
	}
	return int(n)
}

func (d *decoder) string() string {
	n := d.uvarint()
	if n > uint64(len(d.b)) {
		d.fail("string")
		return ""
	}
	s := string(d.b[:n])
	d.b = d.b[n:]
	return s
}

// SnapshotFile is a snapshot being written to path+".tmp". Commit makes it
// durable and atomically replaces path; Abort discards it.
type SnapshotFile struct {
	path string
	f    *os.File
	bw   *bufio.Writer
}

// CreateSnapshot creates the temporary file for a snapshot at path.
func CreateSnapshot(path string) (*SnapshotFile, error) {
	f, err := os.Create(path + ".tmp")
	if err != nil {
		return nil, fmt.Errorf("create snapshot: %w", err)
	}
	return &SnapshotFile{path: path, f: f, bw: bufio.NewWriterSize(f, 1<<20)}, nil
}

// Encode writes the state to the temporary file and flushes it to the OS. It
// does not fsync, so it is cheap enough to run while the writer is paused.
func (sf *SnapshotFile) Encode(s *State) error {
	if err := s.WriteSnapshot(sf.bw); err != nil {
		return fmt.Errorf("write snapshot: %w", err)
	}
	if err := sf.bw.Flush(); err != nil {
		return fmt.Errorf("write snapshot: %w", err)
	}
	return nil
}

// Size returns the number of bytes written so far.
func (sf *SnapshotFile) Size() int64 {
	off, _ := sf.f.Seek(0, io.SeekCurrent)
	return off + int64(sf.bw.Buffered())
}

// Commit fsyncs the temporary file, renames it over path and fsyncs the
// directory, so a crash leaves either the old or the new snapshot.
func (sf *SnapshotFile) Commit() error {
	if err := sf.f.Sync(); err != nil {
		sf.Abort()
		return fmt.Errorf("sync snapshot: %w", err)
	}
	if err := sf.f.Close(); err != nil {
		_ = os.Remove(sf.f.Name())
		return fmt.Errorf("close snapshot: %w", err)
	}
	if err := os.Rename(sf.f.Name(), sf.path); err != nil {
		_ = os.Remove(sf.f.Name())
		return fmt.Errorf("rename snapshot: %w", err)
	}
	dir, err := os.Open(filepath.Dir(sf.path))
	if err != nil {
		return fmt.Errorf("sync snapshot dir: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync snapshot dir: %w", err)
	}
	return nil
}

// Abort closes and removes the temporary file.
func (sf *SnapshotFile) Abort() {
	_ = sf.f.Close()
	_ = os.Remove(sf.f.Name())
}

// SaveSnapshot writes the state to path atomically. The caller must make sure
// nothing applies events meanwhile.
func SaveSnapshot(path string, s *State) error {
	sf, err := CreateSnapshot(path)
	if err != nil {
		return err
	}
	if err := sf.Encode(s); err != nil {
		sf.Abort()
		return err
	}
	return sf.Commit()
}

// LoadSnapshot reads the snapshot at path. The error wraps fs.ErrNotExist if
// there is none, and ErrBadSnapshot if it is corrupt or of another version.
func LoadSnapshot(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ReadSnapshot(data)
}

// Equal reports whether two states are identical, by comparing their
// canonical snapshot encodings. Both must be quiescent.
func Equal(a, b *State) bool {
	var ba, bb bytes.Buffer
	// Writing to a bytes.Buffer cannot fail.
	_ = a.WriteSnapshot(&ba)
	_ = b.WriteSnapshot(&bb)
	return bytes.Equal(ba.Bytes(), bb.Bytes())
}
