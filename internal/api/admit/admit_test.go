package admit_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/tsouza/cerberus/internal/api/admit"
)

func TestAcquireBelowCap(t *testing.T) {
	t.Parallel()
	l := admit.New("prom", 3)
	rel1, ok := l.Acquire(t.Context())
	if !ok {
		t.Fatalf("acquire 1: want ok, got reject")
	}
	rel2, ok := l.Acquire(t.Context())
	if !ok {
		t.Fatalf("acquire 2: want ok, got reject")
	}
	rel1()
	rel2()
}

func TestAcquireAtCapRejects(t *testing.T) {
	t.Parallel()
	l := admit.New("prom", 1)
	rel, ok := l.Acquire(t.Context())
	if !ok {
		t.Fatalf("acquire 1: want ok, got reject")
	}
	defer rel()
	if _, ok := l.Acquire(t.Context()); ok {
		t.Fatalf("acquire 2: want reject, got ok")
	}
}

func TestReleaseAllowsNext(t *testing.T) {
	t.Parallel()
	l := admit.New("prom", 1)
	rel, ok := l.Acquire(t.Context())
	if !ok {
		t.Fatalf("acquire 1: want ok")
	}
	rel()
	rel2, ok := l.Acquire(t.Context())
	if !ok {
		t.Fatalf("acquire after release: want ok, got reject")
	}
	rel2()
}

func TestReleaseIdempotent(t *testing.T) {
	t.Parallel()
	l := admit.New("prom", 1)
	rel, ok := l.Acquire(t.Context())
	if !ok {
		t.Fatalf("acquire: want ok")
	}
	rel()
	rel() // second call must not panic, must not double-release
	rel2, ok := l.Acquire(t.Context())
	if !ok {
		t.Fatalf("re-acquire: want ok")
	}
	// If double-release wrongly returned a second token, cap=1 would
	// still allow another acquire here. Verify the slot is taken.
	if _, ok := l.Acquire(t.Context()); ok {
		t.Fatalf("re-acquire 2: want reject, got ok — double-release corrupted the semaphore")
	}
	rel2()
}

func TestNilLimiterAlwaysAdmits(t *testing.T) {
	t.Parallel()
	var l *admit.Limiter
	for range 100 {
		rel, ok := l.Acquire(t.Context())
		if !ok {
			t.Fatalf("nil limiter must always admit")
		}
		rel()
	}
}

func TestNewZeroCapReturnsNil(t *testing.T) {
	t.Parallel()
	if l := admit.New("prom", 0); l != nil {
		t.Fatalf("cap=0: want nil limiter, got %v", l)
	}
	if l := admit.New("prom", -1); l != nil {
		t.Fatalf("cap=-1: want nil limiter, got %v", l)
	}
}

func TestMiddlewareBelowCap(t *testing.T) {
	t.Parallel()
	l := admit.New("prom", 2)
	var hits atomic.Int32
	h := l.Middleware(1, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	for range 5 {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("want 200, got %d", rec.Code)
		}
	}
	if hits.Load() != 5 {
		t.Fatalf("want 5 handler hits, got %d", hits.Load())
	}
}

func TestMiddlewareRejectsAtCap(t *testing.T) {
	t.Parallel()
	l := admit.New("prom", 1)
	// Hold the slot so the next request through the middleware hits
	// the cap.
	rel, ok := l.Acquire(t.Context())
	if !ok {
		t.Fatalf("setup acquire: want ok")
	}
	defer rel()

	h := l.Middleware(1, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatalf("handler must not run when limiter is full")
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After: want \"1\", got %q", got)
	}
	if body := rec.Body.String(); !strings.Contains(body, "admission control") {
		t.Fatalf("body should mention admission control, got %q", body)
	}
}

func TestMiddlewareNilLimiterPassesThrough(t *testing.T) {
	t.Parallel()
	var l *admit.Limiter
	var hits atomic.Int32
	h := l.Middleware(1, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	for range 10 {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("want 200, got %d", rec.Code)
		}
	}
	if hits.Load() != 10 {
		t.Fatalf("want 10 hits, got %d", hits.Load())
	}
}

func TestMiddlewareNoRetryAfterWhenZero(t *testing.T) {
	t.Parallel()
	l := admit.New("prom", 1)
	rel, _ := l.Acquire(t.Context())
	defer rel()

	h := l.Middleware(0, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After: want empty (suppressed), got %q", got)
	}
}

func TestConcurrentAcquireRespectsCap(t *testing.T) {
	t.Parallel()
	const cap, workers = 4, 64
	l := admit.New("prom", cap)

	var (
		inflight    atomic.Int32
		maxInflight atomic.Int32
		admitted    atomic.Int32
		rejected    atomic.Int32
		wg          sync.WaitGroup
		start       = make(chan struct{})
	)
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			<-start
			rel, ok := l.Acquire(t.Context())
			if !ok {
				rejected.Add(1)
				return
			}
			admitted.Add(1)
			cur := inflight.Add(1)
			for {
				maxObserved := maxInflight.Load()
				if cur <= maxObserved || maxInflight.CompareAndSwap(maxObserved, cur) {
					break
				}
			}
			inflight.Add(-1)
			rel()
		}()
	}
	close(start)
	wg.Wait()

	if got := maxInflight.Load(); got > int32(cap) {
		t.Fatalf("max concurrent in-flight %d exceeded cap %d", got, cap)
	}
	if admitted.Load()+rejected.Load() != workers {
		t.Fatalf("accounting mismatch: admitted=%d rejected=%d workers=%d",
			admitted.Load(), rejected.Load(), workers)
	}
}

