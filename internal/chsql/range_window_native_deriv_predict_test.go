package chsql_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// This is the HOLLOW-GREEN GUARD for the native deriv / predict_linear
// timeSeries*ToGrid lowerings. The CI chDB substrate is < 25.9, so the native
// aggregates are ABSENT and every version-gated fixture falls back to the
// fan-out — a broken native path would therefore stay invisible on chDB. These
// tests force the feature ENABLED via the chopt EnabledSet's boot-wired
// strategy (NOT a live server) and assert the feature ACTIVATES: the plan
// carries a chplan.RangeWindowNative node and chsql.Emit renders the exact
// native aggregate call with the correct parametric args. The native==fan-out
// numeric differential is a prod/e2e concern above the chDB floor (documented
// in docs/native-clickhouse.md), so it is deliberately NOT asserted here.
//
// The engine stamps allow_experimental_time_series_aggregate_functions=1 on any
// plan carrying a RangeWindowNative node (internal/engine.planHasTSGridNative,
// which is generic over the node TYPE, not the Func) — the companion assertion
// that deriv / predict_linear ride that gate lives in
// internal/engine/ts_grid_native_deriv_predict_test.go.

const nativeDerivPredictRangeStep = 30 * time.Second

// lowerRangeNative parses q, lowers it over the default OTel-CH schema in range
// mode ([2026-01-01T00:00:00Z, +5m] step 30s) with the supplied boot-wired
// strategy table, and returns the plan.
func lowerRangeNative(t *testing.T, q string, lowerers promql.RangeLowerers) chplan.Node {
	t.Helper()
	p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(q)
	if err != nil {
		t.Fatalf("parse %q: %v", q, err)
	}
	rangeStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rangeEnd := rangeStart.Add(5 * time.Minute)
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, schema.DefaultOTelMetrics(),
		rangeStart, rangeEnd, nativeDerivPredictRangeStep, promql.LowerOpts{Lowerers: lowerers})
	if err != nil {
		t.Fatalf("lower %q: %v", q, err)
	}
	return plan
}

// findNativeNode returns the first chplan.RangeWindowNative in plan, or nil.
func findNativeNode(plan chplan.Node) *chplan.RangeWindowNative {
	var found *chplan.RangeWindowNative
	chplan.Walk(plan, func(n chplan.Node) bool {
		if found != nil {
			return false
		}
		if rw, ok := n.(*chplan.RangeWindowNative); ok {
			found = rw
			return false
		}
		return true
	})
	return found
}

func TestNativeDerivActivatesAndEmits(t *testing.T) {
	t.Parallel()

	lowerers := promql.RangeLowerers{
		Deriv: promql.NativeDerivLowerer{Fallback: promql.FanoutDerivLowerer{}},
	}
	plan := lowerRangeNative(t, "sum by(host) (deriv(disk_used_bytes[5m]))", lowerers)

	native := findNativeNode(plan)
	if native == nil {
		t.Fatalf("native deriv feature ENABLED but plan carries no RangeWindowNative — the feature is inert (hollow green): %s",
			spineOfTypes(plan))
	}
	if native.Func != "deriv" {
		t.Errorf("RangeWindowNative.Func = %q, want %q", native.Func, "deriv")
	}
	if len(native.Scalars) != 0 {
		t.Errorf("deriv RangeWindowNative.Scalars = %v, want empty (deriv takes no scalar)", native.Scalars)
	}

	sqlStr, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if !strings.Contains(sqlStr, "timeSeriesDerivToGrid(") {
		t.Errorf("emitted SQL missing timeSeriesDerivToGrid( call — native emit is broken:\n%s", sqlStr)
	}
	// The shared (start, end, step_s, window_s) parametric prefix: step 30s,
	// window 5m = 300s. deriv adds NO trailing param.
	if !strings.Contains(sqlStr, "timeSeriesDerivToGrid(toDateTime(") {
		t.Errorf("emitted SQL deriv call missing whole-second DateTime start bound:\n%s", sqlStr)
	}
	if !strings.Contains(sqlStr, ", 30, 300)(") {
		t.Errorf("emitted SQL deriv call missing (step_s=30, window_s=300) params:\n%s", sqlStr)
	}
}

