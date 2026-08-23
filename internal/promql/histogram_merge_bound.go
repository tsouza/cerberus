package promql

import (
	"github.com/tsouza/cerberus/internal/chplan"
)

// histogram_merge_bound.go closes issue #2385, recalibrated for issue #2490.
//
// The across-SERIES exponential-histogram merge (expHistogramMergeBucketsExpr)
// renders an arrayMap over the MERGED bucket range, and for every one of
// those target buckets it re-scans EVERY contributing row's own bucket
// array in full (expHistogramBucketPositionPickerExpr's
// `arrayMap(j -> ..., arrayEnumerate(arr))`) to pick out the one element
// that lands there, then Kahan-folds the result
// (expHistogramBucketRowContribExpr). That nested shape is NOT the
// `(width) x (series)` product #2385 assumed: real ClickHouse 25.9-alpine
// measurements (below) show peak memory tracking
//
//	rows-in-group x (merged bucket-range width)^3
//
// to within ~5% from width 40 through 1280 and rows 1 through 1000 — the
// per-target-bucket O(row-width) rescan, run once per target bucket, turns
// what looks like a width x series pass into one that is CUBIC in width
// alone once a row's own bucket count tracks the merged width (the common
// case: identical-layout series, or even a SINGLE row with no merge to do
// at all — the `rows=1` measurements below still pay this cost merely to
// reshape one histogram's own array).
//
// #2385's guard checked series-per-group and bucket-width as two
// INDEPENDENT axes, each generous on its own (4096 series, 16384 width).
// Issue #2490's real-world repro — 3,741 series, both with and without a
// rate() wrapper — sits comfortably under BOTH those independent caps and
// still exceeds a 1 GiB cap on real ClickHouse 25.9-alpine, because neither
// axis alone was ever the real cost driver: their PRODUCT, at the true
// cubic-in-width exponent, is. A single axis miscalibration this far off
// cannot be patched by nudging its number — the axis being measured was
// wrong, not just its threshold.
//
// Both factors are knowable only once ClickHouse has read the actual rows —
// the Go-side plan-time gates (requireSubquerySampleBudget's family, in
// internal/engine) work only for STATICALLY derivable quantities like a
// grid's anchor count, and rows-per-group / merged-bucket-width are
// neither. So the bound has to be an emitted, ClickHouse-evaluated check:
// the same `throwIf(<cond>, <msg>) = 0` idiom duplicate_labelset_guard.go
// and info_fn.go already use for a plan-shape invariant only ClickHouse
// itself can verify.
//
// It is wired as a [chplan.Filter]'s WHERE predicate wrapping the merge
// Aggregate BEFORE [expHistogramMergeSortStage] resorts its groupArrays —
// see wrapExpHistogramMergeBudgetGuard — rather than as that Aggregate's
// Having, for two reasons:
//
//   - The merge Aggregate's GroupBy is EMPTY whenever the query carries no
//     `by`/`without` clause (`sum by()` collapses to one group — see
//     histogramAggGroupBy), and an empty-GroupBy Aggregate with
//     DropEmptyOnNoGroup renders as the two-layer `WHERE _cerb_n > 0` shape
//     that cannot ALSO carry a HAVING (see chplan.Aggregate.Having's doc and
//     emitAggregate's explicit ErrUnsupported). A Filter wrapping the
//     Aggregate's output composes identically with both the grouped and the
//     collapsed-to-one-group shapes.
//   - Placed BEFORE the sort stage, a group this guard rejects never pays
//     for expHistogramMergeSortStage's three arraySort calls either — no
//     reason to sort a group about to be refused.
//
// It is exactly as immune to ClickHouse's column-pruning analyzer as a
// HAVING clause would be: it reads real output columns
// (expHistogramMergeProjections / hqQuantileRankScalarMergeProjections
// already read every one of them back downstream), so none of them is ever
// dead code the analyzer could drop.
//
// Rejecting — not truncating the merge to the cap — matches the rest of the
// resource-bound family (requireCompareGridBudget's doc makes the same
// call): a silently partial histogram-quantile answer would be worse than an
// honest refusal, and every peer bound already rejects rather than
// truncates.
//
// # Calibration (real ClickHouse 25.9-alpine, 1 GiB cap — the
// # CERBERUS_CH_QUERY_MAX_MEMORY default)
//
// A throwaway harness (deleted before this fix merged, per
// rate_window_fanout_bound.go's own precedent) lowered + emitted the real
// `histogram_quantile(0.95, sum by(route)(<metric>))` SQL and ran it
// against a real `clickhouse/clickhouse-server:25.9-alpine` container
// (chDB was deliberately NOT used for this measurement — its in-process
// engine does not reliably surface a real `max_memory_usage` abort the way
// a real server does) with `SETTINGS max_memory_usage` set to the 1 GiB
// default, seeding series with an IDENTICAL bucket layout (Scale/Offset)
// across every series — ruling out "expensive only from cross-series
// layout re-alignment" exactly as issue #2490's own repro did:
//
//	rows   width   peak memory (real server, generous cap)
//	   10      40    11.3 MiB
//	   10      80    46.3 MiB
//	   10     160   331.7 MiB
//	   10     320  2561.8 MiB  (already over a 1 GiB cap)
//	   20     160   658.3 MiB
//	   40     160  1312.2 MiB  (over cap)
//	    1     640  2029.4 MiB  (over cap — a SINGLE row, no cross-series
//	                            merge to do at all)
//	    2     640  4054.5 MiB  (over cap)
//	    1    1280 16103.7 MiB  (over cap)
//
// Every one of those fits `rows x width^3 x 8.5 bytes` to within ~5%
// (verified against the 500/1000-row, width-160 points too, where a 1 GiB
// cap aborts with ClickHouse's own "would use N GiB" estimate: 100 rows ->
// 3.15 GiB, 500 rows -> 15.70 GiB, 1000 rows -> 31.40 GiB, all consistent
// with the same per-unit rate). At that rate a single well-formed OTel SDK
// default histogram (~160 buckets) already reaches real risk at a few TENS
// of contributing rows, and #2385's own 4096/16384 numbers — which this
// file used to check independently — permit configurations up to roughly
// 4 orders of magnitude past where real memory pressure starts. See the
// issue for the fuller sweep and the note on why this could not be fixed by
// re-picking two independent numbers.
//
// This does NOT make the underlying algorithm safe at issue #2490's own
// production scale (3,741 series with realistic per-series bucket counts):
// a bound calibrated to the REAL cubic cost has to reject exactly the
// cardinalities native histograms exist to support, which no threshold
// picked on this axis can fix. That is an algorithmic problem in
// expHistogramMergeBucketsExpr's per-target-bucket rescan (a scatter by
// each row's own O(row-width) elements, computing each one's target index
// directly, would be linear in total data instead of cubic in width) —
// tracked in issue #2500; this file's job is only to turn the silent OOM
// into an honest, cheap rejection before ClickHouse allocates for it.

