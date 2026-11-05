package state

// PendingVoidsRetentionMs is how long a void waits for its bet.
const PendingVoidsRetentionMs = 60 * 60 * 1000

// PendingVoids holds voids that arrived before their bet, keyed by the bet's
// eventId. Not safe for concurrent use; only the writer touches it.
type PendingVoids struct {
	m map[string]int64 // bet eventId -> void occurredAt (unix ms)
}

// NewPendingVoids returns an empty set.
func NewPendingVoids() *PendingVoids {
	return &PendingVoids{m: make(map[string]int64)}
}

// Add records a void for betID that occurred at t.
func (pv *PendingVoids) Add(betID string, t int64) { pv.m[betID] = t }

// Take removes betID and reports whether a void was pending for it.
func (pv *PendingVoids) Take(betID string) bool {
	if _, ok := pv.m[betID]; !ok {
		return false
	}
	delete(pv.m, betID)
	return true
}

// Prune drops voids that occurred more than PendingVoidsRetentionMs before now.
func (pv *PendingVoids) Prune(now int64) {
	cutoff := now - PendingVoidsRetentionMs
	for id, t := range pv.m {
		if t < cutoff {
			delete(pv.m, id)
		}
	}
}

// Len returns the number of pending voids.
func (pv *PendingVoids) Len() int { return len(pv.m) }
