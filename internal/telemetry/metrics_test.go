package telemetry_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/tsouza/cerberus/internal/telemetry"
)

// installManualReader swaps the global MeterProvider with one backed by
// a ManualReader so tests can pull a deterministic ResourceMetrics
// snapshot after each instrument call. Returns the reader so tests can
// call Collect; t.Cleanup restores the noop default.
func installManualReader(t *testing.T) *metric.ManualReader {
	t.Helper()
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	telemetry.Install(mp)
	t.Cleanup(func() { telemetry.Install(nil) })
	return reader
}

// collect drains the manual reader once and returns the scope metrics
// for the cerberus telemetry scope. Fails the test if the scope is
// missing — every instrument in this package lives there.
func collect(t *testing.T, reader *metric.ManualReader) metricdata.ScopeMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		if strings.HasSuffix(sm.Scope.Name, "internal/telemetry") {
			return sm
		}
	}
	t.Fatalf("cerberus telemetry scope not found; scopes=%v", rm.ScopeMetrics)
	return metricdata.ScopeMetrics{}
}

// findMetric returns the named metric from sm or fails the test.
func findMetric(t *testing.T, sm metricdata.ScopeMetrics, name string) metricdata.Metrics {
	t.Helper()
	for _, m := range sm.Metrics {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("metric %q not found; have %v", name, metricNames(sm))
	return metricdata.Metrics{}
}

func metricNames(sm metricdata.ScopeMetrics) []string {
	names := make([]string, 0, len(sm.Metrics))
	for _, m := range sm.Metrics {
		names = append(names, m.Name)
	}
	return names
}

// bucketFor returns the index of the histogram bucket an observation of
// value lands in, given the explicit bounds: the first i with
// value <= bounds[i], or len(bounds) for the +Inf overflow bucket.
// Mirrors the OTel SDK's bucket assignment (upper-bound inclusive).
func bucketFor(bounds []float64, value float64) int {
	for i, b := range bounds {
		if value <= b {
			return i
		}
	}
	return len(bounds)
}

// histogramBounds extracts (Bounds, BucketCounts, Count) from a histogram
// metric regardless of its point type (float64 vs int64). Fails the test
// when the metric carries more or fewer than one datapoint — every case
// in TestHistogramBucketBoundaries records exactly one observation
// without attributes.
func histogramBounds(t *testing.T, m metricdata.Metrics) ([]float64, []uint64, uint64) {
	t.Helper()
	switch h := m.Data.(type) {
	case metricdata.Histogram[float64]:
		if len(h.DataPoints) != 1 {
			t.Fatalf("%s: got %d DPs want 1", m.Name, len(h.DataPoints))
		}
		dp := h.DataPoints[0]
		return dp.Bounds, dp.BucketCounts, dp.Count
	case metricdata.Histogram[int64]:
		if len(h.DataPoints) != 1 {
			t.Fatalf("%s: got %d DPs want 1", m.Name, len(h.DataPoints))
		}
		dp := h.DataPoints[0]
		return dp.Bounds, dp.BucketCounts, dp.Count
	default:
		t.Fatalf("%s: unexpected data type %T", m.Name, m.Data)
		return nil, nil, 0
	}
}

// TestHistogramBucketBoundaries pins every histogram instrument to its
// exported explicit-bucket ladder. Regression test for the flat-4.75s
// p95 bug: the instruments used to be built without
// WithExplicitBucketBoundaries, inheriting the OTel SDK's
// millisecond-shaped defaults [0, 5, 10, ..., 10000] while recording
// seconds — every real query duration (2ms–1s) collapsed into the (0,5]
// bucket and histogram_quantile(0.95) interpolated 0.95×5 = 4.75s for
// every cerberus_ql. The test asserts what the dashboard consumer
// actually depends on: the exported datapoint Bounds match the
// seconds-scale (resp. count-scale) ladders, and a representative
// observation lands in its mid-ladder bucket rather than the first one.
func TestHistogramBucketBoundaries(t *testing.T) {
	cases := []struct {
		metricName string
		record     func(ctx context.Context)
		wantBounds []float64
		value      float64 // representative observation recorded by record
	}{
		{
			metricName: "cerberus_queries_duration_seconds",
			record: func(ctx context.Context) {
				telemetry.Get().QueryDuration.Record(ctx, 0.3)
			},
			wantBounds: telemetry.QueryDurationBoundaries,
			value:      0.3, // a realistic query duration → (0.25, 0.5]
		},
		{
			metricName: "cerberus_pipeline_stage_duration_seconds",
			record: func(ctx context.Context) {
				telemetry.Get().StageDuration.Record(ctx, 0.003)
			},
			wantBounds: telemetry.StageDurationBoundaries,
			value:      0.003, // a realistic stage duration → (0.0025, 0.005]
		},
		{
			metricName: "cerberus_optimizer_rules_applied",
			record: func(ctx context.Context) {
				telemetry.Get().RulesApplied.Record(ctx, 2)
			},
			wantBounds: telemetry.RulesAppliedBoundaries,
			value:      2, // → (1, 2]
		},
		{
			metricName: "cerberus_clickhouse_rows_read",
			record: func(ctx context.Context) {
				telemetry.Get().ClickHouseRowsRead.Record(ctx, 50_000)
			},
			wantBounds: telemetry.RowsReadBoundaries,
			value:      50_000, // → (1e4, 1e5]
		},
		{
			metricName: "cerberus_clickhouse_bytes_read",
			record: func(ctx context.Context) {
				telemetry.Get().ClickHouseBytesRead.Record(ctx, 5_000_000)
			},
			wantBounds: telemetry.BytesReadBoundaries,
			value:      5_000_000, // → (1e6, 1e7]
		},
	}
	for _, tc := range cases {
		t.Run(tc.metricName, func(t *testing.T) {
			reader := installManualReader(t)
			tc.record(t.Context())

			sm := collect(t, reader)
			m := findMetric(t, sm, tc.metricName)
			bounds, counts, count := histogramBounds(t, m)

			// (a) The exported bounds ARE the instrument's bounds — i.e.
			// not the SDK's millisecond-shaped defaults.
			assert.Equal(t, tc.wantBounds, bounds,
				"%s: datapoint Bounds must match the exported boundary ladder", tc.metricName)

			// (b) The representative observation lands in its mid-ladder
			// bucket, not the first one (where the ms-default layout
			// dumped every seconds-scale value).
			if count != 1 {
				t.Fatalf("%s: count = %d, want 1", tc.metricName, count)
			}
			wantBucket := bucketFor(tc.wantBounds, tc.value)
			if wantBucket == 0 {
				t.Fatalf("%s: representative value %v falls in the first bucket — pick a mid-ladder value", tc.metricName, tc.value)
			}
			if got := counts[wantBucket]; got != 1 {
				t.Errorf("%s: observation %v landed in counts=%v, want 1 in bucket %d (%v, %v]",
					tc.metricName, tc.value, counts, wantBucket,
					tc.wantBounds[wantBucket-1], tc.wantBounds[wantBucket])
			}
		})
	}
}

// TestObserveQuery_RecordsCounterAndDuration covers the QueryTimer
// happy path: a single Done(ResultOK) call must bump
// cerberus_queries_total by one and record a point on
// cerberus_queries_duration_seconds with matching attributes.
func TestObserveQuery_RecordsCounterAndDuration(t *testing.T) {
	reader := installManualReader(t)

	tm := telemetry.ObserveQuery("promql", "GET /api/v1/query")
	tm.Done(t.Context(), telemetry.ResultOK)

	sm := collect(t, reader)
	total := findMetric(t, sm, "cerberus_queries_total")
	sum, ok := total.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("queries.total: unexpected data type %T", total.Data)
	}
	if len(sum.DataPoints) != 1 || sum.DataPoints[0].Value != 1 {
		t.Fatalf("queries.total: got %+v want one DP with value=1", sum.DataPoints)
	}
	// Attribute round-trip — dashboards rely on these labels.
	attrs := sum.DataPoints[0].Attributes
	if v, ok := attrs.Value("cerberus.ql"); !ok || v.AsString() != "promql" {
		t.Errorf("ql attr: got %v ok=%v", v.AsString(), ok)
	}
	if v, ok := attrs.Value("cerberus.route"); !ok || v.AsString() != "GET /api/v1/query" {
		t.Errorf("route attr: got %v ok=%v", v.AsString(), ok)
	}
	if v, ok := attrs.Value("result"); !ok || v.AsString() != telemetry.ResultOK {
		t.Errorf("result attr: got %v ok=%v", v.AsString(), ok)
	}

	dur := findMetric(t, sm, "cerberus_queries_duration_seconds")
	hist, ok := dur.Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("queries.duration: unexpected data type %T", dur.Data)
	}
	if len(hist.DataPoints) != 1 || hist.DataPoints[0].Count != 1 {
		t.Fatalf("queries.duration: got %+v want one DP count=1", hist.DataPoints)
	}
}

