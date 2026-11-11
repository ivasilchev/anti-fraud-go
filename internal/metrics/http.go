package metrics

import (
	"context"
	"log/slog"
	"math"
	"strconv"
	"sync/atomic"
	"time"
)

// httpBuckets are the upper bounds, in seconds, of the HTTP latency
// histogram. 0.03 and 0.08 are the SPEC's p95 and p99 decision limits.
var httpBuckets = [...]float64{0.00005, 0.0001, 0.00025, 0.0005, 0.001, 0.0025, 0.005, 0.01, 0.02, 0.03, 0.05, 0.08, 0.1, 0.25, 0.5, 1}

// OtherRoute is the route label of requests that match no registered route.
const OtherRoute = "other"

const minStatus, maxStatus = 100, 599

// HTTP counts requests by route and status code and keeps a latency histogram
// per route. Routes are fixed at construction, so recording takes no lock and
// makes no allocation.
type HTTP struct {
	routes []string
	index  map[string]int
	stats  []routeStats
}

type routeStats struct {
	status [maxStatus - minStatus + 1]atomic.Int64
	bucket [len(httpBuckets) + 1]atomic.Int64 // not cumulative; the last one is +Inf
	sumUs  atomic.Int64
	maxUs  atomic.Int64 // since the last summary log
}

// NewHTTP returns HTTP stats for the given route patterns, as registered on
// the ServeMux. OtherRoute is added at the end.
func NewHTTP(routes []string) *HTTP {
	h := &HTTP{index: make(map[string]int, len(routes))}
	for _, r := range routes {
		if _, ok := h.index[r]; ok || r == OtherRoute {
			continue
		}
		h.index[r] = len(h.routes)
		h.routes = append(h.routes, r)
	}
	h.routes = append(h.routes, OtherRoute)
	h.stats = make([]routeStats, len(h.routes))
	return h
}

// Observe records one finished request. pattern is the matched ServeMux
// pattern (http.Request.Pattern), "" when nothing matched.
func (h *HTTP) Observe(pattern string, status int, d time.Duration) {
	i, ok := h.index[pattern]
	if !ok {
		i = len(h.routes) - 1
	}
	s := &h.stats[i]
	if status >= minStatus && status <= maxStatus {
		s.status[status-minStatus].Add(1)
	}
	us := d.Microseconds()
	s.sumUs.Add(us)
	for {
		m := s.maxUs.Load()
		if us <= m || s.maxUs.CompareAndSwap(m, us) {
			break
		}
	}
	sec := d.Seconds()
	b := len(httpBuckets)
	for j, ub := range httpBuckets {
		if sec <= ub {
			b = j
			break
		}
	}
	s.bucket[b].Add(1)
}

func (h *HTTP) appendPrometheus(b []byte) []byte {
	b = append(b, "# HELP http_requests_total HTTP requests by route and status code.\n# TYPE http_requests_total counter\n"...)
	for i, r := range h.routes {
		for c := range h.stats[i].status {
			n := h.stats[i].status[c].Load()
			if n == 0 {
				continue
			}
			b = append(b, `http_requests_total{route="`+r+`",code="`...)
			b = strconv.AppendInt(b, int64(c+minStatus), 10)
			b = append(b, `"} `...)
			b = strconv.AppendInt(b, n, 10)
			b = append(b, '\n')
		}
	}
	b = append(b, "# HELP http_request_duration_seconds HTTP request latency by route, from the handler's start to its return.\n# TYPE http_request_duration_seconds histogram\n"...)
	for i, r := range h.routes {
		s := &h.stats[i]
		var cum int64
		for j, ub := range httpBuckets {
			cum += s.bucket[j].Load()
			b = append(b, `http_request_duration_seconds_bucket{route="`+r+`",le="`...)
			b = strconv.AppendFloat(b, ub, 'g', -1, 64)
			b = append(b, `"} `...)
			b = strconv.AppendInt(b, cum, 10)
			b = append(b, '\n')
		}
		cum += s.bucket[len(httpBuckets)].Load()
		b = append(b, `http_request_duration_seconds_bucket{route="`+r+`",le="+Inf"} `...)
		b = strconv.AppendInt(b, cum, 10)
		b = append(b, "\nhttp_request_duration_seconds_sum{route=\""+r+"\"} "...)
		b = strconv.AppendFloat(b, float64(s.sumUs.Load())/1e6, 'g', -1, 64)
		b = append(b, "\nhttp_request_duration_seconds_count{route=\""+r+"\"} "...)
		b = strconv.AppendInt(b, cum, 10)
		b = append(b, '\n')
	}
	return b
}

// routeSample is a copy of one route's counters, used to compute the change
// between two summary logs.
type routeSample struct {
	n, c2xx, c4xx, c5xx int64
	bucket              [len(httpBuckets) + 1]int64
	sumUs               int64
}

func (h *HTTP) sample(i int) routeSample {
	s := &h.stats[i]
	var out routeSample
	for c := range s.status {
		n := s.status[c].Load()
		switch (c + minStatus) / 100 {
		case 2:
			out.c2xx += n
		case 4:
			out.c4xx += n
		case 5:
			out.c5xx += n
		}
	}
	for j := range s.bucket {
		out.bucket[j] = s.bucket[j].Load()
		out.n += out.bucket[j]
	}
	out.sumUs = s.sumUs.Load()
	return out
}

// quantile returns the upper bound of the bucket that holds quantile q of
// the n requests in bkt. In the +Inf bucket it returns max.
func quantile(bkt *[len(httpBuckets) + 1]int64, n int64, q float64, max time.Duration) time.Duration {
	rank := int64(q*float64(n) + 0.999999)
	var cum int64
	for j, ub := range httpBuckets {
		cum += bkt[j]
		if cum >= rank {
			d := time.Duration(ub * float64(time.Second))
			return min(d, max)
		}
	}
	return max
}

// LogSummaries logs, every interval and for each route that served requests
// in it, the request rate, the status classes and the latency (mean, p50,
// p95, p99 and max). The percentiles are bucket upper bounds, capped at the
// max. It returns when ctx is done.
func (h *HTTP) LogSummaries(ctx context.Context, every time.Duration) {
	prev := make([]routeSample, len(h.routes))
	for i := range prev {
		prev[i] = h.sample(i)
	}
	t := time.NewTicker(every)
	defer t.Stop()
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			elapsed := now.Sub(last)
			last = now
			for i, r := range h.routes {
				cur := h.sample(i)
				p := prev[i]
				prev[i] = cur
				maxD := time.Duration(h.stats[i].maxUs.Swap(0)) * time.Microsecond
				n := cur.n - p.n
				if n == 0 {
					continue
				}
				var d [len(httpBuckets) + 1]int64
				for j := range d {
					d[j] = cur.bucket[j] - p.bucket[j]
				}
				slog.Info("http summary",
					"route", r,
					"requests", n,
					"rps", math.Round(float64(n)/elapsed.Seconds()*10)/10,
					"2xx", cur.c2xx-p.c2xx,
					"4xx", cur.c4xx-p.c4xx,
					"5xx", cur.c5xx-p.c5xx,
					"mean", time.Duration((cur.sumUs-p.sumUs)/n)*time.Microsecond,
					"p50", quantile(&d, n, 0.50, maxD),
					"p95", quantile(&d, n, 0.95, maxD),
					"p99", quantile(&d, n, 0.99, maxD),
					"max", maxD,
				)
			}
		}
	}
}