const (
	// maxHistogramMergeCostUnits bounds `rows x (posWidth^3 + negWidth^3)`
	// — the real cost driver the calibration above measured, not the two
	// independent axes #2385 originally checked. 70,000,000 x ~8.5
	// bytes/unit (the measured rate) is ~595 MiB, comfortably under the 1
	// GiB CERBERUS_CH_QUERY_MAX_MEMORY default with margin for the second
	// ladder, the Count/Sum/ZeroCount groupArray folds this guard does not
	// itself cost-model, and concurrent load — while still admitting
	// ordinary small aggregations (single-digit-to-low-tens of rows at a
	// realistic ~160-bucket width). Recalibrate by binary search against a
	// real ClickHouse server (NOT chDB — see the file doc comment) if this
	// drifts; preserve the rows x width^3 model unless a new measurement
	// shows the exponent itself has moved.
	maxHistogramMergeCostUnits = 70_000_000

	// maxHistogramMergeClampedWidth is NOT a behavioral threshold — the
	// cost formula above already rejects any width past a few hundred (see
	// the calibration table) long before this number matters. It exists
	// purely so cubing a data-determined, otherwise-unbounded width
	// (two series' Offset can diverge by up to the full Int32 range) can
	// never overflow the Int64 arithmetic the cost check runs in:
	// 100,000^3 = 10^15, three orders of magnitude below Int64's ~9.2x10^18
	// ceiling, while still being far larger than the cost formula's own
	// effective width ceiling (~412 at rows=1), so this clamp never
	// changes which queries the guard accepts — only which ones stay
	// overflow-safe while being rejected.
	maxHistogramMergeClampedWidth = 100_000

	// maxHistogramMergeRowCountOverflowGuard is NOT a resurrected copy of
	// #2385's independent series-per-group cap (see this file's header
	// doc for why checking that axis independently was wrong) — it is a
	// pure overflow backstop for histogramMergeCostOverBudgetExpr's
	// `rowCount x (posWidth^3 + negWidth^3)` multiply. Both cubed-width
	// terms clamp to maxHistogramMergeClampedWidth, so their sum tops out
	// at 2 x 100,000^3 = 2x10^15; Int64 wraps past ~9.2x10^18, so any
	// rowCount at or beyond ~4,611 risks the multiply silently wrapping
	// and making the real cost check evaluate false — defeating the
	// guard it exists to enforce. 4096 keeps 4096 x 2x10^15 (~8.2x10^18)
	// under that ceiling with margin, while still comfortably covering
	// every real workload the calibration above measured (issue #2490's
	// own repro is 3,741 rows): past this cap the guard rejects
	// unconditionally, via [orExpr] in
	// [histogramMergeCostOverBudgetExpr], without needing the by-then
	// overflow-risking multiply to prove it — the same role
	// maxHistogramMergeClampedWidth already plays for the width term.
	// Recalibrate only if maxHistogramMergeClampedWidth's own value
	// changes; this is not an independent behavioral tuning knob.
	maxHistogramMergeRowCountOverflowGuard = 4096
)

