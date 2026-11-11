package state

import (
	"context"
	"math/rand/v2"
	"strconv"
	"testing"

	"github.com/devastion/anti-fraud-go/internal/money"
)

// benchMix is the load test's event mix, in percent.
var benchMix = []struct {
	typ EventType
	pct int
}{
	{EventLogin, 15}, {EventDepositApproved, 8}, {EventDepositDeclined, 8},
	{EventBet, 40}, {EventWin, 21}, {EventBetVoided, 1}, {EventPasswordReset, 1},
	{EventPayoutMethodAdded, 1}, {EventWithdrawalRequested, 5},
}

// benchBatches builds n flush-sized batches over players players, 3 events
// per ms (the load test's 3,000/s). OccurredAt holds the offset from the start
// of the batch.
func benchBatches(n, size, players int) [][]Event {
	var types []EventType
	for _, m := range benchMix {
		for range m.pct {
			types = append(types, m.typ)
		}
	}
	pids := make([]string, players)
	for i := range pids {
		pids[i] = "p" + strconv.Itoa(i)
	}
	rng := rand.New(rand.NewPCG(1, 2))
	out := make([][]Event, n)
	for k := range out {
		batch := make([]Event, size)
		lastBet := -1
		for i := range batch {
			pi := rng.IntN(players)
			typ := types[rng.IntN(len(types))]
			if typ == EventBetVoided && lastBet < 0 {
				typ = EventBet // nothing to void yet in this batch
			}
			e := Event{
				EventID:    "b" + strconv.Itoa(k) + "-" + strconv.Itoa(i),
				PlayerID:   pids[pi],
				Type:       typ,
				OccurredAt: int64(i / 3),
			}
			switch typ {
			case EventLogin:
				e.DeviceID = "d" + strconv.Itoa(pi)
			case EventDepositApproved, EventDepositDeclined:
				e.DeviceID = "d" + strconv.Itoa(pi)
				e.PaymentMethodID = "pm" + strconv.Itoa(pi*3+rng.IntN(3))
			case EventPayoutMethodAdded:
				e.PaymentMethodID = "pm" + strconv.Itoa(pi*3+rng.IntN(3))
			case EventBet:
				lastBet = i
			case EventBetVoided:
				e.VoidedEventID, e.PlayerID = batch[lastBet].EventID, batch[lastBet].PlayerID
			}
			switch typ {
			case EventDepositApproved, EventDepositDeclined, EventBet, EventWin, EventWithdrawalRequested:
				e.AmountE4, e.AmountEURE4, e.Currency = 100000, 100000, money.EUR
			}
			batch[i] = e
		}
		out[k] = batch
	}
	return out
}

// BenchmarkApply measures the writer's dispatch path (ResolveVoids + Reduce) for
// one 2,000-event flush of the load test's mix and rate over 1M players. The
// 16 batches repeat every ~11 s of event time, so eventIds recur inside the
// bets window; voids still resolve within their own batch.
func BenchmarkApply(b *testing.B) {
	const size = 2000
	batches := benchBatches(16, size, 1_000_000)
	s := New()
	d := NewDispatcher(s, ResolveVoids(func(context.Context, []string) (map[string]BetRef, error) { return nil, nil }))
	ctx := context.Background()
	actions := make([]Action, size)
	events := make([]Event, size)
	var seq int64
	base := now
	apply := func(k int) {
		copy(events, batches[k%len(batches)])
		for i := range events {
			events[i].OccurredAt += base
			seq++
			actions[i] = Action{Seq: seq, Event: &events[i], Now: events[i].OccurredAt}
		}
		if err := d.Dispatch(ctx, &Batch{Actions: actions}); err != nil {
			b.Fatal(err)
		}
		base += size / 3
	}
	// Warm up: 10 minutes at 3,000 events/s, so the retention windows are full.
	for k := range 900 {
		apply(k)
	}
	b.ReportAllocs()
	k := 0
	for b.Loop() {
		apply(k)
		k++
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*size), "ns/event")
}
