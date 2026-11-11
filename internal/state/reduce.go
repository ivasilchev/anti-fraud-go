package state

// LateThresholdMs is how far behind now an event may occur before it counts
// as late. Late events are still applied, best-effort.
const LateThresholdMs = 5 * 60 * 1000

// MaxFutureMs is how far ahead of the server clock an accepted event may
// occur. It must equal api.MaxFutureSkew: the pruning clock relies on it.
const MaxFutureMs = 5 * 60 * 1000

// BetRef describes a bet referenced by a BET_VOIDED event.
type BetRef struct {
	PlayerID  string
	T         int64
	AmountEUR int64
	// Seq is the bet's seq, or 0 when the bet was found in memory (it was
	// applied before the batch). A void applied before its bet's seq treats
	// the bet as not yet arrived.
	Seq int64
}

// Watermark identifies the last applied event. Seq is 0 when nothing has
// been applied. OccurredAt tells a reseeded log apart from the original even
// when it reuses the same seqs and eventIds.
type Watermark struct {
	Seq        int64
	EventID    string
	OccurredAt int64 // unix ms
}

// Watermark returns the last applied event.
func (s *State) Watermark() Watermark { return s.mark }

// pruneNow is the reference time for pruning per-entity slices and pending
// voids: the latest applied occurredAt minus MaxFutureMs. Every accepted event
// occurred at most MaxFutureMs after it was received, so pruneNow never runs
// ahead of the real server clock and pruning is never more aggressive than
// pruning by receive time. Because it depends only on the events applied, in
// seq order, live state and a replay of the log are identical.
func (s *State) pruneNow() int64 { return s.clock - MaxFutureMs }

// pendingBucket is the second, by pruneNow, in which pending voids were last
// pruned. They are pruned whenever pruneNow enters a new second.
func pendingBucket(clock int64) int64 { return max(0, (clock-MaxFutureMs)/1000) }

// PrepareVoids resolves the bets referenced by the voids in actions, which
// must be sorted by seq, from the batch itself and from memory. It returns the
// resolved bets and the IDs of the bets that must be looked up in the durable
// log. It only reads the state; see ResolveVoids.
func (s *State) PrepareVoids(actions []Action) (refs map[string]BetRef, missing []string) {
	for i := range actions {
		if actions[i].Event.Type != EventBetVoided {
			continue
		}
		if refs == nil {
			refs = s.batchBets(actions)
		}
		ev := actions[i].Event
		if _, ok := refs[ev.VoidedEventID]; ok {
			continue
		}
		if b, ok := s.findBet(ev.PlayerID, ev.VoidedEventID); ok {
			refs[ev.VoidedEventID] = BetRef{PlayerID: ev.PlayerID, T: b.T, AmountEUR: b.AmountEUR}
			continue
		}
		missing = append(missing, ev.VoidedEventID)
	}
	return refs, missing
}

func (s *State) batchBets(actions []Action) map[string]BetRef {
	refs := make(map[string]BetRef)
	for _, a := range actions {
		if ev := a.Event; ev.Type == EventBet {
			refs[ev.EventID] = BetRef{PlayerID: ev.PlayerID, T: ev.OccurredAt, AmountEUR: ev.AmountEURE4, Seq: a.Seq}
		}
	}
	return refs
}

func (s *State) findBet(playerID, betID string) (Bet, bool) {
	sh, _ := s.players.shardFor(playerID)
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	if p := sh.find(playerID); p != nil {
		return p.FindBet(HashID(betID))
	}
	return Bet{}, false
}

// Reduce is the root reducer. It applies b.Actions, in order, to the state;
// b.Bets resolves the bets referenced by voids (bets not in it are unknown).
// It is deterministic and does no I/O, so a replay of the log rebuilds the
// same state. Only the goroutine that dispatches may call it; most callers
// should go through a Dispatcher.
func (s *State) Reduce(b *Batch) {
	for i := range b.Actions {
		a := &b.Actions[i]
		s.clock = max(s.clock, a.Event.OccurredAt)
		s.reduce(a, b.Bets)
		s.mark = Watermark{Seq: a.Seq, EventID: a.Event.EventID, OccurredAt: a.Event.OccurredAt}
		if bucket := pendingBucket(s.clock); bucket > s.prunedBucket {
			s.Pending.Prune(s.pruneNow())
			s.prunedBucket = bucket
		}
	}
}

