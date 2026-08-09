// Package traffic aggregates inbound/outbound P2P RPC volume for ops tuning.
// A single INFO line every INDEXUS_TRAFFIC_LOG_INTERVAL (default 10s) shows
// which routes dominate and how deep reads travel before they resolve. Set
// INDEXUS_TRAFFIC_LOG=0 to disable.
package traffic

import (
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const slowThreshold = 100 * time.Millisecond

type counters struct {
	n         atomic.Int64
	err       atomic.Int64
	slow      atomic.Int64
	forwarded atomic.Int64
	refresh   atomic.Int64
	maxDepth  atomic.Int64
	durNs     atomic.Int64
}

type drained struct {
	path      string
	n         int64
	err       int64
	slow      int64
	forwarded int64
	refresh   int64
	maxDepth  int64
	durNs     int64
}

// The middleware sits above the mux, so it keys a counter on the path a
// stranger sent, not on a route that exists. Left alone that map is an
// unbounded write from off the network. Declared routes always keep their own
// counter; everything else shares one bucket once the map is full, so an
// unknown path costs an increment and never a new entry.
const (
	maxPaths  = 64
	otherPath = "other"
)

var (
	inMu  sync.Mutex
	outMu sync.Mutex
	in    = map[string]*counters{}
	out   = map[string]*counters{}

	routesMu sync.RWMutex
	routes   = map[string]struct{}{}

	startOnce sync.Once
)

// Register declares the paths worth counting on their own. Unregistered paths
// are still counted, but they are the ones folded together under a flood.
func Register(paths ...string) {
	routesMu.Lock()
	defer routesMu.Unlock()
	for _, path := range paths {
		routes[path] = struct{}{}
	}
}

func registered(path string) bool {
	routesMu.RLock()
	defer routesMu.RUnlock()
	_, ok := routes[path]
	return ok
}

func ensureReporter() {
	startOnce.Do(func() {
		if off := strings.TrimSpace(os.Getenv("INDEXUS_TRAFFIC_LOG")); off == "0" || strings.EqualFold(off, "off") || strings.EqualFold(off, "false") {
			return
		}
		interval := 10 * time.Second
		if raw := strings.TrimSpace(os.Getenv("INDEXUS_TRAFFIC_LOG_INTERVAL")); raw != "" {
			if d, err := time.ParseDuration(raw); err == nil && d > 0 {
				interval = d
			}
		}
		go reportLoop(interval)
	})
}

func bucket(m map[string]*counters, mu *sync.Mutex, path string) *counters {
	if path == "" {
		path = "/"
	}
	mu.Lock()
	defer mu.Unlock()
	if c, ok := m[path]; ok {
		return c
	}
	if len(m) >= maxPaths && !registered(path) {
		path = otherPath
		if c, ok := m[path]; ok {
			return c
		}
	}
	c := &counters{}
	m[path] = c
	return c
}

// depth is how many nodes the read already crossed: 0 for a client call, more
// for a peer forwarding it along.
func record(c *counters, depth int, refresh bool, status int, dur time.Duration, failed bool) {
	c.n.Add(1)
	c.durNs.Add(dur.Nanoseconds())
	if failed || status >= 400 {
		c.err.Add(1)
	}
	if dur >= slowThreshold {
		c.slow.Add(1)
	}
	if depth > 0 {
		c.forwarded.Add(1)
	}
	if refresh {
		c.refresh.Add(1)
	}
	for {
		cur := c.maxDepth.Load()
		if int64(depth) <= cur || c.maxDepth.CompareAndSwap(cur, int64(depth)) {
			break
		}
	}
}

func depthOf(r *http.Request) int {
	raw := strings.TrimSpace(r.URL.Query().Get("via"))
	if raw == "" {
		return 0
	}
	return strings.Count(raw, ",") + 1
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.code = code
	s.ResponseWriter.WriteHeader(code)
}

// Middleware records inbound HTTP traffic on the P2P mux.
func Middleware(next http.Handler) http.Handler {
	ensureReporter()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rec, r)
		refresh := r.URL.Query().Get("refresh") == "true"
		record(bucket(in, &inMu, r.URL.Path), depthOf(r), refresh, rec.code, time.Since(start), false)
	})
}

