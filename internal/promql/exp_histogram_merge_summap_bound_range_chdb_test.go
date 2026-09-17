//go:build chdb

// chDB-backed proof that cerberus issue #3027's range-mode budget guard
// (exp_histogram_merge_summap_bound.go's expHistogramMergeSumMapRangeBudgetGuardExpr)
// actually FIRES — and stays silent on a legitimate range-mode merge — at
// real ClickHouse execution, mirroring
// exp_histogram_merge_summap_bound_multigroup_chdb_test.go's own structure
// for the instant multi-group guard. chDB is fine here: these tests pin
// the throwIf's deterministic ARITHMETIC (step/group count and row count,
// known from the seed), not real peak memory — real-memory calibration is
// exp_histogram_merge_summap_bound.go's own header doc, against a real
// standalone server, never chDB (see that file's "Range-mode calibration"
// section).
package promql_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// expHistSumMapRangeBoundStep is the query_range step every case below
// uses to build its anchor grid.
const expHistSumMapRangeBoundStep = 60 * time.Second

// runExpHistSumMapRangeBoundQuery lowers + emits query as a query_range
// request ([start,end] at expHistSumMapRangeBoundStep, `steps` anchors)
// through [expHistSumMapBoundNativeLowerers] and runs it against fixture,
// mirroring runExpHistSumMapMultiGroupBoundQuery's own count()-wrapping
// rationale.
func runExpHistSumMapRangeBoundQuery(t *testing.T, fixture *chdbFixture, query string, steps int) error {
	t.Helper()
	s := schema.DefaultOTelMetrics()
	p := promparser.NewParser(promparser.Options{})
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	end := expHistSumMapRangeBoundBaseline.Add(time.Duration(steps) * expHistSumMapRangeBoundStep)
	start := end.Add(-time.Duration(steps-1) * expHistSumMapRangeBoundStep)
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, s, start, end, expHistSumMapRangeBoundStep,
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

// expHistSumMapRangeBoundBaseline anchors every seed below at a fixed
// instant so [runExpHistSumMapRangeBoundQuery]'s own start/end derivation
// (from `steps` alone) lines up with whatever [seedExpHistSumMapRangeBoundRows]
// wrote for the SAME `steps`.
var expHistSumMapRangeBoundBaseline = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// seedExpHistSumMapRangeBoundRows seeds `steps` step anchors, each with
// `rowsPerStep` distinct single-bucket-wide series (Scale 0,
// PositiveOffset 0, one bucket) contributing to it — trivial per-group
// cost (width 1), so a test using this seed isolates the range-mode
// multi-group axes (total row count / total group count) from the
// existing per-group width^2 term entirely, mirroring
// seedExpHistSumMapMultiGroupBoundRows's identical isolation for instant
// mode. Every row is timestamped one second before its own anchor, well
// inside the default 5m staleness lookback and closer to that anchor than
// to any neighbor (expHistSumMapRangeBoundStep is 60s).
func seedExpHistSumMapRangeBoundRows(t *testing.T, steps, rowsPerStep int) *chdbFixture {
	t.Helper()
	end := expHistSumMapRangeBoundBaseline.Add(time.Duration(steps) * expHistSumMapRangeBoundStep)
	start := end.Add(-time.Duration(steps-1) * expHistSumMapRangeBoundStep)

	var b strings.Builder
	b.WriteString(histogramMergeBoundSeedDDL)
	b.WriteString("INSERT INTO otel_metrics_exponential_histogram (MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n")
	tuples := make([]string, 0, steps*rowsPerStep)
	for step := 0; step < steps; step++ {
		anchor := start.Add(time.Duration(step) * expHistSumMapRangeBoundStep)
		ts := anchor.Add(-time.Second)
		for i := 0; i < rowsPerStep; i++ {
			tuples = append(tuples, fmt.Sprintf(
				"('%s', map('series', 's%d_%d'), toDateTime64('%s', 9), 1, 1.0, 0, 0, 0, [1], 0, [])",
				histogramMergeBoundMetric, step, i, ts.Format("2006-01-02 15:04:05"),
			))
		}
	}
	b.WriteString("    " + strings.Join(tuples, ",\n    ") + ";\n")
	return newChDBFixture(t, b.String())
}

// TestExpHistogramMergeSumMapBudget_ChDB_RangeGroupCountOverflowGuard
// seeds one past [maxHistogramMergeSumMapRangeGroupCountGuard] (600) —
// one row per step, no by()/without(), trivial per-group cost — isolating
// the STEP/group-count axis no per-group check can see
// (exp_histogram_merge_summap_bound.go's own "Range-mode calibration"
// section: the real 600-step checkpoint this ceiling is pinned at).
func TestExpHistogramMergeSumMapBudget_ChDB_RangeGroupCountOverflowGuard(t *testing.T) {
	const steps, rowsPerStep = 601, 1
	fixture := seedExpHistSumMapRangeBoundRows(t, steps, rowsPerStep)

	query := fmt.Sprintf("sum(%s)", histogramMergeBoundMetric)
	err := runExpHistSumMapRangeBoundQuery(t, fixture, query, steps)
	if err == nil {
		t.Fatal("expected the range-mode budget guard to abort the query (601 step anchors exceeds " +
			"maxHistogramMergeSumMapRangeGroupCountGuard=600, even though every individual anchor's own " +
			"cost is trivial), got no error")
	}
	if !strings.Contains(err.Error(), chplan.HistogramMergeBudgetMessage) {
		t.Fatalf("query failed, but not with the merge budget guard's throwIf: %v", err)
	}
}

// TestExpHistogramMergeSumMapBudget_ChDB_RangeTotalRowCountOverflowGuard
// seeds 2 step anchors of 3,001 rows each (6,002 total, one past
// [maxHistogramMergeSumMapRangeTotalRowCountGuard]=6,000) — each anchor's
// OWN row count (3,001) stays under the EXISTING, unchanged per-group
// [maxHistogramMergeRowCountOverflowGuard] backstop (4,096) and the
// step/group count (2) stays far under
// [maxHistogramMergeSumMapRangeGroupCountGuard] (600), so this isolates
// the TOTAL-row-count axis exactly like
// TestExpHistogramMergeSumMapBudget_ChDB_MultiGroupTotalRowCountOverflowGuard
// does for instant mode.
func TestExpHistogramMergeSumMapBudget_ChDB_RangeTotalRowCountOverflowGuard(t *testing.T) {
	const steps, rowsPerStep = 2, 3_001
	fixture := seedExpHistSumMapRangeBoundRows(t, steps, rowsPerStep)

	query := fmt.Sprintf("sum(%s)", histogramMergeBoundMetric)
	err := runExpHistSumMapRangeBoundQuery(t, fixture, query, steps)
	if err == nil {
		t.Fatal("expected the range-mode budget guard to abort the query (2 anchors x 3,001 rows = " +
			"6,002 total rows exceeds maxHistogramMergeSumMapRangeTotalRowCountGuard=6,000, even though " +
			"each individual anchor's own row count stays under the existing per-group backstop and the " +
			"anchor count stays under the range group-count ceiling), got no error")
	}
	if !strings.Contains(err.Error(), chplan.HistogramMergeBudgetMessage) {
		t.Fatalf("query failed, but not with the merge budget guard's throwIf: %v", err)
	}
}

// TestExpHistogramMergeSumMapBudget_ChDB_RangePerGroupCostStillEnforced
// seeds TWO step anchors — one legitimate, one whose OWN row count
// (4,097) is one past [maxHistogramMergeRowCountOverflowGuard] (4,096) —
// proving the range-mode guard still ORs in
// [expHistogramMergeSumMapCostOverBudgetExpr]'s existing per-group check
// rather than replacing it, exactly like
// TestExpHistogramMergeSumMapBudget_ChDB_MultiGroupPerGroupCostStillEnforced
// does for instant mode. The overflow anchor's own 4,097 rows and the
// 2-anchor/4,098-total-row shape both stay far under
// [maxHistogramMergeSumMapRangeTotalRowCountGuard] (6,000) and
// [maxHistogramMergeSumMapRangeGroupCountGuard] (600), isolating the
// PER-GROUP axis cleanly.
//
// This used to seed one anchor with an unusually wide single series
// (width 4,000) instead — cerberus issue #3558's scale-refinement fix
// now downscales EVERY anchor's width to [maxHistogramMergeOutputWidth]
// (160) or narrower before this guard's cost check ever runs, so no
// width shape can trip the per-group check any more (see
// TestExpHistogramMergeSumMapBudget_ChDB_WidthCompactsRatherThanRejects);
// the row-count-overflow backstop is the one axis that fix does not
// touch.
func TestExpHistogramMergeSumMapBudget_ChDB_RangePerGroupCostStillEnforced(t *testing.T) {
	const steps = 2
	const overflowAnchorRows = 4_097 // one past maxHistogramMergeRowCountOverflowGuard (4,096)
	end := expHistSumMapRangeBoundBaseline.Add(time.Duration(steps) * expHistSumMapRangeBoundStep)
	start := end.Add(-time.Duration(steps-1) * expHistSumMapRangeBoundStep)
	ts0 := start.Add(-time.Second).Format("2006-01-02 15:04:05")
	ts1 := start.Add(expHistSumMapRangeBoundStep).Add(-time.Second).Format("2006-01-02 15:04:05")

	var b strings.Builder
	b.WriteString(histogramMergeBoundSeedDDL)
	b.WriteString("INSERT INTO otel_metrics_exponential_histogram " + histogramMergeBoundInsertColumns + " VALUES\n")
	tuples := make([]string, 0, overflowAnchorRows+1)
	for i := 0; i < overflowAnchorRows; i++ {
		tuples = append(tuples, fmt.Sprintf(
			"('%s', map('series', 's0_%d'), toDateTime64('%s', 9), 1, 1.0, 0, 0, 0, [1], 0, [])",
			histogramMergeBoundMetric, i, ts0,
		))
	}
	tuples = append(tuples, fmt.Sprintf(
		"('%s', map('series', 's1'), toDateTime64('%s', 9), 1, 1.0, 0, 0, 0, [1], 0, [])",
		histogramMergeBoundMetric, ts1,
	))
	b.WriteString("    " + strings.Join(tuples, ",\n    ") + ";\n")
	fixture := newChDBFixture(t, b.String())

	query := fmt.Sprintf("sum(%s)", histogramMergeBoundMetric)
	err := runExpHistSumMapRangeBoundQuery(t, fixture, query, steps)
	if err == nil {
		t.Fatal("expected the range-mode guard to still reject the first anchor's own 4,097-row " +
			"overflow (one past the row-count backstop) via the existing per-group check, got no error")
	}
	if !strings.Contains(err.Error(), chplan.HistogramMergeBudgetMessage) {
		t.Fatalf("query failed, but not with the merge budget guard's throwIf: %v", err)
	}
}

// TestExpHistogramMergeSumMapBudget_ChDB_RangeWithinBudget seeds a small,
// legitimate range-mode merge (3 step anchors, 2 series each) and asserts
// the range-mode guard does not fire on ordinary data, mirroring
// TestExpHistogramMergeSumMapBudget_ChDB_MultiGroupWithinBudget for
// instant mode.
func TestExpHistogramMergeSumMapBudget_ChDB_RangeWithinBudget(t *testing.T) {
	const steps, rowsPerStep = 3, 2
	fixture := seedExpHistSumMapRangeBoundRows(t, steps, rowsPerStep)

	query := fmt.Sprintf("sum(%s)", histogramMergeBoundMetric)
	if err := runExpHistSumMapRangeBoundQuery(t, fixture, query, steps); err != nil {
		t.Fatalf("a legitimate 3-anchor, 2-series-per-anchor range merge must not trip the range-mode budget guard: %v", err)
	}
}

// TestExpHistogramMergeSumMapBudget_ChDB_RangeScaleDivergenceCompactsRatherThanRejects
// is cerberus issue #3558's own repro shape for the range-mode mergedScale
// mechanism: ONE step anchor with two individually-narrow series (one
// bucket apiece, Scale 0) whose PositiveOffset diverges enough (0 vs
// 4,000) that the natural merge would span 4,001 buckets — the SAME shape
// TestExpHistogramMergeSumMapBudget_ChDB_MultiGroupScaleDivergenceCompactsRatherThanRejects
// proves for instant multi-group mode, here routed through range mode's
// own anchor-prepended WindowExpr partition. The query must succeed with
// a compacted merge, not reject.
func TestExpHistogramMergeSumMapBudget_ChDB_RangeScaleDivergenceCompactsRatherThanRejects(t *testing.T) {
	const steps = 1
	end := expHistSumMapRangeBoundBaseline.Add(time.Duration(steps) * expHistSumMapRangeBoundStep)
	start := end.Add(-time.Duration(steps-1) * expHistSumMapRangeBoundStep)
	ts0 := start.Add(-time.Second).Format("2006-01-02 15:04:05")

	var b strings.Builder
	b.WriteString(histogramMergeBoundSeedDDL)
	b.WriteString("INSERT INTO otel_metrics_exponential_histogram (MetricName, Attributes, TimeUnix, Count, Sum, Scale, ZeroCount, PositiveOffset, PositiveBucketCounts, NegativeOffset, NegativeBucketCounts) VALUES\n")
	tuples := []string{
		fmt.Sprintf("('%s', map('series', 'near'), toDateTime64('%s', 9), 1, 1.0, 0, 0, 0, [1], 0, [])",
			histogramMergeBoundMetric, ts0),
		fmt.Sprintf("('%s', map('series', 'far'), toDateTime64('%s', 9), 1, 1.0, 0, 0, 4000, [1], 0, [])",
			histogramMergeBoundMetric, ts0),
	}
	b.WriteString("    " + strings.Join(tuples, ",\n    ") + ";\n")
	fixture := newChDBFixture(t, b.String())

	query := fmt.Sprintf("sum(%s)", histogramMergeBoundMetric)
	if err := runExpHistSumMapRangeBoundQuery(t, fixture, query, steps); err != nil {
		t.Fatalf("a scale-divergent two-series range-mode sumMap merge must be compacted to a bounded width, not rejected: %v", err)
	}
}
