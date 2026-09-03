//go:build chdb

// chDB-backed proof that cerberus issue #2865's multi-group budget guard
// (exp_histogram_merge_summap_bound.go's expHistogramMergeSumMapMultiGroupBudgetGuardExpr)
// actually FIRES — and stays silent on a legitimate multi-group merge — at
// real ClickHouse execution, mirroring exp_histogram_merge_summap_bound_chdb_test.go's
// own structure for the single-group guard. chDB is fine here: these tests
// pin the throwIf's deterministic ARITHMETIC (group count and row count,
// known from the seed), not real peak memory — real-memory calibration is
// exp_histogram_merge_summap_bound.go's own header doc, against a real
// standalone server, never chDB (see that file's "Multi-group calibration"
// section).
package promql_test

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// runExpHistSumMapMultiGroupBoundQuery lowers + emits query through
// [expHistSumMapBoundNativeLowerers] (the shared table exp_histogram_merge_summap_chdb_test.go
// / exp_histogram_merge_summap_bound_chdb_test.go both use) and runs it
// against fixture, mirroring runExpHistSumMapBoundQueryWithOpts's own
// count()-wrapping rationale.
func runExpHistSumMapMultiGroupBoundQuery(t *testing.T, fixture *chdbFixture, query string) error {
	t.Helper()
	s := schema.DefaultOTelMetrics()
	p := promparser.NewParser(promparser.Options{})
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, s, histogramMergeBoundEvalTS, histogramMergeBoundEvalTS, 0,
		promql.LowerOpts{Lowerers: expHistSumMapBoundNativeLowerers})
	if err != nil {
		t.Fatalf("LowerAtRangeOpts(%q): %v", query, err)
	}
	sqlStr, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
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
	return rows.Err()
}

// seedExpHistSumMapMultiGroupBoundRows seeds `groups` route groups, each
// with `rowsPerGroup` single-bucket-wide series (Scale 0, PositiveOffset
// 0, one bucket) — a trivial per-group cost (width 1), so a test using
// this seed isolates the multi-group axes (total row count / group count)
// from the existing per-group width^2 term entirely.
func seedExpHistSumMapMultiGroupBoundRows(t *testing.T, groups, rowsPerGroup int) *chdbFixture {
	t.Helper()
	var b strings.Builder
	b.WriteString(histogramMergeBoundSeedDDL)
	b.WriteString("INSERT INTO otel_metrics_exponential_histogram (MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n")
	tuples := make([]string, 0, groups*rowsPerGroup)
	for g := 0; g < groups; g++ {
		route := "r" + strconv.Itoa(g)
		for i := 0; i < rowsPerGroup; i++ {
			tuples = append(tuples, fmt.Sprintf(
				"('%s', map('route', '%s', 'series', 's%d'), toDateTime64('2026-01-01 00:00:00', 9), 1, 1.0, 0, 0, 0, [1], 0, [])",
				histogramMergeBoundMetric, route, i,
			))
		}
	}
	b.WriteString("    " + strings.Join(tuples, ",\n    ") + ";\n")
	return newChDBFixture(t, b.String())
}

// TestExpHistogramMergeSumMapBudget_ChDB_MultiGroupGroupCountOverflowGuard
// seeds one past [maxHistogramMergeSumMapGroupCountGuard] (40,000) — one
// row per group, trivial per-group cost — isolating the group-COUNT axis
// no per-group check can see (exp_histogram_merge_summap_bound.go's own
// "Multi-group calibration" section: the real 40,000-group checkpoint this
// ceiling is pinned at).
func TestExpHistogramMergeSumMapBudget_ChDB_MultiGroupGroupCountOverflowGuard(t *testing.T) {
	const groups, rowsPerGroup = 40_001, 1
	fixture := seedExpHistSumMapMultiGroupBoundRows(t, groups, rowsPerGroup)

	query := fmt.Sprintf("sum by(route) (%s)", histogramMergeBoundMetric)
	err := runExpHistSumMapMultiGroupBoundQuery(t, fixture, query)
	if err == nil {
		t.Fatal("expected the multi-group budget guard to abort the query (40,001 groups exceeds " +
			"maxHistogramMergeSumMapGroupCountGuard=40,000, even though every individual group's own " +
			"cost is trivial), got no error")
	}
	if !strings.Contains(err.Error(), chplan.HistogramMergeBudgetMessage) {
		t.Fatalf("query failed, but not with the merge budget guard's throwIf: %v", err)
	}
}

