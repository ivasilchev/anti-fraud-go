package integration

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestDuplicateInSameBatch(t *testing.T) {
	a := newApp(t)
	now := t0
	res := a.ingest(now, []ev{
		decline("e1", "p1", "pm1", now.Add(-time.Minute)),
		decline("e1", "p1", "pm2", now.Add(-time.Minute)),
		decline("e2", "p1", "pm2", now.Add(-time.Minute)),
	})
	if res != (ingestResult{Accepted: 2, Duplicates: 1}) {
		t.Fatalf("got %+v, want 2 accepted, 1 duplicate", res)
	}
	if n := a.countEvents(""); n != 2 {
		t.Fatalf("events in db = %d, want 2", n)
	}
	// The first occurrence wins: e1 is stored with pm1.
	if n := a.countEvents("event_id = 'e1' AND payment_method_id = 'pm1'"); n != 1 {
		t.Fatal("first occurrence of e1 was not the one stored")
	}
	if got := a.metrics.Duplicates.Load(); got != 1 {
		t.Fatalf("duplicate_events_total = %d, want 1", got)
	}
}

func TestDuplicateAcrossBatchesIsNotAppliedTwice(t *testing.T) {
	a := newApp(t)
	now := t0
	batch := []ev{
		decline("d1", "p1", "pm1", now.Add(-4*time.Minute)),
		decline("d2", "p1", "pm2", now.Add(-3*time.Minute)),
		decline("d3", "p1", "pm3", now.Add(-2*time.Minute)),
		decline("d4", "p1", "pm1", now.Add(-1*time.Minute)),
	}
	if res := a.ingest(now, batch); res != (ingestResult{Accepted: 4}) {
		t.Fatalf("first batch: got %+v", res)
	}
	if res := a.ingest(now, batch); res != (ingestResult{Duplicates: 4}) {
		t.Fatalf("resent batch: got %+v", res)
	}
	// Single-object form, overlapping the batch.
	if res := a.ingest(now, batch[0]); res != (ingestResult{Duplicates: 1}) {
		t.Fatalf("resent single event: got %+v", res)
	}
	// 4 declines (not 8 or 9): R1 needs 5.
	a.expectDecision(now, depositAttempt("p1", "pm9"), "ALLOW")

	a.ingest(now, decline("d5", "p1", "pm2", now))
	a.expectDecision(now, depositAttempt("p1", "pm9"), "REVIEW R1_CARD_TESTING")
	if n := a.countEvents(""); n != 5 {
		t.Fatalf("events in db = %d, want 5", n)
	}
}

func TestDuplicateWithDifferentPayloadFirstWins(t *testing.T) {
	a := newApp(t)
	now := t0
	a.ingest(now, deposit("dep", "p1", "100.00", "pm1", now.Add(-3*time.Minute)))
	if res := a.ingest(now, deposit("dep", "p1", "10.00", "pm1", now.Add(-3*time.Minute))); res.Duplicates != 1 {
		t.Fatalf("got %+v, want a duplicate", res)
	}
	a.ingest(now, bet("b1", "p1", "50.00", now.Add(-2*time.Minute)))
	// Turnover 50 is below the first deposit (100), not the second (10).
	a.expectDecision(now, withdrawal("p1", "pm1"), "HOLD R2_CASHOUT_WITHOUT_PLAY")
}

func TestConcurrentDuplicateSubmissions(t *testing.T) {
	a := newApp(t)
	now := t0
	const clients = 32
	batch := make([]ev, 4)
	for i := range batch {
		batch[i] = decline(fmt.Sprintf("c%d", i), "p1", fmt.Sprintf("pm%d", i%3), now.Add(-time.Duration(i+1)*time.Minute))
	}

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		accepted int
		dups     int
	)
	start := make(chan struct{})
	for c := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			// Half send the batch, half send its events one by one.
			var res ingestResult
			if c%2 == 0 {
				res = a.ingest(now, batch)
			} else {
				for _, e := range batch {
					r := a.ingest(now, e)
					res.Accepted += r.Accepted
					res.Duplicates += r.Duplicates
				}
			}
			mu.Lock()
			accepted += res.Accepted
			dups += res.Duplicates
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()

	if accepted != len(batch) || dups != clients*len(batch)-len(batch) {
		t.Fatalf("accepted=%d duplicates=%d, want %d and %d", accepted, dups, len(batch), clients*len(batch)-len(batch))
	}
	if n := a.countEvents(""); n != len(batch) {
		t.Fatalf("events in db = %d, want %d", n, len(batch))
	}
	a.expectDecision(now, depositAttempt("p1", "pmX"), "ALLOW")
}
