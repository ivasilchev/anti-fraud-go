package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/devastion/anti-fraud-go/internal/ingest"
	"github.com/devastion/anti-fraud-go/internal/money"
	"github.com/devastion/anti-fraud-go/internal/state"
)

// voidEdgeCases covers every way a void is resolved: from recent bets, from
// Postgres (bet pruned from memory), pending until the bet arrives, against
// another player's bet, and a second void of the same bet.
func voidEdgeCases(prefix string) (before, after []ev) {
	p, q := prefix+"vp", prefix+"vq"
	at := func(d time.Duration) time.Time { return t0.Add(-d) }
	before = []ev{
		deposit(prefix+"vdep", p, "100.00", prefix+"vpm", at(4*time.Minute)),
		bet(prefix+"recent", p, "40.00", at(3*time.Minute)),
		void(prefix+"v-recent", p, prefix+"recent", at(2*time.Minute)),
		// 30 minutes late: appended, then pruned by the next bet of p.
		bet(prefix+"old", p, "5.00", at(30*time.Minute)),
		bet(prefix+"next", p, "70.00", at(2*time.Minute)),
		void(prefix+"v-old", p, prefix+"old", at(time.Minute)), // resolved via Postgres
		void(prefix+"v-early", p, prefix+"later", at(time.Minute)),
		bet(prefix+"other", q, "9.00", at(time.Minute)),
		void(prefix+"v-other", p, prefix+"other", at(time.Minute)), // another player's bet: ignored
		void(prefix+"v-orphan", p, prefix+"never", at(time.Minute)),
	}
	after = []ev{
		bet(prefix+"later", p, "60.00", at(90*time.Second)), // arrives voided
		void(prefix+"v-recent-again", p, prefix+"recent", at(30*time.Second)),
		bet(prefix+"after", p, "20.00", at(20*time.Second)),
	}
	return before, after
}

// quiesced runs fn while the app's writer applies nothing.
func (a *app) quiesced(fn func()) {
	a.t.Helper()
	if err := a.w.Pause(func() error { fn(); return nil }); err != nil {
		a.t.Fatal(err)
	}
}

func TestSnapshotPlusTailReplayEqualsFullReplay(t *testing.T) {
	a := newApp(t)
	ctx := context.Background()

	voidsBefore, voidsAfter := voidEdgeCases("x-")
	for _, e := range scenario("a-") {
		a.ingest(t0, e)
	}
	for _, e := range voidsBefore {
		a.ingest(t0, e)
	}
	a.ingest(t0, []ev{bet("dup", "a-p", "1.00", t0), bet("dup", "a-p", "1.00", t0)})
	if n := a.st.Pending.Len(); n != 2 {
		t.Fatalf("pending voids before the snapshot = %d, want 2", n)
	}
	// Turnover 70 ("next") of a 100 deposit.
	a.expectDecision(t0, withdrawal("x-vp", "x-vpm"), "HOLD R2_CASHOUT_WITHOUT_PLAY")

	path := filepath.Join(t.TempDir(), "snapshot.bin")
	info, err := a.w.Snapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("snapshot: %+v", info)

	tail := append(scenario("b-"), voidsAfter...)
	a.ingest(t0, tail) // one batch
	for _, e := range scenario("c-") {
		a.ingest(t0, e)
	}
	tailLen := int64(len(tail) + len(scenario("c-")) - 1) // the second void of "recent" is a duplicate
	if n := a.st.Pending.Len(); n != 1 {
		t.Fatalf("pending voids after the tail = %d, want 1 (the orphan)", n)
	}
	// Turnover 90: "later" arrived voided, else it would be 150 and ALLOW.
	a.expectDecision(t0, withdrawal("x-vp", "x-vpm"), "HOLD R2_CASHOUT_WITHOUT_PLAY")
	a.ingest(t0, bet("x-top-up", "x-vp", "10.00", t0))
	a.expectDecision(t0, withdrawal("x-vp", "x-vpm"), "ALLOW")
	tailLen++

	for _, chunk := range []int{1, 3, 0} {
		full, stats, err := ingest.Recover(ctx, a.db, "", chunk)
		if err != nil {
			t.Fatal(err)
		}
		if stats.SnapshotSeq != 0 || stats.Replayed != int64(a.countEvents("")) {
			t.Fatalf("chunk %d: full replay stats %+v, want all %d events", chunk, stats, a.countEvents(""))
		}
		fromSnap, stats, err := ingest.Recover(ctx, a.db, path, chunk)
		if err != nil {
			t.Fatal(err)
		}
		if stats.SnapshotSeq != info.Seq || stats.Replayed != tailLen || stats.SnapshotSkipped != "" {
			t.Fatalf("chunk %d: snapshot recovery stats %+v, want snapshot %d + %d events", chunk, stats, info.Seq, tailLen)
		}
		if !state.Equal(fromSnap, full) {
			t.Fatalf("chunk %d: snapshot + tail replay differs from full replay", chunk)
		}
		a.quiesced(func() {
			if !state.Equal(full, a.st) {
				t.Fatalf("chunk %d: full replay differs from the live state", chunk)
			}
		})
	}
}

