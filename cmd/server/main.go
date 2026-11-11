// Command server runs the anti-fraud decision service.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/devastion/anti-fraud-go/internal/api"
	"github.com/devastion/anti-fraud-go/internal/ingest"
	"github.com/devastion/anti-fraud-go/internal/metrics"
	"github.com/devastion/anti-fraud-go/internal/state"
	"github.com/devastion/anti-fraud-go/internal/store"
	"github.com/devastion/anti-fraud-go/internal/store/sqlite"
)

const (
	defaultDBPath           = "/data/events.db"
	defaultSnapshotPath     = "/data/snapshot.bin"
	defaultSnapshotInterval = 5 * time.Minute
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "healthcheck":
			os.Exit(healthcheck())
		case "count-events":
			os.Exit(countEvents(os.Args[2:]))
		}
	}
	if err := run(); err != nil {
		slog.Error("server failed", "err", err)
		os.Exit(1)
	}
}

type config struct {
	dbPath, addr     string
	snapshotPath     string // "" disables snapshots
	snapshotInterval time.Duration
	testMode, pprof  bool
}

func loadConfig() (config, error) {
	c := config{
		dbPath:           os.Getenv("DB_PATH"),
		addr:             os.Getenv("ADDR"),
		snapshotPath:     defaultSnapshotPath,
		snapshotInterval: defaultSnapshotInterval,
		testMode:         os.Getenv("TEST_MODE") == "true",
		pprof:            os.Getenv("PPROF") == "1",
	}
	if c.dbPath == "" {
		c.dbPath = defaultDBPath
	}
	if c.addr == "" {
		c.addr = ":8080"
	}
	if p, ok := os.LookupEnv("SNAPSHOT_PATH"); ok {
		c.snapshotPath = p
	}
	if v := os.Getenv("SNAPSHOT_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return c, fmt.Errorf("SNAPSHOT_INTERVAL %q: want a positive duration", v)
		}
		c.snapshotInterval = d
	}
	if c.snapshotPath != "" {
		if fi, err := os.Stat(filepath.Dir(c.snapshotPath)); err != nil || !fi.IsDir() {
			slog.Warn("snapshot directory is missing, snapshots disabled", "path", c.snapshotPath)
			c.snapshotPath = ""
		}
	}
	return c, nil
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, err := openStore(ctx, cfg.dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	m := &metrics.Metrics{}
	m.RegisterRejectCodes(api.RejectCodes())
	srv := &api.Server{Metrics: m, Now: time.Now, TestMode: cfg.testMode}
	mux := srv.Handler()
	if cfg.pprof {
		mux.HandleFunc("GET /debug/pprof/", pprof.Index)
		mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
	}
	httpSrv := &http.Server{
		Addr:              cfg.addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	// Listen during recovery: /health/live is 200, /health/ready and the API are 503.
	errc := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", cfg.addr, "testMode", cfg.testMode)
		errc <- httpSrv.ListenAndServe()
	}()
	shutdownHTTP := func() {
		srv.SetReady(false)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			slog.Error("http shutdown", "err", err)
		}
	}

	type recovered struct {
		st    *state.State
		stats ingest.RecoverStats
		err   error
	}
	recc := make(chan recovered, 1)
	go func() {
		st, stats, err := ingest.Recover(ctx, db, cfg.snapshotPath, 0)
		recc <- recovered{st, stats, err}
	}()
	var rec recovered
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		slog.Info("interrupted during recovery")
		shutdownHTTP()
		return nil
	case rec = <-recc:
	}
	if rec.err != nil {
		shutdownHTTP()
		return fmt.Errorf("recovery: %w", rec.err)
	}
	st, stats := rec.st, rec.stats
	// Drop append slack and return replay garbage to the OS before serving.
	st.Trim()
	debug.FreeOSMemory()
	players, devices, methods := st.Counts()
	slog.Info("recovered", "snapshotSeq", stats.SnapshotSeq, "replayed", stats.Replayed, "watermark", st.Watermark().Seq,
		"players", players, "devices", devices, "methods", methods, "duration", stats.Duration)
	m.ReplayedEvents.Store(stats.Replayed)
	m.ReplayMicros.Store(stats.Duration.Microseconds())
	m.SnapshotLoadSeq.Store(stats.SnapshotSeq)

	w := ingest.New(db, st, m, ingest.Config{})
	m.SetQueueDepth(w.QueueDepth)
	if stats.SnapshotSeq > 0 && stats.Replayed == 0 {
		w.MarkSnapshot(stats.SnapshotSeq)
	}
	writerDone := make(chan struct{})
	go func() {
		w.Run()
		close(writerDone)
	}()

	snapCtx, stopSnapshots := context.WithCancel(context.Background())
	var snapWG sync.WaitGroup
	if cfg.snapshotPath != "" {
		snapWG.Go(func() { snapshotLoop(snapCtx, w, m, cfg.snapshotPath, cfg.snapshotInterval) })
	}

	srv.State, srv.Ingest = st, w
	srv.SetReady(true)
	slog.Info("ready")

	select {
	case err = <-errc:
	case <-ctx.Done():
	}
	slog.Info("shutting down")
	shutdownHTTP()
	stopSnapshots()
	snapWG.Wait()
	w.Close()
	<-writerDone
	if cfg.snapshotPath != "" {
		takeSnapshot(w, m, cfg.snapshotPath)
	}
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}

func snapshotLoop(ctx context.Context, w *ingest.Writer, m *metrics.Metrics, path string, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			takeSnapshot(w, m, path)
		}
	}
}

func takeSnapshot(w *ingest.Writer, m *metrics.Metrics, path string) {
	info, err := w.Snapshot(path)
	switch {
	case err != nil:
		m.SnapshotFailures.Add(1)
		slog.Error("snapshot failed", "path", path, "err", err)
	case info.Skipped:
		slog.Debug("snapshot skipped, nothing applied since the last one", "seq", info.Seq)
	default:
		m.Snapshots.Add(1)
		m.SnapshotSeq.Store(info.Seq)
		m.SnapshotBytes.Store(info.Bytes)
		m.SnapshotPauseMicros.Store(info.Pause.Microseconds())
		slog.Info("snapshot written", "seq", info.Seq, "bytes", info.Bytes, "pause", info.Pause, "elapsed", info.Elapsed)
	}
}

// openStore opens the event log. It is the only place that names the
// storage engine.
func openStore(ctx context.Context, path string) (store.Store, error) {
	s, err := sqlite.Open(ctx, path)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// countEvents prints how many logged events have an eventId starting with
// args[0] (all events without an argument). The load test uses it to check
// for lost events; it is safe to run next to the server.
func countEvents(args []string) int {
	prefix := ""
	if len(args) > 0 {
		prefix = args[0]
	}
	path := os.Getenv("DB_PATH")
	if path == "" {
		path = defaultDBPath
	}
	ctx := context.Background()
	db, err := openStore(ctx, path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer db.Close()
	n, err := db.CountEvents(ctx, prefix)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println(n)
	return 0
}

// healthcheck is the container healthcheck (distroless has no curl): it
// exits 0 when /health/ready on the local port returns 200.
func healthcheck() int {
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + addr + "/health/ready")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "not ready:", resp.Status)
		return 1
	}
	return 0
}
