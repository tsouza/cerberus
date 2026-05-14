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

// TestTelemetryInstall_ManualReader exercises the integration path the
// PR description called out: install a sdkmetric.MeterProvider with a
// ManualReader, drive one query through every instrument, assert the
// counter incremented and the histograms have data. Mirrors the
// recipe R4.5 will use when it swaps in the OTLP exporter — the only
// difference there is the Reader.
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
		"cerberus.queries.total":                   false,
		"cerberus.queries.duration.seconds":        false,
		"cerberus.pipeline.stage.duration.seconds": false,
		"cerberus.optimizer.rules_applied":         false,
		"cerberus.clickhouse.rows_read":            false,
		"cerberus.clickhouse.bytes_read":           false,
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
