package state

import (
	"context"
	"maps"
)

// The state follows a Redux-like data flow:
//
//	writer/replay ──Dispatch(Batch)──▶ middleware… ──▶ Reduce (root reducer)
//	                                                     ├─ reducePending
//	                                                     ├─ reduceDevice
//	                                                     └─ reducePlayer
//	decisions ──Select──▶ View ──▶ rules.Evaluate (selectors)
//
// Actions are persisted events, dispatched once each, in seq order, by a single
// goroutine. Middleware may read the state and do I/O, and it enriches or
// observes a batch. Only the reducers mutate the state, and they do no I/O.
// Unlike Redux, reducers update the state in place: copying per-entity state
// for 3,000 events/s over 1M players would cost GC time and memory for no
// rule-visible benefit. Readers see a consistent entity through shard locks.
//
// In Redux terms the store is a State together with its Dispatcher. The type
// is not called Store because store.Store is the durable event log.

// Action is one logged event dispatched to the state.
type Action struct {
	Seq   int64
	Event *Event
	// Now is the server time (unix ms) the event was received at, used only to
	// count late events. It is 0 for replayed events, which are never counted.
	Now int64
}

// Batch is the payload of one dispatch.
type Batch struct {
	// Actions are sorted by seq and newer than the state's watermark.
	Actions []Action
	// Bets resolves the bets referenced by the BET_VOIDED actions. ResolveVoids
	// fills it; a void whose bet is missing here waits in the pending voids.
	Bets map[string]BetRef
}

// DispatchFunc sends a batch to the reducers. If it returns an error, the root
// reducer did not run and the state is unchanged.
type DispatchFunc func(ctx context.Context, b *Batch) error

// Middleware wraps the next step of a dispatch. It gets the state to read
// from (getState in Redux terms) and must not mutate it.
type Middleware func(st *State, next DispatchFunc) DispatchFunc

// Dispatcher is a dispatch pipeline over a State: its middleware chain ending
// in the root reducer. A state may have more than one dispatcher (the live
// writer and a replay resolve voids differently), but only one may dispatch
// at a time.
type Dispatcher struct {
	state    *State
	dispatch DispatchFunc
}

// NewDispatcher returns a dispatcher over st. The first middleware is the outermost: it
// sees the batch first and its next step is the second middleware.
func NewDispatcher(st *State, mw ...Middleware) *Dispatcher {
	var d DispatchFunc = func(_ context.Context, b *Batch) error {
		st.Reduce(b)
		return nil
	}
	for i := len(mw) - 1; i >= 0; i-- {
		d = mw[i](st, d)
	}
	return &Dispatcher{state: st, dispatch: d}
}

// Dispatch runs b through the middleware and the root reducer.
func (d *Dispatcher) Dispatch(ctx context.Context, b *Batch) error { return d.dispatch(ctx, b) }

// State returns the state the dispatcher reduces into.
func (d *Dispatcher) State() *State { return d.state }

// BetLookup fetches bets by eventId from the durable log.
type BetLookup func(ctx context.Context, ids []string) (map[string]BetRef, error)

// ResolveVoids fills Batch.Bets for the voids in a batch: from the batch
// itself, then from memory, then through lookup for the rest. A lookup error
// stops the dispatch before any action is reduced.
func ResolveVoids(lookup BetLookup) Middleware {
	return func(st *State, next DispatchFunc) DispatchFunc {
		return func(ctx context.Context, b *Batch) error {
			refs, missing := st.PrepareVoids(b.Actions)
			if len(missing) > 0 {
				found, err := lookup(ctx, missing)
				if err != nil {
					return err
				}
				maps.Copy(refs, found)
			}
			if b.Bets == nil {
				b.Bets = refs
			} else {
				maps.Copy(b.Bets, refs)
			}
			return next(ctx, b)
		}
	}
}

// CountLate reports, for each batch, the number of actions received more than
// LateThresholdMs after they occurred. Replayed actions (Now == 0) never count.
func CountLate(report func(n int)) Middleware {
	return func(_ *State, next DispatchFunc) DispatchFunc {
		return func(ctx context.Context, b *Batch) error {
			late := 0
			for i := range b.Actions {
				if a := &b.Actions[i]; a.Now != 0 && a.Now-a.Event.OccurredAt > LateThresholdMs {
					late++
				}
			}
			if err := next(ctx, b); err != nil {
				return err
			}
			if late > 0 {
				report(late)
			}
			return nil
		}
	}
}
