package chsql

import (
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// histogramProjectionPlan returns a HistogramProjection carrying every
// required column name, so a test can vary GroupBy / GroupByAliases alone.
func histogramProjectionPlan(groupBy []chplan.Expr, aliases []string) *chplan.HistogramProjection {
	return &chplan.HistogramProjection{
		Input:                      &chplan.Scan{Table: "otel_metrics_exponential_histogram"},
		GroupBy:                    groupBy,
		GroupByAliases:             aliases,
		ScaleColumn:                "Scale",
		ZeroCountColumn:            "ZeroCount",
		PositiveOffsetColumn:       "PositiveOffset",
		PositiveBucketCountsColumn: "PositiveBucketCounts",
		NegativeOffsetColumn:       "NegativeOffset",
		NegativeBucketCountsColumn: "NegativeBucketCounts",
		CountColumn:                "Count",
		SumColumn:                  "Sum",
	}
}

// TestMutation_HistogramProjection_FewerAliasesThanGroupKeys pins that
// GroupByAliases is OPTIONAL per group key: chplan permits an unaliased key,
// so a plan carrying more keys than aliases must render the surplus keys bare
// rather than indexing off the end of the alias slice.
//
// Kills histogram_projection.go:`i < len(h.GroupByAliases)` under BOTH of its
// mutators:
//
//   - CONDITIONALS_BOUNDARY (`i <= len(...)`): at i == 1 with one alias the
//     mutated guard is `1 <= 1`, so it reads GroupByAliases[1] out of a
//     one-element slice and panics.
//   - CONDITIONALS_NEGATION (`i >= len(...)`): the same read happens at i == 1
//     (`1 >= 1`) and panics too; and had it not, the first key would have lost
//     its alias, which the assertion below also rejects.
func TestMutation_HistogramProjection_FewerAliasesThanGroupKeys(t *testing.T) {
	t.Parallel()

	sql := emitNodeSQL(t, histogramProjectionPlan(
		[]chplan.Expr{&chplan.ColumnRef{Name: "ServiceName"}, &chplan.ColumnRef{Name: "MetricName"}},
		[]string{"svc"},
	))
	if !strings.HasPrefix(sql, "SELECT `ServiceName` AS `svc`, `MetricName`, ") {
		t.Fatalf("the aliased key must render `AS `svc`` and the surplus key bare, got %q", sql)
	}
}

// TestMutation_HistogramProjection_EveryAliasProjected pins the other half of
// the same guard: when an alias IS supplied for a key it must reach the SELECT
// list, because that alias is the label name the decode side binds the group
// column by.
//
// Kills histogram_projection.go:`i < len(h.GroupByAliases)` under
// CONDITIONALS_NEGATION (`i >= len(...)`)
// without relying on an out-of-range panic: with two keys and two aliases the
// inverted guard is false at every index, so both keys render bare.
func TestMutation_HistogramProjection_EveryAliasProjected(t *testing.T) {
	t.Parallel()

	sql := emitNodeSQL(t, histogramProjectionPlan(
		[]chplan.Expr{&chplan.ColumnRef{Name: "ServiceName"}, &chplan.ColumnRef{Name: "MetricName"}},
		[]string{"svc", "metric"},
	))
	if !strings.HasPrefix(sql, "SELECT `ServiceName` AS `svc`, `MetricName` AS `metric`, ") {
		t.Fatalf("every supplied alias must reach the SELECT list, got %q", sql)
	}
}
