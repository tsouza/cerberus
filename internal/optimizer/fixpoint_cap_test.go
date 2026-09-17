package optimizer_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/telemetry"
)

// fixpointCapHitsFor collects the current
// cerberus_optimizer_fixpoint_cap_hits_total value for batch from reader,
// or 0 when the counter has no data point for it.
func fixpointCapHitsFor(t *testing.T, reader *sdkmetric.ManualReader, batch string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "cerberus_optimizer_fixpoint_cap_hits_total" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s is %T, want an int64 Sum", m.Name, m.Data)
			}
			for _, dp := range sum.DataPoints {
				if v, ok := dp.Attributes.Value(telemetry.AttrOptimizerBatch); ok && v.AsString() == batch {
					return dp.Value
				}
			}
		}
	}
	return 0
}

// TestRunBatch_FixpointCapHitIsReported pins that a FixedPoint batch whose
// rules never converge does not leave its loop silently: the cap being hit
// is logged at WARN with the batch's name, and counted on
// cerberus_optimizer_fixpoint_cap_hits_total under the same name. A
// converging batch beside it produces neither. Not parallel: it swaps the
// process-global logger and meter provider.
func TestRunBatch_FixpointCapHitIsReported(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))

	reader := sdkmetric.NewManualReader()
	telemetry.Install(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { telemetry.Install(nil) })

	const (
		flapping   = "fp-flapping"
		converging = "fp-converging"
		capIters   = 5
	)
	var flapCalls, renameCalls int
	d := optimizer.NewWithBatches(
		optimizer.Batch{
			Name:     flapping,
			Strategy: optimizer.FixedPoint(capIters),
			Rules:    []optimizer.Rule{alwaysChangingRule("flap", &flapCalls)},
		},
		optimizer.Batch{
			Name:     converging,
			Strategy: optimizer.FixedPoint(capIters),
			Rules:    []optimizer.Rule{renamingRule("rename", "a", "b", &renameCalls)},
		},
	)

	d.Run(context.Background(), &chplan.Scan{Table: "a"})

	if flapCalls != capIters {
		t.Fatalf("flapping rule ran %d times, want the cap %d — the test's premise (a batch that hits its cap) does not hold", flapCalls, capIters)
	}
	out := logs.String()
	if !strings.Contains(out, `"level":"WARN"`) || !strings.Contains(out, `"batch":"`+flapping+`"`) {
		t.Errorf("no WARN naming batch %q was logged when it hit its iteration cap; got logs:\n%s", flapping, out)
	}
	if strings.Contains(out, `"batch":"`+converging+`"`) {
		t.Errorf("the converging batch %q was reported as hitting its cap; got logs:\n%s", converging, out)
	}
	if got := fixpointCapHitsFor(t, reader, flapping); got != 1 {
		t.Errorf("cerberus_optimizer_fixpoint_cap_hits_total{%s=%q} = %d, want 1", telemetry.AttrOptimizerBatch, flapping, got)
	}
	if got := fixpointCapHitsFor(t, reader, converging); got != 0 {
		t.Errorf("cerberus_optimizer_fixpoint_cap_hits_total{%s=%q} = %d, want 0", telemetry.AttrOptimizerBatch, converging, got)
	}
}

// TestDefaultDriver_NeverHitsItsCap runs the production batch list over the
// shapes the joint-fixpoint tests already enumerate and asserts none of
// them is reported: Default()'s batches are documented as converging well
// inside defaultMaxIterations, and this is the assertion behind that
// claim now that the cap being hit is observable at all.
func TestDefaultDriver_NeverHitsItsCap(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))

	for _, build := range pairPlans {
		optimizer.Default().Run(context.Background(), build())
	}
	if strings.Contains(logs.String(), "iteration cap") {
		t.Errorf("a Default() batch hit its iteration cap on a pair plan:\n%s", logs.String())
	}
}
