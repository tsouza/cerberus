//go:build chdb

// chDB-backed proof that the two-operand exponential-histogram binop
// merge's scale refinement (#3558, histogram_merge_bound.go's
// wrapExpHistogramMergeScaleRefinement) and its bucket-width budget guard
// (#2428, histogram_native_binop.go's histogramBinopBucketWidthBudgetGuardExpr)
// behave correctly at real ClickHouse execution — not merely that the
// emitted SQL contains the right tokens: a scale-divergent merge is
// compacted to a bounded width rather than rejected, and a legitimate,
// well-within-budget merge stays untouched. Mirrors
// histogram_merge_bound_chdb_test.go's structure for the cross-series
// merge guard (#2385/#3555), reusing that file's seed DDL / insert column
// list / eval timestamp: all three guards bound the identical
// arrayMap-over-mergedLength shape, just over a different Aggregate.
package promql_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/testsql"
)

// histogramBinopMergeBoundMetricA / B are the two distinct metric names
// `<a> + <b>` binds together — the binop merge (unlike the cross-series
// merge sum() reads over ONE metric's own series) always joins two
// DIFFERENT selector expressions, so this needs two metric names sharing a
// seeded row with the SAME series label for default one-to-one vector
// matching to find them matched.
const (
	histogramBinopMergeBoundMetricA = "http_binop_bound_a_exp_hist"
	histogramBinopMergeBoundMetricB = "http_binop_bound_b_exp_hist"
)

// histogramBinopMergeBoundRow renders one INSERT VALUES tuple for the given
// metric, matching histogramMergeBoundInsertColumns — a single-bucket
// positive-only distribution at the given series label and PositiveOffset
// (Scale 0), the same shape histogramMergeBoundRow renders for the
// cross-series merge's own seed.
func histogramBinopMergeBoundRow(metric, series string, offset int) string {
	return fmt.Sprintf(
		"('%s', map('series', '%s'), toDateTime64('2026-01-01 00:00:00', 9), 1, 1.0, 0, 0, %d, [1], 0, [])",
		metric, series, offset,
	)
}

// runHistogramBinopMergeBoundQuery lowers + emits `<metricA> + <metricB>`
// and runs it against fixture, returning the query error (nil on success).
// See runHistogramMergeBoundQuery's doc for why the query is wrapped in an
// outer `SELECT count() FROM (...)`: chdb-go's parquet driver cannot decode
// the merged output's Map/Array(UInt64) histogram columns, and wrapping in
// count() still forces ClickHouse to fully evaluate the merge — including
// the Having clause's throwIf — without needing to decode any of them.
func runHistogramBinopMergeBoundQuery(t *testing.T, fixture *chdbFixture) error {
	t.Helper()
	s := schema.DefaultOTelMetrics()
	p := promparser.NewParser(promparser.Options{})
	query := fmt.Sprintf("%s + %s", histogramBinopMergeBoundMetricA, histogramBinopMergeBoundMetricB)
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, histogramMergeBoundEvalTS, histogramMergeBoundEvalTS)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}
	if err := testsql.CheckSeedCoversFanOut(fixture.seed, sqlStr); err != nil {
		t.Fatalf("seed does not cover the emitted fan-out: %v\nSQL: %s", err, sqlStr)
	}
	wrapped := "SELECT count() FROM (" + sqlStr + ")"
	rows, qerr := fixture.db.Query(wrapped, args...)
	if qerr != nil {
		return qerr
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		t.Fatal("count() query returned no rows")
	}
	var n int64
	if err := rows.Scan(&n); err != nil {
		t.Fatalf("scan count(): %v", err)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if n != 1 {
		t.Fatalf("query returned count()=%d, want exactly 1 (the merged binop row)", n)
	}
	return nil
}

// TestHistogramBinopMergeBudget_ChDB_ScaleDivergenceCompactsRatherThanRejects
// seeds one series on each of the two operand metrics, sharing the
// `series` label so default one-to-one matching pairs them, whose
// PositiveOffset diverges enough (0 vs 20000, both Scale 0, one bucket
// each) that the NATURAL merge — at min(Scale) alone, with no further
// downscale — would span 20001 buckets, so `rows(2, fixed by construction)
// x width(20001)^2` crosses maxHistogramMergeCostUnits
// (histogram_merge_bound.go) by many orders of magnitude.
//
// Before cerberus issue #3558's fix this aborted with the budget guard's
// own throwIf: min(Scale) alone only guarantees every operand CAN be
// downscaled onto the merge, not that the resulting range is narrow — two
// individually-narrow operands (one bucket each) with merely DIFFERENT
// central values blew the guard exactly like this, mirroring
// TestHistogramMergeBudget_ChDB_ScaleDivergenceCompactsRatherThanRejects's
// identical pre-#3555 shape for the cross-series merge.
// [wrapExpHistogramMergeScaleRefinement] now downscales the merge's shared
// scale FIRST so the merged width never exceeds
// maxHistogramMergeOutputWidth (160), and the guard sees a cost of
// 2 x 160^2 = 51,200 — comfortably under budget — so the query now
// SUCCEEDS with a coarser merged distribution instead of refusing
// outright. The merged row's Scale, width and Count are read back and
// checked against the cap's own arithmetic (see assertCompactedMerge), so
// a refinement that over-narrowed would fail here rather than pass as
// "not rejected".
func TestHistogramBinopMergeBudget_ChDB_ScaleDivergenceCompactsRatherThanRejects(t *testing.T) {
	const farOffset = 20000
	var b strings.Builder
	b.WriteString(histogramMergeBoundSeedDDL)
	b.WriteString("INSERT INTO otel_metrics_exponential_histogram " + histogramMergeBoundInsertColumns + " VALUES\n")
	b.WriteString("    " + histogramBinopMergeBoundRow(histogramBinopMergeBoundMetricA, "x", 0) + ",\n")
	b.WriteString("    " + histogramBinopMergeBoundRow(histogramBinopMergeBoundMetricB, "x", farOffset) + ";\n")
	fixture := newChDBFixture(t, b.String())

	query := fmt.Sprintf("%s + %s", histogramBinopMergeBoundMetricA, histogramBinopMergeBoundMetricB)
	got, err := readMergedHistogramShape(t, fixture, query, promql.LowerOpts{})
	if err != nil {
		t.Fatalf("a scale-divergent two-operand binop merge must be compacted to a bounded width, not rejected: %v", err)
	}
	assertCompactedMerge(t, got, 0, farOffset+1, 2)
}

