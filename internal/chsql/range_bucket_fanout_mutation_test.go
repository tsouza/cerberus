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
// entry", tell which branch owns which entry, or prove the loop keeps going
// past a hoisted (or un-hoisted) entry instead of stopping there — this plan
// bookends the one ALIASED entry with a bare entry on each side, so each of
// the six mutations below changes the emitted SQL:
//
//   - INVERT_LOOPCTRL (`continue` -> `break`) at the hoist loop: the loop
//     hits `continue` on the BARE entry at index 0, so `break` there would
//     exit before ever reaching the aliased entry at index 1 — `_rbf_key_2`
//     would never be materialized.
//   - CONDITIONALS_NEGATION (`== ""` -> `!= ""`) on the same guard: flips
//     which entry gets hoisted, materializing a bare entry's raw column
//     reference under a needless synthetic alias and leaving the aliased
//     entry's own expression untouched.
//   - INVERT_LOOPCTRL in the collapse SELECT-list loop: it hits `continue`
//     on the ALIASED entry (index 1), so `break` there would drop the
//     bare entry at index 2 — `RawKey2` — from the SELECT-list entirely.
//   - CONDITIONALS_NEGATION (`hoisted != ""` -> `hoisted == ""`) in the same
//     loop: swaps which entry reads its hoisted column vs. re-renders its
//     raw expression.
//   - The same two mutations, independently, in the GROUP BY loop.
//
// A two-entry (bare, aliased) plan — the first version of this test — still
// left the collapse/GROUP BY loops' own INVERT_LOOPCTRL mutants alive:
// hitting `continue` on the LAST entry is indistinguishable from `break`,
// since neither leaves anything else in the slice to skip. The trailing
// bare entry (RawKey2) is what gives that mutation something to drop.
func TestMutation_RangeBucketFanout_MixedAliasedGroupByHoist(t *testing.T) {
	t.Parallel()

	plan := &chplan.RangeBucketFanout{
		Input: closedTimestampTestScan(
			"otel_metrics_exponential_histogram", "TimeUnix", "RawKey1", "Attributes", "RawKey2", "BucketCounts",
		),
		Start:    time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC),
		End:      time.Date(2026, 5, 13, 12, 5, 0, 0, time.UTC),
		Step:     30 * time.Second,
		Lookback: 5 * time.Minute,
		GroupBy: []chplan.Expr{
			&chplan.ColumnRef{Name: "RawKey1"},    // index 0: no alias -> stays raw, never hoisted
			&chplan.ColumnRef{Name: "Attributes"}, // index 1: aliased -> hoisted to _rbf_key_2
			&chplan.ColumnRef{Name: "RawKey2"},    // index 2: no alias -> must still be reached
		},
		GroupByAliases: []string{"", "Attributes", ""},
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
		collapseSelect = "SELECT anchor_ts AS `anchor_ts`, `RawKey1`, `_rbf_key_2` AS `Attributes`, `RawKey2`, argMax(`BucketCounts`, `TimeUnix`) AS `BucketCounts`"
		groupBy        = "GROUP BY anchor_ts, `RawKey1`, `_rbf_key_2`, `RawKey2`"
	)
	if !strings.Contains(sql, fanoutHoist) {
		t.Errorf("the ALIASED entry (index 1, Attributes) must be materialized in the fan-out SELECT as %q:\n%s", fanoutHoist, sql)
	}
	if !strings.HasPrefix(sql, collapseSelect) {
		t.Errorf("collapse SELECT-list must read both bare entries raw and `_rbf_key_2` under the Attributes alias, in order:\nwant prefix %q\ngot  %s", collapseSelect, sql)
	}
	if !strings.Contains(sql, groupBy) {
		t.Errorf("GROUP BY must reference both bare entries' raw columns and the aliased entry's hoisted column, in order:\nwant %q\ngot  %s", groupBy, sql)
	}
}
