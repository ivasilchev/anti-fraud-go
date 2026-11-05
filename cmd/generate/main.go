// Command generate loads the synthetic data set into Postgres and writes the
// manifest of planted scenarios. It truncates the events table first.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"time"

	"github.com/devastion/anti-fraud-go/internal/gen"
	"github.com/devastion/anti-fraud-go/internal/store"
)

func main() {
	var (
		players  = flag.Int("players", 1_000_000, "number of background players")
		events   = flag.Int("events", 10_000_000, "total number of events, planted ones included")
		days     = flag.Int("days", 90, "days of history")
		seed     = flag.Uint64("seed", 42, "random seed")
		dsn      = flag.String("dsn", os.Getenv("DATABASE_URL"), "Postgres connection string (default $DATABASE_URL)")
		manifest = flag.String("manifest", "manifest.json", "where to write the manifest")
	)
	flag.Parse()
	if err := run(*dsn, *manifest, gen.Config{Players: *players, Events: *events, Days: *days, Seed: *seed, Now: time.Now()}); err != nil {
		slog.Error("generate failed", "err", err)
		os.Exit(1)
	}
}

func run(dsn, manifestPath string, cfg gen.Config) error {
	if dsn == "" {
		return fmt.Errorf("--dsn or DATABASE_URL is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	db, err := store.Open(ctx, dsn) // creates the schema if needed
	if err != nil {
		return err
	}
	defer db.Close()

	slog.Info("generating", "players", cfg.Players, "events", cfg.Events, "days", cfg.Days, "seed", cfg.Seed)
	start := time.Now()
	m, stats, err := gen.Load(ctx, db.Pool(), cfg, func(n int64) {
		el := time.Since(start)
		slog.Info("progress", "events", n, "elapsed", el.Round(time.Millisecond),
			"eventsPerSec", int(float64(n)/el.Seconds()))
	})
	if err != nil {
		return err
	}
	if err := m.Write(manifestPath); err != nil {
		return err
	}
	slog.Info("done", "events", stats.Events, "copy", stats.Copy.Round(time.Millisecond),
		"eventsPerSec", int(float64(stats.Events)/stats.Copy.Seconds()),
		"vacuumAnalyze", stats.Vacuum.Round(time.Millisecond), "total", time.Since(start).Round(time.Millisecond),
		"plantedCases", len(m.Cases), "manifest", manifestPath)
	return nil
}
