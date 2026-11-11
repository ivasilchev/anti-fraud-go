package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/devastion/anti-fraud-go/internal/ingest"
	"github.com/devastion/anti-fraud-go/internal/metrics"
	"github.com/devastion/anti-fraud-go/internal/state"
)

func TestMaxFutureSkewMatchesStatePruningClock(t *testing.T) {
	if MaxFutureSkew != state.MaxFutureMs*time.Millisecond {
		t.Fatalf("api.MaxFutureSkew = %v, state.MaxFutureMs = %dms", MaxFutureSkew, state.MaxFutureMs)
	}
}

func TestHealthAndGatingBeforeAndAfterReplay(t *testing.T) {
	m := &metrics.Metrics{}
	m.RegisterRejectCodes(RejectCodes())
	s := &Server{Metrics: m, Now: time.Now}
	h := s.Handler()
	do := func(method, path, body string) (int, string) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
		return rec.Code, rec.Body.String()
	}
	check := func(method, path, body string, wantCode int, wantBody string) {
		t.Helper()
		code, got := do(method, path, body)
		if code != wantCode || !strings.Contains(got, wantBody) {
			t.Fatalf("%s %s = %d %s, want %d containing %q", method, path, code, got, wantCode, wantBody)
		}
	}

	// Replay in progress: State and Ingest are not even set yet.
	check("GET", "/health/live", "", http.StatusOK, "ok")
	check("GET", "/health/ready", "", http.StatusServiceUnavailable, "not_ready")
	check("POST", "/decisions", `{"action":"BONUS_CLAIM","playerId":"p1"}`, http.StatusServiceUnavailable, `"NOT_READY"`)
	check("POST", "/events", `{}`, http.StatusServiceUnavailable, `"NOT_READY"`)

	s.State = state.New()
	s.SetReady(true)
	check("GET", "/health/ready", "", http.StatusOK, "ready")
	check("POST", "/decisions", `{"action":"BONUS_CLAIM","playerId":"p1"}`, http.StatusOK, `"ALLOW"`)

	// Shutting down: not ready, but requests on open connections are served.
	s.SetReady(false)
	check("GET", "/health/ready", "", http.StatusServiceUnavailable, "not_ready")
	check("GET", "/health/live", "", http.StatusOK, "ok")
	check("POST", "/decisions", `{"action":"BONUS_CLAIM","playerId":"p1"}`, http.StatusOK, `"ALLOW"`)
}

func TestAppendIngestResultListsStoredEventsInOrder(t *testing.T) {
	events := []state.Event{
		{EventID: `a"b\c`, UUID: [16]byte{0x01, 0x8f, 0x2a, 0x3b, 0x4c, 0x5d, 0x7e, 0x6f, 0x80, 0x91, 0xa2, 0xb3, 0xc4, 0xd5, 0xe6, 0xf7}},
		{EventID: "dup"},
		{EventID: "x", UUID: [16]byte{15: 0xff}},
	}
	res := ingest.Result{Accepted: 2, Duplicates: 1, Stored: []bool{true, false, true}}
	got := string(appendIngestResult(nil, events, res))
	want := `{"accepted":2,"duplicates":1,"events":[` +
		`{"eventId":"a\"b\\c","uuid":"018f2a3b-4c5d-7e6f-8091-a2b3c4d5e6f7"},` +
		`{"eventId":"x","uuid":"00000000-0000-0000-0000-0000000000ff"}]}`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	if got := string(appendIngestResult(nil, events[1:2], ingest.Result{Duplicates: 1, Stored: []bool{false}})); got != `{"accepted":0,"duplicates":1,"events":[]}` {
		t.Fatalf("all duplicates: got %s", got)
	}
}
