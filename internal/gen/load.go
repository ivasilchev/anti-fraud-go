package gen

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/devastion/anti-fraud-go/internal/money"
	"github.com/devastion/anti-fraud-go/internal/state"
)

var copyColumns = []string{"event_id", "player_id", "type", "occurred_at", "amount_e4", "amount_eur_e4",
	"currency", "device_id", "ip", "payment_method_id", "voided_event_id"}

const copyBatch = 8192

// LoadStats describes a load.
type LoadStats struct {
	Events int64
	Copy   time.Duration // generation and COPY, which run concurrently
	Vacuum time.Duration // VACUUM ANALYZE after the load
}

// Load truncates the events table (restarting seq at 1) and streams the
// generated data set into it with one COPY, in occurredAt order. progress,
// if not nil, is called after every million events. The schema must exist.
func Load(ctx context.Context, pool *pgxpool.Pool, cfg Config, progress func(events int64)) (*Manifest, LoadStats, error) {
	var stats LoadStats
	if _, err := pool.Exec(ctx, "TRUNCATE events RESTART IDENTITY"); err != nil {
		return nil, stats, fmt.Errorf("truncate: %w", err)
	}
	start := time.Now()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	batches := make(chan []state.Event, 4)
	free := make(chan []state.Event, 6)
	for range cap(free) {
		free <- make([]state.Event, 0, copyBatch)
	}
	type genResult struct {
		m   *Manifest
		err error
	}
	genDone := make(chan genResult, 1)
	go func() {
		defer close(batches)
		buf := <-free
		send := func() error {
			select {
			case batches <- buf:
			case <-ctx.Done():
				return ctx.Err()
			}
			select {
			case buf = <-free:
			case <-ctx.Done():
				return ctx.Err()
			}
			buf = buf[:0]
			return nil
		}
		m, err := Generate(cfg, func(ev *state.Event) error {
			buf = append(buf, *ev)
			if len(buf) == copyBatch {
				return send()
			}
			return nil
		})
		if err == nil && len(buf) > 0 {
			err = send()
		}
		genDone <- genResult{m, err}
	}()

	src := &copySource{batches: batches, free: free, row: make([]any, len(copyColumns)), progress: progress}
	n, copyErr := pool.CopyFrom(ctx, pgx.Identifier{"events"}, copyColumns, src)
	cancel() // unblocks the generator if COPY failed
	res := <-genDone
	switch {
	case res.err != nil && copyErr == nil:
		return nil, stats, fmt.Errorf("generate: %w", res.err)
	case copyErr != nil:
		return nil, stats, fmt.Errorf("copy: %w", copyErr)
	}
	stats.Events, stats.Copy = n, time.Since(start)

	start = time.Now()
	// Sets hint bits and the visibility map now rather than during the
	// service's first replay, and gives the planner statistics.
	if _, err := pool.Exec(context.Background(), "VACUUM (ANALYZE) events"); err != nil {
		return nil, stats, fmt.Errorf("vacuum: %w", err)
	}
	stats.Vacuum = time.Since(start)
	return res.m, stats, nil
}

// copySource feeds batches of events to CopyFrom, recycling the buffers.
type copySource struct {
	batches  <-chan []state.Event
	free     chan<- []state.Event
	batch    []state.Event
	i        int
	n        int64
	row      []any
	progress func(int64)
}

func (s *copySource) Next() bool {
	s.i++
	if s.i >= len(s.batch) {
		if s.batch != nil {
			s.free <- s.batch
		}
		b, ok := <-s.batches
		if !ok {
			s.batch = nil
			return false
		}
		s.batch, s.i = b, 0
	}
	s.n++
	if s.progress != nil && s.n%1_000_000 == 0 {
		s.progress(s.n)
	}
	return true
}

// Values reuses one row slice: CopyFrom encodes it before calling Next again.
func (s *copySource) Values() ([]any, error) {
	ev := &s.batch[s.i]
	r := s.row
	r[0], r[1], r[2], r[3] = ev.EventID, ev.PlayerID, int16(ev.Type), time.UnixMilli(ev.OccurredAt)
	if ev.Currency != money.NoCurrency {
		r[4], r[5], r[6] = ev.AmountE4, ev.AmountEURE4, ev.Currency.String()
	} else {
		r[4], r[5], r[6] = nil, nil, nil
	}
	r[7], r[8], r[9], r[10] = null(ev.DeviceID), null(ev.IP), null(ev.PaymentMethodID), null(ev.VoidedEventID)
	return r, nil
}

func (s *copySource) Err() error { return nil }

func null(s string) any {
	if s == "" {
		return nil
	}
	return s
}
