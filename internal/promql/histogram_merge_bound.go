package promql

import (
	"github.com/tsouza/cerberus/internal/chplan"
)

// histogram_merge_bound.go closed issue #2385, was recalibrated for issue
// #2490, and — the cost model below — was recalibrated a second time for
// issue #2500, which changed the ALGORITHM this file bounds.
//
// The across-SERIES exponential-histogram merge (expHistogramMergeBucketsExpr)
// used to render an arrayMap over the MERGED bucket range, and for every
// one of those target buckets re-scan EVERY contributing row's own bucket
// array in full (expHistogramBucketPositionPickerExpr's old
// `arrayMap(j -> ..., arrayEnumerate(arr))` picker) to pick out the one
// element that lands there, then Kahan-fold the result
// (expHistogramBucketRowContribExpr). That nested shape was NOT the
// `(width) x (series)` product #2385 assumed: real ClickHouse 25.9-alpine
// measurements showed peak memory tracking
//
//	rows-in-group x (merged bucket-range width)^3
//
// — the per-target-bucket O(row-width) rescan, run once per target bucket,
// turned what looks like a width x series pass into one that was CUBIC in
// width alone once a row's own bucket count tracked the merged width (the
// common case). Issue #2500 fixed the ALGORITHM rather than re-picking a
// threshold on that cubic axis:
// [expHistogramBucketPositionPickerExpr] now computes each target's
// contribution from a row via a directly-computed arraySlice — the
// intersection of the target's own absolute bucket range with the row's
// populated range, worked out by arithmetic — instead of rescanning the
// row's entire array to find it. Real measurements on the same
// 25.9-alpine server (the new calibration below) now show peak memory
// tracking
//
//	rows-in-group x (merged bucket-range width)^2
//
// — quadratic, not cubic, and see that section for why: ClickHouse's own
// evaluation of the nested arrayMap/arrayReduce shape still replicates
// captured per-row state once per target bucket, which the picker rewrite
// does not remove — it only removed the redundant O(row-width) RESCAN each
// of those replicas used to pay for. A truly linear rewrite (a real
// scatter, e.g. via `groupArrayInsertAt`) was investigated and rejected:
// ClickHouse's `arrayReduce` requires its aggregate-function-name argument
// to be provably constant-foldable at analysis time, and mergedLength — the
// natural scatter-buffer size — is unavoidably derived through the same
// arrayMap/arrayMin chain [expHistogramMergeBucketsBoundsExpr] uses for the
// bucket range itself, which ClickHouse's analyzer cannot see through (this
// fails even with the aggregate name bound to its own top-level `WITH`
// alias, nowhere near a lambda). A quadratic-in-width, linear-in-rows real
// cost is what actually shipped, and the calibration below is against that.
//
// #2385's guard checked series-per-group and bucket-width as two
// INDEPENDENT axes, each generous on its own (4096 series, 16384 width).
// Issue #2490's real-world repro — 3,741 series, both with and without a
// rate() wrapper — sat comfortably under BOTH those independent caps and
// still exceeded a 1 GiB cap on real ClickHouse 25.9-alpine, because neither
// axis alone was ever the real cost driver: their PRODUCT is (at the CUBIC
// exponent #2385 never measured, before #2500; at the QUADRATIC one after).
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
// # Calibration (real ClickHouse 25.9-alpine, issue #2500's quadratic-cost
// # algorithm, 1 GiB cap — the CERBERUS_CH_QUERY_MAX_MEMORY default)
//
// A throwaway harness (deleted before this fix merged, per
// rate_window_fanout_bound.go's own precedent) lowered + emitted the real
// `histogram_quantile(0.95, sum by(route)(<metric>))` SQL — same query
// shape the original calibration used — and ran it against a real
// `clickhouse/clickhouse-server:25.9-alpine` container (chDB was
// deliberately NOT used — see the original calibration's own note on why),
// reading each query's true peak `memory_usage` back from
// `system.query_log` after `SYSTEM FLUSH LOGS`, with `max_memory_usage` set
// generously (30 GiB) so the query actually COMPLETES and reports a real
// peak rather than an abort estimate. Series shared an IDENTICAL bucket
// layout (Scale/Offset), matching the original calibration's own
// methodology:
//
//	rows   width   peak memory (real server, generous cap)
//	   1      160     13.4 MiB
//	   1      640     13.4 MiB
//	   1     1280     29.2 MiB
//	   1     2560    101.3 MiB
//	   1     5120    389.5 MiB
//	   1    10240   1541.9 MiB
//	  10      160     13.4 MiB
//	  40      160     17.5 MiB
//	 100      160     54.3 MiB
//	 500      160    203.6 MiB
//	1000      160    401.9 MiB
//	2000      160    798.9 MiB
//	3741      160   1590.4 MiB  (issue #2490's own row count — now a clean,
//	                             HONEST 1.55 GiB of real work at a realistic
//	                             OTel-default width, not an artefact of a
//	                             mis-costed cubic model; still over a 1 GiB
//	                             cap, correctly, since it genuinely needs
//	                             more memory than that to complete safely)
//
// The width-1 sweep (rows=1, width doubling) is the cleanest signal: each
// doubling past width 2560 multiplies peak memory by very close to 4x
// (101.3 -> 389.5 -> 1541.9, ratios 3.85x and 3.96x, converging on 4x as
// fixed per-query overhead becomes negligible) — quadratic, not cubic
// (which would show ~8x per doubling; the OLD algorithm's own calibration
// table showed exactly that). The rows sweep at a fixed width (160) is
// clean and LINEAR throughout (798.9 / 401.9 = 1.99x for a 2x row
// increase). Fitting `rows x width^2 x bytes/unit` against the highest-
// magnitude, least-overhead-affected point (rows=1, width=10240) gives
// ~15.5 bytes/unit; the rest of the table is consistent with that rate to
// within the same rough margin the original (cubic) calibration reported.
//
// This makes the algorithm safe at ORDINARY production scale — hundreds to
// low thousands of series at a realistic bucket width, roughly two orders
// of magnitude past where the OLD cubic-cost guard started rejecting
// (issue #2490's own doc: ~40 rows at width 160 already exceeded a 1 GiB
// cap under the old model) — without claiming it is free: issue #2490's
// own upper end (3,741 series) still needs more than 1 GiB of real memory
// at a realistic width, and the guard below correctly still rejects that
// exact case, honestly, because it really does need that much memory to
// answer safely — not because of a cubic-cost artefact a bound could paper
// over. See the issue for the fuller sweep.

