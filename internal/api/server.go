package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/devastion/anti-fraud-go/internal/ingest"
	"github.com/devastion/anti-fraud-go/internal/metrics"
	"github.com/devastion/anti-fraud-go/internal/rules"
	"github.com/devastion/anti-fraud-go/internal/state"
)

// TestNowHeader overrides the server clock when TestMode is on.
const TestNowHeader = "X-Test-Now"

// Submitter queues validated events and waits until they are applied.
type Submitter interface {
	Submit(ctx context.Context, events []state.Event, now int64) (ingest.Result, error)
}

// Server serves the HTTP API.
type Server struct {
	State   *state.State
	Ingest  Submitter
	Metrics *metrics.Metrics
	// Now is the server clock.
	Now func() time.Time
	// TestMode lets the X-Test-Now header (RFC 3339) override Now.
	TestMode bool

	ready   atomic.Bool
	serving atomic.Bool
}

// SetReady sets the /health/ready status. The first SetReady(true) also
// starts serving /events and /decisions, which return 503 NOT_READY until
// then; State and Ingest must be set before it. SetReady(false), on shutdown,
// only fails the readiness check: in-flight connections are still served.
func (s *Server) SetReady(ready bool) {
	if ready {
		s.serving.Store(true)
	}
	s.ready.Store(ready)
}

// Handler returns the API routes.
func (s *Server) Handler() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc(RouteEvents, s.handleEvents)
	mux.HandleFunc(RouteDecisions, s.handleDecision)
	mux.HandleFunc(RouteLive, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []byte(`{"status":"ok"}`))
	})
	mux.HandleFunc(RouteReady, func(w http.ResponseWriter, _ *http.Request) {
		if !s.ready.Load() {
			writeJSON(w, http.StatusServiceUnavailable, []byte(`{"status":"not_ready"}`))
			return
		}
		writeJSON(w, http.StatusOK, []byte(`{"status":"ready"}`))
	})
	mux.HandleFunc(RouteMetrics, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		if err := s.Metrics.WritePrometheus(w); err != nil {
			slog.Debug("write metrics", "err", err)
		}
	})
	return mux
}

// now returns the request's clock: X-Test-Now in test mode, else the server clock.
func (s *Server) now(r *http.Request) (time.Time, *Error) {
	if s.TestMode {
		if h := r.Header.Get(TestNowHeader); h != "" {
			t, err := time.Parse(time.RFC3339Nano, h)
			if err != nil {
				return time.Time{}, errValidation([]Detail{{Field: TestNowHeader, Code: CodeInvalidTimestamp}})
			}
			return t, nil
		}
	}
	return s.Now(), nil
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if !s.serving.Load() {
		s.writeError(w, errNotReady())
		return
	}
	now, e := s.now(r)
	if e != nil {
		s.writeError(w, e)
		return
	}
	body, e := ReadBody(r.Body)
	if e != nil {
		s.writeError(w, e)
		return
	}
	events, e := ParseEvents(body, now)
	if e != nil {
		s.writeError(w, e)
		return
	}
	res, err := s.Ingest.Submit(r.Context(), events, now.UnixMilli())
	switch {
	case errors.Is(err, ingest.ErrOverloaded), errors.Is(err, ingest.ErrClosed):
		s.writeError(w, ErrOverloaded())
		return
	case errors.Is(err, context.Canceled):
		return // client went away; the events are still processed
	case err != nil:
		s.writeError(w, errInternal())
		return
	}
	writeJSON(w, http.StatusOK, appendIngestResult(make([]byte, 0, 64+res.Accepted*96), events, res))
}

// appendIngestResult appends the POST /events response body: the counts and,
// in request order, the eventId and uuid of each event that was stored.
func appendIngestResult(b []byte, events []state.Event, res ingest.Result) []byte {
	b = append(b, `{"accepted":`...)
	b = strconv.AppendInt(b, int64(res.Accepted), 10)
	b = append(b, `,"duplicates":`...)
	b = strconv.AppendInt(b, int64(res.Duplicates), 10)
	b = append(b, `,"events":[`...)
	first := true
	for i, stored := range res.Stored {
		if !stored {
			continue
		}
		if !first {
			b = append(b, ',')
		}
		first = false
		b = append(b, `{"eventId":`...)
		b = appendJSONString(b, events[i].EventID)
		b = append(b, `,"uuid":"`...)
		b = appendUUID(b, events[i].UUID)
		b = append(b, `"}`...)
	}
	return append(b, "]}"...)
}

// appendJSONString appends s as a JSON string. Validated IDs are valid UTF-8
// without control characters, so only quotes and backslashes need escaping.
func appendJSONString(b []byte, s string) []byte {
	b = append(b, '"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"' || c == '\\':
			b = append(b, '\\', c)
		case c < 0x20:
			b = append(b, `\u00`...)
			b = append(b, hexDigits[c>>4], hexDigits[c&0xf])
		default:
			b = append(b, c)
		}
	}
	return append(b, '"')
}

const hexDigits = "0123456789abcdef"

// appendUUID appends u in the canonical 8-4-4-4-12 lowercase hex form.
func appendUUID(b []byte, u [16]byte) []byte {
	for i, c := range u {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			b = append(b, '-')
		}
		b = append(b, hexDigits[c>>4], hexDigits[c&0xf])
	}
	return b
}

var respPool = sync.Pool{New: func() any { b := make([]byte, 0, 512); return &b }}

func (s *Server) handleDecision(w http.ResponseWriter, r *http.Request) {
	if !s.serving.Load() {
		s.writeError(w, errNotReady())
		return
	}
	now, e := s.now(r)
	if e != nil {
		s.writeError(w, e)
		return
	}
	body, e := ReadBody(r.Body)
	if e != nil {
		s.writeError(w, e)
		return
	}
	req, e := ParseDecision(body)
	if e != nil {
		s.writeError(w, e)
		return
	}

	var dec rules.Decision
	s.State.Select(req.PlayerID, req.DeviceID, req.PaymentMethodID, func(v state.View) {
		dec = rules.Evaluate(rules.Request{
			Action:             req.Action,
			PaymentMethod:      v.PM,
			PaymentMethodKnown: v.PMKnown,
		}, v.P, v.D, now.UnixMilli())
	})
	s.Metrics.Decisions.Add(1)

	bp := respPool.Get().(*[]byte)
	b := appendDecision((*bp)[:0], &dec)
	writeJSON(w, http.StatusOK, b)
	*bp = b
	respPool.Put(bp)
}

// appendDecision appends the decision response body. Rule names and details
// are plain ASCII without quotes or backslashes, so they need no escaping.
func appendDecision(b []byte, dec *rules.Decision) []byte {
	b = append(b, `{"decision":"`...)
	b = append(b, dec.Outcome.String()...)
	b = append(b, `","reasons":[`...)
	for i, r := range dec.Reasons() {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, `{"rule":"`...)
		b = append(b, r.Rule.String()...)
		b = append(b, `","detail":"`...)
		b = r.AppendDetail(b)
		b = append(b, `"}`...)
	}
	return append(b, "]}"...)
}

func (s *Server) writeError(w http.ResponseWriter, e *Error) {
	s.Metrics.Reject(string(e.Code))
	writeJSON(w, e.Status, e.MarshalBody())
}

func writeJSON(w http.ResponseWriter, status int, body []byte) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		slog.Debug("write response", "err", err)
	}
}

// RejectCodes returns the codes to register with metrics.Metrics.
func RejectCodes() []string {
	out := make([]string, len(AllCodes))
	for i, c := range AllCodes {
		out[i] = string(c)
	}
	return out
}