// resolved is what the root reducer works out for one action before the slice
// reducers run: interned indexes, the pruning time, and the cross-slice facts
// a slice reducer needs but must not look up itself.
type resolved struct {
	player, device, pm uint32
	now                int64 // pruning reference time, see pruneNow

	betVoided bool   // BET: a void for this bet was pending
	voidRef   BetRef // BET_VOIDED: the voided bet, when voidRefOK
	voidRefOK bool
}

// reduce applies one action. It owns the locks and the interning, which the
// slice reducers know nothing about. The writer never holds two shard locks
// at once, so decisions (player lock, then device lock) cannot deadlock.
func (s *State) reduce(a *Action, bets map[string]BetRef) {
	ev := a.Event
	r := resolved{now: s.pruneNow()}

	ps, psIdx := s.players.shardFor(ev.PlayerID)
	ps.mu.Lock()
	r.player = ps.intern(ev.PlayerID, psIdx)
	ps.mu.Unlock()

	if ev.Type == EventDepositApproved || ev.Type == EventDepositDeclined {
		r.pm = s.internMethod(ev.PaymentMethodID)
	}
	switch ev.Type {
	case EventBet:
		r.betVoided = s.Pending.Take(ev.EventID)
	case EventBetVoided:
		r.voidRef, r.voidRefOK = bets[ev.VoidedEventID]
	}

	if ev.DeviceID != "" {
		ds, dsIdx := s.devices.shardFor(ev.DeviceID)
		ds.mu.Lock()
		r.device = ds.intern(ev.DeviceID, dsIdx)
		_, pos := splitIndex(r.device)
		reduceDevice(ds.at(pos), a, &r)
		ds.mu.Unlock()
	}

	ps.mu.Lock()
	_, pos := splitIndex(r.player)
	voidWaits := reducePlayer(ps.at(pos), a, &r)
	ps.mu.Unlock()

	if voidWaits {
		s.Pending.Add(ev.VoidedEventID, ev.OccurredAt)
	}
}

// reduceDevice is the device slice reducer.
func reduceDevice(d *Device, a *Action, r *resolved) {
	t := a.Event.OccurredAt
	d.SeePlayer(r.player, t, r.now)
	if a.Event.Type == EventDepositDeclined {
		d.AddDecline(t, r.pm, r.now)
	}
}

// reducePlayer is the player slice reducer. It returns true when the action is
// a void whose bet has not arrived yet, so the root reducer parks it in the
// pending voids.
func reducePlayer(p *Player, a *Action, r *resolved) (voidWaits bool) {
	ev, t := a.Event, a.Event.OccurredAt
	if ev.DeviceID != "" {
		p.SeeDevice(r.device, t, ev.Type == EventLogin)
	}
	switch ev.Type {
	case EventDepositDeclined:
		p.AddDecline(t, r.pm, r.now)
	case EventDepositApproved:
		p.ApplyDepositApproved(t, a.Seq, ev.AmountEURE4, r.pm)
	case EventBet:
		p.ApplyBet(HashID(ev.EventID), t, ev.AmountEURE4, r.betVoided, r.now)
	case EventBetVoided:
		return reduceVoid(p, a, r)
	case EventPasswordReset:
		p.ApplyPasswordReset(t)
	case EventPayoutMethodAdded:
		p.ApplyPayoutMethodAdded(t)
	}
	return false
}

// reduceVoid handles a BET_VOIDED. The bet is looked up in the player's recent
// bets, then in the resolved reference. A bet later in the same batch, or not
// found at all, makes the void wait. A bet of another player is ignored.
func reduceVoid(p *Player, a *Action, r *resolved) (voidWaits bool) {
	ev := a.Event
	if p.ApplyVoid(HashID(ev.VoidedEventID)) {
		return false
	}
	switch {
	case !r.voidRefOK || r.voidRef.Seq > a.Seq:
		return true
	case r.voidRef.PlayerID == ev.PlayerID:
		p.ApplyVoidOutOfWindow(r.voidRef.T, r.voidRef.AmountEUR)
	}
	return false
}

func (s *State) internMethod(id string) uint32 {
	sh, sIdx := s.methods.shardFor(id)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return sh.intern(id, sIdx)
}
