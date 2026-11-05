// Package metrics holds process counters and renders them in the Prometheus
// text exposition format.
package metrics

import (
	"bytes"
	"io"
	"os"
	"runtime"
	"strconv"
	"sync/atomic"
	"time"
)

// flushBuckets are the upper bounds, in seconds, of the flush latency histogram.
var flushBuckets = [...]float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1}

// Metrics are the service counters. The zero value is ready to use.
type Metrics struct {
	EventsAccepted atomic.Int64
	Duplicates     atomic.Int64
	LateEvents     atomic.Int64
	Decisions      atomic.Int64

	rejected   codeCounters
	flushCount atomic.Int64
	flushSumUs atomic.Int64
	flushBkt   [len(flushBuckets)]atomic.Int64

	// Startup recovery, set once replay completes.
	ReplayedEvents  atomic.Int64
	ReplayMicros    atomic.Int64
	SnapshotLoadSeq atomic.Int64 // watermark of the snapshot loaded at startup, 0 if none

	// Periodic snapshots.
	Snapshots           atomic.Int64
	SnapshotFailures    atomic.Int64
	SnapshotSeq         atomic.Int64 // watermark of the last snapshot written
	SnapshotBytes       atomic.Int64
	SnapshotPauseMicros atomic.Int64 // writer pause of the last snapshot

	// queueDepth reports the number of events waiting to be flushed. It is
	// set after recovery, while /metrics is already being served.
	queueDepth atomic.Pointer[func() int64]
}

// SetQueueDepth sets the function that reports the ingest queue depth.
func (m *Metrics) SetQueueDepth(f func() int64) { m.queueDepth.Store(&f) }

// codeCounters is a fixed set of per-code counters. Codes are registered up
// front, so counting needs no lock.
type codeCounters struct {
	codes    []string
	counters []atomic.Int64
}

// RegisterRejectCodes declares the error codes counted by Reject. It must be
// called before the metrics are shared between goroutines.
func (m *Metrics) RegisterRejectCodes(codes []string) {
	m.rejected.codes = codes
	m.rejected.counters = make([]atomic.Int64, len(codes))
}

// Reject counts a rejected request by error code. Unknown codes are ignored.
func (m *Metrics) Reject(code string) {
	for i, c := range m.rejected.codes {
		if c == code {
			m.rejected.counters[i].Add(1)
			return
		}
	}
}

// ObserveFlush records the duration of one flush.
func (m *Metrics) ObserveFlush(d time.Duration) {
	m.flushCount.Add(1)
	m.flushSumUs.Add(d.Microseconds())
	s := d.Seconds()
	for i, b := range flushBuckets {
		if s <= b {
			m.flushBkt[i].Add(1)
		}
	}
}

