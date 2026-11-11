package metrics

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestHTTPObserveAndPrometheus(t *testing.T) {
	h := NewHTTP([]string{"POST /decisions", "GET /health/ready"})
	h.Observe("POST /decisions", 200, 2*time.Millisecond)
	h.Observe("POST /decisions", 200, 40*time.Millisecond)
	h.Observe("POST /decisions", 422, 300*time.Microsecond)
	h.Observe("GET /health/ready", 503, time.Millisecond)
	h.Observe("", 404, time.Millisecond)
	h.Observe("POST /decisions", 200, 2*time.Second) // past the last bucket

	m := &Metrics{HTTP: h}
	var buf bytes.Buffer
	if err := m.WritePrometheus(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		`http_requests_total{route="POST /decisions",code="200"} 3`,
		`http_requests_total{route="POST /decisions",code="422"} 1`,
		`http_requests_total{route="GET /health/ready",code="503"} 1`,
		`http_requests_total{route="other",code="404"} 1`,
		`http_request_duration_seconds_bucket{route="POST /decisions",le="0.0005"} 1`,
		`http_request_duration_seconds_bucket{route="POST /decisions",le="0.00025"} 0`,
		`http_request_duration_seconds_bucket{route="POST /decisions",le="0.0025"} 2`,
		`http_request_duration_seconds_bucket{route="POST /decisions",le="0.03"} 2`,
		`http_request_duration_seconds_bucket{route="POST /decisions",le="0.05"} 3`,
		`http_request_duration_seconds_bucket{route="POST /decisions",le="1"} 3`,
		`http_request_duration_seconds_bucket{route="POST /decisions",le="+Inf"} 4`,
		`http_request_duration_seconds_count{route="POST /decisions"} 4`,
		`http_request_duration_seconds_sum{route="POST /decisions"} 2.0423`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, `code="500"`) {
		t.Errorf("zero counters are rendered:\n%s", out)
	}
}

func TestHTTPObserveDoesNotAllocate(t *testing.T) {
	h := NewHTTP([]string{"POST /decisions"})
	if a := testing.AllocsPerRun(100, func() { h.Observe("POST /decisions", 200, time.Millisecond) }); a != 0 {
		t.Fatalf("Observe allocates %v times", a)
	}
}

func TestQuantile(t *testing.T) {
	var bkt [len(httpBuckets) + 1]int64
	bkt[4] = 90               // ≤ 1 ms
	bkt[9] = 9                // ≤ 30 ms
	bkt[len(httpBuckets)] = 1 // > 1 s
	max := 3 * time.Second
	for _, c := range []struct {
		q    float64
		want time.Duration
	}{
		{0.5, time.Millisecond},
		{0.9, time.Millisecond},
		{0.95, 30 * time.Millisecond},
		{0.99, 30 * time.Millisecond},
		{1, max},
	} {
		if got := quantile(&bkt, 100, c.q, max); got != c.want {
			t.Errorf("quantile(%v) = %v, want %v", c.q, got, c.want)
		}
	}
	// A bucket bound above the max is capped at the max.
	if got := quantile(&bkt, 100, 0.5, 700*time.Microsecond); got != 700*time.Microsecond {
		t.Errorf("capped quantile = %v, want 700µs", got)
	}
}