const (
	// maxHistogramMergeCostUnits bounds `rows x (posWidth^2 + negWidth^2)`
	// — the real cost driver the calibration above measured (quadratic
	// in width, since issue #2500's picker rewrite; the two independent
	// axes #2385 originally checked were never the real driver, and
	// before #2500 the true exponent was cubic, not quadratic — see the
	// file doc for both). 60,000,000 x ~15.5 bytes/unit (the measured
	// rate) is ~887 MiB, comfortably under the 1 GiB
	// CERBERUS_CH_QUERY_MAX_MEMORY default with margin for the second
	// ladder, the Count/Sum/ZeroCount groupArray folds this guard does
	// not itself cost-model, and concurrent load — while now admitting
	// roughly two orders of magnitude more series at a realistic
	// ~160-bucket width than the old cubic-cost guard did (up to
	// ~2,300 rows at width 160, vs the old guard's ~40). Recalibrate by
	// binary search against a real ClickHouse server (NOT chDB — see the
	// file doc comment) if this drifts; preserve the rows x width^2
	// model unless a new measurement shows the exponent itself has moved
	// again.
	//
	// # Operator override (cerberus issue #2667)
	//
	// This value is a compile-time DEFAULT, not the only admissible
	// value: [ResourceBounds.HistogramMergeMaxCostUnits], resolved from
	// [EnvHistogramMergeMaxCostUnits] (CERBERUS_PROMQL_HISTOGRAM_MERGE_MAX_COST_UNITS)
	// via [ResourceBoundsFromEnv] and threaded down through
	// [LowerOpts.ResourceBounds] / lowerCtx.resourceBounds to every
	// budget-guard call site, is what the guard actually checks at
	// runtime — see resource_bounds_env.go. This constant remains the
	// value [DefaultResourceBounds] returns and every non-opted-in
	// caller (the spec harness, plan-only helpers, LowerOpts built
	// without setting ResourceBounds) still enforces, so a deployment
	// that never sets the env var keeps this exact, load-bearing
	// calibration: cerberus issue #2385 recorded 19 REAL production OOMs
	// before this guard existed at all, and an operator override exists
	// to let a deployment correct a wrong calibration for its own real
	// traffic WITHOUT a code change, never to invite lowering the
	// default's own protection.
	maxHistogramMergeCostUnits = 60_000_000

	// maxHistogramMergeClampedWidth is NOT a behavioral threshold — the
	// cost formula above already rejects any width past a few thousand
	// (see the calibration table: rows=1, width=10240 alone is already
	// ~1.5 GiB) long before this number matters. It exists purely so
	// squaring a data-determined, otherwise-unbounded width (two series'
	// Offset can diverge by up to the full Int32 range) can never
	// overflow the Int64 arithmetic the cost check runs in: 100,000^2 =
	// 10^10, eight orders of magnitude below Int64's ~9.2x10^18 ceiling
	// — even more headroom than this same clamp needed for the pre-#2500
	// CUBED formula (100,000^3 = 10^15, three orders of magnitude below
	// that ceiling) — while still being far larger than the cost
	// formula's own effective width ceiling (~7,746 at rows=1), so this
	// clamp never changes which queries the guard accepts — only which
	// ones stay overflow-safe while being rejected. Left at 100,000
	// rather than re-derived tighter for the new exponent: squaring
	// needs less overflow protection than cubing did, so the existing
	// value stays valid (and safer) without changing it.
	maxHistogramMergeClampedWidth = 100_000

	// maxHistogramMergeRowCountOverflowGuard is NOT a resurrected copy of
	// #2385's independent series-per-group cap (see this file's header
	// doc for why checking that axis independently was wrong) — it is a
	// pure overflow backstop for histogramMergeCostOverBudgetExpr's
	// `rowCount x (posWidth^2 + negWidth^2)` multiply. Both squared-width
	// terms clamp to maxHistogramMergeClampedWidth, so their sum tops out
	// at 2 x 100,000^2 = 2x10^10; Int64 wraps past ~9.2x10^18, so the
	// multiply has roughly eight orders of magnitude of headroom left at
	// ANY rowCount a real query could carry (even the
	// maxHistogramMergeRowCountOverflowGuard-scale row counts below) —
	// far more headroom than the pre-#2500 cubed formula had (whose sum
	// topped out at 2x10^15, only three orders of magnitude under the
	// ceiling, which is what actually forced this constant to exist).
	// 4096 is kept unchanged rather than loosened now that the multiply
	// has so much more headroom: it still comfortably covers every real
	// workload the calibration above measured (issue #2490's own repro
	// is 3,741 rows) with room to spare, and this constant's role is
	// documentation of a proven-safe ceiling, not a knob to push simply
	// because the arithmetic could now tolerate a larger one. Past this
	// cap the guard rejects unconditionally, via [orExpr] in
	// [histogramMergeCostOverBudgetExpr], without needing the multiply to
	// prove it — the same role maxHistogramMergeClampedWidth already
	// plays for the width term. Recalibrate only if
	// maxHistogramMergeClampedWidth's own value changes; this is not an
	// independent behavioral tuning knob for THIS (rows x width^2) formula.
	//
	// This same constant is reused, unchanged, by
	// [expHistogramMergeSumMapCostOverBudgetExpr]
	// (exp_histogram_merge_summap_bound.go, cerberus issue #2834) as that
	// guard's own row-count backstop — there it is no longer a pure
	// overflow margin: that design's cost formula drops the `rows x`
	// multiplier entirely (see that file's own header doc for why), so
	// this ceiling is the ONLY thing bounding its real, measured
	// rows-linear cost term. That file's own calibration table pins real
	// memory at exactly this row count, so the value stays load-bearing
	// for a second, genuinely behavioral reason — recalibrate both guards
	// together if this changes.
	maxHistogramMergeRowCountOverflowGuard = 4096

	// maxHistogramMergeOutputWidth bounds the per-ladder bucket-range WIDTH
	// [refinedMergeScaleExpr] downscales a merge onto, for every merge path
	// in this package: the cross-series groupArray fold behind
	// histogram_quantile() and histogram-valued sum() (cerberus issue #3555,
	// [expHistogramMergeSortStage]), the two-operand binop merge
	// (histogram_native_binop.go, histogram_native_binop_card.go,
	// histogram_native_mixed_or_vector_arithmetic.go) and the sumMap-based
	// sum()/avg() merge (exp_histogram_merge_summap.go), the last two
	// closed by cerberus issue #3558. All of them compute their shared merge
	// scale as min(Scale) across contributing rows, and min(Scale) alone
	// only guarantees every row CAN be downscaled onto the merge, never that
	// the result is narrow: two rows that are each individually narrow
	// (their OWN samples cluster tightly, so neither ever had a reason to
	// pick a coarser Scale) but differ in TYPICAL MAGNITUDE still merge
	// into an astronomically wide range. Cerberus issue #3555's own live
	// measurement against the `cerberus-self` e2e dashboard's real
	// ClickHouse data is the concrete case: `histogram_quantile(0.95, sum by
	// (cerberus_ql) (rate(cerberus_queries_duration_exp_hist[5m])))` tripped
	// [maxHistogramMergeCostUnits] with as few as 4 contributing series
	// (four cerberus.route values under the SAME cerberus.ql), none
	// individually wide — every row stayed at Scale 20 (the OTel SDK never
	// had a reason to narrow a single series whose OWN samples cluster
	// tightly) — because two rows with merely DIFFERENT typical magnitudes
	// (a ~0.34ms route and a ~50ms route, under 150x apart) produced a
	// merged width of ~7.6 MILLION buckets at Scale 20: Scale 20's own
	// bucket boundaries are only a factor of 1.0000007 apart, and nothing
	// coarsened the group's shared scale to account for the SPREAD BETWEEN
	// rows' central values, only for each row's OWN internal spread.
	//
	// The cap is an OTel-SDK-style bucket budget: the OTel exponential
	// histogram SDK accumulator auto-narrows a SINGLE series' scale whenever
	// its range would exceed MaxSize, and
	// [chplan.OTelExpoHistogramDefaultMaxSize] is the spec's documented
	// default for that budget, the value every major SDK ships unconfigured
	// (internal/telemetry collects cerberus's own duration histogram with
	// it). Capping the MERGED output at that same width means cross-series
	// and cross-operand merging never throws away more resolution than a
	// single series already tolerates by default, while turning an
	// unbounded, data-shape-dependent width into a fixed one: at 160,
	// [histogramMergeCostOverBudgetExpr]'s own `rows x width^2` formula
	// admits rows up to maxHistogramMergeCostUnits / (160^2) = 2,343 before
	// the fold path's own guard fires on width-capped input, comfortably
	// above the handful of series a `sum by(cerberus_ql)` (or any
	// similarly-shaped production query) actually needs.
	//
	// This is a deliberate DIVERGENCE from reference Prometheus, not a
	// mirror of it. Prometheus's FloatHistogram.Add merges at the minimum of
	// the two operands' schemas and stores the result as sparse spans, so a
	// wide merged range costs it nothing and it never applies a bucket
	// budget; cerberus stores a merged ladder as one dense array per ladder,
	// whose width is the memory the cost guard bounds, so a merge whose
	// natural width exceeds the cap is answered at a coarser scale than
	// Prometheus computes (docs/compatibility.md, "PromQL" divergences, and
	// the exp_histogram_sum_wide_merge_compaction_count parity fixture,
	// which pins the count both engines agree on across that gap). The
	// alternative — rejecting such a merge outright — was what every path
	// did before #3555, and it refused real production queries.
	//
	// This is a resolution FLOOR shared across every histogram-merge path:
	// the justification (never coarser than what a single series already
	// tolerates) is about the WIDTH definition itself, independent of which
	// algorithm produced it, not a per-path memory-cost calibration — those
	// stay separate, already-calibrated per-path constants
	// ([maxHistogramMergeCostUnits], [sumMapMergeCostMultiplier] in
	// exp_histogram_merge_summap_bound.go). Recalibrate only alongside a
	// real measurement showing 160 buckets of resolution is insufficient
	// for a real quantile/sum computation's accuracy — this is a resolution
	// floor, not an arbitrary knob.
	maxHistogramMergeOutputWidth = chplan.OTelExpoHistogramDefaultMaxSize
)

