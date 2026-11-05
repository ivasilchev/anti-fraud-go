package state

// LateThresholdMs is how far behind now an event may occur before it counts
// as late. Late events are still applied, best-effort.
const LateThresholdMs = 5 * 60 * 1000

// MaxFutureMs is how far ahead of the server clock an accepted event may
// occur. It must equal api.MaxFutureSkew: the pruning clock relies on it.
const MaxFutureMs = 5 * 60 * 1000

// Row is a persisted event to apply.
type Row struct {
	Seq   int64
	Event *Event
	// Now is the server time (unix ms) the event was received at, used only to
	// count late events. It is 0 for replayed events, which are never counted.
	Now int64
}

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

// Watermark returns the seq and eventId of the last applied event. seq is 0
// when nothing has been applied.
func (s *State) Watermark() (seq int64, eventID string) { return s.lastSeq, s.lastEventID }

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

// PrepareVoids resolves the bets referenced by the voids in rows, which must
// be sorted by seq, from the batch itself and from memory. It returns the
// resolved bets and the IDs of the bets that must be looked up in the durable
// log. Only the writer goroutine may call it.
func (s *State) PrepareVoids(rows []Row) (refs map[string]BetRef, missing []string) {
	for i := range rows {
		if rows[i].Event.Type != EventBetVoided {
			continue
		}
		if refs == nil {
			refs = s.batchBets(rows)
		}
		ev := rows[i].Event
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

func (s *State) batchBets(rows []Row) map[string]BetRef {
	refs := make(map[string]BetRef)
	for _, r := range rows {
		if ev := r.Event; ev.Type == EventBet {
			refs[ev.EventID] = BetRef{PlayerID: ev.PlayerID, T: ev.OccurredAt, AmountEUR: ev.AmountEURE4, Seq: r.Seq}
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

// Apply applies rows, sorted by seq and newer than the watermark, to the
// state. refs resolves the bets referenced by voids (see PrepareVoids); bets
// not in refs are unknown. It returns the number of late events. Only the
// writer goroutine may call it.
func (s *State) Apply(rows []Row, refs map[string]BetRef) (late int) {
	for i := range rows {
		r := &rows[i]
		if r.Now-r.Event.OccurredAt > LateThresholdMs {
			late++
		}
		s.clock = max(s.clock, r.Event.OccurredAt)
		s.apply(r, refs, s.pruneNow())
		s.lastSeq, s.lastEventID = r.Seq, r.Event.EventID
		if b := pendingBucket(s.clock); b > s.prunedBucket {
			s.Pending.Prune(s.pruneNow())
			s.prunedBucket = b
		}
	}
	return late
}

// apply applies one event. now is the pruning reference time (see pruneNow).
func (s *State) apply(r *Row, refs map[string]BetRef, now int64) {
	ev, t := r.Event, r.Event.OccurredAt

	ps, psIdx := s.players.shardFor(ev.PlayerID)
	ps.mu.Lock()
	player := ps.intern(ev.PlayerID, psIdx)
	ps.mu.Unlock()

	var pm uint32
	if ev.Type == EventDepositApproved || ev.Type == EventDepositDeclined {
		pm = s.internMethod(ev.PaymentMethodID)
	}

	var dev uint32
	if ev.DeviceID != "" {
		ds, dsIdx := s.devices.shardFor(ev.DeviceID)
		ds.mu.Lock()
		dev = ds.intern(ev.DeviceID, dsIdx)
		_, pos := splitIndex(dev)
		d := ds.at(pos)
		d.SeePlayer(player, t, now)
		if ev.Type == EventDepositDeclined {
			d.AddDecline(t, pm, now)
		}
		ds.mu.Unlock()
	}

	ps.mu.Lock()
	defer ps.mu.Unlock()
	_, pos := splitIndex(player)
	p := ps.at(pos)
	if ev.DeviceID != "" {
		p.SeeDevice(dev, t, ev.Type == EventLogin)
	}
	switch ev.Type {
	case EventDepositDeclined:
		p.AddDecline(t, pm, now)
	case EventDepositApproved:
		p.ApplyDepositApproved(t, r.Seq, ev.AmountEURE4, pm)
	case EventBet:
		voided := s.Pending.Take(ev.EventID)
		p.ApplyBet(HashID(ev.EventID), t, ev.AmountEURE4, voided, now)
	case EventBetVoided:
		s.applyVoid(p, r, refs)
	case EventPasswordReset:
		p.ApplyPasswordReset(t)
	case EventPayoutMethodAdded:
		p.ApplyPayoutMethodAdded(t)
	}
}

// applyVoid handles a BET_VOIDED. The bet is looked up in the player's recent
// bets, then in refs. A bet later in the same batch, or not found at all, makes
// the void pending until the bet arrives. A bet of another player is ignored.
func (s *State) applyVoid(p *Player, r *Row, refs map[string]BetRef) {
	ev := r.Event
	if p.ApplyVoid(HashID(ev.VoidedEventID)) {
		return
	}
	ref, ok := refs[ev.VoidedEventID]
	switch {
	case !ok || ref.Seq > r.Seq:
		s.Pending.Add(ev.VoidedEventID, ev.OccurredAt)
	case ref.PlayerID == ev.PlayerID:
		p.ApplyVoidOutOfWindow(ref.T, ref.AmountEUR)
	}
}

func (s *State) internMethod(id string) uint32 {
	sh, sIdx := s.methods.shardFor(id)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return sh.intern(id, sIdx)
}
