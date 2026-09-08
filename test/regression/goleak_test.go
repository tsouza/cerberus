package regression

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/routememo"
	"github.com/tsouza/cerberus/internal/schema"
)

// Layer 11 — goroutine-leak detector tests (#253). Each test opens a real
// httptest.Server, drives N requests through one handler entrypoint,
// closes the server, and asserts goleak.VerifyNone(t) finds no leaked
// goroutines.
//
// Why this lives in test/regression (not in each api/* package): the
// runtime goroutine inventory at test start is package-local — running
// goleak inside api/prom catches leaks the prom package itself owns,
// but the cross-package tail of stuck OTel exporters / WebSocket
// upgrade goroutines is easier to see when the test exists in a
// standalone package that doesn't import them transitively.
//
// goleak ignores known persistent runtime goroutines (e.g. the Go HTTP
// idle-conn cleaner) via opts.

// verifyNoLeaksAfterCleanup registers the leak assertion so it runs AFTER
// every other cleanup the test registers — in particular after
// httptest.Server.Close.
//
// `defer goleak.VerifyNone(t, ...)` does the opposite: Go runs deferred
// functions BEFORE registered cleanups, so the assertion fired while the
// server was still serving. Every one of these tests was checking the
// goroutine inventory of a LIVE server, which is why the ignore list had to
// include internal/poll.runtime_pollWait — an entry that suppresses every
// goroutine parked in network I/O, i.e. exactly the stuck-exporter and
// stuck-WebSocket leaks this file names as its targets.
//
// t.Cleanup runs LIFO, so registering this FIRST makes it run LAST.
//
// The options are built HERE and captured, never inside the cleanup closure.
// goleak.IgnoreCurrent snapshots the live goroutine set at OPTION-CREATION
// time; building the options inside the closure takes that snapshot at
// verification time, so every goroutine the test leaked is in the snapshot and
// is ignored, and VerifyNone cannot fail for any reason. Call this as the first
// statement of the test, before anything spawns a goroutine, so the baseline is
// the inventory the test inherited rather than the one it created.
func verifyNoLeaksAfterCleanup(t *testing.T) {
	t.Helper()
	opts := goleakOpts()
	t.Cleanup(func() { goleak.VerifyNone(t, opts...) })
}

// goleakOpts excludes the few intermittent goroutines that don't
// constitute a real leak — the http.DefaultTransport idle-conn
// goroutine and OTel's noop tracer background tasks.
//
// internal/poll.runtime_pollWait is deliberately NOT among them. It used to
// be, and it suppressed every goroutine parked in network I/O — the whole
// class this file exists to detect. It was only needed because the
// assertion ran before httptest.Server.Close (see
// verifyNoLeaksAfterCleanup); with the ordering fixed the suite passes
// without it. Do not re-add it: an entry that broad makes the detector
// unable to fail for its stated targets.
//
// IgnoreCurrent must be evaluated before the test body runs — see
// verifyNoLeaksAfterCleanup — which is why this returns options rather than
// running the verification itself.
func goleakOpts() []goleak.Option {
	return []goleak.Option{
		goleak.IgnoreTopFunction("net/http.(*Transport).getConn"),
		goleak.IgnoreTopFunction("net/http.(*persistConn).readLoop"),
		goleak.IgnoreTopFunction("net/http.(*persistConn).writeLoop"),
		// Goroutines the test inherited from the ones that ran before it in
		// this binary: another test's lingering conn-tracking tail is not this
		// test's leak.
		goleak.IgnoreCurrent(),
	}
}

// promStub is a minimal prom.Querier returning canned data — no
// goroutines of its own.
type promStub struct{ samples []chclient.Sample }

func (s *promStub) Query(_ context.Context, _ string, _ ...any) ([]chclient.Sample, error) {
	return s.samples, nil
}

func (s *promStub) QueryCursor(_ context.Context, _ string, _ ...any) (chclient.Cursor, error) {
	return &goleakSliceCursor{samples: s.samples, idx: -1}, nil
}

func (s *promStub) QueryStrings(_ context.Context, _ string, _ ...any) ([]string, error) {
	return nil, nil
}

func (s *promStub) QueryLabelSets(_ context.Context, _ string, _ ...any) ([]map[string]string, error) {
	return nil, nil
}

