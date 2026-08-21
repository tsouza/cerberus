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
		Input:        &chplan.Scan{Table: "otel_metrics_exponential_histogram"},
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
// range_bucket_fanout.go:159 (`if r.MinSamples > fanoutNoMinSampleFilter`, the
// gate on the collapse HAVING).
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
