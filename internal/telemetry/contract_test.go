package telemetry_test

import (
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/tsouza/cerberus/internal/telemetry"
)

// TestMetricNames_PublicContract pins every metric name cerberus exposes
// over OTLP. Dashboards + alerting rules reference these by exact name;
// any rename is a breaking change for downstream consumers. If you
// genuinely need to rename a metric add the new instrument first and
// keep the old one for one release.
func TestMetricNames_PublicContract(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	telemetry.Install(mp)
	t.Cleanup(func() { telemetry.Install(nil) })

	telemetry.ObserveQuery("promql", "GET /api/v1/query").Done(t.Context(), telemetry.ResultOK)
	telemetry.ObserveStage(telemetry.StageParse).Done(t.Context())
	telemetry.ObserveStage(telemetry.StageLower).Done(t.Context())
	telemetry.ObserveStage(telemetry.StageOptimize).Done(t.Context())
	telemetry.ObserveStage(telemetry.StageEmit).Done(t.Context())
	telemetry.ObserveStage(telemetry.StageExecute).Done(t.Context())
	telemetry.RecordRulesApplied(t.Context(), 1)
	telemetry.RecordClickHouseProgress(t.Context(), "promql", 100, 2000)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}

	want := map[string]bool{
		"cerberus_queries_total":                   false,
		"cerberus_queries_duration_seconds":        false,
		"cerberus_pipeline_stage_duration_seconds": false,
		"cerberus_optimizer_rules_applied":         false,
		"cerberus_clickhouse_rows_read":            false,
		"cerberus_clickhouse_bytes_read":           false,
	}
	for _, sm := range rm.ScopeMetrics {
		if !strings.HasSuffix(sm.Scope.Name, "internal/telemetry") {
			continue
		}
		for _, m := range sm.Metrics {
			if _, ok := want[m.Name]; ok {
				want[m.Name] = true
			}
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("metric %q not emitted; rename breaks dashboards", name)
		}
	}
}

// TestMetricUnits_PublicContract pins the unit field per metric. The
// OTel SDK propagates the unit into the OTLP wire format and downstream
// systems (Prometheus, Mimir, vendors) bucket by unit when deriving rate
// vs. count semantics. Changing a unit is a breaking change.
func TestMetricUnits_PublicContract(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	telemetry.Install(mp)
	t.Cleanup(func() { telemetry.Install(nil) })

	telemetry.ObserveQuery("promql", "GET /api/v1/query").Done(t.Context(), telemetry.ResultOK)
	telemetry.ObserveStage(telemetry.StageParse).Done(t.Context())
	telemetry.RecordRulesApplied(t.Context(), 1)
	telemetry.RecordClickHouseProgress(t.Context(), "promql", 100, 2000)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}

	wantUnits := map[string]string{
		"cerberus_queries_total":                   "{query}",
		"cerberus_queries_duration_seconds":        "s",
		"cerberus_pipeline_stage_duration_seconds": "s",
		"cerberus_optimizer_rules_applied":         "{rule}",
		"cerberus_clickhouse_rows_read":            "{row}",
		"cerberus_clickhouse_bytes_read":           "By",
	}
	for _, sm := range rm.ScopeMetrics {
		if !strings.HasSuffix(sm.Scope.Name, "internal/telemetry") {
			continue
		}
		for _, m := range sm.Metrics {
			want, ok := wantUnits[m.Name]
			if !ok {
				continue
			}
			if m.Unit != want {
				t.Errorf("metric %q unit = %q; want %q", m.Name, m.Unit, want)
			}
		}
	}
}

// TestAttributeKeys_PublicContract documents the per-metric attribute
// keys downstream dashboards pivot on. cerberus.ql / cerberus.route /
// result on the query counter; stage on the stage histogram.
func TestAttributeKeys_PublicContract(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	telemetry.Install(mp)
	t.Cleanup(func() { telemetry.Install(nil) })

	telemetry.ObserveQuery("logql", "GET /loki/api/v1/query_range").
		Done(t.Context(), telemetry.ResultOK)
	telemetry.ObserveStage(telemetry.StageEmit).Done(t.Context())

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}

	for _, sm := range rm.ScopeMetrics {
		if !strings.HasSuffix(sm.Scope.Name, "internal/telemetry") {
			continue
		}
		for _, m := range sm.Metrics {
			switch m.Name {
			case "cerberus_queries_total":
				sum := m.Data.(metricdata.Sum[int64])
				if len(sum.DataPoints) == 0 {
					t.Fatalf("queries.total: empty")
				}
				attrs := sum.DataPoints[0].Attributes
				for _, key := range []attribute.Key{"cerberus.ql", "cerberus.route", "result"} {
					if _, ok := attrs.Value(key); !ok {
						t.Errorf("queries.total: missing attr %q", key)
					}
				}
			case "cerberus_pipeline_stage_duration_seconds":
				hist := m.Data.(metricdata.Histogram[float64])
				if len(hist.DataPoints) == 0 {
					t.Fatalf("stage.duration: empty")
				}
				if _, ok := hist.DataPoints[0].Attributes.Value("stage"); !ok {
					t.Errorf("stage.duration: missing stage attr")
				}
			}
		}
	}
}

