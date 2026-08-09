package traffic

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// reset clears the counters and the route registry. The package keeps its state
// in globals, so tests hand it back the way they found it.
func reset(t *testing.T) {
	t.Helper()
	inMu.Lock()
	in = map[string]*counters{}
	inMu.Unlock()
	outMu.Lock()
	out = map[string]*counters{}
	outMu.Unlock()
	routesMu.Lock()
	routes = map[string]struct{}{}
	routesMu.Unlock()
}

func pathCount(mu *sync.Mutex, m *map[string]*counters) int {
	mu.Lock()
	defer mu.Unlock()
	return len(*m)
}

func hasPath(mu *sync.Mutex, m *map[string]*counters, path string) bool {
	mu.Lock()
	defer mu.Unlock()
	_, ok := (*m)[path]
	return ok
}

func serve(h http.Handler, target string) {
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, target, nil))
}

// The middleware runs before the mux, so it counts paths that route nowhere.
// Keyed by the raw path that makes the map a write anyone on the network can
// perform without limit; the cap is what turns it into a bounded increment.
func TestUnknownPathsCannotGrowTheMapWithoutBound(t *testing.T) {
	reset(t)
	h := Middleware(http.NotFoundHandler())

	const flood = maxPaths * 20
	for i := 0; i < flood; i++ {
		serve(h, fmt.Sprintf("/%d-not-a-route", i))
	}

	if got := pathCount(&inMu, &in); got > maxPaths+1 {
		t.Fatalf("%d paths tracked after %d unknown requests, cap is %d(+other)", got, flood, maxPaths)
	}
	if !hasPath(&inMu, &in, otherPath) {
		t.Fatalf("the flood never folded into %q, so something else absorbed it", otherPath)
	}

	// Folded is not dropped: the volume still has to show up somewhere.
	snap := snapshot(&inMu, in)
	if snap.total != flood {
		t.Fatalf("counted %d of %d flooded requests", snap.total, flood)
	}
}

// Folding is only acceptable if it never costs a real route its own line, which
// is the whole reason the report is read.
func TestDeclaredRoutesKeepTheirOwnCounterUnderAFlood(t *testing.T) {
	reset(t)
	Register("/set", "/item")
	h := Middleware(http.NotFoundHandler())

	for i := 0; i < maxPaths*5; i++ {
		serve(h, fmt.Sprintf("/%d-not-a-route", i))
	}
	for i := 0; i < 3; i++ {
		serve(h, "/set")
	}
	serve(h, "/item")

	for _, path := range []string{"/set", "/item"} {
		if !hasPath(&inMu, &in, path) {
			t.Fatalf("declared route %s lost its bucket to the flood", path)
		}
	}
	snap := snapshot(&inMu, in)
	if !strings.Contains(snap.byPath, "/set:3") {
		t.Fatalf("/set did not report its own 3 calls: %s", snap.byPath)
	}
	if !strings.Contains(snap.byPath, "/item:1") {
		t.Fatalf("/item did not report its own call: %s", snap.byPath)
	}
}

// A route registered after the map is already full still has to get its own
// bucket — routes are declared at startup, but a flood can arrive first.
func TestARouteDeclaredAfterTheFloodStillGetsItsOwnCounter(t *testing.T) {
	reset(t)
	h := Middleware(http.NotFoundHandler())

	for i := 0; i < maxPaths*2; i++ {
		serve(h, fmt.Sprintf("/%d-not-a-route", i))
	}
	Register("/late")
	serve(h, "/late")

	if !hasPath(&inMu, &in, "/late") {
		t.Fatal("a route declared after the map filled was folded into other")
	}
}

func TestMiddlewareRecordsStatusDepthAndRefresh(t *testing.T) {
	reset(t)
	Register("/set")
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("boom") == "true" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	serve(h, "/set")
	serve(h, "/set?refresh=true")
	serve(h, "/set?via=a,b,c")
	serve(h, "/set?boom=true")

	snap := snapshot(&inMu, in)
	if snap.total != 4 {
		t.Fatalf("total = %d, want 4", snap.total)
	}
	if !strings.Contains(snap.err, "/set:1") {
		t.Fatalf("a 500 was not counted as an error: %s", snap.err)
	}
	if !strings.Contains(snap.refresh, "/set:1") {
		t.Fatalf("refresh=true was not counted: %s", snap.refresh)
	}
	if !strings.Contains(snap.forwarded, "/set:1") {
		t.Fatalf("a forwarded read was not counted: %s", snap.forwarded)
	}
	// via=a,b,c is three hops, and depth is what says whether reads resolve
	// nearby or crawl the mesh.
	if snap.maxDepth != 3 {
		t.Fatalf("maxDepth = %d, want 3", snap.maxDepth)
	}
}

