package ingest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"time"

	"github.com/devastion/anti-fraud-go/internal/state"
)

// DefaultReplayChunk is how many replayed events are applied at once. Voids in
// a chunk share one bet lookup.
const DefaultReplayChunk = 8192

// ReplayLog is the part of the durable log that replay reads.
type ReplayLog interface {
	StreamEvents(ctx context.Context, afterSeq int64, fn func(seq int64, ev *state.Event) error) error
	LookupBets(ctx context.Context, ids []string) (map[string]state.BetRef, error)
}

// Replay applies every logged event after the state's watermark, in seq order,
// streaming them from the log in chunks of chunk events (0 means
// DefaultReplayChunk). It returns the number of events applied. On error, the
// state is consistent up to its watermark and Replay can be called again.
// The caller must be the only mutator of st.
//
// Replay applies exactly what the live writer applied, so the result does not
// depend on chunking: voids are resolved against the bet's seq, a bet logged
// after its void leaves the void pending, and pruning follows the log clock.
func Replay(ctx context.Context, log ReplayLog, st *state.State, chunk int) (int64, error) {
	if chunk <= 0 {
		chunk = DefaultReplayChunk
	}
	after := st.Watermark().Seq
	var (
		n      int64
		events = make([]state.Event, 0, chunk)
		seqs   = make([]int64, 0, chunk)
		rows   = make([]state.Row, 0, chunk)
	)
	apply := func() error {
		rows = rows[:0]
		for i := range events {
			rows = append(rows, state.Row{Seq: seqs[i], Event: &events[i]})
		}
		refs, missing := st.PrepareVoids(rows)
		if len(missing) > 0 {
			found, err := log.LookupBets(ctx, missing)
			if err != nil {
				return err
			}
			maps.Copy(refs, found)
		}
		st.Apply(rows, refs)
		n += int64(len(rows))
		events, seqs = events[:0], seqs[:0]
		return nil
	}
	err := log.StreamEvents(ctx, after, func(seq int64, ev *state.Event) error {
		events = append(events, *ev)
		seqs = append(seqs, seq)
		if len(events) == chunk {
			return apply()
		}
		return nil
	})
	if err == nil && len(events) > 0 {
		err = apply()
	}
	if err != nil {
		return n, fmt.Errorf("replay after seq %d: %w", after, err)
	}
	return n, nil
}

// RecoveryLog is the durable log as seen by startup recovery.
type RecoveryLog interface {
	ReplayLog
	// WatermarkAt returns the event at seq, or ok == false if there is none.
	WatermarkAt(ctx context.Context, seq int64) (mark state.Watermark, ok bool, err error)
}

// RecoverStats describes a startup recovery.
type RecoverStats struct {
	// SnapshotSeq is the watermark of the snapshot used, or 0 if none was.
	SnapshotSeq int64
	// SnapshotSkipped says why an existing snapshot was not used, if it wasn't.
	SnapshotSkipped string
	Replayed        int64
	Duration        time.Duration
}

// Recover rebuilds the state: it loads the snapshot at snapshotPath (if any,
// and if it matches the log) and replays the events logged after it. An empty
// snapshotPath means a full replay. A snapshot that is corrupt, of another
// version, or taken from a different log (the event at its watermark seq has
// another eventId or occurredAt) is ignored: the log is the source of truth.
func Recover(ctx context.Context, log RecoveryLog, snapshotPath string, chunk int) (*state.State, RecoverStats, error) {
	start := time.Now()
	var stats RecoverStats
	st, skipped, err := loadSnapshot(ctx, log, snapshotPath)
	if err != nil {
		return nil, stats, err
	}
	stats.SnapshotSkipped = skipped
	if st == nil {
		st = state.New()
	} else {
		stats.SnapshotSeq = st.Watermark().Seq
	}
	stats.Replayed, err = Replay(ctx, log, st, chunk)
	stats.Duration = time.Since(start)
	return st, stats, err
}

// loadSnapshot returns the snapshot state, or nil and a reason to skip it.
func loadSnapshot(ctx context.Context, log RecoveryLog, path string) (*state.State, string, error) {
	if path == "" {
		return nil, "", nil
	}
	st, err := state.LoadSnapshot(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, "", nil
	case err != nil:
		slog.Warn("ignoring snapshot", "path", path, "err", err)
		return nil, err.Error(), nil
	}
	want := st.Watermark()
	if want.Seq == 0 {
		return st, "", nil
	}
	got, ok, err := log.WatermarkAt(ctx, want.Seq)
	if err != nil {
		return nil, "", err
	}
	if !ok || got != want {
		reason := fmt.Sprintf("snapshot watermark seq %d (%q at %d) is not in the log (found %q at %d)",
			want.Seq, want.EventID, want.OccurredAt, got.EventID, got.OccurredAt)
		slog.Warn("ignoring snapshot", "path", path, "reason", reason)
		return nil, reason, nil
	}
	return st, "", nil
}
