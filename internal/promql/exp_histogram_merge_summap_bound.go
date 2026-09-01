package promql

import (
	"github.com/tsouza/cerberus/internal/chplan"
)

// exp_histogram_merge_summap_bound.go closes cerberus issue #2834 (item 3
// of #2757's own "suggested split"): a genuinely ROWS-INDEPENDENT budget
// guard for [expHistogramGroupMergeSumMap] (exp_histogram_merge_summap.go),
// replacing that path's reuse of [histogramMergeBudgetGuardExpr] — the
// `rows x (posWidth^2 + negWidth^2)` guard histogram_merge_bound.go
// calibrated for the OLD groupArray + picker fold. That reuse was provably
// SAFE (posWidth^2 + negWidth^2 <= rows x (posWidth^2 + negWidth^2) for any
// rows >= 1) but conservative: issue #2490's own 3,741-series repro at
// realistic OTel-default width (160) is guard-rejected under the OLD
// formula at cost 3741 x 160^2 = 95,769,600 (over the 60,000,000 default)
// despite needing only ~42 MiB of real memory under the sumMap design (see
// the calibration below) — the `rows x` multiplier no longer tracks this
// design's real dominant cost term.
//
// Every OTHER histogram-merge code path (the classic groupArray + picker
// fold, the binop merge) keeps [histogramMergeBudgetGuardExpr] unchanged —
// this file adds a second guard for the sumMap path alone, threaded through
// the SAME operator-tunable ceiling (see "Operator override" below), never
// a second knob.
//
// # Why "rows-independent" is not "rows-ignorable"
//
// exp_histogram_merge_summap.go's own header doc already states the real
// cost model: a `rows x width` linear pass (sumMap's own keyed aggregation)
// plus a `width^2` reconstruction term that does not shrink as rows grow.
// The calibration below CONFIRMS both terms are real — at fixed width, real
// memory keeps growing with rows (it is not flat) — so this guard does not
// drop the rows axis by pretending the linear term is zero. Instead it
// keeps the EXISTING [maxHistogramMergeRowCountOverflowGuard] (4096) as an
// unconditional row-count backstop — no longer only a pure Int64-overflow
// margin (its role for the OLD formula, per histogram_merge_bound.go's own
// doc) but now ALSO the real safety bound for this design's rows-linear
// term, calibrated below at exactly that ceiling — and drops the
// MULTIPLICATION of that rows count into the width-driven cost term, which
// is what actually restores rows-independence for the axis the design's own
// dominant, width-only reconstruction term needs: a group that clears the
// row-count backstop is bounded on the SAME `posWidth^2 + negWidth^2` shape
// as the old guard, just without the multiply that made ordinary,
// realistic row counts (hundreds to thousands, at realistic OTel-default
// width) pay for a cost term the design does not actually have.
//
// # Calibration (real ClickHouse 26.6, `sum(<native-histogram selector>)`
// # lowered through [NativeExpHistogramMergeLowerer] against the ACTUAL
// # emitted SQL, `SYSTEM FLUSH LOGS` + `system.query_log.memory_usage`,
// # matching histogram_merge_bound.go's own methodology and never chDB)
//
// A throwaway harness (deleted before this fix merged, same precedent
// histogram_merge_bound.go's own calibration cites) seeded N single-row
// series sharing Scale 0 and an identical PositiveBucketCounts width, with
// NegativeBucketCounts empty (the common case — most latency histograms
// carry no negative buckets, matching this design's own header doc), and
// measured real peak memory:
//
//	rows   width   peak memory (real server, max_memory_usage=30GiB)
//	   1     160       4.2 MiB
//	   1     640      10.1 MiB
//	   1    1280      43.0 MiB
//	   1    2560     166.5 MiB
//	   1    5120     654.3 MiB
//	   1   10240    2604.7 MiB  (confirms this design's own header doc: a
//	                             single wide-layout series costs MORE than
//	                             the old fold's 1,828 MiB at this width)
//	  10     160       4.2 MiB
//	  40     160       4.2 MiB
//	 100     160       4.7 MiB
//	 500     160       9.1 MiB
//	1000     160      14.2 MiB
//	2000     160      23.5 MiB
//	3741     160      42.5 MiB  (issue #2490's own repro — matches the
//	                             issue's own "~42 MiB" measurement)
//	4096     160      47.1 MiB
//	4096    1280     299.2 MiB
//	4096    2560     582.3 MiB
//	4096    3200     721.3 MiB
//	4096    3840     861.4 MiB
//	4096    3873     867.8 MiB  (the width ceiling this file picks, pinned
//	                             exactly rather than interpolated)
//	4096    4480     999.8 MiB
//	   1    3873     376.0 MiB
//	2000    3873     440.7 MiB
//
// Two things fall out of this table:
//
//  1. The width-1-row sweep is clean, close to quadratic (166.5 -> 654.3 is
//     3.93x for a 2x width increase, converging on 4x as fixed per-query
//     overhead becomes negligible — the same quadratic signature
//     histogram_merge_bound.go's own OLD-algorithm width sweep shows), but
//     at a HIGHER real bytes-per-unit rate than the old fold: fitting
//     `width^2 x bytes/unit` against the highest-magnitude point (rows=1,
//     width=10240) gives ~26 bytes/unit, versus the OLD guard's calibrated
//     ~15.5 bytes/unit — this design's two-pass reshape (raw groupArrays
//     retained for THIS guard's own read, plus the sumMap keyed pass and
//     its indexOf-based reconstruction) costs more per width^2 unit than
//     the old picker fold, not less; sumMap's savings come from dropping
//     the `rows x` multiplier entirely, not from a cheaper per-unit rate.
//  2. Real memory at FIXED width keeps rising with rows (4096 rows costs
//     18-20x more than 1 row at the same width — 47.1 MiB vs 4.2 MiB at
//     width 160; 999.8-ish trend vs 376.0 MiB at width 3873) — the real,
//     measured rows-linear term this guard's row-count backstop exists to
//     bound, confirming rows-independence is a property of the width-only
//     reconstruction term, not of this design's total real cost.
//
// # The width ceiling
//
// The worst REAL case a guard admitting `posWidth^2 + negWidth^2 <=
// (maxCostUnits / sumMapMergeCostMultiplier)` has to answer for is the
// row-count backstop's OWN ceiling (4096 rows) at the widest width the cost
// formula still admits — the two checks are independent ORs, so a query can
// hit both simultaneously. The rows=4096 sweep above crosses the
// 1 GiB CERBERUS_CH_QUERY_MAX_MEMORY default between width 3840
// (861.4 MiB) and width 4480 (999.8 MiB); width 3873 — pinned exactly
// above, not interpolated — measures 867.8 MiB, comfortably under the cap
// with headroom for the second ladder, concurrent load, and the Count/Sum/
// ZeroCount groupArray folds this guard does not itself cost-model (the
// same margin rationale [maxHistogramMergeCostUnits]'s own doc gives).
// sumMapMergeCostMultiplier is chosen so this SAME width ceiling falls out
// of the SHARED maxCostUnits default (60,000,000, unchanged) without
// introducing a second operator knob — see that constant's own doc.
const (
	// sumMapMergeCostMultiplier converts [maxHistogramMergeCostUnits]'s
	// existing 60,000,000-unit default into this design's OWN, higher,
	// calibrated bytes/unit rate (~26 bytes/unit measured above, versus the
	// OLD guard's ~15.5) for the width-only cost term, so
	// `maxCostUnits / sumMapMergeCostMultiplier` lands on the real-memory-
	// safe width ceiling this file's calibration found (~3873, sqrt(60e6/4)
	// = 3872.98) WITHOUT introducing a second, independently-tuned cost
	// ceiling constant: raising or lowering the SAME
	// CERBERUS_PROMQL_HISTOGRAM_MERGE_MAX_COST_UNITS override (cerberus
	// issue #2667) scales this guard's admitted width proportionally to the
	// OLD guard's admitted rows x width^2 product, keeping the one knob's
	// semantics consistent across both cost models. 4 is not a physically
	// exact unit-rate ratio (26 / 15.5 is closer to 1.68) — it is the
	// smallest whole multiplier whose resulting width ceiling (a real,
	// calibration-pinned data point above, not an interpolation) still
	// clears the 1 GiB target with the SAME margin discipline
	// [maxHistogramMergeCostUnits]'s own doc uses, while staying a small,
	// legible integer rather than a decimal tuned to one measurement run.
	sumMapMergeCostMultiplier = 4
)