// WritePrometheus writes all metrics in the Prometheus text format.
func (m *Metrics) WritePrometheus(w io.Writer) error {
	var b []byte
	counter := func(name, help string, v int64) {
		b = append(b, "# HELP "+name+" "+help+"\n# TYPE "+name+" counter\n"+name+" "...)
		b = strconv.AppendInt(b, v, 10)
		b = append(b, '\n')
	}
	counter("events_accepted_total", "Events newly persisted and applied.", m.EventsAccepted.Load())
	counter("duplicate_events_total", "Events skipped as duplicates.", m.Duplicates.Load())
	counter("late_events_total", "Events applied more than 5 minutes after they occurred.", m.LateEvents.Load())
	counter("decisions_total", "Decisions returned.", m.Decisions.Load())

	b = append(b, "# HELP rejected_requests_total Requests rejected with an error response.\n# TYPE rejected_requests_total counter\n"...)
	for i, c := range m.rejected.codes {
		b = append(b, `rejected_requests_total{code="`+c+`"} `...)
		b = strconv.AppendInt(b, m.rejected.counters[i].Load(), 10)
		b = append(b, '\n')
	}

	b = append(b, "# HELP flush_duration_seconds Group-commit flush latency.\n# TYPE flush_duration_seconds histogram\n"...)
	for i, ub := range flushBuckets {
		b = append(b, `flush_duration_seconds_bucket{le="`...)
		b = strconv.AppendFloat(b, ub, 'g', -1, 64)
		b = append(b, `"} `...)
		b = strconv.AppendInt(b, m.flushBkt[i].Load(), 10)
		b = append(b, '\n')
	}
	count := m.flushCount.Load()
	b = append(b, `flush_duration_seconds_bucket{le="+Inf"} `...)
	b = strconv.AppendInt(b, count, 10)
	b = append(b, "\nflush_duration_seconds_sum "...)
	b = strconv.AppendFloat(b, float64(m.flushSumUs.Load())/1e6, 'g', -1, 64)
	b = append(b, "\nflush_duration_seconds_count "...)
	b = strconv.AppendInt(b, count, 10)
	b = append(b, '\n')

	gauge := func(name, help string, v int64) {
		b = append(b, "# HELP "+name+" "+help+"\n# TYPE "+name+" gauge\n"+name+" "...)
		b = strconv.AppendInt(b, v, 10)
		b = append(b, '\n')
	}
	seconds := func(name, help string, us int64) {
		b = append(b, "# HELP "+name+" "+help+"\n# TYPE "+name+" gauge\n"+name+" "...)
		b = strconv.AppendFloat(b, float64(us)/1e6, 'g', -1, 64)
		b = append(b, '\n')
	}
	gauge("startup_replayed_events", "Events replayed from Postgres at startup.", m.ReplayedEvents.Load())
	seconds("startup_recovery_seconds", "Time to load the snapshot and replay the tail at startup.", m.ReplayMicros.Load())
	gauge("startup_snapshot_seq", "Watermark of the snapshot loaded at startup (0: full replay).", m.SnapshotLoadSeq.Load())
	counter("snapshots_total", "Snapshots written.", m.Snapshots.Load())
	counter("snapshot_failures_total", "Snapshots that failed.", m.SnapshotFailures.Load())
	gauge("snapshot_seq", "Watermark of the last snapshot written.", m.SnapshotSeq.Load())
	gauge("snapshot_bytes", "Size of the last snapshot written.", m.SnapshotBytes.Load())
	seconds("snapshot_pause_seconds", "How long the last snapshot paused the writer.", m.SnapshotPauseMicros.Load())

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	gauge("go_memstats_heap_inuse_bytes", "Bytes in in-use heap spans.", int64(ms.HeapInuse))
	gauge("go_memstats_heap_alloc_bytes", "Bytes of allocated heap objects.", int64(ms.HeapAlloc))
	gauge("go_memstats_sys_bytes", "Bytes obtained from the OS by the Go runtime.", int64(ms.Sys))
	counter("go_gc_cycles_total", "Completed GC cycles.", int64(ms.NumGC))
	if rss, ok := residentBytes(); ok {
		gauge("process_resident_memory_bytes", "Resident set size (Linux only).", rss)
	}

	var depth int64
	if f := m.queueDepth.Load(); f != nil {
		depth = (*f)()
	}
	b = append(b, "# HELP ingest_queue_depth Events waiting to be flushed.\n# TYPE ingest_queue_depth gauge\ningest_queue_depth "...)
	b = strconv.AppendInt(b, depth, 10)
	b = append(b, '\n')

	_, err := w.Write(b)
	return err
}

// residentBytes reads the resident set size from /proc/self/statm.
func residentBytes() (int64, bool) {
	b, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, false
	}
	f := bytes.Fields(b)
	if len(f) < 2 {
		return 0, false
	}
	pages, err := strconv.ParseInt(string(f[1]), 10, 64)
	if err != nil {
		return 0, false
	}
	return pages * int64(os.Getpagesize()), true
}
