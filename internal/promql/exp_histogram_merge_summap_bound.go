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
//
// # avg() confirmation (cerberus issue #2866)
//
// This guard's cost formula reads only the groupArray columns
// [expHistogramGroupMergeAggsSumMap] collects — unaffected by whether the
// caller is sum() or avg(), since avg()'s own extra work
// (expHistogramGroupSeriesCountAgg's plain count() aggregate, plus
// expHistogramAvgScaleProjections' division of the ALREADY-reconstructed
// dense arrays) touches neither. A real ClickHouse 26.6 measurement (same
// methodology as this file's own table above: a throwaway harness, deleted
// before #2866 merged, `sum()` and `avg()` run back-to-back over the SAME
// seed through [NativeExpHistogramMergeLowerer], real peak memory read from
// `system.query_log`) at four shapes spanning both cost axes — {1, 160}
// (parity baseline), {3741, 160} (rows-dominated, issue #2490's own repro),
// {1, 3000} (width-dominated, a single wide series), {4096, 1280} (both
// axes near their admitted limits) — measured avg()'s real peak memory at
// 1.00x-1.02x sum()'s at every point (max divergence 2.37%, at {3741,
// 160}), confirming this issue's own expectation empirically rather than by
// assumption: no guard recalibration for avg() is needed.
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
// [wrapExpHistogramMergeBudgetGuard]. multiGroup selects between the
// single-group guard above (byte-identical to before cerberus issue #2865,
// no golden churn) and one of the two multi-group guards below;
// rangeMode (only meaningful when multiGroup is true — range mode is
// ALWAYS multi-group, see [expHistogramGroupMergeSumMap]'s own doc)
// further selects between [expHistogramMergeSumMapMultiGroupBudgetGuardExpr]
// (instant, cerberus issue #2865) and
// [expHistogramMergeSumMapRangeBudgetGuardExpr] (range mode, cerberus
// issue #3027) — see this file's "Multi-group calibration" and
// "Range-mode calibration" sections for why each shape needs its OWN
// real-measured ceiling rather than sharing one.
func wrapExpHistogramMergeSumMapBudgetGuard(merged chplan.Node, maxCostUnits int64, multiGroup, rangeMode bool) chplan.Node {
	predicate := expHistogramMergeSumMapBudgetGuardExpr(maxCostUnits)
	switch {
	case multiGroup && rangeMode:
		predicate = expHistogramMergeSumMapRangeBudgetGuardExpr(maxCostUnits)
	case multiGroup:
		predicate = expHistogramMergeSumMapMultiGroupBudgetGuardExpr(maxCostUnits)
	}
	return &chplan.Filter{Input: merged, Predicate: predicate}
}

