//go:build chdb

// Construct: histogram_range — classic histogram_quantile over query_range.
//
// Folds in the standalone histogram_range_scaling_chdb guard. The classic
// histogram_quantile range path used to emit, for the per-(series, anchor)
// bucket aggregate, the same N-anchor StepGrid CROSS JOIN + correlated
// per-anchor staleness Filter + per-(series, anchor) argMax(BucketCounts,
// TimeUnix) / argMax(ExplicitBounds, TimeUnix) — the same O(rows x anchors)
// fan-out as range_lwr, just with array-valued aggregates. The #805 fix
// (RangeBucketFanout) ports it to the same single-pass bounded sample-side
// fan-out, so the intermediate (sample, anchor) cardinality is
// rows x (lookback/step + 1) — CONSTANT in the grid width N.
//
// THE REAL MULTIPLIER is range/step (anchor count N). Param = N, swept
// 61 -> 121 -> 241 by shrinking step at a fixed window + row set. The
// production lowering must emit the single-pass shape (asserted by the
// no-CROSS-JOIN precondition); its wall stays ~flat in N and its peak
// intermediate stays a small multiple of scan_rows.
package scaling

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

const histRangeMetric = "http_request_duration"

func init() {
	const lookback = 5 * time.Minute
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)

	register(Construct{
		Name:             "histogram_range",
		Param:            "range/step anchors N",
		Why:              "histogram_quantile range StepGrid CROSS JOIN bucket aggregate (O(rows x anchors))",
		ScanRowsSQL:      "SELECT count() FROM otel_metrics_histogram",
		CardinalityBound: 3.0,
		Seed: func() string {
			ddl := `DROP TABLE IF EXISTS otel_metrics_histogram;
			CREATE TABLE otel_metrics_histogram (
			  MetricName String, Attributes Map(String,String),
			  TimeUnix DateTime64(9),
			  BucketCounts Array(UInt64), ExplicitBounds Array(Float64)
			) ENGINE = MergeTree()
			ORDER BY (MetricName, Attributes, toUnixTimestamp64Nano(TimeUnix));`
			// 200 series x ~900 samples over 24h = 180k rows; 3-bucket
			// classic histogram per row so the array argMax collapse runs.
			ins := `INSERT INTO otel_metrics_histogram SELECT
			  '` + histRangeMetric + `',
			  map('instance', concat('i', toString(number % 200))),
			  toDateTime64('2026-01-01 00:00:00', 9) + toIntervalSecond((intDiv(number, 200)) * 96),
			  [number % 5 + 1, number % 7 + 2, number % 3 + 3],
			  [0.1, 0.5, 1.0]
			FROM numbers(180000);`
			return ddl + ins
		},
		Points: func(t *testing.T) []Point {
			// Precondition: the production lowering must emit the single-pass
			// shape (no CROSS JOIN / StepGrid) before we trust the contrast.
			assertHistogramRangeSinglePass(t, start, end, end.Sub(start)/240)

			anchors := []int64{61, 121, 241}
			pts := make([]Point, 0, len(anchors))
			for _, n := range anchors {
				step := end.Sub(start) / time.Duration(n-1)
				pts = append(pts, Point{
					Param:     n,
					SQL:       histRangeFanoutInner(end, step, lookback, n),
					LevelSQLs: []string{histRangeMembership(end, step, lookback, n)},
				})
			}
			return pts
		},
	})
}

// assertHistogramRangeSinglePass pins that the production lowering routes
// the classic bare histogram_quantile range query through RangeBucketFanout
// (no CROSS JOIN / StepGrid), so the construct times the shape really
// emitted.
func assertHistogramRangeSinglePass(t *testing.T, start, end time.Time, step time.Duration) {
	t.Helper()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr("histogram_quantile(0.95, " + histRangeMetric + "_bucket)")
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := promql.LowerAtRange(context.Background(), expr, schema.DefaultOTelMetrics(), start, end, step)
	if err != nil {
		t.Fatalf("LowerAtRange: %v", err)
	}
	sqlText, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	upper := strings.ToUpper(sqlText)
	for _, banned := range []string{"CROSS JOIN", "STEPGRID"} {
		if strings.Contains(upper, banned) {
			t.Fatalf("histogram_quantile range SQL still contains %q — the single-pass "+
				"RangeBucketFanout rework regressed:\n%s", banned, sqlText)
		}
	}
	if !strings.Contains(sqlText, "arrayJoin(arrayMap") {
		t.Fatalf("histogram_quantile range SQL missing the bounded arrayJoin fan-out:\n%s", sqlText)
	}
}

// histRangeFanoutInner is the bounded sample-side fan-out + GROUP BY
// collapse the RangeBucketFanout emitter produces — the (series, anchor)
// array argMax over only the in-window anchors. Mirrors
// emitRangeBucketFanout. Timing it isolates the fan-out cost from the
// (constant) quantile interpolation wrapped on top.
func histRangeFanoutInner(end time.Time, step, lookback time.Duration, numAnchors int64) string {
	stepNS := step.Nanoseconds()
	endLit := end.UTC().Format("2006-01-02 15:04:05.000000000")
	floorIdx := histFloorIdx(end, stepNS)
	return fmt.Sprintf(`SELECT Attributes, anchor_ts,
	  argMax(BucketCounts, TimeUnix) AS BucketCounts,
	  argMax(ExplicitBounds, TimeUnix) AS ExplicitBounds
	FROM (
	  SELECT *, arrayJoin(arrayMap(i -> toDateTime64('%s', 9) - toIntervalNanosecond(i * %d),
	    range(greatest(0, %s), least(%d, %s)))) AS anchor_ts
	  FROM otel_metrics_histogram WHERE MetricName = '`+histRangeMetric+`'
	)
	GROUP BY Attributes, anchor_ts`,
		endLit, stepNS, floorIdx(-lookback.Nanoseconds()), numAnchors, floorIdx(0))
}

// histRangeMembership strips the GROUP BY collapse to count the raw
// (sample, anchor) membership rows — the intermediate cardinality the fix
// bounds.
func histRangeMembership(end time.Time, step, lookback time.Duration, numAnchors int64) string {
	stepNS := step.Nanoseconds()
	endLit := end.UTC().Format("2006-01-02 15:04:05.000000000")
	floorIdx := histFloorIdx(end, stepNS)
	return fmt.Sprintf(`SELECT TimeUnix,
	  arrayJoin(arrayMap(i -> toDateTime64('%s', 9) - toIntervalNanosecond(i * %d),
	    range(greatest(0, %s), least(%d, %s)))) AS anchor_ts
	FROM otel_metrics_histogram WHERE MetricName = '`+histRangeMetric+`'`,
		endLit, stepNS, floorIdx(-lookback.Nanoseconds()), numAnchors, floorIdx(0))
}

// histFloorIdx returns the bounded-anchor-index expression generator shared
// by the fan-out + membership SQL (the `range(lo, hi)` bounds that make the
// fan-out scale with lookback/step, not N).
func histFloorIdx(end time.Time, stepNS int64) func(addNS int64) string {
	endLit := end.UTC().Format("2006-01-02 15:04:05.000000000")
	dist := fmt.Sprintf("dateDiff('nanosecond', TimeUnix, toDateTime64('%s', 9))", endLit)
	return func(addNS int64) string {
		num := dist
		if addNS < 0 {
			num = fmt.Sprintf("%s - %d", dist, -addNS)
		}
		return fmt.Sprintf("intDiv(%s, toInt64(%d)) - (modulo(%s, toInt64(%d)) < 0) + 1",
			num, stepNS, num, stepNS)
	}
}