// refinedMergeScaleExpr computes a merge scale downscaled far enough that
// the merged bucket-range WIDTH — at rawScale, i.e. the natural
// min(Scale)-only merge every caller started from — stays within
// [maxHistogramMergeOutputWidth] (plus one, see [extraDownscaleStepsExpr]).
// Its callers are the cross-series groupArray fold
// ([expHistogramMergeSortStage], cerberus issue #3555 — the path this
// function was written for), the two-operand binop merge
// ([wrapExpHistogramMergeScaleRefinement] below, histogram_native_binop.go,
// histogram_native_binop_card.go, histogram_native_mixed_or_vector_arithmetic.go)
// and the sumMap-based sum()/avg() merge's single-group pass 1
// (exp_histogram_merge_summap.go), the last two closed by cerberus issue
// #3558. See [extraDownscaleStepsExpr] for the step arithmetic.
//
// scalesArr/posOff/posBuc/negOff/negBuc must be the SAME shape
// [mergedLengthExpr] expects: either the groupArray-collected arrays
// [expHistogramMergeAggs] produces (the binop merge's shape — an
// Aggregate's own output columns), or an equivalent per-group array a
// caller derives some other way (the sumMap merge's single-group pass-1
// shape, exp_histogram_merge_summap.go's expHistogramMergeScaleScalarSubquery
// — which has no Aggregate to attach a HAVING or a post-Aggregate Filter
// to, so it collects its own groupArrays in pass 1 instead). The sumMap
// merge's MULTI-GROUP pass-1 shape (expHistogramMergeScaleWindowProject)
// cannot cheaply collect per-group arrays via a WindowExpr — a windowed
// groupArray materialises the WHOLE partition's array once PER ROW of
// that partition, an O(rows-per-group^2) blowup a per-row pre-pass over
// potentially many groups cannot afford — so it computes naturalWidth
// directly via [extraDownscaleStepsExpr] instead of calling this function.
func refinedMergeScaleExpr(rawScale, scalesArr, posOff, posBuc, negOff, negBuc chplan.Expr) chplan.Expr {
	naturalWidth := greatestExpr(
		mergedLengthExpr(scalesArr, posOff, posBuc, rawScale),
		mergedLengthExpr(scalesArr, negOff, negBuc, rawScale),
	)
	return subExpr(rawScale, extraDownscaleStepsExpr(naturalWidth))
}

