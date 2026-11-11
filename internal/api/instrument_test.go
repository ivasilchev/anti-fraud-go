package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/devastion/anti-fraud-go/internal/metrics"
	"github.com/devastion/anti-fraud-go/internal/state"
)

func TestInstrumentRecordsRouteAndStatus(t *testing.T) {
	m := &metrics.Metrics{HTTP: metrics.NewHTTP(Routes())}
	m.RegisterRejectCodes(RejectCodes())
	s := &Server{Metrics: m, Now: time.Now}
	h := Instrument(s.Handler(), m.HTTP, false)
	do := func(method, path, body string) {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, path, strings.NewReader(body)))
	}

	do("GET", "/health/ready", "") // 503 during replay
	s.State = state.New()
	s.SetReady(true)
	do("GET", "/health/ready", "")
	do("GET", "/health/live", "")
	do("POST", "/decisions", `{"action":"BONUS_CLAIM","playerId":"p1"}`)
	do("POST", "/decisions", `{`)
	do("GET", "/nope", "")
	do("GET", "/decisions", "") // method not allowed

	var buf bytes.Buffer
	if err := m.WritePrometheus(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		`http_requests_total{route="GET /health/ready",code="503"} 1`,
		`http_requests_total{route="GET /health/ready",code="200"} 1`,
		`http_requests_total{route="GET /health/live",code="200"} 1`,
		`http_requests_total{route="POST /decisions",code="200"} 1`,
		`http_requests_total{route="POST /decisions",code="400"} 1`,
		`http_requests_total{route="other",code="404"} 1`,
		`http_requests_total{route="other",code="405"} 1`,
		`http_request_duration_seconds_count{route="POST /decisions"} 2`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestInstrumentedDecisionAllocations(t *testing.T) {
	m := &metrics.Metrics{HTTP: metrics.NewHTTP(Routes())}
	m.RegisterRejectCodes(RejectCodes())
	s := &Server{Metrics: m, Now: time.Now, State: state.New()}
	s.SetReady(true)
	mux := s.Handler()
	body := []byte(`{"action":"BONUS_CLAIM","playerId":"p1"}`)
	run := func(h http.Handler) func() {
		return func() {
			r := httptest.NewRequest("POST", "/decisions", bytes.NewReader(body))
			h.ServeHTTP(httptest.NewRecorder(), r)
		}
	}
	base := testing.AllocsPerRun(200, run(mux))
	inst := testing.AllocsPerRun(200, run(Instrument(mux, m.HTTP, false)))
	if inst > base {
		t.Fatalf("instrumentation adds allocations: %v per request, %v without", inst, base)
	}
}
