package gen

import (
	"fmt"

	"github.com/tsouza/cerberus/test/property"
)

// This file builds the DETERMINISTIC range-window BOUNDARY dataset that
// test/property/range_window_boundary_test.go proves the (T-range, T]
// range-vector window convention against (issue #3449), specifically over
// count_over_time() — one of the incrementally-reducible *_over_time
// functions internal/chsql/range_window.go's emitRangeWindowOverTimeDirect
// answers with a single row-level WHERE predicate
// (emitRangeWindowOverTimeDirectInstant's own `Gt(ts, winStart)` /
// `Lte(ts, end)` pair) rather than the groupArray + arrayFilter windowed-
// array path: that WHERE is the one non-redundant, directly observable
// implementation of the window's start boundary for an instant query —
// the windowed-array path's own arrayFilter boundary (chsql.RangeWindowFilter)
// sits BEHIND an equally strict scan-time WHERE
// (instantWindowScanBoundsFrags, pushed by pushInstantScanBound before the
// array is ever built), so a single-layer mutation to either one alone is
// silently absorbed by the other there — not a real, observable behavior
// change for that family.
//
// The random InstantWindowSweep generator (instant_window.go) draws
// scrape/range/offset axes from a pool and cannot GUARANTEE a sample
// lands exactly on the window boundary every run. This dataset pins one
// deterministically: one sample sits exactly on the excluded start
// boundary (t = T-range), one sits exactly on the included end boundary
// (t = T), and one sits strictly inside the window.

// RangeBoundaryMetricName avoids every _sum/_count/_total/_bucket suffix
// so the schema heuristic keeps it in the gauge table.
const RangeBoundaryMetricName = "range_boundary_probe"

// RangeBoundaryRangeSeconds is the [range] the proof's count_over_time()
// query carries.
const RangeBoundaryRangeSeconds = 60

// RangeBoundaryEvalTsSec is the instant the proof evaluates at, so the
// matched window is the half-open interval (0, 60].
const RangeBoundaryEvalTsSec = RangeBoundaryRangeSeconds

// RangeBoundaryExpectedCount is the PromQL-correct count_over_time()
// result for the (0, 60] window: the t=0 sample sits exactly on the
// EXCLUDED start boundary, so only the t=30 and t=60 samples count.
const RangeBoundaryExpectedCount = 2

// rangeBoundaryLabels is the single series identity the dataset carries.
var rangeBoundaryLabels = map[string]string{"job": "api"}

// RangeBoundaryCase returns the deterministic boundary-exact
// InstantWindowCase: count_over_time() over the gauge series below,
// evaluated at RangeBoundaryEvalTsSec.
func RangeBoundaryCase() InstantWindowCase {
	points := []property.Point{
		{TimestampMs: 0, Value: 100},    // t = T-range: excluded (open-left).
		{TimestampMs: 30_000, Value: 5}, // strictly inside the window.
		{TimestampMs: 60_000, Value: 2}, // t = T: included (closed-right).
	}
	series := []property.SeriesData{{
		MetricName: RangeBoundaryMetricName,
		Labels:     rangeBoundaryLabels,
		Points:     points,
	}}
	ds := property.Dataset{
		DDL:     renderDDL(series),
		Metrics: &property.MetricsModel{Series: series},
	}
	query := fmt.Sprintf(
		"count_over_time(%s%s[%ds])",
		RangeBoundaryMetricName, matcherString(rangeBoundaryLabels), RangeBoundaryRangeSeconds,
	)
	return InstantWindowCase{
		Dataset:      ds,
		RangeSec:     RangeBoundaryRangeSeconds,
		EvalOffset:   0,
		ValueProfile: InstantWindowValueProfileWave,
		Fn:           "count_over_time",
		MetricName:   RangeBoundaryMetricName,
		Labels:       rangeBoundaryLabels,
		LatestSample: RangeBoundaryEvalTsSec,
		Query: property.Query{
			ShapeID: "test.range-window-boundary",
			String:  query,
			EvalTs:  RangeBoundaryEvalTsSec,
		},
	}
}
