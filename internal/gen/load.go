package gen

import (
	"context"
	"encoding/binary"
	"math/rand/v2"
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
//
// Each event gets log metadata as if it had arrived live: ReceivedAt is its
// occurredAt, CreatedAt and ProcessedAt are the load start, and its UUIDv7
// carries occurredAt and random bits from cfg.Seed, so a reseed with the same
// seed gives the same UUIDs.
func Load(ctx context.Context, db store.Store, cfg Config, progress func(events int64)) (*Manifest, LoadStats, error) {
	start := time.Now()
	loadedAt := start.UnixMilli()
	rng := rand.New(rand.NewPCG(cfg.Seed, 0x00dd))
	var m *Manifest
	n, err := db.Import(ctx, func(add func(*state.Event) error) error {
		var count int64
		var err error
		m, err = Generate(cfg, func(ev *state.Event) error {
			ev.UUID = uuidV7(ev.OccurredAt, rng)
			ev.ReceivedAt, ev.CreatedAt, ev.ProcessedAt = ev.OccurredAt, loadedAt, loadedAt
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

// uuidV7 returns a UUIDv7 (RFC 9562) with the timestamp ms and 74 random bits
// from rng.
func uuidV7(ms int64, rng *rand.Rand) [16]byte {
	var u [16]byte
	binary.BigEndian.PutUint64(u[0:8], uint64(ms)<<16|rng.Uint64()&0xffff)
	binary.BigEndian.PutUint64(u[8:16], rng.Uint64())
	u[6] = 0x70 | u[6]&0x0f // version 7
	u[8] = 0x80 | u[8]&0x3f // variant 10
	return u
}