// TestHistogramBinopMergeBudget_ChDB_NarrowMergeKeepsScale pins the other
// half of the refinement for the binop merge: two operands six buckets
// apart already fit the cap and keep their own Scale and every bucket.
func TestHistogramBinopMergeBudget_ChDB_NarrowMergeKeepsScale(t *testing.T) {
	const farOffset = 5
	var b strings.Builder
	b.WriteString(histogramMergeBoundSeedDDL)
	b.WriteString("INSERT INTO otel_metrics_exponential_histogram " + histogramMergeBoundInsertColumns + " VALUES\n")
	b.WriteString("    " + histogramBinopMergeBoundRow(histogramBinopMergeBoundMetricA, "x", 0) + ",\n")
	b.WriteString("    " + histogramBinopMergeBoundRow(histogramBinopMergeBoundMetricB, "x", farOffset) + ";\n")
	fixture := newChDBFixture(t, b.String())

	query := fmt.Sprintf("%s + %s", histogramBinopMergeBoundMetricA, histogramBinopMergeBoundMetricB)
	got, err := readMergedHistogramShape(t, fixture, query, promql.LowerOpts{})
	if err != nil {
		t.Fatalf("a narrow two-operand binop merge must succeed: %v", err)
	}
	if got.Scale != 0 || got.Width != farOffset+1 || got.Count != 2 {
		t.Fatalf("narrow binop merge changed shape: got %+v, want Scale 0, width %d, Count 2", got, farOffset+1)
	}
}

// TestHistogramBinopMergeBudget_ChDB_RowCountFixedNeverOverflowsWidthAlone
// seeds one series on each operand metric whose PositiveOffset diverges
// enough (0 vs 2,000,000, both Scale 0) that EVEN AFTER
// [wrapExpHistogramMergeScaleRefinement]'s width-160 cap, the row-count
// overflow guard is not what would reject it (the fixed operand count is
// always 2, far under [maxHistogramMergeRowCountOverflowGuard]) — the
// SAME cost formula's row-count-overflow disjunct that
// TestHistogramMergeBudget_ChDB_RowCountOverflowGuard isolates for the
// cross-series merge never applies here by construction. This proves the
// refinement, not the row-count backstop, is what admits an even more
// extreme scale divergence than the shape above.
func TestHistogramBinopMergeBudget_ChDB_RowCountFixedNeverOverflowsWidthAlone(t *testing.T) {
	const extremeOffset = 2_000_000
	var b strings.Builder
	b.WriteString(histogramMergeBoundSeedDDL)
	b.WriteString("INSERT INTO otel_metrics_exponential_histogram " + histogramMergeBoundInsertColumns + " VALUES\n")
	b.WriteString("    " + histogramBinopMergeBoundRow(histogramBinopMergeBoundMetricA, "x", 0) + ",\n")
	b.WriteString("    " + histogramBinopMergeBoundRow(histogramBinopMergeBoundMetricB, "x", extremeOffset) + ";\n")
	fixture := newChDBFixture(t, b.String())

	if err := runHistogramBinopMergeBoundQuery(t, fixture); err != nil {
		t.Fatalf("an even more extreme scale-divergent two-operand binop merge must still be compacted "+
			"to a bounded width by the refinement, not rejected: %v", err)
	}
}

// TestHistogramBinopMergeBudget_ChDB_WithinBudget seeds a small, legitimate
// binop merge (adjacent offsets, well inside the bucket-width budget) and
// asserts the query succeeds — the guard must not fire on ordinary data.
func TestHistogramBinopMergeBudget_ChDB_WithinBudget(t *testing.T) {
	var b strings.Builder
	b.WriteString(histogramMergeBoundSeedDDL)
	b.WriteString("INSERT INTO otel_metrics_exponential_histogram " + histogramMergeBoundInsertColumns + " VALUES\n")
	b.WriteString("    " + histogramBinopMergeBoundRow(histogramBinopMergeBoundMetricA, "y", 0) + ",\n")
	b.WriteString("    " + histogramBinopMergeBoundRow(histogramBinopMergeBoundMetricB, "y", 1) + ";\n")
	fixture := newChDBFixture(t, b.String())

	if err := runHistogramBinopMergeBoundQuery(t, fixture); err != nil {
		t.Fatalf("a legitimate binop merge must not trip the budget guard: %v", err)
	}
}