func TestDepthOfCountsHops(t *testing.T) {
	for target, want := range map[string]int{
		"/set":            0,
		"/set?via=":       0,
		"/set?via=a":      1,
		"/set?via=a,b":    2,
		"/set?via=+a+,+b": 2,
	} {
		got := depthOf(httptest.NewRequest(http.MethodGet, target, nil))
		if got != want {
			t.Errorf("depthOf(%q) = %d, want %d", target, got, want)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A peer that never answers is the case the counters exist to surface, and it
// is also the one where there is no status code to read.
func TestTransportCountsFailuresWithNoResponse(t *testing.T) {
	reset(t)
	Register("/item")
	rt := Transport(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.RawQuery, "dead=true") {
			return nil, errors.New("connection refused")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	}))

	req := func(target string) {
		r, _ := http.NewRequest(http.MethodPost, "http://peer"+target, nil)
		resp, err := rt.RoundTrip(r)
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		_ = err
	}
	req("/item")
	req("/item?dead=true")

	snap := snapshot(&outMu, out)
	if snap.total != 2 {
		t.Fatalf("total = %d, want 2", snap.total)
	}
	if !strings.Contains(snap.err, "/item:1") {
		t.Fatalf("a transport error with no response was not counted: %s", snap.err)
	}
}

// The report is a rate, so reading it has to drain it. A window that repeats the
// previous window's numbers is worse than no window at all.
func TestSnapshotDrainsSoWindowsDoNotAccumulate(t *testing.T) {
	reset(t)
	Register("/ping")
	h := Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	serve(h, "/ping")
	serve(h, "/ping")

	if first := snapshot(&inMu, in); first.total != 2 {
		t.Fatalf("first window total = %d, want 2", first.total)
	}
	if second := snapshot(&inMu, in); second.total != 0 {
		t.Fatalf("second window total = %d, want 0 — counters were not drained", second.total)
	}
	if third := snapshot(&inMu, in); third.byPath != "-" {
		t.Fatalf("an empty window reported %q, want %q", third.byPath, "-")
	}
}

func TestSlowCallsAreSeparatedFromFastOnes(t *testing.T) {
	reset(t)
	Register("/slow")
	c := bucket(in, &inMu, "/slow")
	record(c, 0, false, http.StatusOK, slowThreshold+time.Millisecond, false)
	record(c, 0, false, http.StatusOK, time.Millisecond, false)

	snap := snapshot(&inMu, in)
	if !strings.Contains(snap.slow, "/slow:1") {
		t.Fatalf("exactly one call was over %s, got %s", slowThreshold, snap.slow)
	}
}

// Counting must not be the thing that breaks the server. Every path here is hot
// and concurrent in production, including the reporter draining underneath.
func TestConcurrentTrafficAndReportingStayConsistent(t *testing.T) {
	reset(t)
	Register("/set", "/item")

	h := Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	rt := Transport(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
	}))

	const (
		workers = 8
		each    = 100
	)
	stop := make(chan struct{})

	// The reporter runs on its own ticker, so it drains while requests land.
	// It is waited on separately: it stops when told, not when the load does.
	var drained int64
	reporter := make(chan struct{})
	go func() {
		defer close(reporter)
		for {
			select {
			case <-stop:
				drained += snapshot(&inMu, in).total
				return
			default:
			}
			drained += snapshot(&inMu, in).total
			time.Sleep(time.Millisecond)
		}
	}()

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				serve(h, "/set")
				r, _ := http.NewRequest(http.MethodPost, "http://peer/item", nil)
				resp, _ := rt.RoundTrip(r)
				if resp != nil && resp.Body != nil {
					resp.Body.Close()
				}
				// Unknown paths race the cap at the same time.
				serve(h, fmt.Sprintf("/w%d-i%d", w, i))
			}
		}(w)
	}
	wg.Wait()
	close(stop)
	<-reporter

	// Nothing may be counted twice or lost: every inbound request lands in
	// exactly one window.
	total := drained + snapshot(&inMu, in).total
	if want := int64(workers * each * 2); total != want {
		t.Fatalf("inbound requests counted across windows = %d, want %d", total, want)
	}
	if got := pathCount(&inMu, &in); got > maxPaths+1 {
		t.Fatalf("%d paths tracked, cap is %d(+other)", got, maxPaths)
	}
}

// flush is what the ticker calls; it must tolerate an idle node without logging
// and must not deadlock against the maps it drains.
func TestFlushOnAnIdleNodeIsSilentAndSafe(t *testing.T) {
	reset(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		flush(time.Second)
		serve(Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})), "/ping")
		flush(time.Second)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("flush did not return — the reporter would wedge the process")
	}
}