// expHistogramMergeSumMapCostOverBudgetExpr renders the `<rowCount overflow
// guard> OR <cost> > maxCostUnits` condition for the sumMap merge path:
// [sumMapMergeCostMultiplier] x (posWidth^2 + negWidth^2) — this design's
// real, calibrated cost driver (this file's header doc) — WITHOUT the `rows
// x` multiplier [histogramMergeCostOverBudgetExpr] applies for the OLD
// fold's genuinely rows-dependent cost. The leading
// `rowCount > maxHistogramMergeRowCountOverflowGuard` disjunct is reused,
// unchanged, from histogram_merge_bound.go: it is still a pure Int64-
// overflow backstop for THIS multiply, and — per this file's header doc —
// it is now ALSO the real bound on this design's own measured rows-linear
// term, calibrated at exactly that row count above.
//
// rowCount and maxCostUnits are threaded exactly as
// [histogramMergeCostOverBudgetExpr]'s own doc describes: rowCount is the
// caller's `length(scalesArr)` read off the SAME [expHistogramMergeAggs]
// groupArray columns [expHistogramGroupMergeAggsSumMap] still collects (see
// exp_histogram_merge_summap.go's header for why it still needs them), and
// maxCostUnits is [ResourceBounds.HistogramMergeMaxCostUnits] — the SAME
// operator-tunable ceiling the OLD guard reads, never a second knob.
func expHistogramMergeSumMapCostOverBudgetExpr(rowCount chplan.Expr, maxCostUnits int64) chplan.Expr {
	scalesArr := &chplan.ColumnRef{Name: hqAggScalesArrayAlias}
	mergedScale := &chplan.ColumnRef{Name: hqAggMergedScaleAlias}
	posOff := &chplan.ColumnRef{Name: hqAggPosOffsetsArrayAlias}
	posBuc := &chplan.ColumnRef{Name: hqAggPosBucketsArrayAlias}
	negOff := &chplan.ColumnRef{Name: hqAggNegOffsetsArrayAlias}
	negBuc := &chplan.ColumnRef{Name: hqAggNegBucketsArrayAlias}

	cost := mulExpr(
		&chplan.LitInt{V: sumMapMergeCostMultiplier},
		addExpr(
			clampedLadderWidthSquaredExpr(scalesArr, posOff, posBuc, mergedScale),
			clampedLadderWidthSquaredExpr(scalesArr, negOff, negBuc, mergedScale),
		),
	)
	return orExpr(
		gtLit(rowCount, maxHistogramMergeRowCountOverflowGuard),
		gtLit(cost, maxCostUnits),
	)
}

