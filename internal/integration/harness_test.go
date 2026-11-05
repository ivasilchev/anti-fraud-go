// Package integration runs the real HTTP server, writer and Postgres store
// against a testcontainers Postgres.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/devastion/anti-fraud-go/internal/api"
	"github.com/devastion/anti-fraud-go/internal/ingest"
	"github.com/devastion/anti-fraud-go/internal/metrics"
	"github.com/devastion/anti-fraud-go/internal/state"
	"github.com/devastion/anti-fraud-go/internal/store"
)

// adminDSN connects to the container's default database; each test creates its own.
var adminDSN string

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	flag.Parse()
	if testing.Short() {
		return 0
	}
	ctx := context.Background()
	ctr, err := postgres.Run(ctx, "postgres:17-alpine",
		postgres.WithDatabase("antifraud"),
		postgres.WithUsername("antifraud"),
		postgres.WithPassword("antifraud"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start postgres:", err)
		return 1
	}
	defer func() {
		if err := testcontainers.TerminateContainer(ctr); err != nil {
			fmt.Fprintln(os.Stderr, "terminate postgres:", err)
		}
	}()
	adminDSN, err = ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintln(os.Stderr, "connection string:", err)
		return 1
	}
	return m.Run()
}

var dbCounter atomic.Int64

// newDatabase creates an empty database and returns its DSN.
func newDatabase(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	name := fmt.Sprintf("t%d_%d", os.Getpid(), dbCounter.Add(1))
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}

// app is one service instance: store, state, writer and HTTP server. For a
// server running as a separate process (see restart_test.go), only t, url and
// dsn are set.
type app struct {
	t       *testing.T
	url     string
	dsn     string
	db      *store.Store
	st      *state.State
	w       *ingest.Writer
	metrics *metrics.Metrics
}

var httpClient = &http.Client{Timeout: 30 * time.Second}

func newApp(t *testing.T) *app {
	t.Helper()
	return newAppOn(t, newDatabase(t))
}

// newAppOn starts an in-process service with empty state on the database dsn.
func newAppOn(t *testing.T, dsn string) *app {
	t.Helper()
	db, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	m := &metrics.Metrics{}
	m.RegisterRejectCodes(api.RejectCodes())
	st := state.New()
	w := ingest.New(db, st, m, ingest.Config{})
	done := make(chan struct{})
	go func() {
		w.Run()
		close(done)
	}()
	s := &api.Server{State: st, Ingest: w, Metrics: m, Now: time.Now, TestMode: true}
	s.SetReady(true)
	hs := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		hs.Close()
		w.Close()
		<-done
		db.Close()
	})
	return &app{t: t, url: hs.URL, dsn: dsn, db: db, st: st, w: w, metrics: m}
}

// post sends body as JSON with X-Test-Now = now and decodes the JSON response.
func (a *app) post(path string, now time.Time, body any, out any) int {
	a.t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		a.t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, a.url+path, bytes.NewReader(b))
	if err != nil {
		a.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(api.TestNowHeader, now.Format(time.RFC3339Nano))
	resp, err := httpClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		a.t.Fatal(err)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			a.t.Fatalf("decode %s response %q: %v", path, raw, err)
		}
	}
	return resp.StatusCode
}

type ingestResult struct {
	Accepted   int `json:"accepted"`
	Duplicates int `json:"duplicates"`
}

// ingest posts events (one object or a slice) and requires a 200.
func (a *app) ingest(now time.Time, events any) ingestResult {
	a.t.Helper()
	var res ingestResult
	if code := a.post("/events", now, events, &res); code != http.StatusOK {
		a.t.Fatalf("POST /events = %d", code)
	}
	return res
}

type decision struct {
	Decision string `json:"decision"`
	Reasons  []struct {
		Rule   string `json:"rule"`
		Detail string `json:"detail"`
	} `json:"reasons"`
}

func (d decision) String() string {
	s := d.Decision
	for _, r := range d.Reasons {
		s += " " + r.Rule
	}
	return s
}

func (a *app) decide(now time.Time, req map[string]any) decision {
	a.t.Helper()
	var d decision
	if code := a.post("/decisions", now, req, &d); code != http.StatusOK {
		a.t.Fatalf("POST /decisions = %d", code)
	}
	return d
}

// expectDecision checks the outcome and the fired rules, in order.
func (a *app) expectDecision(now time.Time, req map[string]any, want string) {
	a.t.Helper()
	if got := a.decide(now, req).String(); got != want {
		a.t.Fatalf("decision for %v = %q, want %q", req, got, want)
	}
}

func (a *app) countEvents(where string, args ...any) int {
	a.t.Helper()
	var n int
	q := "SELECT count(*) FROM events"
	if where != "" {
		q += " WHERE " + where
	}
	if err := a.db.Pool().QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		a.t.Fatal(err)
	}
	return n
}

// Event builders. ts formats a time as RFC 3339 with milliseconds.
func ts(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z07:00") }

type ev = map[string]any

func decline(id, player, pm string, t time.Time) ev {
	return ev{"eventId": id, "playerId": player, "type": "DEPOSIT_DECLINED", "occurredAt": ts(t),
		"amount": "10.00", "currency": "EUR", "paymentMethodId": pm}
}

func deposit(id, player, amount, pm string, t time.Time) ev {
	return ev{"eventId": id, "playerId": player, "type": "DEPOSIT_APPROVED", "occurredAt": ts(t),
		"amount": amount, "currency": "EUR", "paymentMethodId": pm}
}

func bet(id, player, amount string, t time.Time) ev {
	return ev{"eventId": id, "playerId": player, "type": "BET", "occurredAt": ts(t),
		"amount": amount, "currency": "EUR"}
}

func void(id, player, betID string, t time.Time) ev {
	return ev{"eventId": id, "playerId": player, "type": "BET_VOIDED", "occurredAt": ts(t),
		"voidedEventId": betID}
}

func withdrawal(player, pm string) map[string]any {
	return map[string]any{"action": "WITHDRAWAL", "playerId": player, "amount": "1.00",
		"currency": "EUR", "paymentMethodId": pm}
}

func depositAttempt(player, pm string) map[string]any {
	return map[string]any{"action": "DEPOSIT_ATTEMPT", "playerId": player, "amount": "10.00",
		"currency": "EUR", "paymentMethodId": pm}
}

var t0 = time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
