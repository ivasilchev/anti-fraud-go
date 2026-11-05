package integration

import (
	"testing"
	"time"
)

// Each test sets up a deposit of 100 EUR, so R2 fires once turnover drops below 100.

func TestVoidAfterBetRemovesTurnover(t *testing.T) {
	a := newApp(t)
	now := t0
	a.ingest(now, deposit("dep", "p1", "100.00", "pm1", now.Add(-3*time.Minute)))
	a.ingest(now, bet("b1", "p1", "100.00", now.Add(-2*time.Minute)))
	a.expectDecision(now, withdrawal("p1", "pm1"), "ALLOW")

	a.ingest(now, void("v1", "p1", "b1", now.Add(-time.Minute)))
	a.expectDecision(now, withdrawal("p1", "pm1"), "HOLD R2_CASHOUT_WITHOUT_PLAY")
}

func TestVoidBeforeBetIsPending(t *testing.T) {
	a := newApp(t)
	now := t0
	a.ingest(now, deposit("dep", "p1", "100.00", "pm1", now.Add(-3*time.Minute)))
	a.ingest(now, void("v1", "p1", "b1", now.Add(-time.Minute)))
	a.ingest(now, bet("b1", "p1", "100.00", now.Add(-2*time.Minute)))
	a.expectDecision(now, withdrawal("p1", "pm1"), "HOLD R2_CASHOUT_WITHOUT_PLAY")
	if n := a.st.Pending.Len(); n != 0 {
		t.Fatalf("pending voids = %d, want 0 after the bet arrived", n)
	}
}

func TestVoidBeforeBetInSameBatch(t *testing.T) {
	a := newApp(t)
	now := t0
	a.ingest(now, []ev{
		deposit("dep", "p1", "100.00", "pm1", now.Add(-3*time.Minute)),
		void("v1", "p1", "b1", now.Add(-time.Minute)),
		bet("b1", "p1", "100.00", now.Add(-2*time.Minute)),
	})
	a.expectDecision(now, withdrawal("p1", "pm1"), "HOLD R2_CASHOUT_WITHOUT_PLAY")
}

func TestVoidOfBetNoLongerInMemoryIsLookedUp(t *testing.T) {
	a := newApp(t)
	start := t0.Add(-20 * time.Minute)
	a.ingest(start, deposit("dep", "p1", "100.00", "pm1", start))
	a.ingest(start, bet("b1", "p1", "100.00", start.Add(time.Second)))

	// 20 minutes later a new bet prunes b1 from the recent-bets window.
	now := t0
	a.ingest(now, bet("b2", "p1", "1.00", now))
	a.expectDecision(now, withdrawal("p1", "pm1"), "ALLOW")

	// b1 must be found in Postgres; its amount leaves turnover at 1.
	a.ingest(now, void("v1", "p1", "b1", now))
	a.expectDecision(now, withdrawal("p1", "pm1"), "HOLD R2_CASHOUT_WITHOUT_PLAY")
	if n := a.st.Pending.Len(); n != 0 {
		t.Fatalf("pending voids = %d, want 0", n)
	}
}

func TestSecondVoidOfSameBetIsDuplicate(t *testing.T) {
	a := newApp(t)
	now := t0
	a.ingest(now, []ev{
		deposit("dep", "p1", "100.00", "pm1", now.Add(-3*time.Minute)),
		bet("b1", "p1", "50.00", now.Add(-2*time.Minute)),
		bet("b2", "p1", "100.00", now.Add(-2*time.Minute)),
	})
	if res := a.ingest(now, void("v1", "p1", "b1", now.Add(-time.Minute))); res.Accepted != 1 {
		t.Fatalf("first void: got %+v", res)
	}
	// Turnover is now exactly 100: no R2.
	a.expectDecision(now, withdrawal("p1", "pm1"), "ALLOW")

	// A different eventId voiding the same bet hits the partial unique index.
	if res := a.ingest(now, void("v2", "p1", "b1", now)); res != (ingestResult{Duplicates: 1}) {
		t.Fatalf("second void: got %+v, want 1 duplicate", res)
	}
	// Two voids of the same bet in one batch: one is a duplicate.
	a.ingest(now, bet("b3", "p1", "10.00", now))
	if res := a.ingest(now, []ev{void("v3", "p1", "b3", now), void("v4", "p1", "b3", now)}); res != (ingestResult{Accepted: 1, Duplicates: 1}) {
		t.Fatalf("voids in one batch: got %+v", res)
	}
	a.expectDecision(now, withdrawal("p1", "pm1"), "ALLOW")
	if n := a.countEvents("type = 8"); n != 2 {
		t.Fatalf("voids in db = %d, want 2", n)
	}
}

func TestVoidOfAnotherPlayersBetIsIgnored(t *testing.T) {
	a := newApp(t)
	now := t0
	a.ingest(now, []ev{
		deposit("dep", "p1", "100.00", "pm1", now.Add(-3*time.Minute)),
		bet("b1", "p1", "100.00", now.Add(-2*time.Minute)),
	})
	a.ingest(now, void("v1", "p2", "b1", now.Add(-time.Minute)))
	a.expectDecision(now, withdrawal("p1", "pm1"), "ALLOW")
	if n := a.st.Pending.Len(); n != 0 {
		t.Fatalf("pending voids = %d, want 0 (bet exists, belongs to p1)", n)
	}
}

func TestVoidOfBetBeforeLastDepositKeepsTurnover(t *testing.T) {
	a := newApp(t)
	now := t0
	a.ingest(now, []ev{
		bet("b0", "p1", "500.00", now.Add(-4*time.Minute)),
		deposit("dep", "p1", "100.00", "pm1", now.Add(-3*time.Minute)),
		bet("b1", "p1", "100.00", now.Add(-2*time.Minute)),
	})
	a.ingest(now, void("v0", "p1", "b0", now.Add(-time.Minute)))
	a.expectDecision(now, withdrawal("p1", "pm1"), "ALLOW")
}