// extraDownscaleStepsExpr computes the number of additional downscale
// steps needed to bring naturalWidth (the merge's width at whatever raw
// scale it started from) down to [maxHistogramMergeOutputWidth]:
// ceil(log2(naturalWidth / maxHistogramMergeOutputWidth)), floored at 0.
//
// The bound the step count actually delivers is cap + 1, not cap. A
// downscale step maps absolute bucket index i to i >> 1 (floor), so k steps
// turn the range [lo, hi] into [lo >> k, hi >> k], whose width is
// floor(hi / 2^k) - floor(lo / 2^k) + 1 — at most (hi - lo + 1) / 2^k + 1,
// one more than a pure halving, when lo and hi sit on opposite sides of a
// 2^k boundary (lo = -1, hi = 318: width 320, one step, [-1, 159] = 161
// buckets). The step count is the minimum that halves the natural width
// under the cap; the extra bucket is the alignment slack of the floor,
// which the cost guard's width clamp absorbs without effect (161^2 against
// 160^2 in a budget of 60,000,000).
//
// greatest(1, naturalWidth) keeps log2's argument positive (an
// empty-both-ladders group has naturalWidth 0, which needs no downscale —
// log2(0) would otherwise render -inf) and the outer greatest(0, ...)
// discards a negative step count (naturalWidth already under the cap
// needs no further downscale). Float64 arithmetic is exact enough here:
// naturalWidth is bounded by the same Int32 offset range
// [maxHistogramMergeClampedWidth] bounds downstream, so log2 of it is a
// small, exactly-representable value; a stray rounding error could only
// push the step count one higher than strictly necessary, which only
// makes the result SAFER (narrower), never wrong.
//
// Factored out of [refinedMergeScaleExpr] so a caller that already has
// naturalWidth in hand via some OTHER route than [mergedLengthExpr]'s
// array-based computation — exp_histogram_merge_summap.go's multi-group
// WindowExpr pre-pass derives it from a pair of scalar MIN/MAX window
// aggregates instead, see that file's own doc — can still share this one
// downscale-step formula rather than re-deriving it.
func extraDownscaleStepsExpr(naturalWidth chplan.Expr) chplan.Expr {
	ratio := &chplan.Binary{
		Op:    chplan.OpDiv,
		Left:  toFloat64Expr(greatestExpr(&chplan.LitInt{V: 1}, naturalWidth)),
		Right: &chplan.LitFloat{V: float64(maxHistogramMergeOutputWidth)},
	}
	return greatestExpr(
		&chplan.LitInt{V: 0},
		&chplan.FuncCall{Fn: chplan.FnToInt64, Args: []chplan.Expr{
			&chplan.FuncCall{Fn: chplan.FnCeil, Args: []chplan.Expr{
				&chplan.FuncCall{Fn: chplan.FnLog2, Args: []chplan.Expr{ratio}},
			}},
		}},
	)
}

