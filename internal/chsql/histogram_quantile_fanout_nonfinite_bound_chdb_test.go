//go:build chdb

// chDB-backed regression test for #2495: the classic-histogram
// histogram_quantile ORDINARY FAN-OUT merge path (chplan.RangeBucketFanout
// via promql.FanoutClassicHistogramWindowLowerer — NOT
// chplan.RangeBucketGridNative, fixed separately for its own,
// unrelated non-finite-bound bug) must never leak a literal +Inf or NaN
// value from a row's own ExplicitBounds into the answered quantile.
//
// This is the issue's own repro, reproduced verbatim: one series carries
// four samples over a 5-minute window, one sample's ExplicitBounds ending
// in a literal `inf` and a later sample's in a literal `nan` (malformed
// but real OTLP data can produce either). Before the #2495 fix,
// classicBucketUnionBoundsExpr folded each row's raw ExplicitBounds
// straight into the merged layout with no isFinite filter, so the
// non-finite value survived into the node's own output ExplicitBounds and
// emitHistogramQuantile's overflow-bucket clamp
// (internal/chsql/histogram_quantile.go's highestBound) answered it
// directly: +Inf at the anchors that saw only the +Inf-bound row, NaN once
// the NaN-bound row entered the window too.
package chsql_test

import (
	"context"
	"math"
	"testing"
	"time"

	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/chsqltest"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
)

// hqFanoutNonFiniteQuery is the issue's own PromQL repro: a classic-bucket
// selector, summed by(le) over a sum_over_time window — the shape that
// lowers through lowerHistogramQuantileAgg's classicBucketMergeShaping
// merge fold (histogram_quantile.go:1412 / :1462), not the native path.
const hqFanoutNonFiniteQuery = `histogram_quantile(0.5, sum by(le) (sum_over_time(http_server_request_duration_bucket[5m])))`

// hqFanoutNonFiniteRows is the issue's own repro data: row 2's
// ExplicitBounds ends in a literal `inf`, rows 3 and 4's end in a literal
// `nan`. BucketCounts and ExplicitBounds intentionally carry different
// per-row layouts (some rows omit the trailing overflow slot, some don't)
// — the same heterogeneous-layout shape the merge path must already
// tolerate, now compounded with a non-finite bound.
const hqFanoutNonFiniteRows = `
    ('http_server_request_duration', map('svc','infnan'), toDateTime64('2026-01-01 00:00:00', 9), [1, 2, 3], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', map('svc','infnan'), toDateTime64('2026-01-01 00:01:00', 9), [10, 20, 30, 100], [0.1, 0.5, inf]),
    ('http_server_request_duration', map('svc','infnan'), toDateTime64('2026-01-01 00:02:00', 9), [4, 7, 20, 2], [0.1, 0.5, 1.0, nan]),
    ('http_server_request_duration', map('svc','infnan'), toDateTime64('2026-01-01 00:03:00', 9), [6, 10, 25, 5], [0.1, 0.5, 1.0, nan])`

// TestHistogramQuantileFanout_ChDB_NonFiniteBoundExcluded runs the issue's
// own repro over a 5m/30s grid spanning both the "only the +Inf-bound row
// is in window" anchors and the "both the +Inf- and NaN-bound rows are in
// window" anchors, and asserts every answered quantile is finite.
func TestHistogramQuantileFanout_ChDB_NonFiniteBoundExcluded(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	seed(t, db, hqNativeSeedDDL)
	seed(t, db, "INSERT INTO otel_metrics_histogram "+
		"(MetricName, Attributes, TimeUnix, BucketCounts, ExplicitBounds) VALUES "+hqFanoutNonFiniteRows)

	gridStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	gridEnd := gridStart.Add(5 * time.Minute)
	const gridStep = 30 * time.Second

	p := promparser.NewParser(promparser.Options{})
	expr, err := p.ParseExpr(hqFanoutNonFiniteQuery)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", hqFanoutNonFiniteQuery, err)
	}
	// Zero-value LowerOpts.Lowerers.ClassicHistogram resolves to
	// promql.FanoutClassicHistogramWindowLowerer{} (lower_strategy.go) —
	// the ordinary fan-out this issue is about, never the ClickHouse-native
	// ladder.
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, schema.DefaultOTelMetrics(),
		gridStart, gridEnd, gridStep, promql.LowerOpts{})
	if err != nil {
		t.Fatalf("LowerAtRangeOpts(%q): %v", hqFanoutNonFiniteQuery, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", hqFanoutNonFiniteQuery, err)
	}
	if err := testsql.CheckSeedCoversFanOut(hqNativeSeedDDL, sqlStr); err != nil {
		t.Fatalf("seed does not cover the emitted fan-out: %v\nSQL: %s", err, sqlStr)
	}

	wrapped := "SELECT `TimeUnix`, `Value` FROM (" + sqlStr + ")"
	rows, err := db.Query(wrapped, args...)
	if err != nil {
		t.Fatalf("query: %v\nSQL: %s", err, wrapped)
	}
	defer func() { _ = rows.Close() }()

	var n int
	for rows.Next() {
		var ts time.Time
		var v float64
		if err := rows.Scan(&ts, &v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		n++
		if math.IsNaN(v) {
			t.Errorf("anchor %s: answered quantile is NaN — a non-finite ExplicitBounds "+
				"value leaked into the union and was returned by the overflow-bucket clamp",
				ts.UTC().Format(time.RFC3339))
		}
		if math.IsInf(v, 0) {
			t.Errorf("anchor %s: answered quantile is %v — a non-finite ExplicitBounds "+
				"value leaked into the union and was returned by the overflow-bucket clamp",
				ts.UTC().Format(time.RFC3339), v)
		}
	}
	if err := testsql.TolerantRowsErr(rows.Err()); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if n == 0 {
		t.Fatal("query produced zero rows — the grid must cover at least the anchors " +
			"that saw the +Inf-bound and NaN-bound rows")
	}
}
