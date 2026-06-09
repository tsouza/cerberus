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
			mux.Handle("GET /api/v1/query", telemetry.QueryMiddleware("promql", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
