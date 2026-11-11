// Package ingest is the bounded ingest queue and the single group-commit
// writer that persists events and applies the new ones to memory.
package ingest

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/devastion/anti-fraud-go/internal/metrics"
	"github.com/devastion/anti-fraud-go/internal/state"
	"github.com/devastion/anti-fraud-go/internal/store"
)

// Defaults from SPEC §3.
const (
	DefaultQueueCapacity = 50_000 // events
	DefaultFlushEvents   = 2_000
	DefaultFlushInterval = 5 * time.Millisecond
)

const (
	dbTimeout      = 10 * time.Second
	catchUpTimeout = 5 * time.Minute
	lookupAttempts = 5
	lookupBackoff  = 100 * time.Millisecond
)

// Errors returned by Submit.
var (
	ErrOverloaded = errors.New("ingest: queue is full")
	ErrClosed     = errors.New("ingest: writer is closed")
)

// Log is the durable event log the writer flushes to.
type Log interface {
	InsertEvents(ctx context.Context, events []state.Event) ([]store.Inserted, error)
	ReplayLog
}

// Result is the outcome of one submitted request.
type Result struct {
	Accepted   int
	Duplicates int
	// Stored[i] is true when the i-th submitted event was new to the log.
	// Its UUID is then the one Submit set on the caller's event.
	Stored []bool
}

type request struct {
	events  []state.Event
	now     int64
	dequeue int64 // unix ms when Run took the request off the queue
	done    chan response
}

type response struct {
	res Result
	err error
}

// Config tunes the writer. Zero fields take the defaults.
type Config struct {
	QueueCapacity int
	FlushEvents   int
	FlushInterval time.Duration
	// Now is the clock for created_at and processed_at. Nil means time.Now.
	Now func() time.Time
}

// Writer batches submitted events into one store insert per flush. Run must be
// running for Submit to complete.
type Writer struct {
	log      Log
	st       *state.State
	dispatch *state.Dispatcher
	metrics  *metrics.Metrics
	cfg      Config

	mu     sync.RWMutex // guards closed and sends on ch
	closed bool
	ch     chan *request
	queued atomic.Int64 // events submitted and not yet acknowledged

	ctrl    chan pauseRequest
	stopped chan struct{} // closed when Run returns

	// needCatchUp is set when an insert failed: it may have committed anyway,
	// so the log can hold events that are not in memory. Writer goroutine only.
	needCatchUp bool

	lastSnapshotSeq atomic.Int64 // watermark of the last snapshot written, or -1
}

type pauseRequest struct {
	fn   func() error
	done chan error
}