// # Multi-group calibration (cerberus issue #2865, real ClickHouse 26.6,
// # a standalone `docker run clickhouse/clickhouse-server:26.6-alpine`
// # container — not chDB — seeded with otel_metrics_exponential_histogram-
// # shaped rows and measured via the ACTUAL emitted SQL for `sum
// # by(route)(<selector>)` (chsql.Emit over promql.LowerAtRangeOpts's own
// # output, not a hand-written approximation — the same discipline this
// # file's single-group table above uses) run through
// # `SYSTEM FLUSH LOGS` + `system.query_log.memory_usage`)
//
// The guard above (unchanged since #2834) bounds each OUTPUT GROUP
// independently: it is a Filter predicate evaluated per row of the merge
// Aggregate's OWN output, one row per group, so it can only ever see that
// one group's own width/row shape. That is not the whole cost once a query
// can produce MANY groups, because [expHistogramMergeScaleWindowProject]'s
// WindowExpr pre-pass (exp_histogram_merge_summap.go) must materialise
// EVERY row of perSeries — across every group in the query at once —
// before pass 2's Aggregate (and this guard) ever runs.
//
// A real measurement isolates the group-count axis directly: fixed width
// (160 buckets, the realistic OTel-SDK-default this file's own single-
// group table is calibrated on) and exactly one row per group, so total
// rows and group count move together and the row-count axis stays
// negligible:
//
//	groups   total rows   peak memory   MiB/group
//	  1,000       1,000       12.1 MiB     0.0121
//	  4,096       4,096       44.2 MiB     0.0108
//	 10,000      10,000      105.4 MiB     0.0105
//	 40,000      40,000      381.1 MiB     0.0095
//	100,000     100,000      960.6 MiB     0.0096
//	200,000     200,000     1896.5 MiB     0.0095
//
// A second sweep isolates the row-count axis instead, holding group count
// low (100, far inside the ceiling below) and width fixed at 160 while
// rows/group grows:
//
//	groups   rows/group   total rows   peak memory
//	  100          100       10,000       27.8 MiB
//	  100        1,000      100,000       78.7 MiB
//	  100        1,500      150,000       67.2 MiB
//
// Two things fall out of this pair of sweeps:
//
//  1. Group count is the DOMINANT axis, converging to a real, roughly
//     constant ~0.0095 MiB/group as fixed per-query overhead amortises
//     away (the 1,000-group point's higher apparent rate is that
//     overhead, not a different regime) — confirming this issue's own
//     scoping-session prediction that group count is a real, independent
//     cost axis the per-group guard cannot see: 200,000 tiny groups
//     (1 row each) cost 1896.5 MiB, while 100,000 rows spread over only
//     100 groups cost a mere 78.7 MiB — same order of total rows, 24x
//     less memory, because it is FEWER GROUPS.
//  2. Total rows at LOW, fixed group count is comparatively cheap and
//     does not grow as steeply (78.7 MiB at 100,000 rows / 100 groups,
//     the 150,000-row point's lower reading a measurement-noise artifact
//     of a single run rather than a real reversal) — the opposite
//     ordering from the single-group guard's own width^2 term, and why
//     this guard bounds the two axes SEPARATELY rather than folding one
//     into the other the way the single-group guard folds rows into its
//     flat row-count backstop.
//
// [maxHistogramMergeSumMapGroupCountGuard] is pinned at the exact,
// measured 40,000-group checkpoint (381.1 MiB, ~37% of the 1024 MiB
// CERBERUS_CH_QUERY_MAX_MEMORY default target — comfortable headroom for
// the second ladder, concurrent load and the per-group cost this axis
// deliberately ignores, the same margin discipline
// [maxHistogramMergeCostUnits]'s own doc uses) rather than the
// 100,000/200,000 points above, which already consume 94%-185% of that
// budget on group count ALONE. [maxHistogramMergeSumMapTotalRowCountGuard]
// is set well above its own measured checkpoints (100,000 rows at 100
// groups costs under 80 MiB) at a round, still-conservative 200,000 —
// this axis exists to catch the "few groups, each with an enormous row
// count" shape the group-count ceiling cannot see on its own (each
// individual group's OWN row count is separately bounded by the existing,
// unchanged [maxHistogramMergeRowCountOverflowGuard] backstop, reused via
// [expHistogramMergeSumMapCostOverBudgetExpr] below), not the common case.
//
// Both new ceilings are independent of [maxHistogramMergeCostUnits] /
// [sumMapMergeCostMultiplier] (the existing, unchanged per-group knob):
// they bound the WindowExpr pre-pass's own total-scale cost, a mechanism
// the single-group guard's cost model never had to account for, so folding
// them into the SAME operator override would conflate two different real
// cost drivers behind one number. A future session that wants operator
// control over these too can add it the same way cerberus issue #2667
// added [EnvHistogramMergeMaxCostUnits] for the per-group ceiling — not
// done here to keep this issue's own scope to the guard's EXISTENCE and
// calibration, not a new knob no operator has asked for yet.
const (
	// maxHistogramMergeSumMapTotalRowCountGuard bounds the TOTAL row count
	// entering the WindowExpr pre-pass across every group in a multi-group
	// merge — see this file's "Multi-group calibration" section above for
	// why this is set well above its own measured checkpoints (a backstop
	// for the "few huge groups" shape, not a common-case ceiling).
	maxHistogramMergeSumMapTotalRowCountGuard = 200_000

	// maxHistogramMergeSumMapGroupCountGuard bounds the TOTAL number of
	// DISTINCT output groups a multi-group merge may produce — see this
	// file's "Multi-group calibration" section above for the measured
	// 40,000-group / 381.1 MiB checkpoint this is pinned at.
	maxHistogramMergeSumMapGroupCountGuard = 40_000
)