// wrapExpHistogramMergeScaleRefinement further downscales merged — a raw
// merge carrying the [expHistogramMergeAggs] groupArray aliases: the
// cross-series fold's own Aggregate ([expHistogramMergeSortStage],
// cerberus issue #3555) or the two-operand binop merge (the
// [chplan.Aggregate] or two-element-array [chplan.Project] shapes
// histogram_native_binop.go / histogram_native_binop_card.go /
// histogram_native_mixed_or_vector_arithmetic.go build, cerberus issue
// #3558) — so hqAggMergedScaleAlias holds a scale that ALSO bounds the
// merged bucket-range width to [maxHistogramMergeOutputWidth], not just
// min(Scale) across the contributing rows. See [refinedMergeScaleExpr]'s
// doc for the downscale arithmetic itself.
//
// Rendered as a passthrough Project with a single Replacement — the same
// `* REPLACE` idiom this package already uses elsewhere for an in-place
// column rewrite — so hqAggMergedScaleAlias keeps its name and every
// downstream reader ([expHistogramMergeOffsetExpr],
// [histogramBinopMergedBucketsExpr], [histogramBinopBucketWidthBudgetGuardExpr])
// needs no change to see the refined value; ClickHouse's `SELECT * REPLACE(...)`
// evaluates the replacement expression against merged's OWN, UNCHANGED
// output columns, so referencing hqAggMergedScaleAlias inside the
// replacement expression for that SAME alias reads the pre-replacement
// (min(Scale)) value, not a self-reference.
//
// Must run BEFORE the caller's own bucket-width budget guard (whether that
// guard is a Having conjunct or a downstream Filter) so the guard's own
// cost computation, which reads hqAggMergedScaleAlias, sees this refined
// value rather than the raw min(Scale) the merge produced.
func wrapExpHistogramMergeScaleRefinement(merged chplan.Node) chplan.Node {
	scalesArr := &chplan.ColumnRef{Name: hqAggScalesArrayAlias}
	rawScale := &chplan.ColumnRef{Name: hqAggMergedScaleAlias}
	posOff := &chplan.ColumnRef{Name: hqAggPosOffsetsArrayAlias}
	posBuc := &chplan.ColumnRef{Name: hqAggPosBucketsArrayAlias}
	negOff := &chplan.ColumnRef{Name: hqAggNegOffsetsArrayAlias}
	negBuc := &chplan.ColumnRef{Name: hqAggNegBucketsArrayAlias}

	return &chplan.Project{
		Input: merged,
		Replacements: []chplan.Projection{
			{Expr: refinedMergeScaleExpr(rawScale, scalesArr, posOff, posBuc, negOff, negBuc), Alias: hqAggMergedScaleAlias},
		},
	}
}

