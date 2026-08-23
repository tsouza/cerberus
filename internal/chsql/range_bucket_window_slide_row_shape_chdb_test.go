//go:build chdb

package chsql_test

import (
	"context"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/chsqltest"
)

// TestRangeBucketWindowSlide_RowShapeContractWithNonFiniteBounds is the
// regression proof for the bug an adversarial review of Task 6's dual-emit
// test found and internal/chsql/range_bucket_window_slide.go's
// windowSlideCanonBySeriesFrag now fixes: a row whose own ExplicitBounds
// carries a literal `+Inf` or `NaN` value (real, unvalidated OTLP data can
// produce either) used to leak into the per-series canonical bound union
// BEFORE the sentinel +Inf gets pushed onto it. ClickHouse's arraySort
// places NaN AFTER +Inf (confirmed against real chDB:
// arraySort([1,2,inf,nan]) = [1,2,inf,nan]), so a NaN anywhere in the
// series' bounded scan pushed the TRUE +Inf sentinel off the tail of the
// sorted, deduped union — the position hasGatedExtendedArrayFrag's
// finiteCanon assumes it occupies. The row-shape contract every consumer
// of this node relies on (BucketCounts is either length(ExplicitBounds) or
// length(ExplicitBounds)+1 — see hasGatedExtendedArrayFrag's own doc) broke
// silently: BucketCounts came out one element longer than either allowed
// shape.
//
// This is deliberately NOT a dual-emit comparison against the fan-out
// (histogram_quantile_window_slide_chdb_test.go's own suite): tracing the
// SAME malformed-bound shape through the fan-out's shared merge machinery
// (internal/promql/histogram_quantile.go's classicBucketUnionBoundsExpr)
// found that IT ALSO fails to filter isFinite before treating a row's own
// reported bound values as legitimate rungs — a separate, pre-existing bug
// in the shared merge path, independent of window-slide, filed as its own
// issue rather than fixed here (out of scope for the window-slide
// emitter). A dual-emit VALUE comparison over this exact shape would
// therefore assert window-slide against an equally-broken reference for
// every anchor whose window reaches the malformed rows — worse than no
// test at all. What IS in scope, and what this test pins directly, is
// window-slide's OWN row-shape contract: querying
// chplan.RangeBucketWindowSlide's raw output (bypassing
// chplan.HistogramQuantile entirely, mirroring the review's own "raw
// ladder proof" methodology) and asserting the length relationship holds
// for every emitted row.
func TestRangeBucketWindowSlide_RowShapeContractWithNonFiniteBounds(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	if _, err := db.Exec(rangeBucketWindowSlideBoundDDL); err != nil {
		t.Fatalf("ddl: %v", err)
	}

	// The same adversarial seed histogram_quantile_window_slide_chdb_test.go's
	// wsCases() malformed_bounds sibling case documents: one series where a
	// DIFFERENT row reports a literal `+Inf` bound and a DIFFERENT row
	// reports a literal `NaN` bound — the exact co-occurrence
	// hasGatedExtendedArrayFrag's finiteCanon mishandled pre-fix.
	seedRows := `
    ('http_server_request_duration', 'infnan', toDateTime64('2026-01-01 00:00:00', 9), [1, 2, 3], [0.1, 0.5, 1.0]),
    ('http_server_request_duration', 'infnan', toDateTime64('2026-01-01 00:01:00', 9), [10, 20, 30, 100], [0.1, 0.5, inf]),
    ('http_server_request_duration', 'infnan', toDateTime64('2026-01-01 00:02:00', 9), [4, 7, 20, 2], [0.1, 0.5, 1.0, nan]),
    ('http_server_request_duration', 'infnan', toDateTime64('2026-01-01 00:03:00', 9), [6, 10, 25, 5], [0.1, 0.5, 1.0, nan])`
	cols := "(MetricName, SeriesID, TimeUnix, BucketCounts, ExplicitBounds)"
	if _, err := db.Exec("INSERT INTO otel_metrics_histogram " + cols + " VALUES " + seedRows); err != nil {
		t.Fatalf("seed insert: %v", err)
	}

	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	step := 30 * time.Second
	end := start.Add(10 * time.Minute)
	node := &chplan.RangeBucketWindowSlide{
		Input: &chplan.Scan{
			Table:   "otel_metrics_histogram",
			Columns: []string{"SeriesID", "TimeUnix", "BucketCounts", "ExplicitBounds"},
		},
		Start:             start,
		End:               end,
		Step:              step,
		Range:             5 * time.Minute, // ratio 10 == windowSlideMinLookbackStepRatio, matching wsWindow/hqNativeGridStep
		GroupBy:           []chplan.Expr{&chplan.ColumnRef{Name: "SeriesID"}},
		GroupByAliases:    []string{"SeriesID"},
		AnchorAlias:       "anchor_ts",
		TimestampCol:      "TimeUnix",
		BucketCountsCol:   "BucketCounts",
		ExplicitBoundsCol: "ExplicitBounds",
	}
	sqlStr, args, err := chsql.Emit(context.Background(), node)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	// Project just the two lengths — avoids the chdb-go driver's Array(T)
	// scan entirely, and is exactly the invariant under test.
	wrapped := "SELECT anchor_ts, length(BucketCounts) AS bc_len, length(ExplicitBounds) AS eb_len FROM (" + sqlStr + ") ORDER BY anchor_ts"

	rows, err := db.Query(wrapped, args...)
	if err != nil {
		t.Fatalf("query: %v\nSQL: %s", err, wrapped)
	}
	defer func() { _ = rows.Close() }()

	var n int
	for rows.Next() {
		var anchor time.Time
		var bcLen, ebLen int64
		if err := rows.Scan(&anchor, &bcLen, &ebLen); err != nil {
			t.Fatalf("scan: %v", err)
		}
		n++
		// hasGatedExtendedArrayFrag's own doc: BucketCounts is either
		// length(ExplicitBounds) or length(ExplicitBounds)+1 — never
		// anything else. windowSlideCanonBySeriesFrag's own construction
		// (the +Inf overflow rung always carries unconditional presence)
		// means window-slide's own output is ALWAYS the "+1" shape, but
		// the assertion checks the full documented contract rather than
		// over-fitting to that detail.
		if bcLen != ebLen && bcLen != ebLen+1 {
			t.Errorf("anchor=%s: BucketCounts len=%d, ExplicitBounds len=%d — violates the "+
				"row-shape contract (must be len(ExplicitBounds) or len(ExplicitBounds)+1)",
				anchor.UTC().Format(time.RFC3339Nano), bcLen, ebLen)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	if n == 0 {
		t.Fatal("query produced zero rows — the malformed-bound series must still answer a populated grid")
	}
	t.Logf("checked row-shape contract on %d anchor rows", n)
}
