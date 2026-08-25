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

	"github.com/tsouza/cerberus/internal/chplan"
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

// TestHistogramBinopCardMergeBudget_ChDB_BucketWidthExceeded is
// TestHistogramBinopMergeBudget_ChDB_BucketWidthExceeded's group_left()
// sibling: same divergent-offset seed (0 vs 20000, both Scale 0, one
// bucket each — merged bucket ladder spans 20001 buckets, crossing
// maxHistogramMergeCostUnits by many orders of magnitude), but reached via
// `+ on(series) group_left()` instead of default one-to-one matching, so
// it exercises mergeTwoHistogramProjectionsCard rather than
// mergeTwoHistogramProjections. Before the fix this query had NO guard at
// all and would have let ClickHouse attempt to allocate the unbounded
// merged array.
func TestHistogramBinopCardMergeBudget_ChDB_BucketWidthExceeded(t *testing.T) {
	var b strings.Builder
	b.WriteString(histogramMergeBoundSeedDDL)
	b.WriteString("INSERT INTO otel_metrics_exponential_histogram " + histogramMergeBoundInsertColumns + " VALUES\n")
	b.WriteString("    " + histogramBinopMergeBoundRow(histogramBinopMergeBoundMetricA, "x", 0) + ",\n")
	b.WriteString("    " + histogramBinopMergeBoundRow(histogramBinopMergeBoundMetricB, "x", 20000) + ";\n")
	fixture := newChDBFixture(t, b.String())

	err := runHistogramBinopCardMergeBoundQuery(t, fixture)
	if err == nil {
		t.Fatal("expected the group_left() binop merge budget guard to abort the query (merged width 20001 > 16384), got no error")
	}
	if !strings.Contains(err.Error(), chplan.HistogramMergeBudgetMessage) {
		t.Fatalf("query failed, but not with the merge budget guard's throwIf: %v", err)
	}
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