// mergedLengthExpr returns a FULLY BOUND rendering of one signed ladder's
// merged bucket-range length, reusing expHistogramOverMergedBucketRangeExpr
// — the SAME hqLet-bound helper the real merge site
// (expHistogramMergeBucketsExpr) reuses — rather than calling
// expHistogramMergeBucketsBoundsExpr directly. Calling the bounds helper
// raw would render its own, UNBOUND copy of the `arrayMin(arrayMap(...))`
// merged-start subtree — exactly the cerberus issue #2267 hazard
// expHistogramOverMergedBucketRangeExpr's own doc describes, and exactly
// what TestExpHistogramMergedStartIsBoundOncePerSite pins against: every
// render of that subtree must be one of that helper's own bindings.
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

// clampedLadderWidthSquaredExpr renders one signed ladder's merged width,
// clamped to [maxHistogramMergeClampedWidth] (see that constant's doc for
// why the clamp itself is never the behavioral bound), squared via plain
// integer multiplication — never `pow`, which returns Float64 and loses
// exact precision on values this large. mergedLengthExpr is called exactly
// ONCE per ladder, and the clamped width is bound once
// ([paramClampedLadderWidth]) and squared from that binding. Reading the
// same Go node twice is not enough: the emitter renders the expression DAG
// as a tree (see hqLet), so `clamped * clamped` would print the whole
// width computation twice, and the older ClickHouse analyzer re-analyses
// each printed copy once per derived-query level above the guard.
//
// Squared, not cubed: issue #2500 rewrote expHistogramBucketPositionPickerExpr
// (the merge's actual per-target-bucket work) from an O(row-width) rescan
// to a directly-computed arraySlice, which real ClickHouse 25.9-alpine
// measurements (this file's header doc) show turned the merge's true cost
// from cubic-in-width to quadratic-in-width. This function's own name and
// shape changed to match; see the header doc's calibration section for the
// real measurements backing the new exponent.
func clampedLadderWidthSquaredExpr(scalesArr, offArr, bucArr, mergedScale chplan.Expr) chplan.Expr {
	clamped := &chplan.FuncCall{
		Fn:   chplan.FnLeast,
		Args: []chplan.Expr{mergedLengthExpr(scalesArr, offArr, bucArr, mergedScale), &chplan.LitInt{V: maxHistogramMergeClampedWidth}},
	}
	return hqLet(paramClampedLadderWidth, clamped, func(width chplan.Expr) chplan.Expr {
		return mulExpr(width, width)
	})
}

