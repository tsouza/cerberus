package chsql_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
)

// Every backward-walked fan-out — RangeLWR, RangeBucketFanout and the
// RangeWindow family — renders its anchor grid as `<base> -
// toIntervalNanosecond(i * <step>)`, and on a query_range grid that base
// must be the Start-anchored newest anchor `Start + floor((End-Start)/Step)
// * Step`, never the raw End. The chDB property test proves the reported
// timestamps; this pins the emitted base itself, untagged, on a span that
// is deliberately NOT a step multiple so the two candidates render as
// different literals.
var (
	gridBaseStart = time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	// 5m15s over a 30s step: eleven anchors, newest at 12:05:00, raw end 12:05:15.
	gridBaseEnd  = gridBaseStart.Add(5*time.Minute + 15*time.Second)
	gridBaseStep = 30 * time.Second
)

const (
	gridBaseStartAnchored = "toDateTime64('2026-05-13 12:05:00.000000000', 9) - toIntervalNanosecond(i * 30000000000)"
	gridBaseRawEnd        = "toDateTime64('2026-05-13 12:05:15.000000000', 9) - toIntervalNanosecond(i * 30000000000)"
)

func TestRangeGridBaseIsStartAnchored(t *testing.T) {
	t.Parallel()
	plans := map[string]chplan.Node{
		"RangeLWR": &chplan.RangeLWR{
			Input:         rangeLWRTestInput("otel_metrics_gauge"),
			Start:         gridBaseStart,
			End:           gridBaseEnd,
			Step:          gridBaseStep,
			Lookback:      5 * time.Minute,
			MetricNameCol: "MetricName",
			AttributesCol: "Attributes",
			TimestampCol:  "TimeUnix",
			ValueCol:      "Value",
		},
		"RangeBucketFanout": &chplan.RangeBucketFanout{
			Input:        closedTimestampTestScan("otel_metrics_exponential_histogram", "TimeUnix", "Attributes", "BucketCounts"),
			Start:        gridBaseStart,
			End:          gridBaseEnd,
			Step:         gridBaseStep,
			Lookback:     5 * time.Minute,
			GroupBy:      []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
			AnchorAlias:  "anchor_ts",
			TimestampCol: "TimeUnix",
			AggFuncs: []chplan.AggFunc{{
				Fn:    chplan.FnArgMax,
				Alias: "BucketCounts",
				Args:  []chplan.Expr{&chplan.ColumnRef{Name: "BucketCounts"}, &chplan.ColumnRef{Name: "TimeUnix"}},
			}},
		},
		"RangeWindow": &chplan.RangeWindow{
			Input:           rangeWindowPublicTestScan("otel_metrics_sum"),
			Func:            "rate",
			Range:           2 * time.Minute,
			Start:           gridBaseStart,
			End:             gridBaseEnd,
			Step:            gridBaseStep,
			OuterRange:      gridBaseEnd.Sub(gridBaseStart),
			TimestampColumn: "TimeUnix",
			ValueColumn:     "Value",
			GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		},
	}
	for name, plan := range plans {
		t.Run(name, func(t *testing.T) {
			sql, _, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			if !strings.Contains(sql, gridBaseStartAnchored) {
				t.Errorf("%s must walk its grid from the Start-anchored newest anchor %q:\n%s", name, gridBaseStartAnchored, sql)
			}
			if strings.Contains(sql, gridBaseRawEnd) {
				t.Errorf("%s walks its grid from the raw End %q, shifting every anchor by the 15s remainder:\n%s", name, gridBaseRawEnd, sql)
			}
		})
	}
}
