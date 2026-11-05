package ingest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/devastion/anti-fraud-go/internal/money"
	"github.com/devastion/anti-fraud-go/internal/state"
	"github.com/devastion/anti-fraud-go/internal/store"
)

// fakeLog mimics the INSERT ... ON CONFLICT DO NOTHING semantics in memory.
// If block is set, InsertEvents waits until it is closed. If err is set,
// InsertEvents fails, after committing the events if commitOnErr is set.
type fakeLog struct {
	mu          sync.Mutex
	seq         int64
	ids         map[string]bool
	logged      []loggedEvent
	block       chan struct{}
	err         error
	commitOnErr bool
}

type loggedEvent struct {
	seq int64
	ev  state.Event
}

func (f *fakeLog) InsertEvents(_ context.Context, events []state.Event) ([]store.Inserted, error) {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil && !f.commitOnErr {
		return nil, f.err
	}
	var out []store.Inserted
	for _, ev := range events {
		if f.ids[ev.EventID] {
			continue
		}
		f.ids[ev.EventID] = true
		f.seq++
		f.logged = append(f.logged, loggedEvent{f.seq, ev})
		out = append(out, store.Inserted{Seq: f.seq, EventID: ev.EventID})
	}
	if f.err != nil {
		return nil, f.err
	}
	return out, nil
}

func (f *fakeLog) LookupBets(context.Context, []string) (map[string]state.BetRef, error) {
	return nil, nil
}

func (f *fakeLog) StreamEvents(_ context.Context, after int64, fn func(int64, *state.Event) error) error {
	f.mu.Lock()
	logged := slices.Clone(f.logged)
	f.mu.Unlock()
	for _, l := range logged {
		if l.seq > after {
			if err := fn(l.seq, &l.ev); err != nil {
				return err
			}
		}
	}
	return nil
}

func startWriter(t *testing.T, log *fakeLog, cfg Config) *Writer {
	t.Helper()
	return startWriterWithState(t, log, state.New(), cfg)
}

func startWriterWithState(t *testing.T, log *fakeLog, st *state.State, cfg Config) *Writer {
	t.Helper()
	if log.ids == nil {
		log.ids = map[string]bool{}
	}
	w := New(log, st, nil, cfg)
	done := make(chan struct{})
	go func() {
		w.Run()
		close(done)
	}()
	t.Cleanup(func() {
		w.Close()
		<-done
	})
	return w
}

func bets(ids ...string) []state.Event {
	out := make([]state.Event, len(ids))
	for i, id := range ids {
		out[i] = state.Event{EventID: id, PlayerID: "p1", Type: state.EventBet, OccurredAt: 1,
			AmountE4: 10000, AmountEURE4: 10000, Currency: money.EUR}
	}
	return out
}

func TestSubmitCountsDuplicatesWithinAndAcrossRequests(t *testing.T) {
	w := startWriter(t, &fakeLog{}, Config{})
	ctx := context.Background()
	res, err := w.Submit(ctx, bets("a", "b", "a"), 1)
	if err != nil || res != (Result{Accepted: 2, Duplicates: 1}) {
		t.Fatalf("got %+v, %v", res, err)
	}
	res, err = w.Submit(ctx, bets("b", "c"), 1)
	if err != nil || res != (Result{Accepted: 1, Duplicates: 1}) {
		t.Fatalf("got %+v, %v", res, err)
	}
	if d := w.QueueDepth(); d != 0 {
		t.Fatalf("queue depth = %d after acknowledgement, want 0", d)
	}
}