// expHistogramMergeSumMapMultiGroupCostOverBudgetExpr widens
// [expHistogramMergeSumMapCostOverBudgetExpr]'s per-group check with the
// two multi-group-only axes this file's "Multi-group calibration" section
// measures: the TOTAL row count and TOTAL distinct group count across the
// whole query, both collected once per output row via max() over the
// WindowExpr pre-pass's own whole-query-partitioned columns
// (exp_histogram_merge_summap.go's expHistogramMergeScaleWindowProject).
func expHistogramMergeSumMapMultiGroupCostOverBudgetExpr(rowCount chplan.Expr, maxCostUnits int64) chplan.Expr {
	totalRowCount := &chplan.ColumnRef{Name: hqAggMultiGroupTotalRowCountAlias}
	totalGroupCount := &chplan.ColumnRef{Name: hqAggMultiGroupTotalGroupCountAlias}
	return orExpr(
		expHistogramMergeSumMapCostOverBudgetExpr(rowCount, maxCostUnits),
		orExpr(
			gtLit(totalRowCount, maxHistogramMergeSumMapTotalRowCountGuard),
			gtLit(totalGroupCount, maxHistogramMergeSumMapGroupCountGuard),
		),
	)
}

// expHistogramMergeSumMapMultiGroupBudgetGuardExpr is
// [expHistogramMergeSumMapBudgetGuardExpr]'s multi-group counterpart:
// identical throwIf(...) = 0 shape, reading the SAME per-group columns
// plus the two multi-group totals through
// [expHistogramMergeSumMapMultiGroupCostOverBudgetExpr].
func expHistogramMergeSumMapMultiGroupBudgetGuardExpr(maxCostUnits int64) chplan.Expr {
	scalesArr := &chplan.ColumnRef{Name: hqAggScalesArrayAlias}
	seriesCount := &chplan.FuncCall{Fn: chplan.FnLength, Args: []chplan.Expr{scalesArr}}

	return &chplan.Binary{
		Op: chplan.OpEq,
		Left: &chplan.FuncCall{
			Fn: chplan.FnThrowIf,
			Args: []chplan.Expr{
				expHistogramMergeSumMapMultiGroupCostOverBudgetExpr(seriesCount, maxCostUnits),
				&chplan.InlineString{V: chplan.HistogramMergeBudgetMessage},
			},
		},
		Right: &chplan.LitInt{V: 0},
	}
}