// mergedLengthExpr returns a FULLY BOUND rendering of one signed ladder's
// merged bucket-range length, reusing expHistogramOverMergedBucketRangeExpr
// — the SAME hqLet-bound helper the real merge site
// (expHistogramMergeBucketsExpr) reuses — rather than calling
// expHistogramMergeBucketsBoundsExpr directly. Calling the bounds helper
// raw would render its own, UNBOUND copy of the `arrayMin(arrayMap(...))`
// merged-start subtree — exactly the cerberus issue #2267 hazard
// expHistogramOverMergedBucketRangeExpr's own doc describes, and exactly
// what TestExpHistogramMergedStartIsBoundOncePerSite pins against: every
// render of that subtree must be one of that helper's own hqLet bindings.
// Going through it here keeps the guard from becoming a second, unbound
// occurrence of the very hazard the merge site was already fixed for.
//
// Reused by [histogramMergeCostOverBudgetExpr] below and, through it, by
// [histogramBinopBucketWidthBudgetGuardExpr] (histogram_native_binop.go,
// cerberus issue #2428): the two-operand binop merge
// (histogramBinopMergedBucketsExpr) renders the identical unbounded
// arrayMap-over-mergedLength shape this file's own cost guard bounds for
// the cross-series merge, so the same fully-bound length rendering closes
// that narrower instance of the same bug class without a second, divergent
// implementation.
func mergedLengthExpr(scalesArr, offArr, bucArr, mergedScale chplan.Expr) chplan.Expr {
	return expHistogramOverMergedBucketRangeExpr(
		scalesArr, offArr, bucArr, mergedScale,
		func(_, mergedLength chplan.Expr) chplan.Expr { return mergedLength },
	)
}

// clampedLadderWidthCubedExpr renders one signed ladder's merged width,
// clamped to [maxHistogramMergeClampedWidth] (see that constant's doc for
// why the clamp itself is never the behavioral bound), cubed via plain
// integer multiplication — never `pow`, which returns Float64 and loses
// exact precision on values this large. mergedLengthExpr is called exactly
// ONCE per ladder and the resulting Frag reused three times below, so
// cubing only triples the cheap O(rows) `greatest`/comparison wrapper
// around it, not the width computation itself (see mergedLengthExpr's own
// doc on why a second CALL — as opposed to a second READ of the same Frag
// — would be the #2267 hazard again).
func clampedLadderWidthCubedExpr(scalesArr, offArr, bucArr, mergedScale chplan.Expr) chplan.Expr {
	clamped := &chplan.FuncCall{
		Fn:   chplan.FnLeast,
		Args: []chplan.Expr{mergedLengthExpr(scalesArr, offArr, bucArr, mergedScale), &chplan.LitInt{V: maxHistogramMergeClampedWidth}},
	}
	return mulExpr(mulExpr(clamped, clamped), clamped)
}

