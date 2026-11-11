package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devastion/anti-fraud-go/internal/store/sqlite"
)

var (
	buildOnce sync.Once
	serverBin string
	buildErr  error
)

// buildServer compiles cmd/server once per test run.
func buildServer(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "antifraud-server")
		if err != nil {
			buildErr = err
			return
		}
		serverBin = filepath.Join(dir, "server")
		out, err := exec.Command("go", "build", "-o", serverBin, "github.com/devastion/anti-fraud-go/cmd/server").CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("go build: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return serverBin
}

// proc is the service running as a child process.
type proc struct {
	*app
	cmd    *exec.Cmd
	logs   *syncBuffer
	exited chan struct{} // closed once the process has exited
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// startServer runs the server on the database file dbPath and waits until it is ready.
func startServer(t *testing.T, dbPath, snapshotPath string, extraEnv ...string) *proc {
	t.Helper()
	addr := "127.0.0.1:" + strconv.Itoa(freePort(t))
	cmd := exec.Command(buildServer(t))
	cmd.Env = append(os.Environ(), "DB_PATH="+dbPath, "ADDR="+addr, "TEST_MODE=true",
		"SNAPSHOT_PATH="+snapshotPath, "SNAPSHOT_INTERVAL=1h")
	cmd.Env = append(cmd.Env, extraEnv...)
	logs := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = logs, logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &proc{app: &app{t: t, url: "http://" + addr, dbPath: dbPath}, cmd: cmd, logs: logs}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-exited
		if t.Failed() {
			t.Logf("server logs:\n%s", logs.String())
		}
	})
	p.exited = exited

	deadline := time.Now().Add(60 * time.Second)
	for {
		select {
		case <-exited:
			t.Fatalf("server exited during startup:\n%s", logs.String())
		default:
		}
		if code, _ := p.get("/health/ready"); code == http.StatusOK {
			return p
		}
		if time.Now().After(deadline) {
			t.Fatalf("server not ready after 60s:\n%s", logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (p *proc) get(path string) (int, string) {
	resp, err := httpClient.Get(p.url + path)
	if err != nil {
		return 0, ""
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	_, _ = b.ReadFrom(resp.Body)
	return resp.StatusCode, b.String()
}

// kill sends SIGKILL: no graceful shutdown, no final snapshot.
func (p *proc) kill() {
	p.t.Helper()
	if err := p.cmd.Process.Kill(); err != nil {
		p.t.Fatal(err)
	}
	<-p.exited
}

// stop sends SIGTERM and waits for a clean exit.
func (p *proc) stop() {
	p.t.Helper()
	if err := p.cmd.Process.Signal(os.Interrupt); err != nil {
		p.t.Fatal(err)
	}
	select {
	case <-p.exited:
	case <-time.After(30 * time.Second):
		p.t.Fatal("server did not stop within 30s")
	}
	if code := p.cmd.ProcessState.ExitCode(); code != 0 {
		p.t.Fatalf("server exited with %d:\n%s", code, p.logs.String())
	}
}

// metric returns the value of an unlabelled metric.
func (p *proc) metric(name string) int64 {
	p.t.Helper()
	code, body := p.get("/metrics")
	if code != http.StatusOK {
		p.t.Fatalf("GET /metrics = %d", code)
	}
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), name+" "); ok {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				p.t.Fatalf("metric %s = %q", name, v)
			}
			return n
		}
	}
	p.t.Fatalf("metric %s not found", name)
	return 0
}

// recovery checks how the server recovered: from which snapshot, replaying how many events.
func (p *proc) recovery(wantSnapshotSeq, wantReplayed int64) {
	p.t.Helper()
	if got := p.metric("startup_snapshot_seq"); got != wantSnapshotSeq {
		p.t.Fatalf("startup_snapshot_seq = %d, want %d", got, wantSnapshotSeq)
	}
	if got := p.metric("startup_replayed_events"); got != wantReplayed {
		p.t.Fatalf("startup_replayed_events = %d, want %d", got, wantReplayed)
	}
}