// New returns a writer. m may be nil.
func New(log Log, st *state.State, m *metrics.Metrics, cfg Config) *Writer {
	if cfg.QueueCapacity <= 0 {
		cfg.QueueCapacity = DefaultQueueCapacity
	}
	if cfg.FlushEvents <= 0 {
		cfg.FlushEvents = DefaultFlushEvents
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = DefaultFlushInterval
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if m == nil {
		m = &metrics.Metrics{}
	}
	w := &Writer{
		log:     log,
		st:      st,
		metrics: m,
		cfg:     cfg,
		// Every request holds at least one event, so a channel with one slot
		// per event of capacity never blocks a sender admitted by queued.
		ch:      make(chan *request, cfg.QueueCapacity),
		ctrl:    make(chan pauseRequest),
		stopped: make(chan struct{}),
	}
	// Late events are counted before voids are resolved, and both before
	// the reducers run. The writer's bet lookup never fails the dispatch.
	w.dispatch = state.NewDispatcher(st,
		state.CountLate(func(n int) { m.LateEvents.Add(int64(n)) }),
		state.ResolveVoids(func(_ context.Context, ids []string) (map[string]state.BetRef, error) {
			return w.lookupBets(ids), nil
		}),
	)
	w.lastSnapshotSeq.Store(-1)
	return w
}

// QueueDepth returns the number of events submitted but not yet acknowledged.
func (w *Writer) QueueDepth() int64 { return w.queued.Load() }

// Submit queues events, received at now (unix ms), and waits until the new
// ones are persisted and applied. It sets each event's UUID (a new UUIDv7)
// and ReceivedAt (now) before queueing. It returns ErrOverloaded without
// queueing when the queue is full. If ctx ends first, the events are still
// processed.
func (w *Writer) Submit(ctx context.Context, events []state.Event, now int64) (Result, error) {
	n := int64(len(events))
	if w.queued.Add(n) > int64(w.cfg.QueueCapacity) {
		w.queued.Add(-n)
		return Result{}, ErrOverloaded
	}
	for i := range events {
		id, err := uuid.NewV7()
		if err != nil {
			w.queued.Add(-n)
			return Result{}, fmt.Errorf("ingest: new uuid: %w", err)
		}
		events[i].UUID, events[i].ReceivedAt = id, now
	}
	req := &request{events: events, now: now, done: make(chan response, 1)}
	w.mu.RLock()
	if w.closed {
		w.mu.RUnlock()
		w.queued.Add(-n)
		return Result{}, ErrClosed
	}
	w.ch <- req
	w.mu.RUnlock()

	select {
	case r := <-req.done:
		return r.res, r.err
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}

// Close stops accepting events. Run returns once the queue is drained.
func (w *Writer) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.closed {
		w.closed = true
		close(w.ch)
	}
}

// Run is the writer loop. It flushes every FlushInterval after the first
// queued request, or as soon as FlushEvents events are queued, and returns
// after Close once every queued request has been flushed. Between flushes it
// runs the functions passed to Pause. Run must be called exactly once.
func (w *Writer) Run() {
	defer close(w.stopped)
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	var batch []*request
	for {
		var first *request
		select {
		case r, ok := <-w.ch:
			if !ok {
				return
			}
			first = r
			first.dequeue = w.cfg.Now().UnixMilli()
		case p := <-w.ctrl:
			p.done <- w.paused(p.fn)
			continue
		}
		batch = append(batch[:0], first)
		n := len(first.events)
		timer.Reset(w.cfg.FlushInterval)
	collect:
		for n < w.cfg.FlushEvents {
			select {
			case r, ok := <-w.ch:
				if !ok {
					break collect
				}
				r.dequeue = w.cfg.Now().UnixMilli()
				batch = append(batch, r)
				n += len(r.events)
			case <-timer.C:
				break collect
			}
		}
		timer.Stop()
		w.flush(batch, n)
		clear(batch)
	}
}

func (w *Writer) flush(batch []*request, n int) {
	start := time.Now()
	defer w.queued.Add(-int64(n))

	fail := func(err error) {
		for _, r := range batch {
			r.done <- response{err: err}
		}
	}
	// Memory must hold every logged event before newer ones are applied.
	if err := w.catchUp(); err != nil {
		fail(err)
		return
	}
	processed := w.cfg.Now().UnixMilli()
	events := make([]state.Event, 0, n)
	for _, r := range batch {
		for _, ev := range r.events {
			ev.CreatedAt, ev.ProcessedAt = r.dequeue, processed
			events = append(events, ev)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
	inserted, err := w.log.InsertEvents(ctx, events)
	cancel()
	if err != nil {
		slog.Error("flush failed", "events", n, "err", err)
		w.needCatchUp = true
		fail(err)
		return
	}

	// The store inserts the first occurrence of a duplicated eventId, so the
	// first occurrence in submission order is the accepted one.
	seqs := make(map[string]int64, len(inserted))
	for _, in := range inserted {
		seqs[in.EventID] = in.Seq
	}
	actions := make([]state.Action, 0, len(inserted))
	results := make([]Result, len(batch))
	k := 0 // index into events, which follows batch order
	for bi, r := range batch {
		results[bi].Stored = make([]bool, len(r.events))
		for i := range r.events {
			ev := &events[k]
			k++
			if seq, ok := seqs[ev.EventID]; ok {
				delete(seqs, ev.EventID)
				actions = append(actions, state.Action{Seq: seq, Event: ev, Now: r.now})
				results[bi].Stored[i] = true
				results[bi].Accepted++
			} else {
				results[bi].Duplicates++
			}
		}
	}
	slices.SortFunc(actions, func(a, b state.Action) int { return cmp.Compare(a.Seq, b.Seq) })

	if err := w.dispatch.Dispatch(context.Background(), &state.Batch{Actions: actions}); err != nil {
		// Committed but not applied: the next flush catches up from the log.
		slog.Error("dispatch failed", "events", len(actions), "err", err)
		w.needCatchUp = true
		fail(err)
		return
	}

	w.metrics.EventsAccepted.Add(int64(len(actions)))
	w.metrics.Duplicates.Add(int64(n - len(actions)))
	w.metrics.ObserveFlush(time.Since(start))
	for bi, r := range batch {
		r.done <- response{res: results[bi]}
	}
}

// lookupBets fetches bets for voids that are not in memory. The events are
// already committed, so a failed lookup is retried; if it keeps failing the
// voids are treated as unmatched and wait in pendingVoids.
func (w *Writer) lookupBets(ids []string) map[string]state.BetRef {
	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), dbTimeout)
		refs, err := w.log.LookupBets(ctx, ids)
		cancel()
		if err == nil {
			return refs
		}
		if attempt == lookupAttempts {
			slog.Error("bet lookup failed, voids left pending", "voids", len(ids), "err", err)
			return nil
		}
		time.Sleep(lookupBackoff)
	}
}

