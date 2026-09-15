package chsql

import (
	"context"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// This file pins histogram_quantile_rankwalk_native.go's emission against the
// gremlins mutation lane (phase2-other, scope ./internal/chsql). Before it the
// file's only tests were a `//go:build integration` real-ClickHouse
// differential and chplan_ir_discriminates_test.go, which asserts only that
// the native and legacy quantile plans emit DIFFERENT SQL. That makes the
// emitter's lines COVERED without pinning any token in them, which is how five
// mutants stayed alive here. Each test names the construct it defends.

// rankWalkNativePlan builds an emittable native-aggregate HistogramQuantile.
func rankWalkNativePlan() *chplan.HistogramQuantile {
	return &chplan.HistogramQuantile{
		Input:                      classicQuantileInput(),
		Phi:                        0.9,
		MetricNameColumn:           "MetricName",
		AttributesColumn:           "Attributes",
		TimestampColumn:            "TimeUnix",
		BucketCountsColumn:         "BucketCounts",
		ExplicitBoundsColumn:       "ExplicitBounds",
		GroupBy:                    []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		GroupByAliases:             []string{"AttrsAlias"},
		UseNativeQuantileAggregate: true,
	}
}

// TestEmitHistogramQuantileRankWalkNative_LegacyColumnsDoNotDriveInput pins that
// physical inputs come from the child schema for the rank-walk path too.
func TestEmitHistogramQuantileRankWalkNative_LegacyColumnsDoNotDriveInput(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		counts string
		bounds string
	}{
		{name: "both present", counts: "BucketCounts", bounds: "ExplicitBounds"},
		{name: "bounds unset", counts: "BucketCounts", bounds: ""},
		{name: "counts unset", counts: "", bounds: "ExplicitBounds"},
		{name: "both unset", counts: "", bounds: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := rankWalkNativePlan()
			h.BucketCountsColumn = tc.counts
			h.ExplicitBoundsColumn = tc.bounds
			_, _, err := Emit(context.Background(), h)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
		})
	}
}

// TestEmitHistogramQuantileRankWalkNative_GroupByAliases pins the alias
// pairing between h.GroupBy and h.GroupByAliases, and with it the fact that a
// statement is emitted at all.
//
// Kills the CONDITIONALS_NEGATION mutant of
// histogram_quantile_rankwalk_native.go:`if i < len(h.GroupByAliases)`, which
// inverts the guard to `i >= len(…)` and so drops the alias from every group
// key that HAS one — the aliased case below then projects a bare
// “ `Attributes` “ and the output column the caller reads by name is gone.
//
// Kills the CONDITIONALS_BOUNDARY mutant of the same construct
// (`i <= len(…)`) via the second case: with fewer aliases than group keys the
// mutant indexes h.GroupByAliases one past its end and panics, where the
// original leaves the surplus key unaliased. A plan with a short alias slice
// is exactly what that guard exists for, so pinning it is pinning the
// contract, not manufacturing an input.
//
// Kills the CONDITIONALS_NEGATION mutant of the `if err != nil` guard below
// histogram_quantile_rankwalk_native.go:`sub, err := e.subqueryFrag(h.Input)`:
// negated, the guard returns a nil error on the SUCCESS path before any stage
// is built, so the emitter produces no statement. The projection assertions
// below fail on the empty result.
func TestEmitHistogramQuantileRankWalkNative_GroupByAliases(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		groupBy []chplan.Expr
		aliases []string
		want    string
	}{
		{
			name:    "one key one alias",
			groupBy: []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
			aliases: []string{"AttrsAlias"},
			want:    "SELECT `Attributes` AS `AttrsAlias`, ",
		},
		{
			name:    "two keys one alias",
			groupBy: []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}, &chplan.ColumnRef{Name: "MetricName"}},
			aliases: []string{"AttrsAlias"},
			want:    "SELECT `Attributes` AS `AttrsAlias`, `MetricName`, ",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := rankWalkNativePlan()
			h.GroupBy = tc.groupBy
			h.GroupByAliases = tc.aliases
			sql, _, err := Emit(context.Background(), h)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			if !strings.HasPrefix(sql, tc.want) {
				t.Errorf("emitted SQL does not start with %q\nSQL: %s", tc.want, sql)
			}
			// The aggregate itself: present only when the emitter ran past
			// its own error guards and built every stage.
			if !strings.Contains(sql, "quantilePrometheusHistogram(") {
				t.Errorf("emitted SQL carries no quantilePrometheusHistogram call\nSQL: %s", sql)
			}
		})
	}
}

// TestHistogramQuantileRankWalkNativeValueFrag_ComputedPhiNaNGuard pins that
// the leading `isNaN(phi) -> nan` branch is added for a COMPUTED phi and
// omitted for a literal one.
//
// Kills the CONDITIONALS_NEGATION mutant of
// histogram_quantile_rankwalk_native.go:`if h.PhiExpr == nil`, which swaps the
// two: a literal-phi plan would gain a guard the existing fixtures do not
// carry, and a computed-phi plan — the only one that can actually be NaN at
// runtime — would lose the guard Prometheus's bucketQuantile contract
// requires. Asserting the PREFIX is what discriminates, because the clamp
// inside the aggregate's parametric argument spells `isNaN` on both paths.
func TestHistogramQuantileRankWalkNativeValueFrag_ComputedPhiNaNGuard(t *testing.T) {
	t.Parallel()
	helpers := hqClassicHelperColumns{
		keptIdx:      hqClassicKeptIdxColumn,
		buckets:      hqClassicBucketsColumn,
		bounds:       hqClassicBoundsColumn,
		cum:          hqClassicCumColumn,
		observations: hqClassicObservationsColumn,
	}
	const guard = "if(isNaN("

	literal := rankWalkNativePlan()
	if got := renderFragToSQL(histogramQuantileRankWalkNativeValueFrag(literal, helpers)); strings.HasPrefix(got, guard) {
		t.Errorf("literal-phi Value starts with the NaN guard %q, want the bare core\n  %s", guard, got)
	}

	computed := rankWalkNativePlan()
	computed.PhiExpr = &chplan.ColumnRef{Name: "PhiCol"}
	if got := renderFragToSQL(histogramQuantileRankWalkNativeValueFrag(computed, helpers)); !strings.HasPrefix(got, guard) {
		t.Errorf("computed-phi Value does not start with the NaN guard %q\n  %s", guard, got)
	}
}