// histogramMergeCostOverBudgetExpr renders the `<rowCount overflow guard> OR
// <cost> > maxHistogramMergeCostUnits` condition the calibration in this
// file's header doc backs: rowCount x (posWidth^3 + negWidth^3), the real
// measured cost driver, rather than the two independent axes issue #2385
// originally checked (see that doc for why the axes themselves — not just
// their thresholds — were wrong).
//
// The leading `rowCount > maxHistogramMergeRowCountOverflowGuard` disjunct
// is not a second business threshold — it exists purely so the multiply
// below can never wrap Int64 and silently defeat the real cost check (see
// that constant's doc). Because this is an OR of two independently
// evaluated booleans, ClickHouse computing a wrapped, meaningless `cost`
// for a rowCount past that guard cannot flip the overall predicate to
// false: the first disjunct alone already forces rejection at that point.
//
// rowCount is the caller's own row-count expression: `length(scalesArr)`
// for the N-series cross-series merge
// ([histogramMergeBudgetGuardExpr]), or the literal `2`
// for the binop two-operand merge
// ([histogramBinopBucketWidthBudgetGuardExpr], histogram_native_binop.go),
// whose own `count() = 2` conjunct already fixes it there (well under the
// overflow guard, so that disjunct is always false for the binop path).
// Reading the SAME groupArray aliases expHistogramMergeAggs collects
// (hqAggScalesArrayAlias and the four offset/bucket array aliases) is what
// lets both callers share this one cost model instead of each re-deriving
// it.
func histogramMergeCostOverBudgetExpr(rowCount chplan.Expr) chplan.Expr {
	scalesArr := &chplan.ColumnRef{Name: hqAggScalesArrayAlias}
	mergedScale := &chplan.ColumnRef{Name: hqAggMergedScaleAlias}
	posOff := &chplan.ColumnRef{Name: hqAggPosOffsetsArrayAlias}
	posBuc := &chplan.ColumnRef{Name: hqAggPosBucketsArrayAlias}
	negOff := &chplan.ColumnRef{Name: hqAggNegOffsetsArrayAlias}
	negBuc := &chplan.ColumnRef{Name: hqAggNegBucketsArrayAlias}

	cost := mulExpr(
		rowCount,
		addExpr(
			clampedLadderWidthCubedExpr(scalesArr, posOff, posBuc, mergedScale),
			clampedLadderWidthCubedExpr(scalesArr, negOff, negBuc, mergedScale),
		),
	)
	return orExpr(
		gtLit(rowCount, maxHistogramMergeRowCountOverflowGuard),
		gtLit(cost, maxHistogramMergeCostUnits),
	)
}

// histogramMergeBudgetGuardExpr renders the throwIf(...) = 0 predicate that
// bounds the shared across-series exponential-histogram merge. It reads the
// SAME groupArray aliases expHistogramMergeAggs collects
// (hqAggScalesArrayAlias and the four offset/bucket array aliases), so it
// must be attached directly above the Aggregate that produces them — see
// wrapExpHistogramMergeBudgetGuard.
//
// seriesCount — length(scalesArr) — is cheap: O(rows-per-group), reading
// the length of a groupArray this guard already has in scope. The
// expensive part it gates is expHistogramMergeBucketsExpr's per-target-
// bucket rescan, never reached for a group this guard rejects.
func histogramMergeBudgetGuardExpr() chplan.Expr {
	scalesArr := &chplan.ColumnRef{Name: hqAggScalesArrayAlias}
	seriesCount := &chplan.FuncCall{Fn: chplan.FnLength, Args: []chplan.Expr{scalesArr}}

	return &chplan.Binary{
		Op: chplan.OpEq,
		Left: &chplan.FuncCall{
			Fn: chplan.FnThrowIf,
			Args: []chplan.Expr{
				histogramMergeCostOverBudgetExpr(seriesCount),
				&chplan.InlineString{V: chplan.HistogramMergeBudgetMessage},
			},
		},
		Right: &chplan.LitInt{V: 0},
	}
}

// wrapExpHistogramMergeBudgetGuard wraps merged — the raw across-series
// exponential-histogram merge [chplan.Aggregate] built from
// [expHistogramMergeAggs] plus [expHistogramMergeSeriesOrderKeyAgg] — in the
// budget guard's Filter. [expHistogramMergeSortStage] calls this on every
// Aggregate it receives BEFORE resorting its groupArrays, and all four of
// that function's callers (histogram_quantile.go, histogram_quantile_range.go,
// and the two in histogram_native_sum.go — instant quantile, range quantile,
// and the histogram-VALUED sum() publishers) already route through it, so
// the guard is wired into every across-series merge shape from this one
// choke point rather than needing to be added at each call site.
func wrapExpHistogramMergeBudgetGuard(merged chplan.Node) chplan.Node {
	return &chplan.Filter{Input: merged, Predicate: histogramMergeBudgetGuardExpr()}
}

// orExpr returns `a OR b`. Mirrors andExpr, this package's existing
// conjunction helper.
func orExpr(a, b chplan.Expr) chplan.Expr {
	return &chplan.Binary{Op: chplan.OpOr, Left: a, Right: b}
}

// gtLit returns `expr > n`.
func gtLit(expr chplan.Expr, n int) chplan.Expr {
	return &chplan.Binary{Op: chplan.OpGt, Left: expr, Right: &chplan.LitInt{V: int64(n)}}
}