// expHistogramMergeSumMapBudgetGuardExpr renders the throwIf(...) = 0
// predicate that bounds [expHistogramGroupMergeSumMap] — the sumMap-path
// mirror of [histogramMergeBudgetGuardExpr], reading the SAME groupArray
// aliases (see that function's own doc) but through
// [expHistogramMergeSumMapCostOverBudgetExpr]'s rows-independent cost
// model instead.
func expHistogramMergeSumMapBudgetGuardExpr(maxCostUnits int64) chplan.Expr {
	scalesArr := &chplan.ColumnRef{Name: hqAggScalesArrayAlias}
	seriesCount := &chplan.FuncCall{Fn: chplan.FnLength, Args: []chplan.Expr{scalesArr}}

	return &chplan.Binary{
		Op: chplan.OpEq,
		Left: &chplan.FuncCall{
			Fn: chplan.FnThrowIf,
			Args: []chplan.Expr{
				expHistogramMergeSumMapCostOverBudgetExpr(seriesCount, maxCostUnits),
				&chplan.InlineString{V: chplan.HistogramMergeBudgetMessage},
			},
		},
		Right: &chplan.LitInt{V: 0},
	}
}

// wrapExpHistogramMergeSumMapBudgetGuard wraps merged — the raw sumMap
// merge [chplan.Aggregate] built by [expHistogramGroupMergeSumMap] — in
// this file's own budget guard, the sumMap-path counterpart of
// [wrapExpHistogramMergeBudgetGuard].
func wrapExpHistogramMergeSumMapBudgetGuard(merged chplan.Node, maxCostUnits int64) chplan.Node {
	return &chplan.Filter{Input: merged, Predicate: expHistogramMergeSumMapBudgetGuardExpr(maxCostUnits)}
}
