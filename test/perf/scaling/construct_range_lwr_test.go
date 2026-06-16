//go:build chdb

// Construct: range_lwr — bare instant-vector query_range latest-per-series.
//
// Folds in the standalone range_lwr_scaling_chdb guard. The bare
// `query_range` LWR path used to emit an N-anchor StepGrid CROSS JOIN + a
// correlated per-anchor staleness Filter + a per-(series, anchor) argMax —
// an O(rows x anchors) shape whose wall scaled LINEARLY with the anchor
// count N. The #804 fix replaced it with a single-pass bounded sample-side
// fan-out: each sample fans out (via arrayJoin over a bounded `range(lo,
// hi)` index set) to ONLY the <= lookback/step + 1 anchors whose staleness
// window contains it, so the intermediate (sample, anchor) cardinality is
// rows x (lookback/step + 1) — CONSTANT in the grid width N.
//
// THE REAL MULTIPLIER is range/step (the anchor count N), NOT a fixed
// numAnchors — the original guard's bug was sweeping numAnchors constant.
// Here Param = N = window/step, swept 61 -> 121 -> 241 by shrinking step
// at a fixed window + fixed row set. The production lowering's wall stays
// ~flat in N (sub-linear) and its peak intermediate stays a small multiple
// of scan_rows.
package scaling

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

const rangeLWRMetric = "demo_memory_usage_bytes"

func init() {
	const lookback = 5 * time.Minute
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)

	register(Construct{
		Name:        "range_lwr",
		Param:       "range/step anchors N",
		Why:         "bare query_range LWR StepGrid CROSS JOIN (O(rows x anchors))",
		ScanRowsSQL: "SELECT count() FROM otel_metrics_gauge",
		// rows x (lookback/step+1): at the densest swept step (6m) this is
		// rows x (5m/6m+1) ~ rows x 1; a couple x scan_rows is generous
		// headroom while a rows x N regression (241x) blows straight past.
		CardinalityBound: 3.0,
		Seed: func() string {
			// ResourceAttributes mirrors the OTel-CH default schema: the rc.5
			// read path projects mapUpdate(sanitize(ResourceAttributes), …),
			// so the seed table must carry the column (left empty via DEFAULT)
			// or the chDB round-trip 502s with UNKNOWN_IDENTIFIER.
			ddl := `DROP TABLE IF EXISTS otel_metrics_gauge;
			CREATE TABLE otel_metrics_gauge (
			  ServiceName String, MetricName String, Attributes Map(String,String),
			  ResourceAttributes Map(String,String) DEFAULT map(),
			  TimeUnix DateTime64(9), Value Float64
			) ENGINE = MergeTree()
			ORDER BY (MetricName, Attributes, toUnixTimestamp64Nano(TimeUnix));`
			// 200 series x ~900 samples over 24h = 180k rows; FIXED across
			// every step variant so anchor count N is the only variable.
			ins := `INSERT INTO otel_metrics_gauge (ServiceName, MetricName, Attributes, TimeUnix, Value) SELECT
			  'svc', '` + rangeLWRMetric + `',
			  map('instance', concat('i', toString(number % 200))),
			  toDateTime64('2026-01-01 00:00:00', 9) + toIntervalSecond((intDiv(number, 200)) * 96),
			  toFloat64(number)
			FROM numbers(180000);`
			return ddl + ins
		},
		Points: func(t *testing.T) []Point {
			anchors := []int64{61, 121, 241}
			pts := make([]Point, 0, len(anchors))
			for _, n := range anchors {
				step := end.Sub(start) / time.Duration(n-1)
				sqlText, args := emitRangeLWRSQL(t, start, end, step)
				pts = append(pts, Point{
					Param:     n,
					SQL:       sqlText,
					Args:      args,
					LevelSQLs: []string{rangeLWRFanoutInner(end, step, lookback, n)},
				})
			}
			return pts
		},
	})
}

// emitRangeLWRSQL lowers the bare selector over [start,end] at `step`
// through the real parse -> lower -> emit chain — the production single-pass
// shape under test.
func emitRangeLWRSQL(t *testing.T, start, end time.Time, step time.Duration) (string, []any) {
	t.Helper()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(rangeLWRMetric)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := promql.LowerAtRange(context.Background(), expr, schema.DefaultOTelMetrics(), start, end, step)
	if err != nil {
		t.Fatalf("LowerAtRange: %v", err)
	}
	sqlText, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	return sqlText, args
}

// rangeLWRFanoutInner is the bounded sample-side fan-out subquery the
// RangeLWR emitter produces BEFORE the GROUP BY collapse — one row per
// (sample, covered anchor). Mirrors internal/chsql.lwrAnchorFanoutFrag.
// Counting its rows measures the intermediate cardinality the fix bounds.
func rangeLWRFanoutInner(end time.Time, step, lookback time.Duration, numAnchors int64) string {
	stepNS := step.Nanoseconds()
	endLit := end.UTC().Format("2006-01-02 15:04:05.000000000")
	dist := fmt.Sprintf("dateDiff('nanosecond', TimeUnix, toDateTime64('%s', 9))", endLit)
	floorIdx := func(addNS int64) string {
		num := dist
		if addNS < 0 {
			num = fmt.Sprintf("%s - %d", dist, -addNS)
		}
		return fmt.Sprintf("intDiv(%s, toInt64(%d)) - (modulo(%s, toInt64(%d)) < 0) + 1",
			num, stepNS, num, stepNS)
	}
	return fmt.Sprintf(`SELECT TimeUnix, Value,
	  arrayJoin(arrayMap(i -> toDateTime64('%s', 9) - toIntervalNanosecond(i * %d),
	    range(greatest(0, %s), least(%d, %s)))) AS anchor_ts
	FROM otel_metrics_gauge WHERE MetricName = '`+rangeLWRMetric+`'`,
		endLit, stepNS, floorIdx(-lookback.Nanoseconds()), numAnchors, floorIdx(0))
}
