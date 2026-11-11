package integration

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/devastion/anti-fraud-go/internal/gen"
	"github.com/devastion/anti-fraud-go/internal/store/sqlite"
)

func TestGeneratedDataSetLoadsAndVerifies(t *testing.T) {
	dbPath := newDatabase(t)
	ctx := context.Background()
	db, err := sqlite.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Leftovers must be truncated, and seq must restart at 1.
	if _, err := db.DB().ExecContext(ctx, `INSERT INTO events (event_id, player_id, type, occurred_at) VALUES ('old', 'x', 1, 0)`); err != nil {
		t.Fatal(err)
	}

	cfg := gen.Config{Players: 3_000, Events: 40_000, Days: 90, Seed: 42, Now: time.Now()}
	m, stats, err := gen.Load(ctx, db, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Events != int64(cfg.Events) {
		t.Fatalf("loaded %d events, want %d", stats.Events, cfg.Events)
	}
	var n, minSeq, maxSeq, outOfOrder int64
	if err := db.DB().QueryRowContext(ctx, `SELECT count(*), min(seq), max(seq) FROM events`).Scan(&n, &minSeq, &maxSeq); err != nil {
		t.Fatal(err)
	}
	if n != int64(cfg.Events) || minSeq != 1 || maxSeq != n {
		t.Fatalf("count %d, seq %d..%d; want %d rows with seq 1..%d", n, minSeq, maxSeq, cfg.Events, cfg.Events)
	}
	if err := db.DB().QueryRowContext(ctx, `SELECT count(*) FROM (
		SELECT occurred_at < lag(occurred_at) OVER (ORDER BY seq) AS back FROM events) s WHERE back`).Scan(&outOfOrder); err != nil {
		t.Fatal(err)
	}
	if outOfOrder != 0 {
		t.Fatalf("%d rows have an earlier occurred_at than the previous seq", outOfOrder)
	}

	p := startServer(t, dbPath, "")
	p.recovery(0, int64(cfg.Events))
	ctxWait, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	client := &http.Client{Timeout: 10 * time.Second}
	if err := gen.WaitReady(ctxWait, client, p.url); err != nil {
		t.Fatal(err)
	}
	for _, r := range gen.Verify(ctx, client, p.url, m) {
		if !r.OK() {
			t.Errorf("%s (%s): got %v (err %v), want %v", r.Case.Name, r.Case.About, r.Got, r.Err, r.Case.Expected)
		}
	}
}