func (s *promStub) QueryMetricMeta(_ context.Context, _, _ string, _ ...any) ([]chclient.MetricMetaRow, error) {
	return nil, nil
}

func (s *promStub) QueryExemplars(_ context.Context, _ string, _ ...any) ([]chclient.ExemplarRow, error) {
	return nil, nil
}

type goleakSliceCursor struct {
	samples []chclient.Sample
	idx     int
	cur     chclient.Sample
}

func (c *goleakSliceCursor) Next() bool {
	c.idx++
	if c.idx >= len(c.samples) {
		return false
	}
	c.cur = c.samples[c.idx]
	return true
}
func (c *goleakSliceCursor) Sample() chclient.Sample { return c.cur }
func (c *goleakSliceCursor) Err() error              { return nil }
func (c *goleakSliceCursor) Close() error            { return nil }
func (c *goleakSliceCursor) Inspected() int64 {
	if c.idx > len(c.samples) {
		return int64(len(c.samples))
	}
	return int64(c.idx)
}

func TestNoGoroutineLeak_PromQuery(t *testing.T) {
	verifyNoLeaksAfterCleanup(t)

	h := prom.New(&promStub{samples: []chclient.Sample{
		{MetricName: "up", Labels: map[string]string{"job": "api"}, Timestamp: time.Now(), Value: 1},
	}}, schema.DefaultOTelMetrics(), slog.Default())
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	for range 50 {
		resp, err := http.Get(srv.URL + "/api/v1/query?query=up")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

func TestNoGoroutineLeak_PromQueryRange(t *testing.T) {
	verifyNoLeaksAfterCleanup(t)

	start := time.Unix(1717995600, 0).UTC()
	end := start.Add(2 * time.Minute)
	h := prom.New(&promStub{samples: []chclient.Sample{
		{MetricName: "up", Labels: map[string]string{"job": "api"}, Timestamp: start, Value: 1},
		{MetricName: "up", Labels: map[string]string{"job": "api"}, Timestamp: end, Value: 2},
	}}, schema.DefaultOTelMetrics(), slog.Default())
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	url := fmt.Sprintf("%s/api/v1/query_range?query=up&start=%d&end=%d&step=60",
		srv.URL, start.Unix(), end.Unix())
	for range 50 {
		resp, err := http.Get(url)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

func TestNoGoroutineLeak_PromLabels(t *testing.T) {
	verifyNoLeaksAfterCleanup(t)

	h := prom.New(&promStub{}, schema.DefaultOTelMetrics(), slog.Default())
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	for range 50 {
		resp, err := http.Get(srv.URL + "/api/v1/labels")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		_ = resp.Body.Close()
	}
}

func TestNoGoroutineLeak_PromSeries(t *testing.T) {
	verifyNoLeaksAfterCleanup(t)

	h := prom.New(&promStub{}, schema.DefaultOTelMetrics(), slog.Default())
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	for range 30 {
		resp, err := http.Get(srv.URL + `/api/v1/series?match%5B%5D=up`)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		_ = resp.Body.Close()
	}
}

func TestNoGoroutineLeak_PromMetadata(t *testing.T) {
	verifyNoLeaksAfterCleanup(t)

	h := prom.New(&promStub{}, schema.DefaultOTelMetrics(), slog.Default())
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	for range 30 {
		resp, err := http.Get(srv.URL + "/api/v1/metadata")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		_ = resp.Body.Close()
	}
}

// --- Loki ----------------------------------------------------------------

type lokiStub struct{ samples []chclient.Sample }

func (s *lokiStub) Query(_ context.Context, _ string, _ ...any) ([]chclient.Sample, error) {
	return s.samples, nil
}

func (s *lokiStub) QueryStrings(_ context.Context, _ string, _ ...any) ([]string, error) {
	return nil, nil
}

func (s *lokiStub) QueryDetectedFieldRows(_ context.Context, _ string, _ ...any) ([]chclient.DetectedFieldRow, error) {
	return nil, nil
}

func (s *lokiStub) QueryTimestampedLines(_ context.Context, _ string, _ ...any) ([]chclient.TimestampedLine, error) {
	return nil, nil
}

func (s *lokiStub) QueryIndexStats(_ context.Context, _ string, _ ...any) (chclient.IndexStatsRow, error) {
	return chclient.IndexStatsRow{}, nil
}

func (s *lokiStub) QueryIndexVolume(_ context.Context, _ string, _ ...any) ([]chclient.IndexVolumeRow, error) {
	return nil, nil
}

func (s *lokiStub) QueryLabelSets(_ context.Context, _ string, _ ...any) ([]map[string]string, error) {
	return nil, nil
}

func (s *lokiStub) QueryLabelCardinalities(_ context.Context, _ string, _ ...any) ([]chclient.LabelCardinalityRow, error) {
	return nil, nil
}

var _ loki.Querier = (*lokiStub)(nil)

func TestNoGoroutineLeak_LokiQuery(t *testing.T) {
	verifyNoLeaksAfterCleanup(t)

	h := loki.New(&lokiStub{}, schema.DefaultOTelLogs(), slog.Default())
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	for range 30 {
		resp, err := http.Get(srv.URL + `/loki/api/v1/query?query=%7Bjob%3D%22api%22%7D`)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		_ = resp.Body.Close()
	}
}

func TestNoGoroutineLeak_LokiLabels(t *testing.T) {
	verifyNoLeaksAfterCleanup(t)

	h := loki.New(&lokiStub{}, schema.DefaultOTelLogs(), slog.Default())
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	for range 30 {
		resp, err := http.Get(srv.URL + "/loki/api/v1/labels")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		_ = resp.Body.Close()
	}
}

func TestNoGoroutineLeak_LokiIndexStats(t *testing.T) {
	verifyNoLeaksAfterCleanup(t)

	h := loki.New(&lokiStub{}, schema.DefaultOTelLogs(), slog.Default())
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	for range 30 {
		resp, err := http.Get(srv.URL + `/loki/api/v1/index/stats?query=%7Bjob%3D%22api%22%7D`)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		_ = resp.Body.Close()
	}
}

// --- Tempo ---------------------------------------------------------------

type tempoStub struct{ samples []chclient.Sample }

func (s *tempoStub) Query(_ context.Context, _ string, _ ...any) ([]chclient.Sample, error) {
	return s.samples, nil
}

func (s *tempoStub) QueryStrings(_ context.Context, _ string, _ ...any) ([]string, error) {
	return nil, nil
}

var _ tempo.Querier = (*tempoStub)(nil)

func TestNoGoroutineLeak_TempoSearch(t *testing.T) {
	verifyNoLeaksAfterCleanup(t)

	h := tempo.New(&tempoStub{}, schema.DefaultOTelTraces(), "v1.0.0", slog.Default())
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	for range 30 {
		resp, err := http.Get(srv.URL + `/api/search?q=%7B%7D`)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		_ = resp.Body.Close()
	}
}

func TestNoGoroutineLeak_TempoTraceByID(t *testing.T) {
	verifyNoLeaksAfterCleanup(t)

	h := tempo.New(&tempoStub{}, schema.DefaultOTelTraces(), "v1.0.0", slog.Default())
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// Drive both the v1 (bare-trace) and v2 (TraceByIDResponse
	// envelope) endpoints — since the v2 envelope fix they are
	// distinct handler funcs sharing serveTraceByID, and the goleak
	// net must cover every mounted entrypoint.
	for _, path := range []string{"/api/traces/abc123", "/api/v2/traces/abc123"} {
		for range 30 {
			resp, err := http.Get(srv.URL + path)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			_ = resp.Body.Close()
		}
	}
}

// TestNoGoroutineLeak_TempoMetricsQueryInstant exercises the instant
// variant of the TraceQL metrics pipeline — the handler shares the
// matrix-shape RangeWindow plan with /api/metrics/query_range but
// runs it over a single bucket (step=end-start). A regression here
// would surface as a goroutine spawned per request by the metrics
// engine path that the handler then forgets to tear down.
func TestNoGoroutineLeak_TempoMetricsQueryInstant(t *testing.T) {
	verifyNoLeaksAfterCleanup(t)

	h := tempo.New(&tempoStub{}, schema.DefaultOTelTraces(), "v1.0.0", slog.Default())
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	for range 30 {
		u := fmt.Sprintf("%s/api/metrics/query?q=%%7B%%7D%%20%%7C%%20rate()&start=%d&end=%d",
			srv.URL, time.Now().Add(-1*time.Hour).Unix(), time.Now().Unix())
		resp, err := http.Get(u)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		_ = resp.Body.Close()
	}
}

// TestNoGoroutineLeak_UnderError — drives requests against a failing CH
// stub so the error path is the one exercised. Different goroutine
// graph than the happy path (no streaming cursor open, immediate
// envelope write); pinned separately.
func TestNoGoroutineLeak_UnderError(t *testing.T) {
	verifyNoLeaksAfterCleanup(t)

	failQ := &failingProm{err: errors.New("ch chaos")}
	h := prom.New(failQ, schema.DefaultOTelMetrics(), slog.Default())
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	for range 30 {
		resp, err := http.Get(srv.URL + "/api/v1/query?query=up")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		_ = resp.Body.Close()
	}
}

type failingProm struct {
	err error
}

func (f *failingProm) Query(_ context.Context, _ string, _ ...any) ([]chclient.Sample, error) {
	return nil, f.err
}

func (f *failingProm) QueryCursor(_ context.Context, _ string, _ ...any) (chclient.Cursor, error) {
	return nil, f.err
}

func (f *failingProm) QueryStrings(_ context.Context, _ string, _ ...any) ([]string, error) {
	return nil, f.err
}

func (f *failingProm) QueryLabelSets(_ context.Context, _ string, _ ...any) ([]map[string]string, error) {
	return nil, f.err
}

func (f *failingProm) QueryMetricMeta(_ context.Context, _, _ string, _ ...any) ([]chclient.MetricMetaRow, error) {
	return nil, f.err
}

func (f *failingProm) QueryExemplars(_ context.Context, _ string, _ ...any) ([]chclient.ExemplarRow, error) {
	return nil, f.err
}

// TestNoGoroutineLeak_ConcurrentRequests — parallel requests must not
// leak. Catches connection-pool leaks more reliably than serial.
func TestNoGoroutineLeak_ConcurrentRequests(t *testing.T) {
	verifyNoLeaksAfterCleanup(t)

	h := prom.New(&promStub{samples: []chclient.Sample{{MetricName: "up", Value: 1}}},
		schema.DefaultOTelMetrics(), slog.Default())
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				resp, err := http.Get(srv.URL + "/api/v1/query?query=up")
				if err != nil {
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
		}()
	}
	wg.Wait()
}

// --- Route memo -----------------------------------------------------------

// routeMemoTestPressureWindow mirrors the cluster-wide pressure-damper
// horizon a real Memo is constructed with (see docs/solver.md); its exact
// value is immaterial here since this test never crosses the damper's
// distinct-key threshold, but a named constant keeps the constructor call
// self-explanatory instead of a bare duration literal.
const routeMemoTestPressureWindow = time.Minute

// TestNoGoroutineLeak_RouteMemo pins that the failure-driven route memo
// (internal/routememo, see docs/solver.md) has no resident background
// goroutine, by construction: Memo owns exactly one non-blocking
// primitive — a buffered dispatch-token channel — and every exported
// method runs synchronously on the caller's goroutine under its own
// mutex. There is no eviction sweep, no TTL timer, and no retry loop
// spawned anywhere in the package, so driving a Memo through activity and
// letting it go out of scope must leave the goroutine inventory
// unchanged.
//
// The sequence below simulates a handful of route-A failures, the
// corroboration-triggered probe they earn, a retry admission against the
// resulting memo-hit, and a second key whose own probe fails negatively —
// the same call shapes the solver's dispatch path drives in production,
// just without a background goroutine anywhere to leak.
func TestNoGoroutineLeak_RouteMemo(t *testing.T) {
	verifyNoLeaksAfterCleanup(t)

	m := routememo.New(routeMemoTestPressureWindow)
	k := routememo.Key{RootKind: "*chplan.Aggregate"}

	// A single route-A resource failure must not yet corroborate into a
	// probe: one transient rejection teaches the memo nothing on its own.
	if _, ok, _ := m.ObserveRouteAFailureAndMaybeBeginProbe(k); ok {
		t.Fatalf("single resource failure must not admit a probe")
	}

	// A second consecutive failure on the same key crosses the
	// corroboration floor and admits a probe dispatch under the
	// process-wide dispatch-token budget.
	release, ok, _ := m.ObserveRouteAFailureAndMaybeBeginProbe(k)
	if !ok {
		t.Fatalf("second consecutive resource failure must admit a probe")
	}
	m.Observe(k, routememo.RouteB, routememo.OutcomeSuccess)
	release()

	// Subsequent traffic on the same key memo-hits route B directly;
	// exercise the shared dispatch-token budget the way a memo-hit retry
	// would, proving the token released above was actually returned to
	// the channel rather than leaked into a stuck goroutine.
	if state, stale := m.Lookup(k); state != routememo.PreferB || stale {
		t.Fatalf("Lookup(k) = (%v, stale=%v), want (PreferB, stale=false)", state, stale)
	}
	hitRelease, hitOK := m.AdmitDispatch()
	if !hitOK {
		t.Fatalf("memo-hit dispatch must be admitted once the prior token is released")
	}
	hitRelease()

	// A distinct key that corroborates and then fails its own route-B
	// probe records the negative-evidence BothFail state — the path a
	// key takes when route B does not, in fact, help.
	other := routememo.Key{RootKind: "*chplan.RangeWindow"}
	m.Observe(other, routememo.RouteA, routememo.OutcomeResourceFailure)
	otherRelease, otherOK, _ := m.ObserveRouteAFailureAndMaybeBeginProbe(other)
	if !otherOK {
		t.Fatalf("second consecutive resource failure on a distinct key must admit a probe")
	}
	m.Observe(other, routememo.RouteB, routememo.OutcomeResourceFailure)
	otherRelease()

	if state, _ := m.Lookup(other); state != routememo.BothFail {
		t.Fatalf("Lookup(other) = %v, want BothFail", state)
	}
}

// recordingLeakT is a goleak.TestingT that records failure instead of failing
// the surrounding test, so a test can assert that the leak detector DOES fire.
type recordingLeakT struct {
	mu     sync.Mutex
	failed bool
}

func (r *recordingLeakT) Error(...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failed = true
}

// TestGoleakDetectorCanFail is the meta-test for the 14 leak tests above: it
// proves the guard they all call actually reports a leaked goroutine.
//
// The whole suite was vacuous before this test existed. goleak.IgnoreCurrent
// snapshots the live goroutine set when the OPTION is constructed, and
// verifyNoLeaksAfterCleanup built its options inside the cleanup closure — so
// the snapshot was taken at verification time, contained every goroutine the
// test had leaked, and ignored all of them. A goroutine deliberately parked for
// 30s passed all 14 tests.
//
// Both arms below run the identical leak against the identical options, and
// differ only in WHEN the options are built. That is the entire mechanism, so
// the "after" arm is asserted too: if goleak ever changed IgnoreCurrent to
// snapshot lazily, the "after" arm would start failing and tell us the
// ordering rule this file is built on no longer holds.
func TestGoleakDetectorCanFail(t *testing.T) {
	tests := []struct {
		name string
		// buildBeforeLeak mirrors verifyNoLeaksAfterCleanup: options are
		// constructed before the test body spawns anything.
		buildBeforeLeak bool
		wantDetected    bool
	}{
		{name: "options built before the leak detect it", buildBeforeLeak: true, wantDetected: true},
		{name: "options built after the leak ignore it", buildBeforeLeak: false, wantDetected: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var opts []goleak.Option
			if tc.buildBeforeLeak {
				opts = goleakOpts()
			}

			// A goroutine that outlives the body, released only on cleanup so
			// this test leaks nothing of its own.
			release := make(chan struct{})
			running := make(chan struct{})
			t.Cleanup(func() { close(release) })
			go func() {
				close(running)
				<-release
			}()
			<-running

			if !tc.buildBeforeLeak {
				opts = goleakOpts()
			}

			rec := &recordingLeakT{}
			goleak.VerifyNone(rec, opts...)

			rec.mu.Lock()
			got := rec.failed
			rec.mu.Unlock()
			if got != tc.wantDetected {
				t.Fatalf("leak detected = %v, want %v — the Layer 11 detector's "+
					"IgnoreCurrent ordering no longer behaves as this file assumes", got, tc.wantDetected)
			}
		})
	}
}
