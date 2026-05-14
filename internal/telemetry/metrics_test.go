package telemetry_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

// TestObserveQuery_RecordsCounterAndDuration covers the QueryTimer
// happy path: a single Done(ResultOK) call must bump
// cerberus.queries.total by one and record a point on
// cerberus.queries.duration.seconds with matching attributes.
func TestObserveQuery_RecordsCounterAndDuration(t *testing.T) {
	reader := installManualReader(t)

	tm := telemetry.ObserveQuery("promql", "GET /api/v1/query")
	tm.Done(t.Context(), telemetry.ResultOK)

	sm := collect(t, reader)
	total := findMetric(t, sm, "cerberus.queries.total")
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

	dur := findMetric(t, sm, "cerberus.queries.duration.seconds")
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
	m := findMetric(t, sm, "cerberus.pipeline.stage.duration.seconds")
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
	m := findMetric(t, sm, "cerberus.optimizer.rules_applied")
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
	rows := findMetric(t, sm, "cerberus.clickhouse.rows_read")
	rh, ok := rows.Data.(metricdata.Histogram[int64])
	if !ok || len(rh.DataPoints) != 1 || rh.DataPoints[0].Sum != 1234 {
		t.Fatalf("rows_read: got %+v want sum=1234", rh.DataPoints)
	}
	bytes := findMetric(t, sm, "cerberus.clickhouse.bytes_read")
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
	mux.Handle("GET /api/v1/query", telemetry.QueryMiddleware("promql", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
	total := findMetric(t, sm, "cerberus.queries.total")
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

// TestQueryMiddleware_ResultError verifies a 5xx is bucketed as error.
// 4xx stays ok by design (handler returned a well-formed rejection;
// the gateway behaved correctly).
func TestQueryMiddleware_ResultError(t *testing.T) {
	reader := installManualReader(t)

	mux := http.NewServeMux()
	mux.Handle("GET /api/v1/query", telemetry.QueryMiddleware("promql", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/query")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()

	sm := collect(t, reader)
	total := findMetric(t, sm, "cerberus.queries.total")
	sum := total.Data.(metricdata.Sum[int64])
	if len(sum.DataPoints) != 1 {
		t.Fatalf("queries.total DPs: got %d want 1", len(sum.DataPoints))
	}
	if v, _ := sum.DataPoints[0].Attributes.Value("result"); v.AsString() != telemetry.ResultError {
		t.Errorf("result: got %q want error", v.AsString())
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
