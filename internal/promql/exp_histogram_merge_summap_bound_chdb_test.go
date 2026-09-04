//go:build chdb

// chDB-backed proof that [expHistogramMergeSumMapBudgetGuardExpr]
// (exp_histogram_merge_summap_bound.go, cerberus issue #2834) actually
// FIRES — and stays silent on a legitimate merge — at real ClickHouse
// execution, mirroring histogram_merge_bound_chdb_test.go's own structure
// for the OLD guard. chDB is fine here: these tests pin the throwIf's
// deterministic ARITHMETIC (row count and width, known from the seed), not
// real peak memory — real-memory calibration is
// exp_histogram_merge_summap_bound.go's own header doc, against a real
// server, never chDB (see that file).
//
// [TestExpHistogramMergeSumMapBudget_ChDB_LargeRowCountAdmitted] is the
// core regression proof for this issue: the SAME shape
// [TestExpHistogramMergeSumMapBudget_ChDB_FanoutStillRejectsLargeRowCount]
// proves the OLD, untouched [histogramMergeBudgetGuardExpr] still rejects
// (issue #2490's own repro) is admitted once lowered through
// [promql.NativeExpHistogramMergeLowerer].
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

// expHistSumMapBoundNativeLowerers opts a query into
// [promql.NativeExpHistogramMergeLowerer], falling back to the default fold
// for every ineligible shape — the same table
// exp_histogram_merge_summap_chdb_test.go's differential proof uses.
var expHistSumMapBoundNativeLowerers = promql.RangeLowerers{
	ExpHistogramMerge: promql.NativeExpHistogramMergeLowerer{},
}

// runExpHistSumMapBoundQuery lowers + emits `sum(<metric>)` through the
// given lowerers table (the shipped defaults for every other LowerOpts
// field) and runs it against fixture — the convenience wrapper around
// [runExpHistSumMapBoundQueryWithOpts], mirroring
// runHistogramMergeBoundQuery / runHistogramMergeBoundQueryWithOpts's own
// split (histogram_merge_bound_chdb_test.go).
func runExpHistSumMapBoundQuery(t *testing.T, fixture *chdbFixture, lowerers promql.RangeLowerers) error {
	t.Helper()
	return runExpHistSumMapBoundQueryWithOpts(t, fixture, promql.LowerOpts{Lowerers: lowerers})
}

