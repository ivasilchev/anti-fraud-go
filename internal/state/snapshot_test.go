package state

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/devastion/anti-fraud-go/internal/money"
)

// richEvents exercises every per-entity slice and the pending voids.
func richEvents() []Event {
	eur := func(e Event, amount int64) Event {
		e.AmountE4, e.AmountEURE4, e.Currency = amount, amount, money.EUR
		return e
	}
	t := now - 4*minute
	var out []Event
	add := func(e Event) { e.EventID = "e" + strconv.Itoa(len(out)); out = append(out, e) }
	for i := range 6 {
		p := "p" + strconv.Itoa(i%3)
		add(Event{PlayerID: p, Type: EventLogin, OccurredAt: t, DeviceID: "d" + strconv.Itoa(i%2)})
		add(eur(Event{PlayerID: p, Type: EventDepositDeclined, OccurredAt: t + 1, DeviceID: "d0",
			PaymentMethodID: "pm" + strconv.Itoa(i)}, 50000))
		add(eur(Event{PlayerID: p, Type: EventDepositApproved, OccurredAt: t + 2, PaymentMethodID: "pm" + strconv.Itoa(i)}, 1000000))
		add(eur(Event{PlayerID: p, Type: EventBet, OccurredAt: t + 3}, 20000))
		add(Event{PlayerID: p, Type: EventPasswordReset, OccurredAt: t + 4})
		add(Event{PlayerID: p, Type: EventPayoutMethodAdded, OccurredAt: t + 5, PaymentMethodID: "pmx"})
		t += 10_000
	}
	add(Event{PlayerID: "p0", Type: EventBetVoided, OccurredAt: t, VoidedEventID: "e3"})          // voids a recent bet
	add(Event{PlayerID: "p1", Type: EventBetVoided, OccurredAt: t, VoidedEventID: "not-yet"})     // pending
	add(Event{PlayerID: "p2", Type: EventBetVoided, OccurredAt: t, VoidedEventID: "never-comes"}) // pending
	return out
}

func applyAll(s *State, events []Event, firstSeq int64) {
	for i := range events {
		rows := []Row{{Seq: firstSeq + int64(i), Event: &events[i], Now: now}}
		refs, _ := s.PrepareVoids(rows)
		s.Apply(rows, refs)
	}
}

func encode(t *testing.T, s *State) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := s.WriteSnapshot(&b); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestSnapshotRoundTrip(t *testing.T) {
	s := New()
	applyAll(s, richEvents(), 1)
	if s.Pending.Len() != 2 {
		t.Fatalf("setup: %d pending voids, want 2", s.Pending.Len())
	}
	data := encode(t, s)
	got, err := ReadSnapshot(data)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encode(t, got), data) {
		t.Fatal("re-encoded snapshot differs")
	}
	if seq, id := got.Watermark(); seq != int64(len(richEvents())) || id != "e38" {
		t.Fatalf("watermark = %d %q", seq, id)
	}
	gp, gd, gm := got.Counts()
	sp, sd, sm := s.Counts()
	if gp != sp || gd != sd || gm != sm {
		t.Fatalf("counts = %d/%d/%d, want %d/%d/%d", gp, gd, gm, sp, sd, sm)
	}

	// Lookups and further applies behave the same on the restored state.
	more := []Event{
		{EventID: "x1", PlayerID: "p9", Type: EventLogin, OccurredAt: now, DeviceID: "d9"},
		{EventID: "not-yet", PlayerID: "p1", Type: EventBet, OccurredAt: now, AmountE4: 1, AmountEURE4: 1, Currency: money.EUR},
		{EventID: "x2", PlayerID: "p0", Type: EventDepositDeclined, OccurredAt: now, DeviceID: "d0", PaymentMethodID: "pm-new",
			AmountE4: 1, AmountEURE4: 1, Currency: money.EUR},
	}
	applyAll(s, more, 100)
	applyAll(got, more, 100)
	if !Equal(s, got) {
		t.Fatal("restored state diverges after applying more events")
	}
	v := got.Acquire("p0", "d0", "pm0")
	defer v.Release()
	if v.P == nil || v.D == nil || !v.PMKnown || !v.P.HasApprovedPM(v.PM) {
		t.Fatalf("lookups on restored state: %+v", v)
	}
}

func TestSnapshotOfEmptyState(t *testing.T) {
	got, err := ReadSnapshot(encode(t, New()))
	if err != nil {
		t.Fatal(err)
	}
	if seq, _ := got.Watermark(); seq != 0 {
		t.Fatalf("watermark = %d", seq)
	}
}