func TestSubmitRejectsWhenQueueIsFull(t *testing.T) {
	log := &fakeLog{block: make(chan struct{})}
	w := startWriter(t, log, Config{QueueCapacity: 3, FlushInterval: time.Millisecond})
	ctx := context.Background()

	errc := make(chan error, 1)
	go func() {
		_, err := w.Submit(ctx, bets("a", "b", "c"), 1)
		errc <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for w.QueueDepth() != 3 {
		if time.Now().After(deadline) {
			t.Fatal("first request was never queued")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := w.Submit(ctx, bets("d"), 1); !errors.Is(err, ErrOverloaded) {
		t.Fatalf("got %v, want ErrOverloaded", err)
	}
	if d := w.QueueDepth(); d != 3 {
		t.Fatalf("rejected request changed queue depth to %d", d)
	}
	close(log.block)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if _, err := w.Submit(ctx, bets("d"), 1); err != nil {
		t.Fatalf("after drain: %v", err)
	}
}

func TestSubmitReportsFlushErrors(t *testing.T) {
	w := startWriter(t, &fakeLog{err: errors.New("db down")}, Config{})
	if _, err := w.Submit(context.Background(), bets("a"), 1); err == nil {
		t.Fatal("want an error")
	}
	if d := w.QueueDepth(); d != 0 {
		t.Fatalf("queue depth = %d, want 0", d)
	}
}

func TestSubmitAfterCloseFails(t *testing.T) {
	w := New(&fakeLog{ids: map[string]bool{}}, state.New(), nil, Config{})
	w.Close()
	w.Run() // returns immediately: the queue is closed and empty
	if _, err := w.Submit(context.Background(), bets("a"), 1); !errors.Is(err, ErrClosed) {
		t.Fatalf("got %v, want ErrClosed", err)
	}
}

func TestFailedFlushThatCommittedIsCaughtUpBeforeTheNextFlush(t *testing.T) {
	log := &fakeLog{err: errors.New("connection lost after commit"), commitOnErr: true}
	st := state.New()
	w := startWriterWithState(t, log, st, Config{})
	ctx := context.Background()
	if _, err := w.Submit(ctx, bets("a", "b"), 1); err == nil {
		t.Fatal("want the flush error")
	}
	if seq := st.Watermark().Seq; seq != 0 {
		t.Fatalf("watermark = %d after a failed flush, want 0", seq)
	}

	log.mu.Lock()
	log.err = nil
	log.mu.Unlock()
	// The client retries a and b and sends c: a and b are duplicates now, but
	// the catch-up applies them before c.
	res, err := w.Submit(ctx, bets("a", "b", "c"), 1)
	if err != nil || res != (Result{Accepted: 1, Duplicates: 2}) {
		t.Fatalf("got %+v, %v", res, err)
	}
	if m := st.Watermark(); m.Seq != 3 || m.EventID != "c" {
		t.Fatalf("watermark = %d %q, want 3 \"c\"", m.Seq, m.EventID)
	}
	v := st.Acquire("p1", "", "")
	defer v.Release()
	if v.P == nil || len(v.P.RecentBets) != 3 {
		t.Fatalf("player state = %+v, want 3 bets", v.P)
	}
}

func TestSnapshotIsAConsistentCutAndSkipsWhenUnchanged(t *testing.T) {
	log := &fakeLog{}
	st := state.New()
	w := startWriterWithState(t, log, st, Config{})
	ctx := context.Background()
	if _, err := w.Submit(ctx, bets("a", "b"), 1); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "snapshot.bin")
	info, err := w.Snapshot(path)
	if err != nil || info.Skipped || info.Seq != 2 || info.Bytes == 0 {
		t.Fatalf("got %+v, %v", info, err)
	}
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary file left behind: %v", err)
	}
	if info, err := w.Snapshot(path); err != nil || !info.Skipped {
		t.Fatalf("second snapshot = %+v, %v; want skipped", info, err)
	}
	loaded, err := state.LoadSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Equal(loaded, st) {
		t.Fatal("loaded snapshot differs from the live state")
	}

	if _, err := w.Submit(ctx, bets("c"), 1); err != nil {
		t.Fatal(err)
	}
	if info, err := w.Snapshot(path); err != nil || info.Skipped || info.Seq != 3 {
		t.Fatalf("third snapshot = %+v, %v", info, err)
	}
}

func TestPauseAfterRunReturnedRunsDirectly(t *testing.T) {
	w := New(&fakeLog{ids: map[string]bool{}}, state.New(), nil, Config{})
	w.Close()
	w.Run()
	ran := false
	if err := w.Pause(func() error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("ran = %v, err = %v", ran, err)
	}
}

func TestReplayIsIndependentOfChunkSize(t *testing.T) {
	log := &fakeLog{ids: map[string]bool{}}
	var events []state.Event
	for i := range 50 {
		p := "p" + string(rune('a'+i%5))
		events = append(events, state.Event{EventID: "e" + string(rune('A'+i)), PlayerID: p, Type: state.EventBet,
			OccurredAt: int64(i) * 60_000, AmountE4: 10000, AmountEURE4: 10000, Currency: money.EUR})
	}
	if _, err := log.InsertEvents(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	ref := state.New()
	if _, err := Replay(context.Background(), log, ref, 1000); err != nil {
		t.Fatal(err)
	}
	for _, chunk := range []int{1, 3, 7} {
		st := state.New()
		n, err := Replay(context.Background(), log, st, chunk)
		if err != nil || n != 50 {
			t.Fatalf("chunk %d: n = %d, err = %v", chunk, n, err)
		}
		if !state.Equal(st, ref) {
			t.Fatalf("chunk %d: state differs from a single-chunk replay", chunk)
		}
	}
}