func TestNativePredictLinearActivatesAndEmits(t *testing.T) {
	t.Parallel()

	lowerers := promql.RangeLowerers{
		PredictLinear: promql.NativePredictLinearLowerer{Fallback: promql.FanoutPredictLinearLowerer{}},
	}
	plan := lowerRangeNative(t, "sum by(host) (predict_linear(disk_used_bytes[10m], 3600))", lowerers)

	native := findNativeNode(plan)
	if native == nil {
		t.Fatalf("native predict_linear feature ENABLED but plan carries no RangeWindowNative — the feature is inert (hollow green): %s",
			spineOfTypes(plan))
	}
	if native.Func != "predict_linear" {
		t.Errorf("RangeWindowNative.Func = %q, want %q", native.Func, "predict_linear")
	}
	if len(native.Scalars) != 1 || native.Scalars[0] != 3600 {
		t.Errorf("predict_linear RangeWindowNative.Scalars = %v, want [3600] (the whole-second horizon t)", native.Scalars)
	}

	sqlStr, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if !strings.Contains(sqlStr, "timeSeriesPredictLinearToGrid(") {
		t.Errorf("emitted SQL missing timeSeriesPredictLinearToGrid( call — native emit is broken:\n%s", sqlStr)
	}
	// The (start, end, step_s=30, window_s=600) prefix + the horizon t=3600 as
	// the 5th parametric arg.
	if !strings.Contains(sqlStr, ", 30, 600, 3600)(") {
		t.Errorf("emitted SQL predict_linear call missing (step_s=30, window_s=600, predict_offset_s=3600) params:\n%s", sqlStr)
	}
}

// TestNativePredictLinearFractionalFallsBack pins the eligibility carve-out: a
// FRACTIONAL horizon t cannot ride timeSeriesPredictLinearToGrid's integer 5th
// parametric arg, so even with the native feature ENABLED the lowering
// delegates to the fan-out (exact `intercept + slope*t` Float64 arithmetic).
func TestNativePredictLinearFractionalFallsBack(t *testing.T) {
	t.Parallel()

	lowerers := promql.RangeLowerers{
		PredictLinear: promql.NativePredictLinearLowerer{Fallback: promql.FanoutPredictLinearLowerer{}},
	}
	plan := lowerRangeNative(t, "sum by(host) (predict_linear(disk_used_bytes[10m], 1.5))", lowerers)

	if native := findNativeNode(plan); native != nil {
		t.Fatalf("fractional-horizon predict_linear MUST stay on the fan-out, but got a RangeWindowNative (Func=%q, Scalars=%v)",
			native.Func, native.Scalars)
	}
	sqlStr, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	if !strings.Contains(sqlStr, "simpleLinearRegression") {
		t.Errorf("fractional-horizon predict_linear fan-out missing simpleLinearRegression:\n%s", sqlStr)
	}
	if strings.Contains(sqlStr, "timeSeriesPredictLinearToGrid(") {
		t.Errorf("fractional-horizon predict_linear must NOT emit the native aggregate:\n%s", sqlStr)
	}
}

// TestNativeDerivPredictFeatureDisabledStaysFanout pins the default (feature
// OFF) path: with the all-fan-out table, neither deriv nor predict_linear
// produces a RangeWindowNative — the byte-identical fallback the < 25.9
// substrate runs.
func TestNativeDerivPredictFeatureDisabledStaysFanout(t *testing.T) {
	t.Parallel()

	for _, q := range []string{
		"sum by(host) (deriv(disk_used_bytes[5m]))",
		"sum by(host) (predict_linear(disk_used_bytes[10m], 3600))",
	} {
		q := q
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			// Zero-value RangeLowerers => withDefaults => all fan-out.
			plan := lowerRangeNative(t, q, promql.RangeLowerers{})
			if native := findNativeNode(plan); native != nil {
				t.Fatalf("feature OFF but %q produced a RangeWindowNative (Func=%q) — the default path must stay fan-out",
					q, native.Func)
			}
			sqlStr, _, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("emit: %v", err)
			}
			if !strings.Contains(sqlStr, "simpleLinearRegression") {
				t.Errorf("feature-OFF %q fan-out missing simpleLinearRegression:\n%s", q, sqlStr)
			}
			if strings.Contains(sqlStr, "ToGrid(") {
				t.Errorf("feature-OFF %q must NOT emit any native *ToGrid aggregate:\n%s", q, sqlStr)
			}
		})
	}
}

// spineOfTypes renders a compact node-type spine of plan for failure messages
// so a hollow-green failure shows WHAT shape leaked through instead of the
// native node.
func spineOfTypes(n chplan.Node) string {
	var b strings.Builder
	var walk func(chplan.Node, int)
	walk = func(n chplan.Node, depth int) {
		if n == nil {
			return
		}
		b.WriteString(strings.Repeat("  ", depth))
		fmt.Fprintf(&b, "%T", n)
		b.WriteByte('\n')
		for _, c := range n.Children() {
			walk(c, depth+1)
		}
	}
	walk(n, 0)
	return b.String()
}