// TestObserveStage_RecordsHistogram exercises the stage timer: each
// Done() lands a histogram point labelled with the stage name.
func TestObserveStage_RecordsHistogram(t *testing.T) {
	reader := installManualReader(t)

	for _, stage := range []string{
		telemetry.StageParse,
		telemetry.StageLower,
		telemetry.StageOptimize,
		telemetry.StageEmit,
		telemetry.StageExecute,
	} {
		telemetry.ObserveStage(stage).Done(t.Context())
	}

	sm := collect(t, reader)
	m := findMetric(t, sm, "cerberus_pipeline_stage_duration_seconds")
	hist, ok := m.Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("stage.duration: unexpected data type %T", m.Data)
	}
	// One DP per distinct stage attribute value.
	if got, want := len(hist.DataPoints), 5; got != want {
		t.Fatalf("stage.duration DPs: got %d want %d", got, want)
	}
}

// TestRecordRulesApplied_RecordsHistogram covers the optimizer's
// rules_applied histogram.
func TestRecordRulesApplied_RecordsHistogram(t *testing.T) {
	reader := installManualReader(t)

	telemetry.RecordRulesApplied(t.Context(), 7)
	telemetry.RecordRulesApplied(t.Context(), 3)

	sm := collect(t, reader)
	m := findMetric(t, sm, "cerberus_optimizer_rules_applied")
	hist, ok := m.Data.(metricdata.Histogram[int64])
	if !ok {
		t.Fatalf("rules_applied: unexpected data type %T", m.Data)
	}
	if len(hist.DataPoints) != 1 || hist.DataPoints[0].Count != 2 {
		t.Fatalf("rules_applied: got %+v want one DP with count=2", hist.DataPoints)
	}
}