func TestReadSnapshotRejectsDamage(t *testing.T) {
	s := New()
	applyAll(s, richEvents(), 1)
	good := encode(t, s)
	withCRC := func(b []byte) []byte {
		body := b[:len(b)-4]
		return binary.LittleEndian.AppendUint32(bytes.Clone(body), crcOf(body))
	}
	flipped := bytes.Clone(good)
	flipped[len(flipped)/2] ^= 1
	version := bytes.Clone(good)
	version[len(snapshotMagic)] = 2
	trailing := withCRC(append(bytes.Clone(good[:len(good)-4]), 7, 0, 0, 0, 0)) // one extra byte
	truncated := withCRC(append(bytes.Clone(good[:len(good)/2]), 0, 0, 0, 0))

	for name, data := range map[string][]byte{
		"empty":      nil,
		"magic":      append([]byte("NOTASNAP"), good[8:]...),
		"checksum":   flipped,
		"version":    version,
		"trailing":   trailing,
		"truncated":  truncated,
		"tiny":       good[:10],
		"huge count": hugeCount(),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadSnapshot(data); !errors.Is(err, ErrBadSnapshot) {
				t.Fatalf("got %v, want ErrBadSnapshot", err)
			}
		})
	}
}

func TestSaveAndLoadSnapshotFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot.bin")
	if _, err := LoadSnapshot(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file: got %v", err)
	}
	s := New()
	applyAll(s, richEvents(), 1)
	if err := SaveSnapshot(path, s); err != nil {
		t.Fatal(err)
	}
	// A crash mid-write leaves only the temporary file, which is never read.
	if err := os.WriteFile(path+".tmp", []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	if !Equal(got, s) {
		t.Fatal("loaded state differs")
	}
}

func TestStateDoesNotDependOnReceiveTime(t *testing.T) {
	// Pruning follows the log clock, so the receive time (Row.Now) only
	// affects late counting: a replay (Now = 0) builds the same state.
	events := richEvents()
	live, replayed := New(), New()
	for i := range events {
		rows := []Row{{Seq: int64(i + 1), Event: &events[i], Now: now + int64(i)*minute}}
		refs, _ := live.PrepareVoids(rows)
		live.Apply(rows, refs)
		rows[0].Now = 0
		refs, _ = replayed.PrepareVoids(rows)
		if late := replayed.Apply(rows, refs); late != 0 {
			t.Fatalf("replayed rows counted as late: %d", late)
		}
	}
	if !Equal(live, replayed) {
		t.Fatal("state depends on receive time")
	}
}

func TestPruningFollowsTheLogClock(t *testing.T) {
	s := New()
	ev := func(id string, typ EventType, t int64) Event {
		return Event{EventID: id, PlayerID: "p1", Type: typ, OccurredAt: t, AmountE4: 1, AmountEURE4: 1,
			Currency: money.EUR, PaymentMethodID: "pm1"}
	}
	events := []Event{
		ev("d1", EventDepositDeclined, now-21*minute),
		ev("b1", EventBet, now-12*minute),
		// The clock is now; pruneNow = now - 5m. The decline (16m before
		// pruneNow) and the bet (7m before) are pruned on the next append.
		ev("d2", EventDepositDeclined, now),
		ev("b2", EventBet, now),
		// An older, late event does not move the clock back.
		ev("d3", EventDepositDeclined, now-19*minute),
	}
	applyAll(s, events, 1)
	v := s.Acquire("p1", "", "")
	defer v.Release()
	if len(v.P.Declines) != 2 || v.P.Declines[0].T != now || v.P.Declines[1].T != now-19*minute {
		t.Fatalf("declines = %+v", v.P.Declines)
	}
	if len(v.P.RecentBets) != 1 || v.P.RecentBets[0].T != now {
		t.Fatalf("bets = %+v", v.P.RecentBets)
	}
}

func crcOf(b []byte) uint32 { return crc32.Checksum(b, castagnoli) }

// hugeCount is a valid header followed by a shard claiming 2^40 methods.
func hugeCount() []byte {
	b := append([]byte(snapshotMagic), 1, 0, 0, 0)
	b = binary.AppendVarint(b, 0)
	b = appendString(b, "")
	b = binary.AppendVarint(b, 0)
	b = binary.AppendUvarint(b, 1<<40)
	return binary.LittleEndian.AppendUint32(b, crcOf(b))
}
