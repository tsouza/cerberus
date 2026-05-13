package optimizer_test

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/schema"
)

// sumRollup5m is the canonical 5-minute sum rollup used across the
// safety-condition tests. Mirrors the default registry entry but is
// declared locally so a registry tweak doesn't silently change what
// the unit tests are exercising.
var sumRollup5m = schema.Rollup{
	BaseTable:   "otel_metrics_sum",
	RollupTable: "otel_metrics_sum_5m",
	Window:      5 * time.Minute,
	AggOp:       schema.RollupAggSum,
	ValueColumn: "Sum",
}

// baseRangeWindow builds a RangeWindow over a Scan(otel_metrics_sum)
// with the supplied func / range / step. Series-identity is the
// OTel-CH default (`Attributes`).
func baseRangeWindow(fn string, rng, step time.Duration) *chplan.RangeWindow {
	return &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_sum"},
		Func:            fn,
		Range:           rng,
		Step:            step,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
}

func runRule(t *testing.T, plan chplan.Node, rollups []schema.Rollup) chplan.Node {
	t.Helper()
	rule := optimizer.MVSubstitution(rollups, "Value")
	out, _ := rule.Apply(plan)
	return out
}

// TestMVSubstitution_SumOverTimeApplies covers the happy path:
// sum_over_time(metric[1h]) with step=5m over a 5-minute sum-rollup.
// All four conditions hold; the rule must rewrite Scan.Table and
// RangeWindow.ValueColumn.
func TestMVSubstitution_SumOverTimeApplies(t *testing.T) {
	t.Parallel()

	plan := baseRangeWindow("sum_over_time", time.Hour, 5*time.Minute)
	out := runRule(t, plan, []schema.Rollup{sumRollup5m})

	rw, ok := out.(*chplan.RangeWindow)
	if !ok {
		t.Fatalf("expected RangeWindow, got %T", out)
	}
	scan, ok := rw.Input.(*chplan.Scan)
	if !ok {
		t.Fatalf("expected Scan child, got %T", rw.Input)
	}
	if scan.Table != "otel_metrics_sum_5m" {
		t.Errorf("Scan.Table: got %q, want otel_metrics_sum_5m", scan.Table)
	}
	if rw.ValueColumn != "Sum" {
		t.Errorf("RangeWindow.ValueColumn: got %q, want Sum (rollup pre-aggregated column)", rw.ValueColumn)
	}
}

// TestMVSubstitution_StepTooSmall covers safety condition (1):
// step < rollup window must skip. 30s step over a 5m rollup would
// produce more anchors than the rollup has buckets to distinguish.
func TestMVSubstitution_StepTooSmall(t *testing.T) {
	t.Parallel()

	plan := baseRangeWindow("sum_over_time", time.Hour, 30*time.Second)
	out := runRule(t, plan, []schema.Rollup{sumRollup5m})

	scan := out.(*chplan.RangeWindow).Input.(*chplan.Scan)
	if scan.Table != "otel_metrics_sum" {
		t.Errorf("expected rule to skip (step=30s < window=5m); Scan.Table got %q", scan.Table)
	}
}

// TestMVSubstitution_RangeNotMultiple covers safety condition (2):
// range not a multiple of the rollup window must skip. 7m range
// over a 5m rollup straddles bucket boundaries.
func TestMVSubstitution_RangeNotMultiple(t *testing.T) {
	t.Parallel()

	plan := baseRangeWindow("sum_over_time", 7*time.Minute, 5*time.Minute)
	out := runRule(t, plan, []schema.Rollup{sumRollup5m})

	scan := out.(*chplan.RangeWindow).Input.(*chplan.Scan)
	if scan.Table != "otel_metrics_sum" {
		t.Errorf("expected rule to skip (range=7m not multiple of window=5m); Scan.Table got %q", scan.Table)
	}
}