// paramClampedLadderWidth is the lambda parameter name
// [clampedLadderWidthSquaredExpr] binds one ladder's clamped merged width
// to before squaring it.
const paramClampedLadderWidth = "mcw"

// histogramMergeCostOverBudgetExpr renders the `<rowCount overflow guard> OR
// <cost> > maxCostUnits` condition the calibration in this file's header
// doc backs: rowCount x (posWidth^2 + negWidth^2), the real measured cost
// driver (quadratic since issue #2500's picker rewrite; cubic before it),
// rather than the two independent axes issue #2385 originally checked (see
// that doc for why the axes themselves — not just their thresholds — were
// wrong).
//
// maxCostUnits is the caller-resolved ceiling — [DefaultResourceBounds]'s
// HistogramMergeMaxCostUnits (== [maxHistogramMergeCostUnits], the shipped,
// calibrated default) unless an operator has overridden it via
// [EnvHistogramMergeMaxCostUnits] (cerberus issue #2667). Threaded as an
// explicit parameter rather than read from the environment here — this
// package may not depend on internal/config (.go-arch-lint.yml) — and never
// defaulted inside this function: a caller that forgets to resolve
// [ResourceBounds.withDefaults] gets a zero ceiling here, which is
// deliberately NOT "unlimited" (see [ResourceBounds]'s own doc), so the
// resolution happens once, at the lowering-entry seam, not per call.
//
// The leading `rowCount > maxHistogramMergeRowCountOverflowGuard` disjunct
// is not a second business threshold — it exists purely so the multiply
// below can never wrap Int64 and silently defeat the real cost check (see
// that constant's doc). Because this is an OR of two independently
// evaluated booleans, ClickHouse computing a wrapped, meaningless `cost`
// for a rowCount past that guard cannot flip the overall predicate to
// false: the first disjunct alone already forces rejection at that point.
// maxHistogramMergeRowCountOverflowGuard itself stays a fixed Go constant,
// not an operator knob — cerberus issue #2667 explicitly excludes it (and
// its width-clamp sibling) as pure overflow-prevention arithmetic dominated
// by whichever maxCostUnits value is in force, never an independently
// tuned threshold.
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
func histogramMergeCostOverBudgetExpr(rowCount chplan.Expr, maxCostUnits int64) chplan.Expr {
	scalesArr := &chplan.ColumnRef{Name: hqAggScalesArrayAlias}
	mergedScale := &chplan.ColumnRef{Name: hqAggMergedScaleAlias}
	posOff := &chplan.ColumnRef{Name: hqAggPosOffsetsArrayAlias}
	posBuc := &chplan.ColumnRef{Name: hqAggPosBucketsArrayAlias}
	negOff := &chplan.ColumnRef{Name: hqAggNegOffsetsArrayAlias}
	negBuc := &chplan.ColumnRef{Name: hqAggNegBucketsArrayAlias}

	cost := mulExpr(
		rowCount,
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
// bucket work, never reached for a group this guard rejects.
//
// maxCostUnits — see [histogramMergeCostOverBudgetExpr]'s doc — is the
// caller-resolved ceiling, threaded down from [wrapExpHistogramMergeBudgetGuard].
func histogramMergeBudgetGuardExpr(maxCostUnits int64) chplan.Expr {
	scalesArr := &chplan.ColumnRef{Name: hqAggScalesArrayAlias}
	seriesCount := &chplan.FuncCall{Fn: chplan.FnLength, Args: []chplan.Expr{scalesArr}}

	return &chplan.Binary{
		Op: chplan.OpEq,
		Left: &chplan.FuncCall{
			Fn: chplan.FnThrowIf,
			Args: []chplan.Expr{
				histogramMergeCostOverBudgetExpr(seriesCount, maxCostUnits),
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
//
// maxCostUnits is the caller-resolved ceiling ([ResourceBounds]
// .HistogramMergeMaxCostUnits, cerberus issue #2667) — [expHistogramMergeSortStage]
// receives it from its own caller's lowerCtx.resourceBounds and passes it
// straight through, so this file has no direct dependency on lowerCtx.
func wrapExpHistogramMergeBudgetGuard(merged chplan.Node, maxCostUnits int64) chplan.Node {
	return &chplan.Filter{Input: merged, Predicate: histogramMergeBudgetGuardExpr(maxCostUnits)}
}

// orExpr returns `a OR b`. Mirrors andExpr, this package's existing
// conjunction helper.
func orExpr(a, b chplan.Expr) chplan.Expr {
	return &chplan.Binary{Op: chplan.OpOr, Left: a, Right: b}
}

// gtLit returns `expr > n`.
func gtLit(expr chplan.Expr, n int64) chplan.Expr {
	return &chplan.Binary{Op: chplan.OpGt, Left: expr, Right: &chplan.LitInt{V: n}}
}