// TestExpHistogramMergeSumMapBudget_ChDB_MultiGroupTotalRowCountOverflowGuard
// seeds 50 groups of 4,001 rows each (200,050 total, one past
// [maxHistogramMergeSumMapTotalRowCountGuard]=200,000) — each group's OWN
// row count (4,001) stays under the EXISTING, unchanged per-group
// [maxHistogramMergeRowCountOverflowGuard] backstop (4,096), so this
// isolates the TOTAL-row-count axis: the "few groups, each with an
// enormous row count" shape exp_histogram_merge_summap_bound.go's own
// "Multi-group calibration" section identifies as this guard's reason to
// exist, distinct from the group-count axis above.
func TestExpHistogramMergeSumMapBudget_ChDB_MultiGroupTotalRowCountOverflowGuard(t *testing.T) {
	const groups, rowsPerGroup = 50, 4_001
	fixture := seedExpHistSumMapMultiGroupBoundRows(t, groups, rowsPerGroup)

	query := fmt.Sprintf("sum by(route) (%s)", histogramMergeBoundMetric)
	err := runExpHistSumMapMultiGroupBoundQuery(t, fixture, query)
	if err == nil {
		t.Fatal("expected the multi-group budget guard to abort the query (50 groups x 4,001 rows = " +
			"200,050 total rows exceeds maxHistogramMergeSumMapTotalRowCountGuard=200,000, even though " +
			"each individual group's own row count stays under the existing per-group backstop), got no error")
	}
	if !strings.Contains(err.Error(), chplan.HistogramMergeBudgetMessage) {
		t.Fatalf("query failed, but not with the merge budget guard's throwIf: %v", err)
	}
}

// TestExpHistogramMergeSumMapBudget_ChDB_MultiGroupPerGroupCostStillEnforced
// seeds TWO groups — one legitimate, one with an unusually wide single
// series (width 4,000, cost 4x4000^2=64,000,000, over the 60,000,000
// default — the SAME shape TestExpHistogramMergeSumMapBudget_ChDB_WidthExceeded
// isolates for the single-group guard) — proving the multi-group guard
// still ORs in [expHistogramMergeSumMapCostOverBudgetExpr]'s existing
// per-group check rather than replacing it.
func TestExpHistogramMergeSumMapBudget_ChDB_MultiGroupPerGroupCostStillEnforced(t *testing.T) {
	var b strings.Builder
	b.WriteString(histogramMergeBoundSeedDDL)
	b.WriteString("INSERT INTO otel_metrics_exponential_histogram " + histogramMergeBoundInsertColumns + " VALUES\n")
	wideBuckets := make([]string, 4000)
	for i := range wideBuckets {
		wideBuckets[i] = "1"
	}
	tuples := []string{
		fmt.Sprintf("('%s', map('route', 'r0', 'series', 's0'), toDateTime64('2026-01-01 00:00:00', 9), 1, 1.0, 0, 0, 0, [%s], 0, [])",
			histogramMergeBoundMetric, strings.Join(wideBuckets, ",")),
		fmt.Sprintf("('%s', map('route', 'r1', 'series', 's1'), toDateTime64('2026-01-01 00:00:00', 9), 1, 1.0, 0, 0, 0, [1], 0, [])",
			histogramMergeBoundMetric),
	}
	b.WriteString("    " + strings.Join(tuples, ",\n    ") + ";\n")
	fixture := newChDBFixture(t, b.String())

	query := fmt.Sprintf("sum by(route) (%s)", histogramMergeBoundMetric)
	err := runExpHistSumMapMultiGroupBoundQuery(t, fixture, query)
	if err == nil {
		t.Fatal("expected the multi-group guard to still reject route r0's own width-4000 series " +
			"(cost 4x4000^2=64,000,000 exceeds the 60,000,000 default) via the existing per-group check, got no error")
	}
	if !strings.Contains(err.Error(), chplan.HistogramMergeBudgetMessage) {
		t.Fatalf("query failed, but not with the merge budget guard's throwIf: %v", err)
	}
}

// TestExpHistogramMergeSumMapBudget_ChDB_MultiGroupWithinBudget seeds a
// small, legitimate multi-group merge (3 groups, 2 series each) and
// asserts the multi-group guard does not fire on ordinary data, mirroring
// TestExpHistogramMergeSumMapBudget_ChDB_WithinBudget for the single-group
// guard.
func TestExpHistogramMergeSumMapBudget_ChDB_MultiGroupWithinBudget(t *testing.T) {
	const groups, rowsPerGroup = 3, 2
	fixture := seedExpHistSumMapMultiGroupBoundRows(t, groups, rowsPerGroup)

	query := fmt.Sprintf("sum by(route) (%s)", histogramMergeBoundMetric)
	if err := runExpHistSumMapMultiGroupBoundQuery(t, fixture, query); err != nil {
		t.Fatalf("a legitimate 3-group, 2-series-per-group merge must not trip the multi-group budget guard: %v", err)
	}
}
