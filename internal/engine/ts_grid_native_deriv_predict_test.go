package engine

import (
	"context"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
)

// TestPlanHasTSGridNativeDerivPredictLinear pins that a plan carrying a native
// deriv / predict_linear RangeWindowNative trips the experimental-setting gate,
// so the engine stamps allow_experimental_time_series_aggregate_functions=1 on
// exactly those queries — the same gate the rate / changes / resets members
// ride. The detector is generic over the node TYPE (not the Func), so this is
// the companion to the promql/chsql hollow-green guard
// (internal/chsql/range_window_native_deriv_predict_test.go): together they
// prove the feature ACTIVATES end-to-end (native node emitted AND the setting
// stamped), which the < 25.9 chDB substrate cannot show.
func TestPlanHasTSGridNativeDerivPredictLinear(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)

	for _, fn := range []string{"deriv", "predict_linear"} {
		fn := fn
		t.Run(fn, func(t *testing.T) {
			t.Parallel()
			node := &chplan.RangeWindowNative{
				Input:           &chplan.Scan{Table: "otel_metrics_gauge"},
				Func:            fn,
				Range:           5 * time.Minute,
				Step:            30 * time.Second,
				Start:           start,
				End:             end,
				TimestampColumn: "TimeUnix",
				ValueColumn:     "Value",
			}
			if fn == "predict_linear" {
				node.Scalars = []float64{3600}
			}
			// Wrap it the way the real plan does (a Project over the native node)
			// to prove the walk finds it below the root, not just at the root.
			plan := &chplan.Project{Input: node}

			if !planHasTSGridNative(plan) {
				t.Fatalf("planHasTSGridNative == false for a native %s plan — the experimental setting would NOT be stamped", fn)
			}

			// The setting the gate stamps must be the canonical experimental one.
			ctx := chclient.WithTSGridSetting(context.Background())
			settings := chclient.QuerySettingsFromContext(ctx)
			if v, ok := settings[chclient.SettingExperimentalTSGridAggregate]; !ok || v != 1 {
				t.Fatalf("expected %s=1 stamped, got %v (present=%v)",
					chclient.SettingExperimentalTSGridAggregate, v, ok)
			}
		})
	}
}
