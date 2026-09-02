package chsql

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// This file pins the DELTA-prefix aggregate MATRIX path's two presence
// guards and their shared earliestAnchor arithmetic
// (deltaMatrixLevelSourceAggregateDailyFanout, applyMatrixFanoutScanBound
// Aggregate, matrixWindowScanBoundsFrags) at the emitted-SQL-TEXT level, no
// chDB required. This complements — rather than duplicates — the
// chdb-tagged behavioural coverage in
// range_window_delta_prefix_aggregate_matrix_chdb_test.go: `just
// mutate-pkg` runs `gremlins unleash` WITHOUT the `chdb` build tag (see
// Justfile's `mutate-pkg` recipe and `mutate-chdb`'s own doc comment), so
// every //go:build chdb test file is invisible to the standard mutation
// gate and cannot kill a single mutant there, however good its assertions.
// These tests exist specifically to be visible to that untagged run.
//
// matrixDeltaGuardTestWindow builds the same shape
// range_window_delta_prefix_aggregate_matrix_chdb_test.go's
// deltaPrefixCanonicalizedMatrixWindow does (a `rate()` RangeWindow with
// DeltaPrefixAggregateInput set, hitting deltaMatrixLevelSourceAggregate),
// with a 3-anchor grid (step 10s, outer range 20s) chosen so
// (numAnchors-1)*stepNS = 20_000_000_000 and stepNS = 10_000_000_000 are
// two distinct, easily-recognised nanosecond literals — an ARITHMETIC_BASE
// flip of either the `-` or the `*` in
// `(numAnchors-1)*stepNS` changes this value to something else entirely
// (40e9 or 0), so asserting the exact literal is a direct kill.
func matrixDeltaGuardTestWindow() (*chplan.RangeWindow, time.Time) {
	end := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	step := 10 * time.Second
	start := end.Add(-2 * step)
	r := &chplan.RangeWindow{
		Input: matrixDeltaGuardShapedInput(
			&chplan.Scan{Table: "otel_metrics_sum"},
			chplan.Projection{Expr: &chplan.ColumnRef{Name: "TimeUnix"}, Alias: "TimeUnix"},
			chplan.Projection{Expr: &chplan.ColumnRef{Name: "Value"}, Alias: "Value"},
			chplan.Projection{Expr: &chplan.ColumnRef{Name: "AggregationTemporality"}, Alias: "AggregationTemporality"},
		),
		Func:              "rate",
		Range:             time.Minute,
		End:               end,
		Start:             start,
		Step:              step,
		OuterRange:        end.Sub(start),
		TimestampColumn:   "TimeUnix",
		ValueColumn:       "Value",
		TemporalityColumn: "AggregationTemporality",
		GroupBy:           []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		DeltaPrefixAggregateInput: matrixDeltaGuardShapedInput(
			&chplan.Scan{Table: "otel_metrics_sum_delta_prefix"},
			chplan.Projection{Expr: &chplan.ColumnRef{Name: "BucketStart"}, Alias: "BucketStart"},
			chplan.Projection{Expr: &chplan.ColumnRef{Name: "PartialSum"}, Alias: "PartialSum"},
		),
	}
	return r, end
}

func matrixDeltaGuardShapedInput(scan *chplan.Scan, extra ...chplan.Projection) *chplan.Project {
	projections := append([]chplan.Projection{
		{Expr: chplan.CanonicalAttributesExpr(&chplan.ColumnRef{Name: "Attributes"}), Alias: "Attributes"},
	}, extra...)
	return &chplan.Project{Input: scan, Projections: projections}
}

func matrixDeltaGuardEmit(t *testing.T, r *chplan.RangeWindow) string {
	t.Helper()
	ctx := WithDeltaPrefixReadEnabled(context.Background(), true)
	sqlText, _, err := Emit(ctx, r)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	return sqlText
}