// TestMVSubstitution_RangeBelowWindow covers safety condition (2)
// boundary: range < window must skip. A 1-minute range over a
// 5-minute rollup has no aligned bucket.
func TestMVSubstitution_RangeBelowWindow(t *testing.T) {
	t.Parallel()

	plan := baseRangeWindow("sum_over_time", time.Minute, 5*time.Minute)
	out := runRule(t, plan, []schema.Rollup{sumRollup5m})

	scan := out.(*chplan.RangeWindow).Input.(*chplan.Scan)
	if scan.Table != "otel_metrics_sum" {
		t.Errorf("expected rule to skip (range=1m < window=5m); Scan.Table got %q", scan.Table)
	}
}

// TestMVSubstitution_RangeExactlyWindowApplies covers the boundary:
// range == window is a multiple of itself; the rule must accept it.
func TestMVSubstitution_RangeExactlyWindowApplies(t *testing.T) {
	t.Parallel()

	plan := baseRangeWindow("sum_over_time", 5*time.Minute, 5*time.Minute)
	out := runRule(t, plan, []schema.Rollup{sumRollup5m})

	scan := out.(*chplan.RangeWindow).Input.(*chplan.Scan)
	if scan.Table != "otel_metrics_sum_5m" {
		t.Errorf("expected rule to fire (range=5m == window=5m); Scan.Table got %q", scan.Table)
	}
}

// TestMVSubstitution_AvgOverTimeBlocked covers safety condition (3):
// avg_over_time does NOT commute with a sum-rollup (or with any of
// the v1 RollupAggOp values, including a hypothetical avg-rollup
// that we deliberately do not allow because per-bucket weights are
// missing). The rule must skip.
func TestMVSubstitution_AvgOverTimeBlocked(t *testing.T) {
	t.Parallel()

	plan := baseRangeWindow("avg_over_time", time.Hour, 5*time.Minute)
	out := runRule(t, plan, []schema.Rollup{sumRollup5m})

	scan := out.(*chplan.RangeWindow).Input.(*chplan.Scan)
	if scan.Table != "otel_metrics_sum" {
		t.Errorf("expected rule to skip (avg_over_time does not commute with sum-rollup without per-bucket weights); Scan.Table got %q", scan.Table)
	}
}

// TestMVSubstitution_NoRollupsForTable covers safety condition (4):
// when the rollup registry has no entry whose BaseTable matches the
// Scan, the rule is a no-op even if other conditions hold.
func TestMVSubstitution_NoRollupsForTable(t *testing.T) {
	t.Parallel()

	// Gauge scan but registry only carries sum rollups.
	plan := &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_gauge"},
		Func:            "sum_over_time",
		Range:           time.Hour,
		Step:            5 * time.Minute,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
	out := runRule(t, plan, []schema.Rollup{sumRollup5m})

	scan := out.(*chplan.RangeWindow).Input.(*chplan.Scan)
	if scan.Table != "otel_metrics_gauge" {
		t.Errorf("expected rule to skip (no rollups declared for otel_metrics_gauge); Scan.Table got %q", scan.Table)
	}
}

// TestMVSubstitution_EmptyRegistryNoOp covers the all-empty case: a
// deployment that has not enabled rollups must never see the rule
// fire.
func TestMVSubstitution_EmptyRegistryNoOp(t *testing.T) {
	t.Parallel()

	plan := baseRangeWindow("sum_over_time", time.Hour, 5*time.Minute)
	out := runRule(t, plan, nil)

	scan := out.(*chplan.RangeWindow).Input.(*chplan.Scan)
	if scan.Table != "otel_metrics_sum" {
		t.Errorf("expected rule to skip (empty registry); Scan.Table got %q", scan.Table)
	}
}

