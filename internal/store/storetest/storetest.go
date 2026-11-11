// Package storetest is the conformance suite for store.Store implementations.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/devastion/anti-fraud-go/internal/money"
	"github.com/devastion/anti-fraud-go/internal/state"
	"github.com/devastion/anti-fraud-go/internal/store"
)

// Run tests the store.Store contract. open must return an empty store; Run
// closes it.
func Run(t *testing.T, open func(t *testing.T) store.Store) {
	tests := map[string]func(t *testing.T, s store.Store){
		"InsertSkipsDuplicates":        testInsertSkipsDuplicates,
		"InsertSkipsSecondVoid":        testInsertSkipsSecondVoid,
		"StreamRoundTripsEvents":       testStreamRoundTripsEvents,
		"StreamAllowsLookupInCallback": testStreamAllowsLookupInCallback,
		"StreamStopsOnCallbackError":   testStreamStopsOnCallbackError,
		"LookupBets":                   testLookupBets,
		"WatermarkAt":                  testWatermarkAt,
		"ImportReplacesLog":            testImportReplacesLog,
		"CountEvents":                  testCountEvents,
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			s := open(t)
			t.Cleanup(func() {
				if err := s.Close(); err != nil {
					t.Error(err)
				}
			})
			test(t, s)
		})
	}
}

var ctx = context.Background()

func ev(id string, typ state.EventType) state.Event {
	return state.Event{EventID: id, PlayerID: "p-" + id, Type: typ, OccurredAt: 1_700_000_000_000}
}

func insert(t *testing.T, s store.Store, events ...state.Event) []store.Inserted {
	t.Helper()
	in, err := s.InsertEvents(ctx, events)
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(in, func(a, b store.Inserted) int { return int(a.Seq - b.Seq) })
	return in
}