// catchUp replays logged events that are not in memory after a failed insert.
// The rows of a failed insert are never applied by the flush itself, so this
// is the only way they reach memory before a restart.
func (w *Writer) catchUp() error {
	if !w.needCatchUp {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), catchUpTimeout)
	defer cancel()
	n, err := Replay(ctx, w.log, w.st, 0)
	w.metrics.EventsAccepted.Add(n)
	if err != nil {
		slog.Error("catch-up after failed flush", "applied", n, "err", err)
		return err
	}
	if n > 0 {
		slog.Warn("caught up events committed by a failed flush", "events", n)
	}
	w.needCatchUp = false
	return nil
}

// paused runs fn in the writer goroutine, after catching up with the log.
func (w *Writer) paused(fn func() error) error {
	if err := w.catchUp(); err != nil {
		return err
	}
	return fn()
}

// Pause runs fn while no events are applied: in the writer goroutine between
// two flushes, or directly once Run has returned. Requests keep queueing
// meanwhile. Run must have been started.
func (w *Writer) Pause(fn func() error) error {
	p := pauseRequest{fn: fn, done: make(chan error, 1)}
	select {
	case w.ctrl <- p:
		return <-p.done
	case <-w.stopped:
		return w.paused(fn)
	}
}

// MarkSnapshot records that a snapshot at watermark seq already exists (for
// example the one loaded at startup), so Snapshot skips it.
func (w *Writer) MarkSnapshot(seq int64) { w.lastSnapshotSeq.Store(seq) }

// SnapshotInfo describes a snapshot written by Snapshot.
type SnapshotInfo struct {
	Seq     int64         // watermark
	Bytes   int64         // file size
	Pause   time.Duration // time the writer was paused for
	Elapsed time.Duration // total time, including fsync and rename
	Skipped bool          // nothing was applied since the last snapshot
}

// Snapshot writes the state to path atomically, as a consistent cut at the
// current watermark. The state is encoded while the writer is paused; fsync
// and rename happen after it resumes. Decisions are not blocked. It does
// nothing when no event was applied since the last snapshot it wrote.
func (w *Writer) Snapshot(path string) (SnapshotInfo, error) {
	start := time.Now()
	var (
		info SnapshotInfo
		sf   *state.SnapshotFile
	)
	err := w.Pause(func() error {
		pauseStart := time.Now()
		info.Seq = w.st.Watermark().Seq
		if info.Seq == w.lastSnapshotSeq.Load() {
			info.Skipped = true
			return nil
		}
		f, err := state.CreateSnapshot(path)
		if err != nil {
			return err
		}
		if err := f.Encode(w.st); err != nil {
			f.Abort()
			return err
		}
		sf = f
		info.Pause = time.Since(pauseStart)
		return nil
	})
	if err != nil || info.Skipped {
		return info, err
	}
	info.Bytes = sf.Size()
	if err := sf.Commit(); err != nil {
		return info, err
	}
	w.lastSnapshotSeq.Store(info.Seq)
	info.Elapsed = time.Since(start)
	return info, nil
}