func TestSnapshotFromAnotherLogIsIgnored(t *testing.T) {
	a := newApp(t)
	ctx := context.Background()
	for _, e := range scenario("a-") {
		a.ingest(t0, e)
	}
	path := filepath.Join(t.TempDir(), "snapshot.bin")
	if _, err := a.w.Snapshot(path); err != nil {
		t.Fatal(err)
	}

	check := func(name string) {
		t.Helper()
		st, stats, err := ingest.Recover(ctx, a.db, path, 0)
		if err != nil {
			t.Fatal(err)
		}
		if stats.SnapshotSeq != 0 || stats.SnapshotSkipped == "" {
			t.Fatalf("%s: stats %+v, want the snapshot skipped", name, stats)
		}
		full, _, err := ingest.Recover(ctx, a.db, "", 0)
		if err != nil {
			t.Fatal(err)
		}
		if !state.Equal(st, full) {
			t.Fatalf("%s: state differs from a full replay", name)
		}
		t.Logf("%s: %s", name, stats.SnapshotSkipped)
	}

	reseed := func(events []state.Event) {
		t.Helper()
		if _, err := a.db.Pool().Exec(ctx, "TRUNCATE events RESTART IDENTITY"); err != nil {
			t.Fatal(err)
		}
		if _, err := a.db.InsertEvents(ctx, events); err != nil {
			t.Fatal(err)
		}
	}

	// The log is reseeded with the same eventIds at the same seqs but other
	// times, as a second make seed with the same parameters does.
	reseed(logEvents(t, scenario("a-"), time.Second))
	want := a.st.Watermark()
	if m, ok, err := a.db.WatermarkAt(ctx, want.Seq); err != nil || !ok || m.EventID != want.EventID {
		t.Fatalf("setup: event at the watermark = %+v, %v, %v; want eventId %q", m, ok, err, want.EventID)
	}
	check("reseeded log, same eventIds")

	// The log is reseeded: same seqs, other events (make seed truncates).
	reseed(logEvents(t, scenario("z-"), 0))
	check("reseeded log")

	// The log was truncated and is now shorter than the snapshot.
	if _, err := a.db.Pool().Exec(ctx, "TRUNCATE events"); err != nil {
		t.Fatal(err)
	}
	check("truncated log")

	// A corrupt snapshot.
	if err := os.WriteFile(path, []byte("AFGOSNAP garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	check("corrupt snapshot")
}

// logEvents converts API events to log events, shifted in time by shift.
func logEvents(t *testing.T, events []ev, shift time.Duration) []state.Event {
	t.Helper()
	out := make([]state.Event, 0, len(events))
	for _, e := range events {
		at, err := time.Parse(time.RFC3339Nano, e["occurredAt"].(string))
		if err != nil {
			t.Fatal(err)
		}
		le := state.Event{EventID: e["eventId"].(string), PlayerID: e["playerId"].(string), OccurredAt: at.Add(shift).UnixMilli()}
		le.Type, _ = state.ParseEventType(e["type"].(string))
		if amt, ok := e["amount"].(string); ok {
			le.AmountE4, _ = money.ParseE4(amt)
			le.AmountEURE4, le.Currency = le.AmountE4, money.EUR
		}
		le.DeviceID, _ = e["deviceId"].(string)
		le.PaymentMethodID, _ = e["paymentMethodId"].(string)
		le.VoidedEventID, _ = e["voidedEventId"].(string)
		out = append(out, le)
	}
	return out
}
