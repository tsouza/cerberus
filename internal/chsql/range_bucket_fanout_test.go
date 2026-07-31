package chsql_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// TestRangeBucketFanoutSuppressesShortSourceSeries proves that rate's
// two-sample requirement is applied before an aggregate can merge separate
// one-sample source series into a seemingly valid output bucket.
func TestRangeBucketFanoutSuppressesShortSourceSeries(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC)
	plan := &chplan.RangeBucketFanout{
		Input:               &chplan.Scan{Table: "otel_metrics_histogram"},
		Start:               start,
		End:                 start.Add(time.Minute),
		Step:                time.Minute,
		Lookback:            time.Minute,
		AnchorAlias:         "anchor_ts",
		TimestampCol:        "TimeUnix",
		MinSamplesPerSeries: 2,
		SeriesKey:           &chplan.ColumnRef{Name: "Attributes"},
		AggFuncs: []chplan.AggFunc{
			{Name: "sumForEach", Args: []chplan.Expr{&chplan.ColumnRef{Name: "BucketCounts"}}, Alias: "BucketCounts"},
			{Name: "any", Args: []chplan.Expr{&chplan.ColumnRef{Name: "ExplicitBounds"}}, Alias: "ExplicitBounds"},
		},
	}

	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "HAVING count() >= 2") {
		t.Errorf("per-source collapse must discard anchors with fewer than two samples:\n%s", sql)
	}
	if got := strings.Count(sql, "sumForEach(`BucketCounts`)"); got != 2 {
		t.Errorf("sumForEach must collapse source series before the final aggregation, got %d occurrences:\n%s", got, sql)
	}
	if !strings.Contains(sql, "HAVING count() >= 2) GROUP BY anchor_ts") {
		t.Errorf("the final aggregation must occur after the per-source HAVING filter:\n%s", sql)
	}
}
