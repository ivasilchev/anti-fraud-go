package api

import (
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/devastion/anti-fraud-go/internal/metrics"
)

// Route patterns served by Handler.
const (
	RouteEvents    = "POST /events"
	RouteDecisions = "POST /decisions"
	RouteLive      = "GET /health/live"
	RouteReady     = "GET /health/ready"
	RouteMetrics   = "GET /metrics"
)

// Routes returns the patterns that Handler registers.
func Routes() []string {
	return []string{RouteEvents, RouteDecisions, RouteLive, RouteReady, RouteMetrics}
}

// Instrument wraps a ServeMux. For every request it records the matched
// pattern, the status code and the latency in stats. With accessLog, it also
// logs one line per request.
func Instrument(mux *http.ServeMux, stats *metrics.HTTP, accessLog bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := swPool.Get().(*statusWriter)
		sw.ResponseWriter, sw.status, sw.bytes = w, 0, 0
		// ServeMux sets r.Pattern on this request when it matches a route.
		mux.ServeHTTP(sw, r)
		d := time.Since(start)
		status := sw.status
		if status == 0 {
			// The handler wrote nothing: net/http sends 200.
			status = http.StatusOK
		}
		bytes := sw.bytes
		sw.ResponseWriter = nil
		swPool.Put(sw)

		stats.Observe(r.Pattern, status, d)
		if accessLog {
			slog.Info("http request",
				"method", r.Method,
				"path", r.URL.Path,
				"route", r.Pattern,
				"status", status,
				"bytes", bytes,
				"duration", d,
				"remote", r.RemoteAddr,
			)
		}
	})
}

var swPool = sync.Pool{New: func() any { return new(statusWriter) }}

// statusWriter records the status code and the body size of a response.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
