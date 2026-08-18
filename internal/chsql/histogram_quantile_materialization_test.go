package chsql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// TestEmitHistogramQuantile_MaterialisesEveryArrayWalkOnce pins the
// classic-quantile emitter's staging contract: each expensive per-row
// array walk it needs — the coalescing index scan, the Float64 bucket
// cast, the coalesced bound array, the cumulative ladder, the
// observation total and the rank-walk stop index — appears EXACTLY ONCE
// in the emitted SQL, materialised into its own derived-query column.
//
// Why this is a hard assertion and not a style preference. The
// interpolation reads those quantities four to eight times each, and
// when they are written inline every reference re-renders the whole
// underlying expression tree. Such a query is only affordable because
// ClickHouse's query analyzer folds the repeated occurrences into one
// evaluation — and that fold is not a property of the SQL, it is a
// property of a setting that can be switched off per query
// (internal/engine/query_settings_rules.go stamps `enable_analyzer=0`
// on some plan shapes). With the analyzer off, every repeat is
// evaluated for real: on the native (exponential) quantile path exactly
// that combination exhausted the server's memory limit inside
// arrayMap/arraySum scalar evaluation. Counting occurrences here makes
// the single evaluation a property of the emitted bytes, so re-inlining
// any of these quantities fails the build instead of waiting for an
// analyzer setting to expose it in production.
func TestEmitHistogramQuantile_MaterialisesEveryArrayWalkOnce(t *testing.T) {
	t.Parallel()

	// The two Attributes-canonicalising arrayMap calls the Scan-side
	// projection contributes are not part of the quantile arithmetic;
	// this plan has none of them, so every arrayMap counted below belongs
	// to the quantile itself.
	newPlan := func(cumulative bool) *chplan.HistogramQuantile {
		return &chplan.HistogramQuantile{
			Input:                  &chplan.Scan{Table: "otel_metrics_histogram"},
			Phi:                    0.9,
			BucketCountsColumn:     "BucketCounts",
			ExplicitBoundsColumn:   "ExplicitBounds",
			BucketCountsCumulative: cumulative,
			GroupBy:                []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
			GroupByAliases:         []string{"Attributes"},
		}
	}

	cases := []struct {
		name string
		// cumulative selects the already-cumulative ladder contract, on
		// which the emitter neither arrayCumSums nor arraySums.
		cumulative bool
		want       map[string]int
	}{
		{
			name:       "per-bucket counts",
			cumulative: false,
			want: map[string]int{
				"arrayFilter(":     1, // the coalescing index scan
				"arrayConcat(":     1, // that scan extended over the overflow rung
				"arrayCumSum(":     1, // the ladder
				"arraySum(":        1, // the observation total
				"arrayFirstIndex(": 1, // the rank walk
				// bucket cast + coalesced bounds + coalesced ladder.
				"arrayMap(": 3,
			},
		},
		{
			name:       "already-cumulative ladder",
			cumulative: true,
			want: map[string]int{
				"arrayFilter(":     1,
				"arrayConcat(":     1,
				"arrayCumSum(":     0, // the input IS the ladder
				"arraySum(":        0, // the total is the ladder's top rung
				"arrayFirstIndex(": 1,
				"arrayMap(":        3,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sql, _, err := chsql.Emit(context.Background(), newPlan(tc.cumulative))
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			for fn, want := range tc.want {
				if got := strings.Count(sql, fn); got != want {
					t.Errorf("%s occurs %d time(s), want %d — a quantile array walk is being re-derived at its use sites instead of read from its staged column\nsql=%s", fn, got, want, sql)
				}
			}
		})
	}
}
