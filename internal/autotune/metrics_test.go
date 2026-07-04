package autotune

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/tsouza/cerberus/internal/routerrules"
)

// gaugeValues collects one manual-reader snapshot into a name→value map over all
// int64 observable gauges.
func gaugeValues(t *testing.T, reader *metric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			g, ok := m.Data.(metricdata.Gauge[int64])
			if !ok || len(g.DataPoints) == 0 {
				continue
			}
			out[m.Name] = g.DataPoints[0].Value
		}
	}
	return out
}

func TestRegisterMetrics_PublishesReporterState(t *testing.T) {
	reader := metric.NewManualReader()
	mp := metric.NewMeterProvider(metric.WithReader(reader))
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(mp)
	t.Cleanup(func() { otel.SetMeterProvider(prev) })

	rep := NewReporter(Status{
		Active:     true,
		Configured: routerrules.Thresholds{MinFanout: 16, MinAnchorPairs: 4000},
		Live:       routerrules.Thresholds{MinFanout: 8, MinAnchorPairs: 1928},
		Stats:      Stats{AppliedTicks: 2, ErrorTicks: 1},
		Outcome:    Outcome{OOMMinFanout: 8, RouteAOomCount: 3, RouteBExecutions: 100, RouteBOomCount: 0},
	})
	if err := RegisterMetrics(rep); err != nil {
		t.Fatalf("RegisterMetrics: %v", err)
	}

	got := gaugeValues(t, reader)
	want := map[string]int64{
		"cerberus_solver_autotune_active":                1,
		"cerberus_solver_autotune_min_fanout":            8,
		"cerberus_solver_autotune_min_anchor_pairs":      1928,
		"cerberus_solver_autotune_configured_min_fanout": 16,
		"cerberus_solver_autotune_applied_total":         2,
		"cerberus_solver_autotune_errors_total":          1,
		"cerberus_solver_autotune_route_a_ooms":          3,
		"cerberus_solver_autotune_route_b_executions":    100,
		"cerberus_solver_autotune_route_b_ooms":          0,
		"cerberus_solver_autotune_oom_min_fanout":        8,
	}
	for name, wv := range want {
		if got[name] != wv {
			t.Errorf("%s = %d, want %d", name, got[name], wv)
		}
	}

	// A nil reporter must be a no-op, not a panic.
	if err := RegisterMetrics(nil); err != nil {
		t.Errorf("RegisterMetrics(nil) = %v, want nil", err)
	}
}
