//go:build chdb

// chDB-backed proof that the two-operand exponential-histogram binop merge's
// bucket-width budget guard (#2428, histogram_native_binop.go's
// histogramBinopBucketWidthBudgetGuardExpr) actually FIRES at real
// ClickHouse execution — not merely that the emitted SQL contains the right
// tokens — and that it stays silent on a legitimate, well-within-budget
// merge. Mirrors histogram_merge_bound_chdb_test.go's structure for the
// cross-series merge guard (#2385), reusing that file's seed DDL / insert
// column list / eval timestamp: both guards bound the identical
// arrayMap-over-mergedLength shape, just over a different Aggregate.
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

// TestHistogramBinopMergeBudget_ChDB_BucketWidthExceeded seeds one series
// on each of the two operand metrics, sharing the `series` label so default
// one-to-one matching pairs them, whose PositiveOffset diverges enough (0
// vs 20000, both Scale 0, one bucket each) that the merged bucket ladder
// spans 20001 buckets — so `rows(2, fixed by construction) x width(20001)^2`
// crosses maxHistogramMergeCostUnits (histogram_merge_bound.go) by many
// orders of magnitude — and asserts the query aborts with the budget
// guard's own throwIf rather than letting ClickHouse allocate an unbounded
// Array(target-bucket-count) per output row (cerberus issue #2428).
func TestHistogramBinopMergeBudget_ChDB_BucketWidthExceeded(t *testing.T) {
	var b strings.Builder
	b.WriteString(histogramMergeBoundSeedDDL)
	b.WriteString("INSERT INTO otel_metrics_exponential_histogram " + histogramMergeBoundInsertColumns + " VALUES\n")
	b.WriteString("    " + histogramBinopMergeBoundRow(histogramBinopMergeBoundMetricA, "x", 0) + ",\n")
	b.WriteString("    " + histogramBinopMergeBoundRow(histogramBinopMergeBoundMetricB, "x", 20000) + ";\n")
	fixture := newChDBFixture(t, b.String())

	err := runHistogramBinopMergeBoundQuery(t, fixture)
	if err == nil {
		t.Fatal("expected the binop merge budget guard to abort the query (merged width 20001 > 16384), got no error")
	}
	// See TestHistogramMergeBudget_ChDB_BucketWidthExceeded's identical
	// comment: this drives chdb-go's raw driver handle directly, never
	// through chclient, so a raw substring check against chdb-go's own
	// exception text is what proves the emitted SQL's throwIf fired at
	// real ClickHouse execution, carrying the right message.
	if !strings.Contains(err.Error(), chplan.HistogramMergeBudgetMessage) {
		t.Fatalf("query failed, but not with the merge budget guard's throwIf: %v", err)
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