// # Range-mode calibration (cerberus issue #3027, real ClickHouse 26.6.4, a
// # standalone `docker run clickhouse/clickhouse-server:26.6-alpine`
// # container — not chDB — seeded with otel_metrics_exponential_histogram-
// # shaped rows and measured via the ACTUAL emitted SQL for a query_range
// # `sum(<selector>)` / `sum by(instance)(<selector>)` (chsql.Emit over
// # promql.LowerAtRangeOpts's own output through
// # [NativeExpHistogramMergeLowerer], run through `SYSTEM FLUSH LOGS` +
// # system.query_log.memory_usage — the SAME discipline the single-group
// # and instant multi-group tables above use, via a throwaway harness
// # deleted before this fix merged, same precedent as those tables)
//
// Range mode is ALWAYS multi-group (every step anchor is its own output
// group — see [expHistogramGroupMergeSumMap]'s own doc), so it always
// goes through the SAME WindowExpr pre-pass the instant multi-group guard
// above bounds. But range mode ALSO pays a cost the instant path never
// does: [chplan.RangeBucketFanout] (histogram_quantile_range.go's
// buildHistogramBucketFanout) resolves one row per series PER STEP
// BEFORE this merge ever runs, via a per-row arrayJoin fan-out over every
// candidate anchor in that row's own staleness lookback window. That
// stage's cost is real and is NOT bounded by anything this file adds (it
// has its own, pre-existing "series-times-anchors resource bound" —
// histogram_quantile_range.go / internal/chsql/lwr_fanout_bound.go); this
// guard only has to be honest that its own two ceilings sit on top of
// that other, already-real cost, not that it can ignore it.
//
// A real measurement isolating the GROUP-COUNT axis (fixed width 160, no
// by()/without() clause — one output group per step anchor, exactly one
// series contributing to each — the range-mode counterpart of the
// instant multi-group table's own "1,000 groups of 1 row each" sweep):
//
//	steps   total rows   peak memory
//	   100         100      67.1 MiB
//	   500         500     303.9 MiB
//	   600         600     364.0 MiB
//	   700         700     424.1 MiB
//	  1000       1,000     604.4 MiB
//
// This axis is cleanly linear (~0.6 MiB/step + a small fixed overhead)
// across every checkpoint above — MUCH steeper than the instant multi-
// group guard's own ~0.0095-0.0138 MiB/group, confirming the issue's own
// prediction that range mode's per-group cost is materially higher (the
// extra RangeBucketFanout pass every group pays before this merge even
// starts). The sweep was NOT extended past ~1,400 steps at this shape:
// somewhere in that neighborhood the measurement stopped being
// deterministic — repeated runs at an IDENTICAL step count (1,406 to
// 1,409 tested) returned either the same ~0.6 MiB/step trend (~850 MiB)
// or a markedly cheaper ~100 MiB reading, nondeterministically, and
// settled back to a fully deterministic (repeat-tested), much cheaper
// ~0.043 MiB/step trend for every larger step count tried (2,000 through
// 40,000). That instability sits well outside the ceiling this file
// picks below and does not change its safety — [maxHistogramMergeCostUnits]'s
// own margin discipline already assumes worst-case, not best-case,
// execution — but it is flagged here, not silently smoothed over, as a
// real ClickHouse behavior (plausibly a plan-selection threshold — e.g.
// aggregation-in-order eligibility — flipping near that row count) a
// future session should understand before raising this ceiling.
//
// A second sweep isolates the ROWS-PER-GROUP axis: a fixed, low step
// count (100, far inside the ceiling below) while the series contributing
// to EACH step grows (the range-mode counterpart of the instant multi-
// group guard's own rows-per-group sweep):
//
//	steps   rows/step   total rows   peak memory
//	   100          10        1,000      68.8 MiB
//	   100          50        5,000     325.9 MiB
//	   100          60        6,000     389.6 MiB
//	   100          80        8,000     521.4 MiB
//	   100         100       10,000     534.2 MiB
//
// Both sweeps land at a REAL cost per unit far above the instant multi-
// group guard's own two sweeps (0.6 MiB/step here versus ~0.01 MiB/group
// there; ~0.065 MiB/row here versus well under 0.001 MiB/row there at
// comparable total row counts) — this is the RangeBucketFanout overhead
// above, not a WindowExpr/sumMap regression: the identical shape run
// through [FanoutExpHistogramMergeLowerer] (the OLD fold) at a few
// checkpoints in this same range was COSTLIER still (e.g. 940.5 MiB at
// 2,000 steps / 1 row-per-step, where the sumMap path measured only
// 137.9 MiB — sumMap keeps its OWN cost advantage over the fold in range
// mode too, it just does not erase RangeBucketFanout's shared cost). This
// is why range mode needs its OWN, separately-pinned ceilings rather than
// reusing the instant multi-group guard's 40,000 / 200,000 — at THOSE
// values this shape's real memory would be roughly 40,000 x 0.6 MiB
// (tens of gigabytes) before even accounting for RangeBucketFanout's own
// resource bound, wildly unsafe.
//
// [maxHistogramMergeSumMapRangeGroupCountGuard] is pinned at the exact,
// measured 600-step checkpoint (364.0 MiB, ~35.5% of the 1024 MiB
// CERBERUS_CH_QUERY_MAX_MEMORY default target — the same margin
// discipline [maxHistogramMergeSumMapGroupCountGuard]'s own doc uses),
// comfortably below the ~1,400-step instability neighborhood identified
// above. [maxHistogramMergeSumMapRangeTotalRowCountGuard] is pinned at
// the measured 6,000-row checkpoint (389.6 MiB, ~38.0% of the same
// target) — both far tighter than the instant multi-group guard's own
// ceilings, reflecting range mode's genuinely higher real per-unit cost
// rather than an arbitrary choice.
//
// avg()'s own cost is not independently re-measured on this axis: this
// guard's cost formula reads the SAME groupArray/window columns
// regardless of whether the caller is sum() or avg() (identical to the
// reasoning [maxHistogramMergeSumMapGroupCountGuard]'s own doc gives,
// which cerberus issue #2866 confirmed empirically for the instant
// shape) — avg()'s extra division happens entirely in the OUTPUT
// projection, after this guard's Filter has already run.
//
// A future session with a wider real-ClickHouse budget should still
// investigate the ~1,400-step nondeterminism directly (it is currently
// only characterized, not root-caused) before considering either ceiling
// for an increase.
const (
	// maxHistogramMergeSumMapRangeTotalRowCountGuard bounds the TOTAL row
	// count entering the WindowExpr pre-pass across every (step, group)
	// pair in a range-mode merge — see this file's "Range-mode
	// calibration" section above for the measured 6,000-row / 389.6 MiB
	// checkpoint this is pinned at.
	maxHistogramMergeSumMapRangeTotalRowCountGuard = 6_000

	// maxHistogramMergeSumMapRangeGroupCountGuard bounds the TOTAL number
	// of DISTINCT (step anchor, label group) pairs a range-mode merge may
	// produce — see this file's "Range-mode calibration" section above
	// for the measured 600-step / 364.0 MiB checkpoint this is pinned at.
	maxHistogramMergeSumMapRangeGroupCountGuard = 600
)