// runExpHistSumMapBoundQueryWithOpts lowers + emits `sum(<metric>)` through
// the given full [promql.LowerOpts] (letting a test thread a non-default
// ResourceBounds — see
// [TestExpHistogramMergeSumMapBudget_ChDB_EnvOverrideSharesKnob]) and runs
// it against fixture, mirroring runHistogramMergeBoundQueryWithOpts
// exactly, including its count()-wrapping rationale (chdb-go's parquet
// driver cannot decode the merged row's Map/Array columns).
func runExpHistSumMapBoundQueryWithOpts(t *testing.T, fixture *chdbFixture, opts promql.LowerOpts) error {
	t.Helper()
	s := schema.DefaultOTelMetrics()
	p := promparser.NewParser(promparser.Options{})
	query := fmt.Sprintf("sum(%s)", histogramMergeBoundMetric)
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAtRangeOpts(context.Background(), expr, s, histogramMergeBoundEvalTS, histogramMergeBoundEvalTS, 0, opts)
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
	var n int64
	if err := rows.Scan(&n); err != nil {
		t.Fatalf("scan count(): %v", err)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if n != 1 {
		t.Fatalf("query returned count()=%d, want exactly 1 (the merged sum() row)", n)
	}
	return nil
}

// seedExpHistSumMapBoundRows seeds `rows` series sharing an identical
// PositiveOffset/width layout, mirroring
// histogramMergeBoundRowWide's row-generation loop.
func seedExpHistSumMapBoundRows(t *testing.T, rows, width int) *chdbFixture {
	t.Helper()
	var b strings.Builder
	b.WriteString(histogramMergeBoundSeedDDL)
	b.WriteString("INSERT INTO otel_metrics_exponential_histogram " + histogramMergeBoundInsertColumns + " VALUES\n")
	tuples := make([]string, rows)
	for i := range tuples {
		tuples[i] = histogramMergeBoundRowWide("s"+strconv.Itoa(i), 0, width)
	}
	b.WriteString("    " + strings.Join(tuples, ",\n    ") + ";\n")
	return newChDBFixture(t, b.String())
}

// TestExpHistogramMergeSumMapBudget_ChDB_LargeRowCountAdmitted seeds issue
// #2490's own repro shape — 3,741 series sharing a realistic OTel-default
// 160-bucket layout — and asserts [promql.NativeExpHistogramMergeLowerer]
// ADMITS it: `sumMapMergeCostMultiplier x (160^2 + 0)` = 4 x 25,600 =
// 102,400, far under the 60,000,000 default, and the row count (3,741)
// stays under [maxHistogramMergeRowCountOverflowGuard] (4096) too. See
// [TestExpHistogramMergeSumMapBudget_ChDB_FanoutStillRejectsLargeRowCount]
// for the same shape under the OLD, unchanged guard.
func TestExpHistogramMergeSumMapBudget_ChDB_LargeRowCountAdmitted(t *testing.T) {
	const rows, width = 3741, 160
	fixture := seedExpHistSumMapBoundRows(t, rows, width)

	if err := runExpHistSumMapBoundQuery(t, fixture, expHistSumMapBoundNativeLowerers); err != nil {
		t.Fatalf("issue #2490's own 3,741-series/width-160 repro must be ADMITTED under the "+
			"sumMap merge's rows-independent guard (real measured cost ~42 MiB): %v", err)
	}
}

// TestExpHistogramMergeSumMapBudget_ChDB_FanoutStillRejectsLargeRowCount
// seeds the IDENTICAL shape as
// [TestExpHistogramMergeSumMapBudget_ChDB_LargeRowCountAdmitted] but lowers
// through the DEFAULT [promql.FanoutExpHistogramMergeLowerer] (the zero
// value of [promql.RangeLowerers], same as every caller that has not opted
// into chopt.FeatureExpHistogramMergeSumMap) — a regression proof that
// [histogramMergeBudgetGuardExpr] (histogram_merge_bound.go) is completely
// UNTOUCHED by this issue: `rows(3741) x width(160)^2` = 95,769,600, over
// the 60,000,000 default, so this shape must still be rejected on every
// non-sumMap merge path.
func TestExpHistogramMergeSumMapBudget_ChDB_FanoutStillRejectsLargeRowCount(t *testing.T) {
	const rows, width = 3741, 160
	fixture := seedExpHistSumMapBoundRows(t, rows, width)

	err := runExpHistSumMapBoundQuery(t, fixture, promql.RangeLowerers{})
	if err == nil {
		t.Fatal("expected the OLD, unchanged merge budget guard to still reject issue #2490's own " +
			"3,741-series/width-160 repro on the default (non-sumMap) fold path, got no error")
	}
	if !strings.Contains(err.Error(), chplan.HistogramMergeBudgetMessage) {
		t.Fatalf("query failed, but not with the merge budget guard's throwIf: %v", err)
	}
}

// TestExpHistogramMergeSumMapBudget_ChDB_WidthExceeded seeds a single
// series (so the row-count backstop cannot be what rejects it) with an
// unusually wide layout: `sumMapMergeCostMultiplier x width^2` =
// 4 x 4000^2 = 64,000,000, over the 60,000,000 default — the width axis of
// this guard's own cost formula, isolated from the row-count backstop the
// way TestHistogramMergeBudget_ChDB_BucketWidthExceeded isolates the OLD
// guard's width axis.
func TestExpHistogramMergeSumMapBudget_ChDB_WidthExceeded(t *testing.T) {
	const rows, width = 1, 4000
	fixture := seedExpHistSumMapBoundRows(t, rows, width)

	err := runExpHistSumMapBoundQuery(t, fixture, expHistSumMapBoundNativeLowerers)
	if err == nil {
		t.Fatal("expected the sumMap merge budget guard to abort the query " +
			"(1 row x width 4000, cost 4x4000^2=64,000,000 exceeds the 60,000,000 default), got no error")
	}
	if !strings.Contains(err.Error(), chplan.HistogramMergeBudgetMessage) {
		t.Fatalf("query failed, but not with the merge budget guard's throwIf: %v", err)
	}
}

// TestExpHistogramMergeSumMapBudget_ChDB_RowCountOverflowGuard seeds 4,097
// single-bucket series — one past
// [maxHistogramMergeRowCountOverflowGuard] (4096) — so the width-driven
// cost term alone (4 x 1^2 = 4) sits nowhere near the 60,000,000 default:
// this isolates the reused row-count backstop
// [expHistogramMergeSumMapCostOverBudgetExpr] ORs in ahead of its own cost
// term, mirroring TestHistogramMergeBudget_ChDB_RowCountOverflowGuard for
// the OLD guard.
func TestExpHistogramMergeSumMapBudget_ChDB_RowCountOverflowGuard(t *testing.T) {
	const rows, width = 4097, 1
	fixture := seedExpHistSumMapBoundRows(t, rows, width)

	err := runExpHistSumMapBoundQuery(t, fixture, expHistSumMapBoundNativeLowerers)
	if err == nil {
		t.Fatal("expected the sumMap merge budget guard to abort the query (4,097 rows exceeds the " +
			"row-count backstop, even though width 1 keeps the cost formula itself trivial), got no error")
	}
	if !strings.Contains(err.Error(), chplan.HistogramMergeBudgetMessage) {
		t.Fatalf("query failed, but not with the merge budget guard's throwIf: %v", err)
	}
}

// TestExpHistogramMergeSumMapBudget_ChDB_WithinBudget seeds a small,
// legitimate merge and asserts the sumMap path's guard does not fire on
// ordinary data, mirroring TestHistogramMergeBudget_ChDB_WithinBudget.
func TestExpHistogramMergeSumMapBudget_ChDB_WithinBudget(t *testing.T) {
	const rows, width = 2, 1
	fixture := seedExpHistSumMapBoundRows(t, rows, width)

	if err := runExpHistSumMapBoundQuery(t, fixture, expHistSumMapBoundNativeLowerers); err != nil {
		t.Fatalf("a legitimate two-series merge must not trip the sumMap budget guard: %v", err)
	}
}

// TestExpHistogramMergeSumMapBudget_ChDB_EnvOverrideSharesKnob proves the
// sumMap guard is NOT a second, incompatible knob (cerberus issue #2834's
// own requirement): it seeds the exact width-4000 single-series shape
// [TestExpHistogramMergeSumMapBudget_ChDB_WidthExceeded] proves the
// 60,000,000 default rejects, raises the SAME
// CERBERUS_PROMQL_HISTOGRAM_MERGE_MAX_COST_UNITS override
// [TestHistogramMergeBudget_ChDB_EnvOverrideRaisesBudget] uses for the OLD
// guard, and asserts the identical query now succeeds — the sumMap guard
// reads [promql.ResourceBounds.HistogramMergeMaxCostUnits], never an
// independent env var.
func TestExpHistogramMergeSumMapBudget_ChDB_EnvOverrideSharesKnob(t *testing.T) {
	const raisedBudget = 100_000_000 // > 64,000,000 (1 row x width(4000) sumMap cost), > the 60M default

	t.Setenv(promql.EnvHistogramMergeMaxCostUnits, strconv.FormatInt(raisedBudget, 10))
	bounds, err := promql.ResourceBoundsFromEnv()
	if err != nil {
		t.Fatalf("ResourceBoundsFromEnv: %v", err)
	}
	if bounds.HistogramMergeMaxCostUnits != raisedBudget {
		t.Fatalf("ResourceBoundsFromEnv().HistogramMergeMaxCostUnits = %d, want %d (the %s override)",
			bounds.HistogramMergeMaxCostUnits, raisedBudget, promql.EnvHistogramMergeMaxCostUnits)
	}

	const rows, width = 1, 4000
	fixture := seedExpHistSumMapBoundRows(t, rows, width)

	opts := promql.LowerOpts{Lowerers: expHistSumMapBoundNativeLowerers, ResourceBounds: bounds}
	if err := runExpHistSumMapBoundQueryWithOpts(t, fixture, opts); err != nil {
		t.Fatalf("the same width-4000 single-series merge %s proves the 60M default rejects must "+
			"succeed once the SHARED %s override raises the budget to %d: %v",
			t.Name(), promql.EnvHistogramMergeMaxCostUnits, raisedBudget, err)
	}
}