// TestDeltaMatrixAggregateDailyFanout_GuardAppliedToAggregateTableScan kills
// range_window.go:deltaMatrixLevelSourceAggregateDailyFanout:`guard != nil`
// (CONDITIONALS_NEGATION): for this window,
// r.TemporalityColumn is always set, so deltaPresenceGuardFrag always
// returns a non-nil guard — flipping the check to `guard == nil` means
// aggDaily.Where(guard) is never called, silently dropping the guard from
// the aggregate-table (otel_metrics_sum_delta_prefix) day-bucket scan. That
// scan's own WHERE clause is bounded by `BucketStart` < ... on one side and
// `GROUP BY \`Attributes\`, \`BucketStart\“ on the other — the guard
// subquery must render somewhere inside that span.
func TestDeltaMatrixAggregateDailyFanout_GuardAppliedToAggregateTableScan(t *testing.T) {
	r, _ := matrixDeltaGuardTestWindow()
	sqlText := matrixDeltaGuardEmit(t, r)

	const startMarker = "`BucketStart` <"
	const endMarker = "GROUP BY `Attributes`, `BucketStart`"
	start := strings.Index(sqlText, startMarker)
	if start == -1 {
		t.Fatalf("aggregate-table day-bucket scan's own filter (%q) not found:\n%s", startMarker, sqlText)
	}
	end := strings.Index(sqlText[start:], endMarker)
	if end == -1 {
		t.Fatalf("aggregate-table day-bucket scan's own GROUP BY (%q) not found after its filter:\n%s", endMarker, sqlText)
	}
	segment := sqlText[start : start+end]
	if !strings.Contains(segment, "SELECT max(") {
		t.Errorf("aggregate-table day-bucket scan is missing its DELTA-presence guard "+
			"(no \"SELECT max(\" between %q and %q) — deltaMatrixLevelSourceAggregateDailyFanout's "+
			"`if guard != nil` gate is not adding it:\n%s", startMarker, endMarker, segment)
	}
}

// TestApplyMatrixFanoutScanBoundAggregate_OrGuardAppliedToFanout kills BOTH
// range_window.go:applyMatrixFanoutScanBoundAggregate:`err != nil`
// (CONDITIONALS_NEGATION — flipping it to `err == nil` makes the error-free
// case return early, before the guard clause is ever appended) and
// range_window.go:applyMatrixFanoutScanBoundAggregate:`guard != nil`
// (CONDITIONALS_NEGATION — flipping it to `guard == nil` skips the same append,
// since guard is always non-nil here). Either mutation removes the
// `OR \`AggregationTemporality\` != 1` disjunct this PR's HIGH-severity fix
// added to the raw sample fanout's own scan bound — see
// applyMatrixFanoutScanBoundAggregate's doc for why ANDing the guard directly
// onto the fanout (instead of OR'ing it against "this row isn't DELTA") used
// to empty the whole matrix for an all-CUMULATIVE counter.
func TestApplyMatrixFanoutScanBoundAggregate_OrGuardAppliedToFanout(t *testing.T) {
	r, _ := matrixDeltaGuardTestWindow()
	sqlText := matrixDeltaGuardEmit(t, r)

	const orGuardMarker = "OR `AggregationTemporality` != 1))"
	if !strings.Contains(sqlText, orGuardMarker) {
		t.Errorf("raw sample fanout is missing its OR-guard disjunct (%q not found) — either "+
			"applyMatrixFanoutScanBoundAggregate's `err != nil` early-return or its `guard != nil` "+
			"gate is discarding the clause before it reaches fanout.Where:\n%s", orGuardMarker, sqlText)
	}
}

// intervalNanosecondsAfter extracts the first n toIntervalNanosecond(...)
// integer literals rendered in sqlText starting at the first occurrence of
// marker, in source order. Used to pin arithmetic on Frags built from
// InlineLit(int64) — the rendered literal IS the value, so an
// ARITHMETIC_BASE / INVERT_NEGATIVES mutation that changes the computed
// value changes what's asserted here directly.
func intervalNanosecondsAfter(t *testing.T, sqlText, marker string, n int) []int64 {
	t.Helper()
	idx := strings.Index(sqlText, marker)
	if idx == -1 {
		t.Fatalf("marker %q not found in:\n%s", marker, sqlText)
	}
	const scanWindow = 300
	end := idx + scanWindow
	if end > len(sqlText) {
		end = len(sqlText)
	}
	segment := sqlText[idx:end]
	re := regexp.MustCompile(`toIntervalNanosecond\((\d+)\)`)
	matches := re.FindAllStringSubmatch(segment, n)
	if len(matches) < n {
		t.Fatalf("expected %d toIntervalNanosecond(...) literals after marker %q, found %d in segment:\n%s",
			n, marker, len(matches), segment)
	}
	out := make([]int64, n)
	for i := 0; i < n; i++ {
		v, err := strconv.ParseInt(matches[i][1], 10, 64)
		if err != nil {
			t.Fatalf("parse %q: %v", matches[i][1], err)
		}
		out[i] = v
	}
	return out
}