func TestRejectedCounter(t *testing.T) {
	t.Parallel()
	// Use the test-only NewWithProvider so this limiter's rejection
	// counter targets *our* manual reader exclusively. Routing through
	// the OTel global would cross-count with the limiters built by
	// parallel tests, which all share that global.
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))

	l := admit.NewWithProvider("prom", 1, mp)
	rel, _ := l.Acquire(t.Context())
	defer rel()

	// Drive two rejections through Acquire.
	if _, ok := l.Acquire(t.Context()); ok {
		t.Fatalf("acquire 2: want reject")
	}
	if _, ok := l.Acquire(t.Context()); ok {
		t.Fatalf("acquire 3: want reject")
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var found bool
	var sum int64
	var sawQL, sawReason bool
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "cerberus_admit_rejected_total" {
				continue
			}
			found = true
			sumData, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("rejected_total data: want Sum[int64], got %T", m.Data)
			}
			for _, dp := range sumData.DataPoints {
				sum += dp.Value
				if v, ok := dp.Attributes.Value("cerberus.ql"); ok && v.AsString() == "promql" {
					sawQL = true
				}
				if v, ok := dp.Attributes.Value("reason"); ok && v.AsString() == admit.ReasonCapExceeded {
					sawReason = true
				}
			}
		}
	}
	if !found {
		t.Fatalf("cerberus_admit_rejected_total not exported")
	}
	if sum != 2 {
		t.Fatalf("rejected_total: want 2, got %d", sum)
	}
	if !sawQL {
		t.Errorf("rejected_total: missing cerberus.ql=promql attribute")
	}
	if !sawReason {
		t.Errorf("rejected_total: missing reason=%q attribute", admit.ReasonCapExceeded)
	}
}

// rejectedStreams collects every cerberus_admit_rejected_total data
// point from a manual-reader snapshot, keyed by its
// "<cerberus.ql>/<reason>" attribute pair. Returning a map (rather
// than a flat sum) lets callers assert both the per-stream value and
// the *number* of distinct streams — the dashboard's
// `sum by (cerberus_ql, reason)` panel renders one series per key,
// so a stray second stream with mismatched attributes is itself a
// bug worth failing on.
func rejectedStreams(t *testing.T, reader *metric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	streams := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "cerberus_admit_rejected_total" {
				continue
			}
			sumData, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("rejected_total data: want Sum[int64], got %T", m.Data)
			}
			if !sumData.IsMonotonic {
				t.Fatalf("rejected_total: want monotonic sum (counter), got non-monotonic")
			}
			for _, dp := range sumData.DataPoints {
				ql, ok := dp.Attributes.Value("cerberus.ql")
				if !ok {
					t.Fatalf("rejected_total data point missing cerberus.ql attribute: %v", dp.Attributes.ToSlice())
				}
				reason, ok := dp.Attributes.Value("reason")
				if !ok {
					t.Fatalf("rejected_total data point missing reason attribute: %v", dp.Attributes.ToSlice())
				}
				key := ql.AsString() + "/" + reason.AsString()
				if _, dup := streams[key]; dup {
					t.Fatalf("rejected_total: duplicate stream for %q in one collection", key)
				}
				streams[key] = dp.Value
			}
		}
	}
	return streams
}

// TestRejectedCounterZeroInitializedAtConstruction pins the fix for
// the cerberus-self dashboard's "Admission rejections" panel showing
// "No data" on healthy deployments: OTel sync counters export
// nothing until their first Add, so the limiter must pre-register
// its (cerberus.ql, reason) stream at 0 when constructed — before
// any Acquire runs, let alone any rejection. This asserts what the
// consumer actually renders: one 0-valued cumulative stream per
// head, with exactly the label set the panel groups by.
func TestRejectedCounterZeroInitializedAtConstruction(t *testing.T) {
	t.Parallel()
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))

	// Construct all three heads; never call Acquire.
	for _, head := range []string{"prom", "loki", "tempo"} {
		if l := admit.NewWithProvider(head, 4, mp); l == nil {
			t.Fatalf("NewWithProvider(%q): want limiter, got nil", head)
		}
	}

	streams := rejectedStreams(t, reader)
	if len(streams) != 3 {
		t.Fatalf("want exactly 3 zero-init streams, got %d: %v", len(streams), streams)
	}
	for _, ql := range []string{"promql", "logql", "traceql"} {
		key := ql + "/" + admit.ReasonCapExceeded
		got, ok := streams[key]
		if !ok {
			t.Errorf("missing zero-init stream %q (panel would show No data for this head)", key)
			continue
		}
		if got != 0 {
			t.Errorf("stream %q: want 0 before any rejection, got %d", key, got)
		}
	}
}

