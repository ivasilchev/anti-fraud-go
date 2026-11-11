package state

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/devastion/anti-fraud-go/internal/money"
)

// noLookup fails the test if a void needs the durable log.
func noLookup(t *testing.T) BetLookup {
	return func(_ context.Context, ids []string) (map[string]BetRef, error) {
		t.Errorf("unexpected bet lookup: %v", ids)
		return nil, nil
	}
}

// notInLog is a durable log that holds no bets.
func notInLog(context.Context, []string) (map[string]BetRef, error) { return nil, nil }

func dispatch(t *testing.T, s *Dispatcher, actions ...Action) {
	t.Helper()
	if err := s.Dispatch(context.Background(), &Batch{Actions: actions}); err != nil {
		t.Fatal(err)
	}
}

func eurEvent(id, player string, typ EventType, at, amount int64) *Event {
	return &Event{EventID: id, PlayerID: player, Type: typ, OccurredAt: at,
		AmountE4: amount, AmountEURE4: amount, Currency: money.EUR, PaymentMethodID: "pm1"}
}

func TestMiddlewareRunsOutermostFirst(t *testing.T) {
	var order []string
	trace := func(name string) Middleware {
		return func(_ *State, next DispatchFunc) DispatchFunc {
			return func(ctx context.Context, b *Batch) error {
				order = append(order, name+">")
				err := next(ctx, b)
				order = append(order, "<"+name)
				return err
			}
		}
	}
	s := NewDispatcher(New(), trace("a"), trace("b"))
	dispatch(t, s, Action{Seq: 1, Event: eurEvent("e1", "p1", EventBet, now, 1)})
	if want := []string{"a>", "b>", "<b", "<a"}; !slices.Equal(order, want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	if s.State().Watermark().Seq != 1 {
		t.Fatal("the root reducer did not run")
	}
}

func TestResolveVoidsLooksUpBetsMissingFromMemory(t *testing.T) {
	st := New()
	t0 := now - 30*minute
	var asked []string
	s := NewDispatcher(st, ResolveVoids(func(_ context.Context, ids []string) (map[string]BetRef, error) {
		asked = append(asked, ids...)
		return map[string]BetRef{"old-bet": {PlayerID: "p1", T: t0 + minute, AmountEUR: 300, Seq: 2}}, nil
	}))
	dispatch(t, s,
		Action{Seq: 1, Event: eurEvent("dep", "p1", EventDepositApproved, t0, 1000)},
		Action{Seq: 2, Event: eurEvent("old-bet", "p1", EventBet, t0+minute, 300)},
		// 20 minutes later: old-bet is pruned from RecentBets.
		Action{Seq: 3, Event: eurEvent("new-bet", "p1", EventBet, t0+20*minute, 500)},
	)
	dispatch(t, s, Action{Seq: 4, Event: &Event{EventID: "void", PlayerID: "p1", Type: EventBetVoided,
		OccurredAt: t0 + 20*minute, VoidedEventID: "old-bet"}})

	if !slices.Equal(asked, []string{"old-bet"}) {
		t.Fatalf("looked up %v, want [old-bet]", asked)
	}
	st.Select("p1", "", "", func(v View) {
		if v.P.TurnoverEUR != 500 {
			t.Errorf("turnover = %d, want 500 after voiding the 300 bet", v.P.TurnoverEUR)
		}
	})
	if st.Pending.Len() != 0 {
		t.Fatal("a resolved void was left pending")
	}
}

func TestFailedLookupLeavesTheStateUnchanged(t *testing.T) {
	st := New()
	boom := errors.New("db down")
	reported := 0
	s := NewDispatcher(st,
		CountLate(func(n int) { reported += n }),
		ResolveVoids(func(context.Context, []string) (map[string]BetRef, error) { return nil, boom }),
	)
	err := s.Dispatch(context.Background(), &Batch{Actions: []Action{
		{Seq: 1, Event: eurEvent("bet", "p1", EventBet, now-10*minute, 1), Now: now},
		{Seq: 2, Event: &Event{EventID: "void", PlayerID: "p1", Type: EventBetVoided, OccurredAt: now, VoidedEventID: "elsewhere"}, Now: now},
	}})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if !Equal(st, New()) {
		t.Fatal("a failed dispatch changed the state")
	}
	if reported != 0 {
		t.Fatalf("late events reported for a failed dispatch: %d", reported)
	}
}

func TestCountLate(t *testing.T) {
	reported := 0
	s := NewDispatcher(New(), CountLate(func(n int) { reported += n }))
	at := now - 10*minute
	dispatch(t, s,
		Action{Seq: 1, Event: eurEvent("e1", "p1", EventBet, at, 1), Now: at + LateThresholdMs},     // exactly 5 min: on time
		Action{Seq: 2, Event: eurEvent("e2", "p1", EventBet, at, 1), Now: at + LateThresholdMs + 1}, // late
		Action{Seq: 3, Event: eurEvent("e3", "p1", EventBet, at, 1)},                                // replayed: never late
	)
	if reported != 1 {
		t.Fatalf("late = %d, want 1", reported)
	}
}

func TestSelectReleasesTheView(t *testing.T) {
	st := New()
	dispatch(t, NewDispatcher(st), Action{Seq: 1, Event: &Event{EventID: "e1", PlayerID: "p1", Type: EventLogin,
		OccurredAt: now, DeviceID: "d1"}})
	var saw bool
	st.Select("p1", "d1", "", func(v View) { saw = v.P != nil && v.D != nil })
	if !saw {
		t.Fatal("selector did not see the player and the device")
	}
	for _, mu := range []interface{ TryLock() bool }{
		&st.players.shards[shardOf("p1")].mu, &st.devices.shards[shardOf("d1")].mu,
	} {
		if !mu.TryLock() {
			t.Fatal("Select did not release its read locks")
		}
	}
}
