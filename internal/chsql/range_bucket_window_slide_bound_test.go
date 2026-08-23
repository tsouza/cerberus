//go:build chdb

package chsql_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/chsqltest"
)

// rangeBucketWindowSlideBoundDDL is the minimal classic-histogram table
// shape RangeBucketWindowSlide reads. No Map-typed Attributes /
// ResourceAttributes / ServiceName columns: the plan node is built directly
// (bypassing PromQL lowering) with a bare ColumnRef group key over a plain
// String series id, so none of the schema-driven canonicalisation those
// columns feed is exercised here — this test is about the resource bound,
// not the lowering. (A Map(String,String) group key was tried first and
// dropped: the chdb-go driver's row scan cannot decode a Map column when
// iterating with rows.Next()/rows.Err() alone, unrelated to this bound.)
const rangeBucketWindowSlideBoundDDL = `
CREATE OR REPLACE TABLE otel_metrics_histogram (
    MetricName String,
    SeriesID String,
    TimeUnix DateTime64(9),
    BucketCounts Array(UInt64),
    ExplicitBounds Array(Float64)
) ENGINE = MergeTree ORDER BY (MetricName, SeriesID, TimeUnix);
`

// buildWindowSlidePlan constructs a bare chplan.RangeBucketWindowSlide over
// a plain Scan directly (bypassing PromQL lowering entirely, matching this
// package's own emit-level test style elsewhere) with the given anchor
// count, and returns the rendered (sql, args).
//
// numAnchors is chosen by the caller to land the UNION ALL source's total
// row count (scanned_rows + series*anchors — see
// maxRangeBucketWindowSlideRows's own doc) on either side of the bound
// together with the caller's own seeded series count: the sentinel side
// alone contributes exactly seriesCount*numAnchors rows, generated ENTIRELY
// by the query itself via arrayJoin over the (Start, End, Step) grid — no
// need to seed millions of real rows in chDB to reach a multi-million-row
// bound.
func buildWindowSlidePlan(t *testing.T, numAnchors int) (string, []any) {
	t.Helper()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	step := time.Second
	end := start.Add(time.Duration(numAnchors-1) * step)
	node := &chplan.RangeBucketWindowSlide{
		Input: &chplan.Scan{
			Table:   "otel_metrics_histogram",
			Columns: []string{"SeriesID", "TimeUnix", "BucketCounts", "ExplicitBounds"},
		},
		Start:             start,
		End:               end,
		Step:              step,
		Range:             10 * step, // ratio 10, matching windowSlideMinLookbackStepRatio — irrelevant to this emit-level test, kept realistic anyway
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
	return sqlStr, args
}

// TestRangeBucketWindowSlideBound_ThrowsWhenOversized confirms
// maxRangeBucketWindowSlideRows's throwIf guard actually fires — issue
// #2486 found a sibling native path (RangeBucketGridNative) shipped with NO
// resource-bound guard at all and genuinely OOMs at real scale; this is the
// check that this node's own guard does not repeat that gap silently (a
// bound that exists in source but has never actually been triggered is
// exactly as untested as no bound at all).
//
// 60 series x 80,001 anchors = 4,800,060 sentinel rows alone, comfortably
// past maxRangeBucketWindowSlideRows (4,000,000) — generated entirely by
// the query's own arrayJoin over the (Start, End, Step) grid, so seeding
// only needs 60 real rows (one per series), not millions.
func TestRangeBucketWindowSlideBound_ThrowsWhenOversized(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	if _, err := db.Exec(rangeBucketWindowSlideBoundDDL); err != nil {
		t.Fatalf("ddl: %v", err)
	}

	const seriesCount = 60
	const numAnchors = 80_001 // 60 * 80,001 = 4,800,060 > maxRangeBucketWindowSlideRows (4,000,000)

	var b strings.Builder
	b.WriteString("INSERT INTO otel_metrics_histogram (MetricName, SeriesID, TimeUnix, BucketCounts, ExplicitBounds) VALUES ")
	for i := 0; i < seriesCount; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "('http_server_request_duration', 'svc-%d', toDateTime64('2026-01-01 00:00:00', 9), [1, 2], [1.0, 2.0])", i)
	}
	if _, err := db.Exec(b.String()); err != nil {
		t.Fatalf("seed insert: %v", err)
	}

	sqlStr, args := buildWindowSlidePlan(t, numAnchors)
	_, err := db.Query(sqlStr, args...)
	if err == nil {
		t.Fatal("expected the resource bound's throwIf to fire for an oversized query, got no error")
	}
	if !strings.Contains(err.Error(), chsql.RangeBucketWindowSlideBudgetMessage) {
		t.Errorf("query failed, but not with the expected resource-bound message %q: %v",
			chsql.RangeBucketWindowSlideBudgetMessage, err)
	}
}

// TestRangeBucketWindowSlideBound_PassesWhenUnderBudget is the negative
// control: a comfortably-under-budget query (60 series x 1,001 anchors =
// 60,060 rows) must NOT trip the guard, proving
// TestRangeBucketWindowSlideBound_ThrowsWhenOversized's failure is really
// the size bound firing and not some unrelated query error every shape of
// this emitter's SQL would hit.
func TestRangeBucketWindowSlideBound_PassesWhenUnderBudget(t *testing.T) {
	db := chsqltest.OpenIsolatedChDB(t)
	if _, err := db.Exec(rangeBucketWindowSlideBoundDDL); err != nil {
		t.Fatalf("ddl: %v", err)
	}

	const seriesCount = 60
	const numAnchors = 1_001 // 60 * 1,001 = 60,060, well under maxRangeBucketWindowSlideRows

	var b strings.Builder
	b.WriteString("INSERT INTO otel_metrics_histogram (MetricName, SeriesID, TimeUnix, BucketCounts, ExplicitBounds) VALUES ")
	for i := 0; i < seriesCount; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "('http_server_request_duration', 'svc-%d', toDateTime64('2026-01-01 00:00:00', 9), [1, 2], [1.0, 2.0])", i)
	}
	if _, err := db.Exec(b.String()); err != nil {
		t.Fatalf("seed insert: %v", err)
	}

	sqlStr, args := buildWindowSlidePlan(t, numAnchors)
	rows, err := db.Query(sqlStr, args...)
	if err != nil {
		t.Fatalf("under-budget query unexpectedly failed: %v", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
	if n == 0 {
		t.Fatal("expected at least one row from the under-budget query")
	}
}