func queryInt(t *testing.T, dbPath, sql string, args ...any) int64 {
	t.Helper()
	db, err := sqlite.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int64
	if err := db.DB().QueryRowContext(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func maxSeq(t *testing.T, dbPath string) int64 {
	t.Helper()
	return queryInt(t, dbPath, "SELECT coalesce(max(seq), 0) FROM events")
}

// countAfter counts logged events after seq. Seqs have gaps: a skipped
// duplicate still consumes one.
func countAfter(t *testing.T, dbPath string, seq int64) int64 {
	t.Helper()
	return queryInt(t, dbPath, "SELECT count(*) FROM events WHERE seq > ?", seq)
}

func allDecisions(a *app, prefixes ...string) []string {
	var out []string
	for _, p := range prefixes {
		out = append(out, scenarioDecisions(a, p)...)
		out = append(out, a.decide(t0, withdrawal(p+"vp", p+"vpm")).String())
	}
	return out
}

func TestRestartAfterKillGivesSameDecisions(t *testing.T) {
	dbPath := newDatabase(t)
	snap := filepath.Join(t.TempDir(), "snapshot.bin")

	p := startServer(t, dbPath, snap)
	p.recovery(0, 0)
	before, after := voidEdgeCases("a-")
	for _, e := range append(scenario("a-"), before...) {
		p.ingest(t0, e)
	}
	// Graceful stop writes a snapshot of everything so far.
	p.stop()
	snapSeq := maxSeq(t, dbPath)

	p = startServer(t, dbPath, snap)
	p.recovery(snapSeq, 0)
	tail := append(scenario("b-"), after...)
	p.ingest(t0, tail)
	tail2, _ := voidEdgeCases("b-")
	for _, e := range tail2 {
		p.ingest(t0, e)
	}
	want := allDecisions(p.app, "a-", "b-")
	tailLen := countAfter(t, dbPath, snapSeq)
	if tailLen <= int64(len(tail)) {
		t.Fatalf("setup: tail of %d events", tailLen)
	}

	// Killed right after the last acknowledgement: no shutdown, no snapshot.
	p.kill()
	p = startServer(t, dbPath, snap)
	p.recovery(snapSeq, tailLen)
	if got := allDecisions(p.app, "a-", "b-"); !slices.Equal(got, want) {
		t.Fatalf("after kill, snapshot + tail:\n got %q\nwant %q", got, want)
	}

	// Without a snapshot: full replay.
	p.kill()
	if err := os.Remove(snap); err != nil {
		t.Fatal(err)
	}
	p = startServer(t, dbPath, snap)
	p.recovery(0, countAfter(t, dbPath, 0))
	if got := allDecisions(p.app, "a-", "b-"); !slices.Equal(got, want) {
		t.Fatalf("after kill, full replay:\n got %q\nwant %q", got, want)
	}

	// Periodic snapshots: one appears within the interval, then a kill.
	p.kill()
	p = startServer(t, dbPath, snap, "SNAPSHOT_INTERVAL=50ms")
	p.ingest(t0, bet("c-bet", "c-p", "1.00", t0))
	last := maxSeq(t, dbPath)
	deadline := time.Now().Add(10 * time.Second)
	for p.metric("snapshot_seq") != last {
		if time.Now().After(deadline) {
			t.Fatalf("no periodic snapshot at seq %d", last)
		}
		time.Sleep(20 * time.Millisecond)
	}
	p.kill()
	p = startServer(t, dbPath, snap)
	p.recovery(last, 0)
	if got := allDecisions(p.app, "a-", "b-"); !slices.Equal(got, want) {
		t.Fatalf("after kill, periodic snapshot:\n got %q\nwant %q", got, want)
	}
}

func TestKillDuringIngestionLosesNoAcknowledgedEvent(t *testing.T) {
	dbPath := newDatabase(t)
	snap := filepath.Join(t.TempDir(), "snapshot.bin")
	p := startServer(t, dbPath, snap, "SNAPSHOT_INTERVAL=30ms")

	// Each event is an approved deposit for its own player. Once applied, a
	// withdrawal to that method holds on R2; unapplied, it reviews on R5.
	var (
		mu    sync.Mutex
		acked []string
		wg    sync.WaitGroup
		stop  = make(chan struct{})
	)
	client := &http.Client{Timeout: 5 * time.Second}
	for g := range 8 {
		wg.Go(func() {
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				id := fmt.Sprintf("k%d-%d", g, i)
				body := fmt.Sprintf(`{"eventId":%q,"playerId":%q,"type":"DEPOSIT_APPROVED","occurredAt":%q,`+
					`"amount":"10.00","currency":"EUR","paymentMethodId":%q}`, id, id, ts(t0), "pm-"+id)
				req, _ := http.NewRequest(http.MethodPost, p.url+"/events", strings.NewReader(body))
				req.Header.Set("X-Test-Now", t0.Format(time.RFC3339))
				resp, err := client.Do(req)
				if err != nil {
					return // the server is gone
				}
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					mu.Lock()
					acked = append(acked, id)
					mu.Unlock()
				}
			}
		})
	}
	time.Sleep(700 * time.Millisecond)
	p.kill()
	close(stop)
	wg.Wait()
	if len(acked) < 50 {
		t.Fatalf("only %d events acknowledged before the kill", len(acked))
	}

	p = startServer(t, dbPath, snap)
	t.Logf("acknowledged %d events; restart replayed %d after snapshot seq %d",
		len(acked), p.metric("startup_replayed_events"), p.metric("startup_snapshot_seq"))
	ids, err := json.Marshal(acked)
	if err != nil {
		t.Fatal(err)
	}
	stored := queryInt(t, dbPath, "SELECT count(*) FROM events WHERE event_id IN (SELECT value FROM json_each(?))", string(ids))
	if stored != int64(len(acked)) {
		t.Fatalf("%d of %d acknowledged events are in the log", stored, len(acked))
	}
	for _, id := range acked {
		if got := p.decide(t0, withdrawal(id, "pm-"+id)).String(); got != "HOLD R2_CASHOUT_WITHOUT_PLAY" {
			t.Fatalf("acknowledged event %s is not in memory after restart: decision %q", id, got)
		}
	}
}
