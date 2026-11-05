package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
