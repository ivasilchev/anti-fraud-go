package gen

import (
	"context"
	"time"

	"github.com/devastion/anti-fraud-go/internal/state"
	"github.com/devastion/anti-fraud-go/internal/store"
)

// LoadStats describes a load.
type LoadStats struct {
	Events   int64
	Duration time.Duration // generation and import, which run in step
}

// Load replaces the log in db with the generated data set, in occurredAt
// order, so seqs start at 1 and follow time. progress, if not nil, is called
// after every million events.
func Load(ctx context.Context, db store.Store, cfg Config, progress func(events int64)) (*Manifest, LoadStats, error) {
	start := time.Now()
	var m *Manifest
	n, err := db.Import(ctx, func(add func(*state.Event) error) error {
		var count int64
		var err error
		m, err = Generate(cfg, func(ev *state.Event) error {
			if err := add(ev); err != nil {
				return err
			}
			if count++; progress != nil && count%1_000_000 == 0 {
				progress(count)
			}
			return nil
		})
		return err
	})
	if err != nil {
		return nil, LoadStats{}, err
	}
	return m, LoadStats{Events: n, Duration: time.Since(start)}, nil
}