// TestMVSubstitution_FirstApplicablePicksCoarsest verifies the v1
// cost-model behaviour: when the registry lists the 1h rollup before
// the 5m rollup and both are applicable (24h range, 1h step), the
// rule picks the coarser-grained rollup.
func TestMVSubstitution_FirstApplicablePicksCoarsest(t *testing.T) {
	t.Parallel()

	rollups := []schema.Rollup{
		{
			BaseTable:   "otel_metrics_sum",
			RollupTable: "otel_metrics_sum_1h",
			Window:      time.Hour,
			AggOp:       schema.RollupAggSum,
			ValueColumn: "Sum",
		},
		sumRollup5m,
	}
	plan := baseRangeWindow("sum_over_time", 24*time.Hour, time.Hour)
	out := runRule(t, plan, rollups)

	scan := out.(*chplan.RangeWindow).Input.(*chplan.Scan)
	if scan.Table != "otel_metrics_sum_1h" {
		t.Errorf("firstApplicable cost model: expected coarsest applicable rollup otel_metrics_sum_1h, got %q", scan.Table)
	}
}

// TestMVSubstitution_FirstApplicableSkipsTooCoarse verifies that when
// the coarsest candidate fails a safety condition but a finer one
// passes, the rule still substitutes against the finer candidate.
// Registry lists 1h first; query uses 5m step + 30m range — 1h is
// rejected (step < window), 5m applies.
func TestMVSubstitution_FirstApplicableSkipsTooCoarse(t *testing.T) {
	t.Parallel()

	rollups := []schema.Rollup{
		{
			BaseTable:   "otel_metrics_sum",
			RollupTable: "otel_metrics_sum_1h",
			Window:      time.Hour,
			AggOp:       schema.RollupAggSum,
			ValueColumn: "Sum",
		},
		sumRollup5m,
	}
	plan := baseRangeWindow("sum_over_time", 30*time.Minute, 5*time.Minute)
	out := runRule(t, plan, rollups)

	scan := out.(*chplan.RangeWindow).Input.(*chplan.Scan)
	if scan.Table != "otel_metrics_sum_5m" {
		t.Errorf("expected rule to skip 1h rollup (step<window) and fall through to 5m rollup, got %q", scan.Table)
	}
}

// TestMVSubstitution_ValueColumnMismatchSkips covers the guard that
// keeps the rule from touching non-sample-shaped RangeWindows (e.g.
// TraceQL matrix shape where ValueColumn is unused). The plan's
// ValueColumn doesn't match the schema's base ValueColumn so the
// rule must decline.
func TestMVSubstitution_ValueColumnMismatchSkips(t *testing.T) {
	t.Parallel()

	plan := baseRangeWindow("sum_over_time", time.Hour, 5*time.Minute)
	plan.ValueColumn = "" // simulate a TraceQL-matrix-shape RangeWindow
	out := runRule(t, plan, []schema.Rollup{sumRollup5m})

	scan := out.(*chplan.RangeWindow).Input.(*chplan.Scan)
	if scan.Table != "otel_metrics_sum" {
		t.Errorf("expected rule to skip (ValueColumn unset); Scan.Table got %q", scan.Table)
	}
}

// TestMVSubstitution_ChangedFlag verifies the Rule.Apply contract:
// when a substitution happens the second return value is true, and
// re-applying the rule against the rewritten plan returns false (the
// rollup is not in the registry as a BaseTable, so the second pass
// finds nothing to do).
func TestMVSubstitution_ChangedFlag(t *testing.T) {
	t.Parallel()

	rule := optimizer.MVSubstitution([]schema.Rollup{sumRollup5m}, "Value")
	plan := baseRangeWindow("sum_over_time", time.Hour, 5*time.Minute)

	out1, ch1 := rule.Apply(plan)
	if !ch1 {
		t.Fatalf("first Apply: expected changed=true on a fresh rewrite")
	}
	_, ch2 := rule.Apply(out1)
	if ch2 {
		t.Errorf("second Apply: expected changed=false (fixpoint after one rewrite), got true")
	}
}

// TestMVSubstitution_RuleName pins the rule name used in trace logs
// and test fixtures.
func TestMVSubstitution_RuleName(t *testing.T) {
	t.Parallel()
	if got := optimizer.MVSubstitution(nil, "Value").Name(); got != "mv-substitution" {
		t.Errorf("Name(): got %q, want mv-substitution", got)
	}
}