// TestRecordClickHouseProgress covers the rows/bytes pair recorded
// from the chclient progress callback.
func TestRecordClickHouseProgress(t *testing.T) {
	reader := installManualReader(t)

	telemetry.RecordClickHouseProgress(t.Context(), "promql", 1234, 56789)

	sm := collect(t, reader)
	rows := findMetric(t, sm, "cerberus_clickhouse_rows_read")
	rh, ok := rows.Data.(metricdata.Histogram[int64])
	if !ok || len(rh.DataPoints) != 1 || rh.DataPoints[0].Sum != 1234 {
		t.Fatalf("rows_read: got %+v want sum=1234", rh.DataPoints)
	}
	bytes := findMetric(t, sm, "cerberus_clickhouse_bytes_read")
	bh, ok := bytes.Data.(metricdata.Histogram[int64])
	if !ok || len(bh.DataPoints) != 1 || bh.DataPoints[0].Sum != 56789 {
		t.Fatalf("bytes_read: got %+v want sum=56789", bh.DataPoints)
	}
}

// TestQueryMiddleware_ResultOK exercises the full middleware path: an
// http.ServeMux with a registered route, wrapped by QueryMiddleware,
// served via httptest. Asserts the counter increments with the correct
// route attribute pulled from r.Pattern.
func TestQueryMiddleware_ResultOK(t *testing.T) {
	reader := installManualReader(t)

	// Per-route registration is how the real api/{prom,loki,tempo}
	// handlers wire the middleware — that ordering lets the middleware
	// see r.Pattern after the mux resolves the route.
	mux := http.NewServeMux()
	mux.Handle("GET /api/v1/query", telemetry.QueryMiddleware("promql", noopPanicRenderer, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/query?query=up")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()

	sm := collect(t, reader)
	total := findMetric(t, sm, "cerberus_queries_total")
	sum := total.Data.(metricdata.Sum[int64])
	if len(sum.DataPoints) != 1 {
		t.Fatalf("queries.total DPs: got %d want 1", len(sum.DataPoints))
	}
	dp := sum.DataPoints[0]
	if v, _ := dp.Attributes.Value("cerberus.route"); v.AsString() != "GET /api/v1/query" {
		t.Errorf("route: got %q want %q", v.AsString(), "GET /api/v1/query")
	}
	if v, _ := dp.Attributes.Value("result"); v.AsString() != telemetry.ResultOK {
		t.Errorf("result: got %q want ok", v.AsString())
	}
}

// TestQueryMiddleware_ResultError verifies any status >= 400 (4xx parse
// rejection, 4xx lower rejection, 5xx execution failure) is bucketed as
// error. The `cerberus_queries_total{result}` series is a query-outcome
// metric (not an HTTP SLO), so a 400 parse rejection counts as a failed
// query — the same way the "Error rate by language" dashboard reads it.
func TestQueryMiddleware_ResultError(t *testing.T) {
	cases := []struct {
		name   string
		status int
	}{
		{"400_bad_request", http.StatusBadRequest},
		{"422_unprocessable_entity", http.StatusUnprocessableEntity},
		{"500_internal", http.StatusInternalServerError},
		{"502_bad_gateway", http.StatusBadGateway},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := installManualReader(t)

			mux := http.NewServeMux()
			status := tc.status
			mux.Handle("GET /api/v1/query", telemetry.QueryMiddleware("promql", noopPanicRenderer, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
			})))
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)

			resp, err := http.Get(srv.URL + "/api/v1/query")
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			_ = resp.Body.Close()

			sm := collect(t, reader)
			total := findMetric(t, sm, "cerberus_queries_total")
			sum := total.Data.(metricdata.Sum[int64])
			if len(sum.DataPoints) != 1 {
				t.Fatalf("queries.total DPs: got %d want 1", len(sum.DataPoints))
			}
			if v, _ := sum.DataPoints[0].Attributes.Value("result"); v.AsString() != telemetry.ResultError {
				t.Errorf("result: got %q want error", v.AsString())
			}
		})
	}
}