// expHistogramMergeSumMapRangeCostOverBudgetExpr is
// [expHistogramMergeSumMapMultiGroupCostOverBudgetExpr]'s range-mode
// counterpart: the SAME per-group cost term and the SAME max()-collected
// whole-query totals, checked against range mode's OWN, separately
// calibrated ceilings.
func expHistogramMergeSumMapRangeCostOverBudgetExpr(rowCount chplan.Expr, maxCostUnits int64) chplan.Expr {
	totalRowCount := &chplan.ColumnRef{Name: hqAggMultiGroupTotalRowCountAlias}
	totalGroupCount := &chplan.ColumnRef{Name: hqAggMultiGroupTotalGroupCountAlias}
	return orExpr(
		expHistogramMergeSumMapCostOverBudgetExpr(rowCount, maxCostUnits),
		orExpr(
			gtLit(totalRowCount, maxHistogramMergeSumMapRangeTotalRowCountGuard),
			gtLit(totalGroupCount, maxHistogramMergeSumMapRangeGroupCountGuard),
		),
	)
}

// expHistogramMergeSumMapRangeBudgetGuardExpr is
// [expHistogramMergeSumMapMultiGroupBudgetGuardExpr]'s range-mode
// counterpart: identical throwIf(...) = 0 shape, reading the SAME columns
// through [expHistogramMergeSumMapRangeCostOverBudgetExpr] instead.
func expHistogramMergeSumMapRangeBudgetGuardExpr(maxCostUnits int64) chplan.Expr {
	scalesArr := &chplan.ColumnRef{Name: hqAggScalesArrayAlias}
	seriesCount := &chplan.FuncCall{Fn: chplan.FnLength, Args: []chplan.Expr{scalesArr}}

	return &chplan.Binary{
		Op: chplan.OpEq,
		Left: &chplan.FuncCall{
			Fn: chplan.FnThrowIf,
			Args: []chplan.Expr{
				expHistogramMergeSumMapRangeCostOverBudgetExpr(seriesCount, maxCostUnits),
				&chplan.InlineString{V: chplan.HistogramMergeBudgetMessage},
			},
		},
		Right: &chplan.LitInt{V: 0},
	}
}
