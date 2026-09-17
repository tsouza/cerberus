package chsql_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// fanoutMinSamplesPlan returns the array-aggregate histogram fan-out with the
// per-function "no sample emitted" floor set to minSamples.
func fanoutMinSamplesPlan(minSamples int) *chplan.RangeBucketFanout {
	return &chplan.RangeBucketFanout{
		Input:        closedTimestampTestScan("otel_metrics_exponential_histogram", "TimeUnix", "Attributes", "BucketCounts"),
		Start:        time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC),
		End:          time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC),
		Step:         30 * time.Second,
		Lookback:     5 * time.Minute,
		GroupBy:      []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		AnchorAlias:  "anchor_ts",
		TimestampCol: "TimeUnix",
		MinSamples:   minSamples,
		AggFuncs: []chplan.AggFunc{
			{
				Fn:    chplan.FnArgMax,
				Alias: "BucketCounts",
				Args: []chplan.Expr{
					&chplan.ColumnRef{Name: "BucketCounts"},
					&chplan.ColumnRef{Name: "TimeUnix"},
				},
			},
		},
	}
}

// TestMutation_RangeBucketFanout_MinSamplesFilterThreshold defends
// range_bucket_fanout.go:`r.MinSamples > fanoutNoMinSampleFilter` (the gate on
// the collapse HAVING).
//
// fanoutNoMinSampleFilter is 1: an anchor whose window holds no sample already
// receives no fanned row and so produces no GROUP BY row, which makes "at
// least one sample" free and a `HAVING uniqExact(<ts>) >= 1` pure overhead —
// it re-derives a per-group aggregate to assert something the fan-out already
// guarantees. Only the `rate` / `increase` floor of two scrapes needs the
// filter.
//
// Two mutations sit on this comparison and both cases below separate them:
//
//   - CONDITIONALS_BOUNDARY (`>` -> `>=`) makes MinSamples == 1 emit the
//     redundant HAVING.
//   - CONDITIONALS_NEGATION (`>` -> `<=`) inverts the gate outright: the
//     redundant floor is emitted and the load-bearing one (MinSamples == 2,
//     the two-scrape rule) is dropped, so every anchor holding a single scrape
//     emits a rate sample reference PromQL suppresses.
func TestMutation_RangeBucketFanout_MinSamplesFilterThreshold(t *testing.T) {
	t.Parallel()

	// The two-scrape floor `rate` / `increase` need, and the free floors
	// (0 and 1 samples) the bounded fan-out already enforces structurally.
	const (
		rateMinSamples = 2
		freeHaving     = "HAVING uniqExact(`TimeUnix`)"
		rateHaving     = "HAVING uniqExact(`TimeUnix`) >= 2"
	)

	for _, minSamples := range []int{0, 1} {
		sql, _, err := chsql.Emit(context.Background(), fanoutMinSamplesPlan(minSamples))
		if err != nil {
			t.Fatalf("Emit (MinSamples=%d): %v", minSamples, err)
		}
		if strings.Contains(sql, freeHaving) {
			t.Errorf("MinSamples=%d needs no HAVING (the fan-out already emits no row for an empty anchor):\n%s", minSamples, sql)
		}
	}

	sql, _, err := chsql.Emit(context.Background(), fanoutMinSamplesPlan(rateMinSamples))
	if err != nil {
		t.Fatalf("Emit (MinSamples=%d): %v", rateMinSamples, err)
	}
	if !strings.Contains(sql, rateHaving) {
		t.Errorf("MinSamples=%d must emit %q (the rate/increase two-scrape rule):\n%s", rateMinSamples, rateHaving, sql)
	}
}

// TestMutation_RangeBucketFanout_MixedAliasedGroupByHoist defends the
// per-index hoist loop issue #3551 added: every ALIASED GroupBy entry is
// materialized in the fan-out SELECT under its own synthetic column
// (rangeBucketFanoutGroupKeyAlias) rather than re-derived from a same-scope
// alias in the collapse's own GROUP BY. Every other RangeBucketFanout test in
// this package uses a single-entry GroupBy that is either fully aliased or
// fully bare, which cannot distinguish "hoist this entry" from "hoist every
// entry" or "hoist no entry" — this plan mixes one of each, in that order, so
// each of the four mutations below changes the emitted SQL:
//
//   - INVERT_LOOPCTRL (`continue` -> `break`) at the hoist loop: with a bare
//     entry at index 0, `break` would exit before ever reaching the aliased
//     entry at index 1, so `_rbf_key_2` would never be materialized.
//   - CONDITIONALS_NEGATION (`== ""` -> `!= ""`) on the same guard: flips
//     which entry gets hoisted, materializing the bare entry's raw column
//     reference under a needless synthetic alias and leaving the aliased
//     entry's own expression untouched.
//   - CONDITIONALS_NEGATION (`hoisted != ""` -> `hoisted == ""`) in the
//     collapse SELECT-list loop: swaps which entry reads its hoisted column
//     vs. re-renders its raw expression.
//   - The same mutation in the GROUP BY loop, independently.
//
// A single-entry plan cannot catch any of these: with only one GroupBy
// entry, "the loop keeps going" and "the branch picks the other entry" have
// nothing else in the slice to get wrong.
func TestMutation_RangeBucketFanout_MixedAliasedGroupByHoist(t *testing.T) {
	t.Parallel()

	plan := &chplan.RangeBucketFanout{
		Input: closedTimestampTestScan(
			"otel_metrics_exponential_histogram", "TimeUnix", "RawKey", "Attributes", "BucketCounts",
		),
		Start:    time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC),
		End:      time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC),
		Step:     30 * time.Second,
		Lookback: 5 * time.Minute,
		GroupBy: []chplan.Expr{
			&chplan.ColumnRef{Name: "RawKey"},     // index 0: no alias -> stays raw, never hoisted
			&chplan.ColumnRef{Name: "Attributes"}, // index 1: aliased -> hoisted to _rbf_key_2
		},
		GroupByAliases: []string{"", "Attributes"},
		AnchorAlias:    "anchor_ts",
		TimestampCol:   "TimeUnix",
		AggFuncs: []chplan.AggFunc{
			{
				Fn:    chplan.FnArgMax,
				Alias: "BucketCounts",
				Args: []chplan.Expr{
					&chplan.ColumnRef{Name: "BucketCounts"},
					&chplan.ColumnRef{Name: "TimeUnix"},
				},
			},
		},
	}

	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	const (
		fanoutHoist    = "`Attributes` AS _rbf_key_2"
		collapseSelect = "SELECT anchor_ts AS `anchor_ts`, `RawKey`, `_rbf_key_2` AS `Attributes`, argMax(`BucketCounts`, `TimeUnix`) AS `BucketCounts`"
		groupBy        = "GROUP BY anchor_ts, `RawKey`, `_rbf_key_2`"
	)
	if !strings.Contains(sql, fanoutHoist) {
		t.Errorf("the ALIASED entry (index 1, Attributes) must be materialized in the fan-out SELECT as %q:\n%s", fanoutHoist, sql)
	}
	if !strings.HasPrefix(sql, collapseSelect) {
		t.Errorf("collapse SELECT-list must read `RawKey` raw and `_rbf_key_2` under the Attributes alias:\nwant prefix %q\ngot  %s", collapseSelect, sql)
	}
	if !strings.Contains(sql, groupBy) {
		t.Errorf("GROUP BY must reference the BARE entry's raw column and the ALIASED entry's hoisted column, not the other way around:\nwant %q\ngot  %s", groupBy, sql)
	}
}