// TestMatrixWindowScanBoundsFrags_EarliestAnchorArithmetic kills
// range_window.go:matrixWindowScanBoundsFrags:`(numAnchors-1)*stepNS` under
// ARITHMETIC_BASE and INVERT_NEGATIVES on the `-` in `numAnchors-1`, and
// ARITHMETIC_BASE on the `*`. matrixWindowScanBoundsFrags computes
// earliestAnchor = end - (numAnchors-1)*stepNS, consumed as the guard
// subquery's own windowLo probe (`\`TimeUnix\` > <end literal> -
// toIntervalNanosecond((numAnchors-1)*stepNS) - toIntervalNanosecond
// (rangeNS) AND \`TimeUnix\` <= <end literal>` — anchored on
// "`TimeUnix` > toDateTime64(" specifically (not just "`TimeUnix` > "),
// since the regroup stage's own window_pairs construction ALSO uses a
// strict `TimeUnix` > comparison, but against `anchor_ts`, not the end
// literal, and renders earlier in the text). With numAnchors=3, stepNS=10s:
// the correct value is (3-1)*10_000_000_000 = 20_000_000_000, distinct
// from every mutated outcome (40_000_000_000 for either `-`->`+` or the
// negation-of-1 shape, 0 for `*`->`/` since 2/10_000_000_000 truncates to
// 0).
func TestMatrixWindowScanBoundsFrags_EarliestAnchorArithmetic(t *testing.T) {
	r, _ := matrixDeltaGuardTestWindow()
	sqlText := matrixDeltaGuardEmit(t, r)

	const windowLoMarker = "`TimeUnix` > toDateTime64("
	const wantEarliestAnchorTerm = 20_000_000_000 // (numAnchors-1)*stepNS = (3-1)*10s
	const wantRangeTerm = 60_000_000_000          // rangeNS = 1 minute

	got := intervalNanosecondsAfter(t, sqlText, windowLoMarker, 2)
	if got[0] != wantEarliestAnchorTerm {
		t.Errorf("guard windowLo's first toIntervalNanosecond(...) = %d, want %d "+
			"((numAnchors-1)*stepNS with numAnchors=3, stepNS=10s) — matrixWindowScanBoundsFrags' "+
			"earliestAnchor arithmetic is wrong:\n%s", got[0], wantEarliestAnchorTerm, sqlText)
	}
	if got[1] != wantRangeTerm {
		t.Errorf("guard windowLo's second toIntervalNanosecond(...) = %d, want %d (rangeNS = 1 minute):\n%s",
			got[1], wantRangeTerm, sqlText)
	}
}

// TestApplyMatrixFanoutScanBoundAggregate_EarliestDayArithmetic kills
// range_window.go:applyMatrixFanoutScanBoundAggregate:`(numAnchors-1)*stepNS`
// under ARITHMETIC_BASE and INVERT_NEGATIVES on the `-` in `numAnchors-1`, and
// ARITHMETIC_BASE on the `*` —
// applyMatrixFanoutScanBoundAggregate's OWN local earliestAnchor
// (identical source shape to matrixWindowScanBoundsFrags', but a distinct
// call site / distinct mutant), which derives the raw fanout's own
// `\`TimeUnix\` >= toStartOfDay(...)` day-granularity scan bound —
// textually distinguishable from the guard's own windowLo probe (that one
// reads `> ... AND <=`; this one reads `>= toStartOfDay(...)`, the ONLY
// such shape in the emitted matrix query).
func TestApplyMatrixFanoutScanBoundAggregate_EarliestDayArithmetic(t *testing.T) {
	r, _ := matrixDeltaGuardTestWindow()
	sqlText := matrixDeltaGuardEmit(t, r)

	const earliestDayMarker = "`TimeUnix` >= toStartOfDay("
	const wantEarliestAnchorTerm = 20_000_000_000 // (numAnchors-1)*stepNS = (3-1)*10s
	const wantRangeTerm = 60_000_000_000          // rangeNS = 1 minute

	got := intervalNanosecondsAfter(t, sqlText, earliestDayMarker, 2)
	if got[0] != wantEarliestAnchorTerm {
		t.Errorf("fanout's earliestDay first toIntervalNanosecond(...) = %d, want %d "+
			"((numAnchors-1)*stepNS with numAnchors=3, stepNS=10s) — applyMatrixFanoutScanBoundAggregate's "+
			"own earliestAnchor arithmetic is wrong:\n%s", got[0], wantEarliestAnchorTerm, sqlText)
	}
	if got[1] != wantRangeTerm {
		t.Errorf("fanout's earliestDay second toIntervalNanosecond(...) = %d, want %d (rangeNS = 1 minute):\n%s",
			got[1], wantRangeTerm, sqlText)
	}
}

