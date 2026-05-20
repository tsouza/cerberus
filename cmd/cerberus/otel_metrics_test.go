package main

import (
	"strings"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/tsouza/cerberus/internal/telemetry"
)

// TestTelemetryInstall_NilNoopByDefault verifies the
// telemetry.Install(nil) path used by main.go installs a usable noop
// MeterProvider. Recording on the cached instruments must succeed and
// produce no panic; the manual-reader integration test below covers
// the recording side end-to-end.
func TestTelemetryInstall_NilNoopByDefault(t *testing.T) {
	telemetry.Install(nil)
	t.Cleanup(func() { telemetry.Install(nil) })

	telemetry.ObserveQuery("promql", "GET /api/v1/query").
		Done(t.Context(), telemetry.ResultOK)
}

// TestTelemetryInstall_ManualReader exercises the metric-export
// integration path: install a sdkmetric.MeterProvider with a
// ManualReader, drive one query through every instrument, assert the
// counter incremented and the histograms have data. The OTLP exporter
// recipe mirrors this — the only difference is the Reader.
func TestTelemetryInstall_ManualReader(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	telemetry.Install(mp)
	t.Cleanup(func() { telemetry.Install(nil) })

	// Drive every instrument once.
	telemetry.ObserveQuery("promql", "GET /api/v1/query").
		Done(t.Context(), telemetry.ResultOK)
	telemetry.ObserveStage(telemetry.StageParse).Done(t.Context())
	telemetry.ObserveStage(telemetry.StageOptimize).Done(t.Context())
	telemetry.RecordRulesApplied(t.Context(), 3)
	telemetry.RecordClickHouseProgress(t.Context(), "promql", 100, 1024)

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
			t.Errorf("metric %q not emitted; scopes=%v", name, rm.ScopeMetrics)
		}
	}
}