// noopPanicRenderer is the [telemetry.PanicRenderer] the success/error
// middleware tests pass when no panic is expected. It writes a minimal
// 500 envelope so a regression that starts panicking still produces a
// well-formed response the test can observe.
func noopPanicRenderer(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write([]byte(`{"status":"error"}`))
}

// TestQueryMiddleware_PanicRecovered_RendersEnvelopeAndCountsError pins
// the RC1 robustness contract at the shared-middleware layer: a handler
// panic must NOT drop the connection. The middleware must (a) recover,
// (b) invoke the head's PanicRenderer so a 500 body reaches the client,
// and (c) still record the failed query on cerberus_queries_total with
// result=error — the metric-defer fix. Before the fix, t.Done() ran
// inline after ServeHTTP and a panic bypassed it entirely.
func TestQueryMiddleware_PanicRecovered_RendersEnvelopeAndCountsError(t *testing.T) {
	reader := installManualReader(t)

	rendered := false
	renderer := func(w http.ResponseWriter, _ *http.Request) {
		rendered = true
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"status":"error","errorType":"internal"}`))
	}

	mux := http.NewServeMux()
	mux.Handle("GET /api/v1/query", telemetry.QueryMiddleware("promql", renderer,
		http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			panic("boom: simulated handler panic")
		})))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/query?query=up")
	if err != nil {
		t.Fatalf("GET: %v (connection must not drop on panic)", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status: got %d want 500", resp.StatusCode)
	}
	if !rendered {
		t.Error("PanicRenderer was not invoked on the recovered panic")
	}

	sm := collect(t, reader)
	total := findMetric(t, sm, "cerberus_queries_total")
	sum := total.Data.(metricdata.Sum[int64])
	if len(sum.DataPoints) != 1 {
		t.Fatalf("queries.total DPs: got %d want 1 (panic must still count)", len(sum.DataPoints))
	}
	if v, _ := sum.DataPoints[0].Attributes.Value("result"); v.AsString() != telemetry.ResultError {
		t.Errorf("result: got %q want error", v.AsString())
	}
	// The duration histogram must also record the panicked query so its
	// count stays balanced with the total counter.
	dur := findMetric(t, sm, "cerberus_queries_duration_seconds")
	dh := dur.Data.(metricdata.Histogram[float64])
	if len(dh.DataPoints) != 1 || dh.DataPoints[0].Count != 1 {
		t.Errorf("query_duration: got %+v want one DP with count=1", dh.DataPoints)
	}
}

// TestQueryMiddleware_PanicAfterCommit_NoDoubleWrite verifies that when a
// handler panics AFTER it already committed a status line / body bytes,
// the middleware does NOT call the renderer (the wire is past the point
// of re-rendering) but still records the query so the metric stays
// balanced.
func TestQueryMiddleware_PanicAfterCommit_NoDoubleWrite(t *testing.T) {
	reader := installManualReader(t)

	rendererCalled := false
	renderer := func(w http.ResponseWriter, _ *http.Request) {
		rendererCalled = true
		w.WriteHeader(http.StatusInternalServerError)
	}

	mux := http.NewServeMux()
	mux.Handle("GET /api/v1/query", telemetry.QueryMiddleware("promql", renderer,
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("partial body"))
			panic("boom: panic after partial write")
		})))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/query?query=up")
	if err == nil {
		defer resp.Body.Close()
		// A 200 status line already went out; the body is truncated but
		// the status can't change. The renderer must not have fired.
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status: got %d want 200 (already committed)", resp.StatusCode)
		}
	}
	if rendererCalled {
		t.Error("PanicRenderer must NOT fire once the response is committed")
	}

	sm := collect(t, reader)
	total := findMetric(t, sm, "cerberus_queries_total")
	sum := total.Data.(metricdata.Sum[int64])
	if len(sum.DataPoints) != 1 {
		t.Fatalf("queries.total DPs: got %d want 1", len(sum.DataPoints))
	}
	if v, _ := sum.DataPoints[0].Attributes.Value("result"); v.AsString() != telemetry.ResultError {
		t.Errorf("result: got %q want error (committed-200 then panic is still a failed query)", v.AsString())
	}
}

// TestInstall_NilFallsBackToNoop verifies the nil-provider path
// installs the noop without panic; subsequent recordings are silently
// dropped — Get() still returns a usable Instruments struct.
func TestInstall_NilFallsBackToNoop(t *testing.T) {
	telemetry.Install(nil)
	t.Cleanup(func() { telemetry.Install(nil) })
	// Should be a no-op, not a panic.
	telemetry.ObserveStage(telemetry.StageEmit).Done(context.Background())
}

// inflightValue reads the (signed) cumulative value the manual reader
// reports for cerberus_query_inflight for the given ql label. Returns
// 0 when the metric / label combination hasn't been recorded yet.
func inflightValue(t *testing.T, reader *metric.ManualReader, ql string) int64 {
	t.Helper()
	sm := collect(t, reader)
	for _, m := range sm.Metrics {
		if m.Name != "cerberus_query_inflight" {
			continue
		}
		sum, ok := m.Data.(metricdata.Sum[int64])
		if !ok {
			t.Fatalf("query_inflight: unexpected data type %T", m.Data)
		}
		for _, dp := range sum.DataPoints {
			if v, _ := dp.Attributes.Value("cerberus.ql"); v.AsString() == ql {
				return dp.Value
			}
		}
	}
	return 0
}

// TestObserveQueryInflight_IncrementDecrement covers the inc/dec
// round-trip: a single ObserveQueryInflight call must leave the gauge
// at 1; invoking the returned closure must restore it to 0. This is
// the synthetic-engine-call surrogate — exercises the defer pattern
// without standing up a chclient stub.
func TestObserveQueryInflight_IncrementDecrement(t *testing.T) {
	reader := installManualReader(t)

	dec := telemetry.ObserveQueryInflight(t.Context(), "promql")
	if got := inflightValue(t, reader, "promql"); got != 1 {
		t.Fatalf("after increment: got %d want 1", got)
	}
	dec()
	if got := inflightValue(t, reader, "promql"); got != 0 {
		t.Fatalf("after decrement: got %d want 0", got)
	}
}

// TestObserveQueryInflight_BalancedAcrossPanic anchors the defer
// contract: even when the caller panics between increment and the
// decrement defer, the gauge must return to zero. This is the
// property the engine relies on — Engine.QueryPlan defers the
// decrement so panics in optimize / emit / execute don't strand a
// stuck +1 on the gauge.
func TestObserveQueryInflight_BalancedAcrossPanic(t *testing.T) {
	reader := installManualReader(t)

	assert.Panics(t, func() {
		defer telemetry.ObserveQueryInflight(t.Context(), "logql")()
		panic("synthetic engine failure")
	}, "synthetic panic must propagate; the defer-decrement is what we verify after")

	if got := inflightValue(t, reader, "logql"); got != 0 {
		t.Fatalf("post-panic inflight: got %d want 0 (defer must decrement)", got)
	}
}

// TestObserveQueryInflight_PerLanguageLabels confirms the cerberus.ql
// attribute lands on the gauge so the dashboard's
// `sum by (cerberus_ql) (cerberus_query_inflight)` pivot resolves
// the three head identifiers independently.
func TestObserveQueryInflight_PerLanguageLabels(t *testing.T) {
	reader := installManualReader(t)

	decProm := telemetry.ObserveQueryInflight(t.Context(), "promql")
	decLoki := telemetry.ObserveQueryInflight(t.Context(), "logql")
	decTempo := telemetry.ObserveQueryInflight(t.Context(), "traceql")
	t.Cleanup(func() {
		decProm()
		decLoki()
		decTempo()
	})

	for _, ql := range []string{"promql", "logql", "traceql"} {
		if got := inflightValue(t, reader, ql); got != 1 {
			t.Errorf("inflight[%s]: got %d want 1", ql, got)
		}
	}
}