// The four tests below are white-box unit tests directly against
// deltaMatrixLevelSourceAggregateDailyFanout / deltaMatrixLevelSourceAggregate,
// bypassing PromQL lowering entirely: a real query's RangeWindow.GroupBy is
// always `[ColumnRef(AttributesColumn)]` (see chplan.RangeWindow.GroupBy's
// own doc and internal/promql/lower.go's rangeFn lowering), so groupFrags
// is never actually empty on the production path — the
// `make([]Frag, 0, len(groupFrags)+1)` / `len(groupColumns)+1` capacity
// hints at 4070/4100/4133/4180 are therefore unobservable through any
// emitted-SQL assertion: a wrong non-negative capacity produces the exact
// same final slice contents (append reallocates transparently), so the
// mutation is behaviourally equivalent for every real query shape. It is
// only observable when the capacity computation goes NEGATIVE — Go's
// runtime panics on `make([]T, 0, -1)` — which requires an empty base
// slice these functions never see end-to-end. Calling them directly with
// groupFrags=nil (equivalently r.GroupBy=nil for groupColumns) is the only
// way to exercise — and kill — those four ARITHMETIC_BASE mutants.
func TestDeltaMatrixLevelSourceAggregateDailyFanout_EmptyGroupFragsDoesNotPanic(t *testing.T) {
	e := &emitter{}
	r := &chplan.RangeWindow{
		DeltaPrefixAggregateInput: &chplan.Scan{Table: "agg_table"},
	}
	end := Col("end_ts")
	// numAnchors=3, stepNS/rangeNS arbitrary but nonzero — only groupFrags'
	// emptiness matters for the capacity hints under test (4070, 4100).
	qb, keys, err := e.deltaMatrixLevelSourceAggregateDailyFanout(r, nil, end, 10_000_000_000, 60_000_000_000, 3)
	if err != nil {
		t.Fatalf("deltaMatrixLevelSourceAggregateDailyFanout with empty groupFrags: %v", err)
	}
	if qb == nil {
		t.Fatal("deltaMatrixLevelSourceAggregateDailyFanout returned a nil QueryBuilder")
	}
	if len(keys) != 0 {
		t.Fatalf("aggDailyKeys = %v, want empty for empty groupFrags", keys)
	}
}

func TestDeltaMatrixLevelSourceAggregate_EmptyGroupFragsDoesNotPanic(t *testing.T) {
	e := &emitter{}
	r := &chplan.RangeWindow{
		DeltaPrefixAggregateInput: &chplan.Scan{Table: "agg_table"},
		// GroupBy left nil: plainGroupColumnNames(nil) returns an empty,
		// non-error column slice, matching the empty groupFrags param below.
	}
	end := Col("end_ts")
	regroupSource := verbatim("regroup_src")
	// numAnchors=3, stepNS/rangeNS arbitrary but nonzero — only
	// groupFrags'/groupColumns' emptiness matters for the capacity hints
	// under test (4133, 4180).
	frag, err := e.deltaMatrixLevelSourceAggregate(r, regroupSource, nil, []string{"window_pairs"}, false, end, 10_000_000_000, 60_000_000_000, 3)
	if err != nil {
		t.Fatalf("deltaMatrixLevelSourceAggregate with empty groupFrags/GroupBy: %v", err)
	}
	if frag == nil {
		t.Fatal("deltaMatrixLevelSourceAggregate returned a nil Frag")
	}
}
