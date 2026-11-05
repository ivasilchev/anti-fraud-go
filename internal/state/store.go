package state

import "sync"

// ShardCount is the number of shards for players, devices and payment methods.
// An interned index encodes its shard in the low 8 bits and its position
// within the shard in the high 24 bits.
const ShardCount = 256

const shardBits = 8

// shardOf picks the shard for an ID. FNV-1a is stable across processes, so the
// index of an ID depends only on insertion order within its shard.
func shardOf(id string) uint32 { return uint32(HashID(id) & (ShardCount - 1)) }

func makeIndex(shard, pos uint32) uint32 { return pos<<shardBits | shard }

func splitIndex(idx uint32) (shard, pos uint32) { return idx & (ShardCount - 1), idx >> shardBits }

// shard holds one slice of an entity table and the dictionary for its IDs.
// The writer goroutine is the only mutator; readers take the read lock.
type shard[T any] struct {
	mu   sync.RWMutex
	ids  map[string]uint32 // id -> position in items
	rev  []string          // position -> id
	item []T
}

// table is a sharded dictionary of IDs with per-entity state T.
type table[T any] struct {
	shards [ShardCount]shard[T]
}

func newTable[T any]() *table[T] {
	t := &table[T]{}
	for i := range t.shards {
		t.shards[i].ids = make(map[string]uint32)
	}
	return t
}

// shardFor returns the shard of id and its shard number.
func (t *table[T]) shardFor(id string) (*shard[T], uint32) {
	s := shardOf(id)
	return &t.shards[s], s
}

// intern returns the index of id, adding it with zero state if it is new.
// The caller must hold the write lock of id's shard.
func (sh *shard[T]) intern(id string, s uint32) uint32 {
	if pos, ok := sh.ids[id]; ok {
		return makeIndex(s, pos)
	}
	pos := uint32(len(sh.item))
	sh.ids[id] = pos
	sh.rev = append(sh.rev, id)
	var zero T
	sh.item = append(sh.item, zero)
	return makeIndex(s, pos)
}

// find returns the state of id, or nil. The caller must hold a lock on the shard.
func (sh *shard[T]) find(id string) *T {
	pos, ok := sh.ids[id]
	if !ok {
		return nil
	}
	return &sh.item[pos]
}

// at returns the state at position pos. The caller must hold a lock on the shard.
func (sh *shard[T]) at(pos uint32) *T { return &sh.item[pos] }

// len returns the number of entities across all shards.
func (t *table[T]) len() int {
	n := 0
	for i := range t.shards {
		sh := &t.shards[i]
		sh.mu.RLock()
		n += len(sh.item)
		sh.mu.RUnlock()
	}
	return n
}

// State is the complete in-memory rule state.
type State struct {
	players *table[Player]
	devices *table[Device]
	methods *table[struct{}] // payment method dictionary only

	// Pending is touched only by the writer goroutine.
	Pending *PendingVoids

	// Writer-only bookkeeping, persisted in snapshots.
	clock        int64  // latest applied occurredAt (unix ms)
	lastSeq      int64  // seq of the last applied event
	lastEventID  string // eventId of the last applied event
	prunedBucket int64  // see pendingBucket
}

// New returns an empty state.
func New() *State {
	return &State{
		players: newTable[Player](),
		devices: newTable[Device](),
		methods: newTable[struct{}](),
		Pending: NewPendingVoids(),
	}
}

// Counts returns the number of interned players, devices and payment methods.
func (s *State) Counts() (players, devices, methods int) {
	return s.players.len(), s.devices.len(), s.methods.len()
}

// View is a read-locked view of the state for one decision. Call Release
// exactly once when done; P and D must not be used after that.
type View struct {
	// P is nil for an unknown player; D is nil when no deviceId was given or
	// the device is unknown.
	P *Player
	D *Device
	// PM is the interned payment method; meaningful only when PMKnown.
	PM      uint32
	PMKnown bool

	ps *sync.RWMutex
	ds *sync.RWMutex
}

// Acquire looks up the player, device and payment method of a decision and
// read-locks their shards. Locks are always taken player first, then device;
// the writer never holds two shard locks at once, so this cannot deadlock.
// deviceID and pmID may be empty.
func (s *State) Acquire(playerID, deviceID, pmID string) View {
	var v View
	if pmID != "" {
		sh, sIdx := s.methods.shardFor(pmID)
		sh.mu.RLock()
		if pos, ok := sh.ids[pmID]; ok {
			v.PM, v.PMKnown = makeIndex(sIdx, pos), true
		}
		sh.mu.RUnlock()
	}
	ps, _ := s.players.shardFor(playerID)
	ps.mu.RLock()
	v.ps = &ps.mu
	v.P = ps.find(playerID)
	if deviceID != "" {
		ds, _ := s.devices.shardFor(deviceID)
		ds.mu.RLock()
		v.ds = &ds.mu
		v.D = ds.find(deviceID)
	}
	return v
}

// Release drops the read locks taken by Acquire.
func (v *View) Release() {
	if v.ds != nil {
		v.ds.RUnlock()
	}
	v.ps.RUnlock()
	v.P, v.D, v.ps, v.ds = nil, nil, nil, nil
}

// Trim shrinks every slice to its length, releasing the spare capacity that
// append leaves behind (about 100 MB after replaying 10M events). It changes
// no state, only capacity. It takes each shard's write lock in turn; call it
// from the writer goroutine or before the writer starts.
func (s *State) Trim() {
	trimTable(s.players, func(p *Player) {
		p.RecentBets = trimmed(p.RecentBets)
		p.Declines = trimmed(p.Declines)
		p.Devices = trimmed(p.Devices)
		p.ApprovedPMs = trimmed(p.ApprovedPMs)
	})
	trimTable(s.devices, func(d *Device) {
		d.Declines = trimmed(d.Declines)
		d.Players = trimmed(d.Players)
	})
	trimTable(s.methods, func(*struct{}) {})
}

func trimTable[T any](t *table[T], trimItem func(*T)) {
	for i := range t.shards {
		sh := &t.shards[i]
		sh.mu.Lock()
		for j := range sh.item {
			trimItem(&sh.item[j])
		}
		sh.item = trimmed(sh.item)
		sh.rev = trimmed(sh.rev)
		sh.mu.Unlock()
	}
}

// trimmed returns s with cap == len, or nil when it is empty.
func trimmed[E any](s []E) []E {
	if len(s) == 0 {
		return nil
	}
	if cap(s) == len(s) {
		return s
	}
	return append(make([]E, 0, len(s)), s...)
}