// Transport wraps a RoundTripper to count outbound peer RPCs.
func Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	ensureReporter()
	return &countingTransport{base: base}
}

type countingTransport struct {
	base http.RoundTripper
}

func (t *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	start := time.Now()
	resp, err := t.base.RoundTrip(req)
	refresh := req.URL.Query().Get("refresh") == "true"
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	record(bucket(out, &outMu, req.URL.Path), depthOf(req), refresh, status, time.Since(start), err != nil)
	return resp, err
}

func reportLoop(interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for range t.C {
		flush(interval)
	}
}

func flush(window time.Duration) {
	inSnap := snapshot(&inMu, in)
	outSnap := snapshot(&outMu, out)
	if inSnap.total == 0 && outSnap.total == 0 {
		return
	}
	slog.Info("p2p traffic",
		"window", window.String(),
		"in_total", inSnap.total,
		"out_total", outSnap.total,
		"in", inSnap.byPath,
		"out", outSnap.byPath,
		"in_forwarded", inSnap.forwarded,
		"out_forwarded", outSnap.forwarded,
		"in_refresh", inSnap.refresh,
		"out_refresh", outSnap.refresh,
		"in_slow_100ms", inSnap.slow,
		"out_slow_100ms", outSnap.slow,
		"in_err", inSnap.err,
		"out_err", outSnap.err,
		"in_max_depth", inSnap.maxDepth,
		"out_max_depth", outSnap.maxDepth,
		"in_avg_ms", inSnap.avgMs,
		"out_avg_ms", outSnap.avgMs,
	)
}

type snap struct {
	total     int64
	byPath    string
	forwarded string
	refresh   string
	slow      string
	err       string
	maxDepth  int64
	avgMs     int64
}

func snapshot(mu *sync.Mutex, m map[string]*counters) snap {
	mu.Lock()
	rows := make([]drained, 0, len(m))
	var total, durNs, maxDepth int64
	for path, c := range m {
		n := c.n.Swap(0)
		errN := c.err.Swap(0)
		slowN := c.slow.Swap(0)
		fwdN := c.forwarded.Swap(0)
		refN := c.refresh.Swap(0)
		d := c.durNs.Swap(0)
		md := c.maxDepth.Swap(0)
		if n == 0 && errN == 0 {
			continue
		}
		rows = append(rows, drained{
			path: path, n: n, err: errN, slow: slowN,
			forwarded: fwdN, refresh: refN, maxDepth: md, durNs: d,
		})
		total += n
		durNs += d
		if md > maxDepth {
			maxDepth = md
		}
	}
	mu.Unlock()

	sort.Slice(rows, func(i, j int) bool { return rows[i].n > rows[j].n })

	format := func(get func(drained) int64) string {
		parts := make([]string, 0, len(rows))
		for _, r := range rows {
			v := get(r)
			if v == 0 {
				continue
			}
			parts = append(parts, r.path+":"+strconv.FormatInt(v, 10))
		}
		if len(parts) == 0 {
			return "-"
		}
		return strings.Join(parts, ",")
	}

	avg := int64(0)
	if total > 0 {
		avg = (durNs / total) / int64(time.Millisecond)
	}

	return snap{
		total:     total,
		byPath:    format(func(r drained) int64 { return r.n }),
		forwarded: format(func(r drained) int64 { return r.forwarded }),
		refresh:   format(func(r drained) int64 { return r.refresh }),
		slow:      format(func(r drained) int64 { return r.slow }),
		err:       format(func(r drained) int64 { return r.err }),
		maxDepth:  maxDepth,
		avgMs:     avg,
	}
}
