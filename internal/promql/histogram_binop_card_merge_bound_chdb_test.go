//go:build chdb

// chDB-backed proof that the group_left()/group_right() two-operand
// exponential-histogram binop merge (histogram_native_binop_card.go's
// mergeTwoHistogramProjectionsCard) enforces the SAME bucket-width budget
// guard the default one-to-one path does
// (histogram_native_binop.go's histogramBinopBucketWidthBudgetGuardExpr,
// see histogram_binop_merge_bound_chdb_test.go's identical one-to-one
// proof) — a pre-release audit finding: mergeTwoHistogramProjectionsCard
// computed the identical unbounded arrayMap(range(mergedLength), ...)
// bucket ladder via the shared histogramBinopMergeProjections but never
// wired the guard in, so `hist_a + on(...) group_left() hist_b` with
// widely divergent Scale/Offset built an unbounded, expensive merged
// bucket ladder with nothing capping it.
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

// runHistogramBinopCardMergeBoundQuery lowers + emits
// `<metricA> + on(series) group_left() <metricB>` and runs it against
// fixture, returning the query error (nil on success). Mirrors
// runHistogramBinopMergeBoundQuery's outer `SELECT count() FROM (...)`
// wrap for the identical reason: chdb-go's parquet driver cannot decode
// the merged output's Map/Array(UInt64) histogram columns, and wrapping in
// count() still forces ClickHouse to fully evaluate the merge — including
// the guard's throwIf — without needing to decode any of them.
func runHistogramBinopCardMergeBoundQuery(t *testing.T, fixture *chdbFixture) error {
	t.Helper()
	s := schema.DefaultOTelMetrics()
	p := promparser.NewParser(promparser.Options{})
	query := fmt.Sprintf(
		"%s + on(series) group_left() %s",
		histogramBinopMergeBoundMetricA, histogramBinopMergeBoundMetricB,
	)
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

// TestHistogramBinopCardMergeBudget_ChDB_ScaleDivergenceCompactsRatherThanRejects
// is TestHistogramBinopMergeBudget_ChDB_ScaleDivergenceCompactsRatherThanRejects's
// group_left() sibling: same divergent-offset seed (0 vs 20000, both Scale
// 0, one bucket each — the NATURAL merge, at min(Scale) alone, would span
// 20001 buckets, crossing maxHistogramMergeCostUnits by many orders of
// magnitude), but reached via `+ on(series) group_left()` instead of
// default one-to-one matching, so it exercises
// mergeTwoHistogramProjectionsCard rather than mergeTwoHistogramProjections.
//
// Before cerberus issue #2428's own fix this query had NO guard at all and
// would have let ClickHouse attempt to allocate the unbounded merged
// array; before issue #3558's fix (this test's own prior form asserted a
// rejection here) the guard aborted the query outright. Cerberus issue
// #3558's [wrapExpHistogramMergeScaleRefinement] now downscales the
// merge's shared scale FIRST so the merged width never exceeds
// maxHistogramMergeOutputWidth (160), and the query now SUCCEEDS with a
// coarser merged distribution instead of refusing outright — see
// TestHistogramBinopMergeBudget_ChDB_ScaleDivergenceCompactsRatherThanRejects's
// identical one-to-one proof.
func TestHistogramBinopCardMergeBudget_ChDB_ScaleDivergenceCompactsRatherThanRejects(t *testing.T) {
	const farOffset = 20000
	var b strings.Builder
	b.WriteString(histogramMergeBoundSeedDDL)
	b.WriteString("INSERT INTO otel_metrics_exponential_histogram " + histogramMergeBoundInsertColumns + " VALUES\n")
	b.WriteString("    " + histogramBinopMergeBoundRow(histogramBinopMergeBoundMetricA, "x", 0) + ",\n")
	b.WriteString("    " + histogramBinopMergeBoundRow(histogramBinopMergeBoundMetricB, "x", farOffset) + ";\n")
	fixture := newChDBFixture(t, b.String())

	query := fmt.Sprintf("%s + on(series) group_left() %s", histogramBinopMergeBoundMetricA, histogramBinopMergeBoundMetricB)
	got, err := readMergedHistogramShape(t, fixture, query, promql.LowerOpts{})
	if err != nil {
		t.Fatalf("a scale-divergent group_left() binop merge must be compacted to a bounded width, not rejected: %v", err)
	}
	assertCompactedMerge(t, got, 0, farOffset+1, 2)
}

// TestHistogramBinopCardMergeBudget_ChDB_WithinBudget seeds a small,
// legitimate group_left() binop merge (adjacent offsets, well inside the
// bucket-width budget) and asserts the query succeeds — the guard must not
// fire on ordinary data.
func TestHistogramBinopCardMergeBudget_ChDB_WithinBudget(t *testing.T) {
	var b strings.Builder
	b.WriteString(histogramMergeBoundSeedDDL)
	b.WriteString("INSERT INTO otel_metrics_exponential_histogram " + histogramMergeBoundInsertColumns + " VALUES\n")
	b.WriteString("    " + histogramBinopMergeBoundRow(histogramBinopMergeBoundMetricA, "y", 0) + ",\n")
	b.WriteString("    " + histogramBinopMergeBoundRow(histogramBinopMergeBoundMetricB, "y", 1) + ";\n")
	fixture := newChDBFixture(t, b.String())

	if err := runHistogramBinopCardMergeBoundQuery(t, fixture); err != nil {
		t.Fatalf("a legitimate group_left() binop merge must not trip the budget guard: %v", err)
	}
}