func stream(t *testing.T, s store.Store, after int64) (seqs []int64, events []state.Event) {
	t.Helper()
	err := s.StreamEvents(ctx, after, func(seq int64, ev *state.Event) error {
		seqs, events = append(seqs, seq), append(events, *ev)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return seqs, events
}

func ids(in []store.Inserted) []string {
	out := make([]string, len(in))
	for i, r := range in {
		out[i] = r.EventID
	}
	return out
}

func testInsertSkipsDuplicates(t *testing.T, s store.Store) {
	first := ev("e1", state.EventLogin)
	first.DeviceID = "d-first"
	second := first
	second.DeviceID = "d-second"
	in := insert(t, s, first, ev("e2", state.EventLogin), second)
	if got := ids(in); !slices.Equal(got, []string{"e1", "e2"}) {
		t.Fatalf("inserted %v, want [e1 e2]", got)
	}
	if in[0].Seq >= in[1].Seq {
		t.Fatalf("seqs %d, %d do not follow insertion order", in[0].Seq, in[1].Seq)
	}
	if in := insert(t, s, ev("e2", state.EventLogin), ev("e3", state.EventLogin)); !slices.Equal(ids(in), []string{"e3"}) {
		t.Fatalf("second call inserted %v, want [e3]", ids(in))
	}
	if in[1].Seq <= in[0].Seq {
		t.Fatal("seqs must increase across calls")
	}
	_, events := stream(t, s, 0)
	if len(events) != 3 || events[0].DeviceID != "d-first" {
		t.Fatalf("stored %+v, want 3 events and the first e1", events)
	}
}

func testInsertSkipsSecondVoid(t *testing.T, s store.Store) {
	v1 := ev("v1", state.EventBetVoided)
	v1.VoidedEventID = "b1"
	v2 := ev("v2", state.EventBetVoided)
	v2.VoidedEventID = "b1"
	other := ev("v3", state.EventBetVoided)
	other.VoidedEventID = "b2"
	if in := insert(t, s, v1, v2, other); !slices.Equal(ids(in), []string{"v1", "v3"}) {
		t.Fatalf("inserted %v, want [v1 v3]", ids(in))
	}
	// Only voids are unique per voidedEventId.
	login := ev("l1", state.EventLogin)
	login.VoidedEventID = "b1"
	if in := insert(t, s, login); len(in) != 1 {
		t.Fatal("a non-void event with the same voidedEventId was skipped")
	}
}

func testStreamRoundTripsEvents(t *testing.T, s store.Store) {
	full := state.Event{EventID: "e1", PlayerID: "p1", Type: state.EventDepositApproved, OccurredAt: -5,
		AmountE4: 123_4500, AmountEURE4: 113_5740, Currency: money.USD,
		DeviceID: "d1", IP: "10.0.0.1", PaymentMethodID: "pm1", VoidedEventID: "x"}
	bare := state.Event{EventID: "e2", PlayerID: "p2", Type: state.EventPasswordReset, OccurredAt: 1_700_000_000_123}
	in := insert(t, s, full, bare)
	seqs, events := stream(t, s, 0)
	if !slices.Equal(events, []state.Event{full, bare}) {
		t.Fatalf("streamed %+v\nwant     %+v", events, []state.Event{full, bare})
	}
	if !slices.Equal(seqs, []int64{in[0].Seq, in[1].Seq}) {
		t.Fatalf("streamed seqs %v, inserted %+v", seqs, in)
	}
	if _, events := stream(t, s, in[0].Seq); len(events) != 1 || events[0].EventID != "e2" {
		t.Fatalf("stream after %d = %+v, want e2 only", in[0].Seq, events)
	}
}

func testStreamAllowsLookupInCallback(t *testing.T, s store.Store) {
	var batch []state.Event
	for i := range 50 {
		b := ev(fmt.Sprint("b", i), state.EventBet)
		b.AmountE4, b.AmountEURE4, b.Currency = 10_000, 10_000, money.EUR
		batch = append(batch, b)
	}
	insert(t, s, batch...)
	n := 0
	err := s.StreamEvents(ctx, 0, func(_ int64, ev *state.Event) error {
		refs, err := s.LookupBets(ctx, []string{ev.EventID})
		if err == nil && len(refs) != 1 {
			err = fmt.Errorf("lookup of %s found %d bets", ev.EventID, len(refs))
		}
		n++
		return err
	})
	if err != nil || n != len(batch) {
		t.Fatalf("streamed %d of %d events, err %v", n, len(batch), err)
	}
}

func testStreamStopsOnCallbackError(t *testing.T, s store.Store) {
	insert(t, s, ev("e1", state.EventLogin), ev("e2", state.EventLogin))
	stop := errors.New("stop")
	calls := 0
	err := s.StreamEvents(ctx, 0, func(int64, *state.Event) error {
		calls++
		return stop
	})
	if !errors.Is(err, stop) || calls != 1 {
		t.Fatalf("err %v after %d calls, want %v after 1", err, calls, stop)
	}
}

func testLookupBets(t *testing.T, s store.Store) {
	b := ev("b1", state.EventBet)
	b.AmountE4, b.AmountEURE4, b.Currency = 20_000, 18_400, money.USD
	in := insert(t, s, ev("w1", state.EventWin), b)
	refs, err := s.LookupBets(ctx, []string{"b1", "w1", "missing"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]state.BetRef{"b1": {PlayerID: "p-b1", T: b.OccurredAt, AmountEUR: 18_400, Seq: in[1].Seq}}
	if len(refs) != 1 || refs["b1"] != want["b1"] {
		t.Fatalf("LookupBets = %+v, want %+v", refs, want)
	}
}

func testWatermarkAt(t *testing.T, s store.Store) {
	in := insert(t, s, ev("e1", state.EventLogin))
	m, ok, err := s.WatermarkAt(ctx, in[0].Seq)
	want := state.Watermark{Seq: in[0].Seq, EventID: "e1", OccurredAt: 1_700_000_000_000}
	if err != nil || !ok || m != want {
		t.Fatalf("WatermarkAt = %+v, %v, %v; want %+v", m, ok, err, want)
	}
	if _, ok, err := s.WatermarkAt(ctx, in[0].Seq+1); err != nil || ok {
		t.Fatalf("WatermarkAt past the end = %v, %v; want not found", ok, err)
	}
}

func testImportReplacesLog(t *testing.T, s store.Store) {
	insert(t, s, ev("old1", state.EventLogin), ev("old2", state.EventLogin))
	n, err := s.Import(ctx, func(add func(*state.Event) error) error {
		for _, id := range []string{"n1", "n2", "n3"} {
			e := ev(id, state.EventLogin)
			if err := add(&e); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil || n != 3 {
		t.Fatalf("Import = %d, %v; want 3", n, err)
	}
	seqs, events := stream(t, s, 0)
	if !slices.Equal(seqs, []int64{1, 2, 3}) || events[0].EventID != "n1" || events[2].EventID != "n3" {
		t.Fatalf("after import: seqs %v, events %+v", seqs, events)
	}
	// The imported log keeps de-duplicating.
	if in := insert(t, s, ev("n2", state.EventLogin), ev("n4", state.EventLogin)); !slices.Equal(ids(in), []string{"n4"}) || in[0].Seq != 4 {
		t.Fatalf("insert after import = %+v, want n4 at seq 4", in)
	}

	failed := errors.New("generator failed")
	if _, err := s.Import(ctx, func(func(*state.Event) error) error { return failed }); !errors.Is(err, failed) {
		t.Fatalf("Import error = %v, want %v", err, failed)
	}
}

func testCountEvents(t *testing.T, s store.Store) {
	insert(t, s, ev("lt-1-a", state.EventLogin), ev("lt-1-b", state.EventLogin), ev("lt-10-a", state.EventLogin),
		ev("lt-2-a", state.EventLogin), ev("x", state.EventLogin))
	for prefix, want := range map[string]int64{"": 5, "lt-1-": 2, "lt-1": 3, "lt-": 4, "nope": 0} {
		if n, err := s.CountEvents(ctx, prefix); err != nil || n != want {
			t.Errorf("CountEvents(%q) = %d, %v; want %d", prefix, n, err, want)
		}
	}
}