// TestStageNames_PublicContract pins the exact stage values dashboards
// filter on — parse / lower / optimize / emit / execute.
func TestStageNames_PublicContract(t *testing.T) {
	want := map[string]string{
		"parse":    telemetry.StageParse,
		"lower":    telemetry.StageLower,
		"optimize": telemetry.StageOptimize,
		"emit":     telemetry.StageEmit,
		"execute":  telemetry.StageExecute,
	}
	for v, got := range want {
		if got != v {
			t.Errorf("stage const for %q = %q; want %q", v, got, v)
		}
	}
}

// TestResultValues_PublicContract pins the bucket labels for the result
// attribute. dashboards group on these literal strings.
func TestResultValues_PublicContract(t *testing.T) {
	if telemetry.ResultOK != "ok" {
		t.Errorf("ResultOK = %q; want ok", telemetry.ResultOK)
	}
	if telemetry.ResultError != "error" {
		t.Errorf("ResultError = %q; want error", telemetry.ResultError)
	}
}

// TestGet_NoopMeterProviderProducesNonNilInstruments confirms Get() is
// usable even with a noop provider — callers don't have to guard
// against nil instruments. Anchor for the auto-install path that goes
// noop when no OTLP endpoint is configured.
func TestGet_NoopMeterProviderProducesNonNilInstruments(t *testing.T) {
	telemetry.Install(nil)
	t.Cleanup(func() { telemetry.Install(nil) })

	inst := telemetry.Get()
	if inst == nil {
		t.Fatal("Get = nil under noop provider")
	}
	if inst.QueriesTotal == nil || inst.QueryDuration == nil ||
		inst.StageDuration == nil || inst.RulesApplied == nil ||
		inst.ClickHouseRowsRead == nil || inst.ClickHouseBytesRead == nil {
		t.Error("noop instrument set has a nil field")
	}
}

// TestReset_RebuildsInstrumentsAgainstNewProvider verifies the
// Reset()-after-Install contract used by tests. Install resets the
// cache; subsequent Get() builds against the freshly-installed
// MeterProvider.
func TestReset_RebuildsInstrumentsAgainstNewProvider(t *testing.T) {
	reader1 := sdkmetric.NewManualReader()
	mp1 := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader1))
	telemetry.Install(mp1)

	telemetry.ObserveQuery("promql", "GET /api/v1/query").Done(t.Context(), telemetry.ResultOK)

	// Re-install with a fresh reader; the next observation must land
	// on it, not the previous one.
	reader2 := sdkmetric.NewManualReader()
	mp2 := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader2))
	telemetry.Install(mp2)
	t.Cleanup(func() { telemetry.Install(nil) })

	telemetry.ObserveQuery("logql", "GET /loki/api/v1/query").Done(t.Context(), telemetry.ResultOK)

	var rm metricdata.ResourceMetrics
	if err := reader2.Collect(t.Context(), &rm); err != nil {
		t.Fatalf("collect reader2: %v", err)
	}
	foundLoki := false
	for _, sm := range rm.ScopeMetrics {
		if !strings.HasSuffix(sm.Scope.Name, "internal/telemetry") {
			continue
		}
		for _, m := range sm.Metrics {
			if m.Name != "cerberus_queries_total" {
				continue
			}
			sum := m.Data.(metricdata.Sum[int64])
			for _, dp := range sum.DataPoints {
				if v, _ := dp.Attributes.Value("cerberus.ql"); v.AsString() == "logql" {
					foundLoki = true
				}
			}
		}
	}
	if !foundLoki {
		t.Error("reader2 missing logql observation — Install did not reset cache")
	}
}

// TestObserveQuery_RoutePopulatedFromExplicitArgument anchors the
// QueryMiddleware-bypass path: callers that call ObserveQuery directly
// must see the route they passed survive to the metric attributes.
func TestObserveQuery_RoutePopulatedFromExplicitArgument(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	telemetry.Install(mp)
	t.Cleanup(func() { telemetry.Install(nil) })

	telemetry.ObserveQuery("traceql", "GET /api/traces/{id}").
		Done(t.Context(), telemetry.ResultError)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		if !strings.HasSuffix(sm.Scope.Name, "internal/telemetry") {
			continue
		}
		for _, m := range sm.Metrics {
			if m.Name != "cerberus_queries_total" {
				continue
			}
			sum := m.Data.(metricdata.Sum[int64])
			if len(sum.DataPoints) != 1 {
				t.Fatalf("queries.total DPs: got %d want 1", len(sum.DataPoints))
			}
			route, _ := sum.DataPoints[0].Attributes.Value("cerberus.route")
			if route.AsString() != "GET /api/traces/{id}" {
				t.Errorf("route attr = %q; want %q", route.AsString(), "GET /api/traces/{id}")
			}
		}
	}
}

// TestStageTimer_DoneOnNilReceiverIsSafe pins the nil-safe defer
// contract used by handlers.
func TestStageTimer_DoneOnNilReceiverIsSafe(t *testing.T) {
	var st *telemetry.StageTimer
	st.Done(t.Context()) // must not panic
}

// TestQueryTimer_DoneOnNilReceiverIsSafe mirrors the above for the
// query timer.
func TestQueryTimer_DoneOnNilReceiverIsSafe(t *testing.T) {
	var qt *telemetry.QueryTimer
	qt.Done(t.Context(), telemetry.ResultOK) // must not panic
}