// TestRejectedCounterZeroInitSharesStreamWithHotPath pins that the
// zero-init at construction and the Add on the Acquire rejection
// path use the *same* attribute set: after a forced rejection the
// pre-registered stream must read 1 — not stay at 0 next to a
// second, differently-labelled stream the dashboard would render as
// a separate series.
func TestRejectedCounterZeroInitSharesStreamWithHotPath(t *testing.T) {
	t.Parallel()
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))

	l := admit.NewWithProvider("prom", 1, mp)
	rel, ok := l.Acquire(t.Context())
	if !ok {
		t.Fatalf("acquire 1: want ok")
	}
	defer rel()
	if _, ok := l.Acquire(t.Context()); ok {
		t.Fatalf("acquire 2: want reject")
	}

	streams := rejectedStreams(t, reader)
	if len(streams) != 1 {
		t.Fatalf("want exactly 1 stream (zero-init and hot path must share attributes), got %d: %v", len(streams), streams)
	}
	key := "promql/" + admit.ReasonCapExceeded
	if got := streams[key]; got != 1 {
		t.Fatalf("stream %q: want 1 after one rejection, got %d", key, got)
	}
}

// fakeServerStream is the minimal grpc.ServerStream stub the
// StreamInterceptor tests need to drive the interceptor through its
// Acquire/Release/Reject paths without standing up a real gRPC
// transport. Only Context() is consulted by the interceptor.
type fakeServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (f *fakeServerStream) Context() context.Context { return f.ctx }

func TestStreamInterceptorBelowCap(t *testing.T) {
	t.Parallel()
	l := admit.New("tempo", 2)
	var hits atomic.Int32
	interceptor := l.StreamInterceptor()
	handler := func(srv any, ss grpc.ServerStream) error {
		hits.Add(1)
		return nil
	}
	for range 5 {
		stream := &fakeServerStream{ctx: t.Context()}
		if err := interceptor(nil, stream, &grpc.StreamServerInfo{}, handler); err != nil {
			t.Fatalf("interceptor: %v", err)
		}
	}
	if hits.Load() != 5 {
		t.Fatalf("want 5 handler hits, got %d", hits.Load())
	}
}

func TestStreamInterceptorRejectsAtCap(t *testing.T) {
	t.Parallel()
	l := admit.New("tempo", 1)
	// Hold the slot so the next request through the interceptor hits
	// the cap.
	rel, ok := l.Acquire(t.Context())
	if !ok {
		t.Fatalf("setup acquire: want ok")
	}
	defer rel()

	interceptor := l.StreamInterceptor()
	handler := func(srv any, ss grpc.ServerStream) error {
		t.Fatalf("handler must not run when limiter is full")
		return nil
	}
	stream := &fakeServerStream{ctx: t.Context()}
	err := interceptor(nil, stream, &grpc.StreamServerInfo{}, handler)
	if err == nil {
		t.Fatalf("want ResourceExhausted, got nil")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("want grpc status, got %v", err)
	}
	if st.Code() != codes.ResourceExhausted {
		t.Fatalf("code: want ResourceExhausted, got %s", st.Code())
	}
}

func TestStreamInterceptorNilLimiterPassesThrough(t *testing.T) {
	t.Parallel()
	var l *admit.Limiter
	var hits atomic.Int32
	interceptor := l.StreamInterceptor()
	handler := func(srv any, ss grpc.ServerStream) error {
		hits.Add(1)
		return nil
	}
	for range 10 {
		stream := &fakeServerStream{ctx: t.Context()}
		if err := interceptor(nil, stream, &grpc.StreamServerInfo{}, handler); err != nil {
			t.Fatalf("interceptor: %v", err)
		}
	}
	if hits.Load() != 10 {
		t.Fatalf("want 10 hits, got %d", hits.Load())
	}
}

func TestStreamInterceptorReleasesOnHandlerError(t *testing.T) {
	t.Parallel()
	// A handler error must still release the slot so subsequent
	// requests can acquire. Mirrors the HTTP Middleware's defer-release
	// guarantee.
	l := admit.New("tempo", 1)
	interceptor := l.StreamInterceptor()
	wantErr := status.Error(codes.Internal, "boom")
	handler := func(srv any, ss grpc.ServerStream) error {
		return wantErr
	}
	stream := &fakeServerStream{ctx: t.Context()}
	if err := interceptor(nil, stream, &grpc.StreamServerInfo{}, handler); err != wantErr {
		t.Fatalf("want handler error pass-through, got %v", err)
	}
	// Slot must have been released — second call still succeeds.
	hit := false
	handler2 := func(srv any, ss grpc.ServerStream) error {
		hit = true
		return nil
	}
	stream2 := &fakeServerStream{ctx: t.Context()}
	if err := interceptor(nil, stream2, &grpc.StreamServerInfo{}, handler2); err != nil {
		t.Fatalf("second call: want ok, got %v", err)
	}
	if !hit {
		t.Fatalf("handler2 did not run — slot was not released after error")
	}
}
