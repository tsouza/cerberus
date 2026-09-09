package promql

import "github.com/tsouza/cerberus/internal/chplan"

// histogram_native_reset.go answers ONE question for an exponential
// (native) histogram's window fold: at which of a series' in-window
// samples did the counter restart?
//
// Reference Prometheus asks it once per SAMPLE, for the whole
// distribution at once. promql/functions.go's histogramRate subtracts
// the window's first histogram from its last and then, for every sample
// the verdict condemns, adds the ENTIRE previous distribution back:
//
//	h := last.CopyToSchema(minSchema)
//	h.Sub(prev)
//	for i, currPoint := range points[1:] {
//	    if curr.DetectReset(prev) { h.Add(prev) }
//	    prev = curr
//	}
//
// [counterIncreaseFold]'s `if(c < p, c, c - p)` telescoping is the same
// arithmetic — with a reset at sample k both reach
// `v_n - v_0 + v_{k-1}`, and both sum that correction over every reset —
// so the fold and reference can only disagree about WHICH samples count
// as resets. Before this file each component answered for itself, and
// three shapes made that disagree with reference (issue #2017):
//
//   - A bucket whose post-restart count already exceeds its pre-restart
//     count at the next scrape. Reference condemns the whole sample and
//     adds that bucket's own previous reading; a per-component rule sees
//     `c >= p` for that one bucket, telescopes it, and under-counts it by
//     exactly that reading.
//   - Sum. `FloatHistogram.DetectReset` deliberately does not read it
//     ("in any bucket, in the zero count, or in the count of
//     observations, but NOT the sum of observations"), so reference's Sum
//     follows the whole-histogram verdict. A histogram with negative
//     observations can have a FALLING Sum with no restart anywhere, and a
//     per-component rule reads that as a reset that never happened.
//   - A resolution increase. `h.Schema > previous.Schema` is an
//     unconditional reset in reference, because a finer schema can only
//     appear after a restart. Reconciling scales by downscaling every row
//     to the group's coarsest scale — which is what the window stage does
//     — leaves nothing for a per-component rule to see.
//
// The fix is a per-series reset MASK: one boolean per consecutive sample
// PAIR, computed once from the whole distribution, and read by every
// component's fold in place of its own comparison. [hqWindowResetsAlias]
// names the column, [expHistogramResetMaskStage] projects it, and
// [counterIncreaseFold] consumes it.
//
// # What the mask encodes, against DetectReset
//
// `FloatHistogram.DetectReset` (model/histogram/float_histogram.go in the
// pinned fork) is true when ANY of:
//
//	Count < previous.Count
//	Schema > previous.Schema                 (resolution INCREASE only)
//	ZeroThreshold < previous.ZeroThreshold   (threshold DECREASE only)
//	the new ZeroThreshold cuts through a populated previous bucket
//	ZeroCount < previous.ZeroCount           (previous adjusted to the new threshold)
//	any positive or negative bucket regressed, prev reconciled to the
//	    current schema — including a populated previous bucket the current
//	    histogram does not have at all
//
// The mask encodes the Count, Schema, ZeroCount and both bucket
// conditions. It does not encode the three ZeroThreshold ones because
// upstream OTel-CH persists no zero-threshold column at all
// ([schema.Metrics.ZeroThresholdColumn] is empty by default), so there is
// no per-row threshold for any reading of the stored data to compare: the
// window stage already treats the zero band as fixed for a series'
// lifetime, and the mask reads the zero bucket the same way. A schema that
// does declare the column persists ONE value per row and aggregates it as
// `max`, which is that same fixed-band reading.
//
// The bucket comparison is made at EACH PAIR's own scale — the coarser
// of its two rows, which IS the current row's wherever the comparison can
// decide anything, matching reference's "prev reconciled to the current
// schema" — never at the window's merged (coarsest-of-everyone) scale.
// (The two readings part only when curr is FINER than prev, and that pair
// is condemned unconditionally by the schema term before any bucket is
// read; see [expHistogramResetPairBucketRegressedExpr] for why the
// distinction is nevertheless load-bearing.)
// Comparing at the merged scale instead (cerberus issue #2095, fixed by
// [expHistogramResetPairBucketRegressedExpr]) let a LATER sample's
// coarser scale pull the comparison scale below an EARLIER pair's own,
// so a fine-bucket regression the coarser bucket absorbed stopped being
// visible: reference condemned the sample, the merged-scale reading did
// not. Rescaling only the pair's own two rows — not the whole window —
// is also cheaper: the cost is linear in the PAIR's bucket span, not the
// window's.
//
// # What it costs, and why the pair comparison is shaped as it is
//
// The mask is one boolean per (series, anchor, sample pair), and the
// bucket half of each verdict reconciles the pair's two rows over their
// merged bucket range. That made it the single most expensive term in an
// exponential-histogram `rate()` query once the bucket ladders themselves
// folded in closed form (histogram_native_window_closed_form.go): on
// cerberus's own `cerberus_queries_duration_exp_hist` telemetry (7
// series, 80-155 stored buckets, ~20 sample pairs per 5m window), a
// 21-anchor `histogram_quantile(0.95, sum by(...) (rate(X[5m])))` peaked
// at 912 MiB against the 1 GiB CERBERUS_CH_QUERY_MAX_MEMORY default and
// answered HTTP 422 — cerberus issue #3178, which is what made the
// compose self-monitoring dashboard's own P95 panel red on `main`.
//
// The cost was NOT the verdict's arithmetic and NOT its cardinality.
// Measured, holding everything else fixed: replacing the whole mask with
// a constant took the query to 152 MiB; keeping the per-pair loop but
// shrinking its target range from ~110 elements to FOUR left it at
// 898 MiB. What the query was paying for was ClickHouse materialising the
// group's per-row bucket ARRAYS once per element of the innermost lambda,
// because that lambda read them by subscript rather than receiving them.
// [expHistogramDenseContribsExpr] is the shape that stops it: both sides
// of the comparison become dense per-target arrays built OUTSIDE the
// comparison, which is then a two-argument `arrayExists` capturing
// nothing. Same slices, same numbers, 238 MiB.
//
// The same lever then applied ONE level further out, and that was
// cerberus issue #3239: with the per-target capture gone, what the mask
// still paid for was the pair lambda's own capture of the group's
// Array(Array) bucket ladders, rebuilt once per PAIR.
// [expHistogramPairBucketLadderArgs] hands each pair its two ladders as
// lambda arguments instead, and carries that measurement.
//
// # Why it is a column and not an expression inlined into the fold
//
// The mask is a rows x buckets nested expression, and the reshape above
// it calls the fold FIVE times (the zero bucket, both signed bucket
// ladders, Count and Sum) with two [counterIncreaseFold] invocations
// inside each. Inlining would render it ten times per query — and the two
// bucket calls evaluate their fold once per TARGET BUCKET, which would
// make a per-bucket cost out of a per-series quantity. Projecting it once
// in its own layer, exactly as the classic path's [hqWindowLadderAlias]
// does for its cumulative ladder, keeps both the rendered text and the
// evaluation linear in the window's rows.

// hqWindowResetsAlias holds the per-series counter-reset mask: one
// element per consecutive sample PAIR in timestamp order, true where
// reference Prometheus's whole-histogram `DetectReset` would condemn the
// later sample of the pair. Positionally aligned with the pairs
// [counterIncreaseFold] forms from its own time-sorted values, so element
// j answers for the step from sample j to sample j+1.
const hqWindowResetsAlias = "_hq_resets"

// Lambda parameter names for the reset mask. `rp` / `rt` sort the row
// positions by timestamp; `ra` / `rb` are one consecutive (previous,
// current) pair of those positions; `rk` is a target bucket index during
// the rescale; `rz` is one target bucket's per-row counts.
const (
	paramResetRowPos       = "rp"
	paramResetRowTime      = "rt"
	paramResetPrevRow      = "ra"
	paramResetCurrRow      = "rb"
	paramResetTargetBucket = "rk"
	paramResetBucketRow    = "rz"
)

// paramResetOrderedRows is the lambda parameter name hqLet binds the
// mask's time-sorted row-position permutation to — a per-series
// quantity every pair reads, so binding it once is what keeps it from
// being recomputed per pair. See hqLet.
const paramResetOrderedRows = "wrp"

// paramResetDenseCurr / paramResetDensePrev bind one target bucket's
// already-densified contribution from the pair's current and previous
// row. They are lambda ARGUMENTS of the comparison rather than captured
// arrays indexed inside it, which is the whole point of the densified
// rendering — see [expHistogramDenseContribsExpr].
const (
	paramResetDenseCurr = "rdc"
	paramResetDensePrev = "rdp"
)

// The four bucket-ladder lambda ARGUMENTS every per-pair mask binds. See
// [expHistogramPairBucketLadderArgs] for why the ladders are arguments
// rather than the per-series arrays read by subscript.
const (
	paramPairPrevPosBuckets = "ppb"
	paramPairCurrPosBuckets = "cpb"
	paramPairPrevNegBuckets = "pnb"
	paramPairCurrNegBuckets = "cnb"
)

// expHistogramPairBucketLadderArgs returns the extra lambda parameters
// and matching arrayMap ARGUMENTS that hand each consecutive sample pair
// its own two rows' stored bucket ladders, positionally aligned with the
// (prev, curr) row positions the caller pairs by popBack/popFront.
//
// # Why the ladders are arguments and not subscripts
//
// A per-pair mask is `arrayMap((prev, curr) -> verdict, popBack(rows),
// popFront(rows))`, and the verdict needs each row's Positive and
// Negative bucket arrays. Reading them as `_hq_pos_buckets[curr]` makes
// the group's whole Array(Array) ladder a CAPTURE of that lambda, and
// ClickHouse materialises a captured column once per element of the
// enclosing array — so a window of n samples rebuilds the entire n-row
// ladder n-1 times, once per pair. That is the same lever
// [expHistogramDenseContribsExpr] found one level further in (cerberus
// issue #3178, where the capture was per TARGET BUCKET), and after that
// fix it was what remained.
//
// Measured on a real ClickHouse 26.6.4 against cerberus's own
// `cerberus_queries_duration_exp_hist` telemetry (10 series, ~99 stored
// buckets, ~30 samples per 5m window), a 21-anchor
// `histogram_quantile(0.95, sum by(...) (rate(X[5m])))` peaked at
// 432.85 MiB; the same query with the mask projection spliced to a
// constant peaks at 57.83 MiB, so the mask was 375 MiB of it. Nothing
// INSIDE the comparison accounted for any of that: hoisting
// `length(<ladder>[curr])` out of the per-target lambda left it at
// 432.82 MiB, shrinking the dense target range from ~157 elements to
// four left it at 432.49 MiB, and replacing the folded bucket array with
// a one-element constant left it at 432.80 MiB. Replacing the ENTIRE
// dense comparison with a bare
// `arrayExists(..., <ladder>[curr], <ladder>[curr])` — one subscript per
// pair and nothing else — still cost 429.71 MiB, while deleting the
// bucket half of the verdict outright cost 57.64 MiB. Reaching the array
// WAS the cost. Handing the two ladders in as arguments takes the query
// to 156.32 MiB. Cerberus issue #3239.
//
// # Why sorting is cheaper than gathering by position
//
// The pairing is over the timestamp-sorted permutation, so the arguments
// must be in that same order. [expHistogramSortRowsByKeyExpr] — the same
// two-argument `arraySort` the across-series merge stage already orders
// its collected arrays with — takes both arrays as arguments and captures
// nothing, and it yields the SAME permutation the mask's
// `arraySort((rp, rt) -> rt, arrayEnumerate(<ts>), <ts>)` yields, because
// ClickHouse derives the permutation from the comparator's values alone
// and those values are that one ts array in both spellings. Gathering
// instead — `arrayMap(p -> <list>[p], <positions>)` — would reintroduce
// the very capture this removes, one per ROW rather than one per pair.
// TestExpHistogramPairLadder_ChDB_SortAgreesWithPositionPermutation
// asserts that agreement against the substrate over tie-heavy keys.
func expHistogramPairBucketLadderArgs() (params []string, args []chplan.Expr) {
	sorted := func(alias string) chplan.Expr {
		return expHistogramSortRowsByKeyExpr(
			&chplan.ColumnRef{Name: alias},
			&chplan.ColumnRef{Name: hqWindowTsListAlias},
		)
	}
	prevOf := func(alias string) chplan.Expr {
		return &chplan.FuncCall{Fn: chplan.FnArrayPopBack, Args: []chplan.Expr{sorted(alias)}}
	}
	currOf := func(alias string) chplan.Expr {
		return &chplan.FuncCall{Fn: chplan.FnArrayPopFront, Args: []chplan.Expr{sorted(alias)}}
	}
	return []string{
			paramPairPrevPosBuckets, paramPairCurrPosBuckets,
			paramPairPrevNegBuckets, paramPairCurrNegBuckets,
		}, []chplan.Expr{
			prevOf(hqAggPosBucketsArrayAlias), currOf(hqAggPosBucketsArrayAlias),
			prevOf(hqAggNegBucketsArrayAlias), currOf(hqAggNegBucketsArrayAlias),
		}
}

// expHistogramResetMaskFor names the reset-mask column for an
// exponential-histogram window reduced by windowFn, or nil for a windowFn
// that does not read it.
//
// Only a COUNTER window has resets to detect: `sum_over_time` reads its
// samples as plain values and the #1692 bare-selector sentinel reduces to
// one sample. Gating on that keeps the mask — a rows x buckets expression
// — out of the emitted SQL of every native-histogram query that would
// never consult it, exactly as needsTemporalityAgg keeps the temporality
// aggregate out of theirs.
//
// The same answer decides both halves of the mechanism, so a fold that
// reads the column and a stage that projects it cannot disagree about
// whether it exists: see expHistogramWindowReshape, which takes this
// function's result and builds the layer if and only if it is non-nil.
func expHistogramResetMaskFor(windowFn string) chplan.Expr {
	if !counterWindowFn(windowFn) {
		return nil
	}
	return &chplan.ColumnRef{Name: hqWindowResetsAlias}
}

// expHistogramResetMaskStage projects the reset mask alongside everything
// the per-series reshape above it reads: the grouping's own key columns
// and every aggregate it collected, forwarded by name.
//
// Forwarding is derived from the caller's own `aggs` list rather than
// spelled out, so an aggregate added for one path (Sum, which only the
// histogram-VALUED paths collect) cannot be left behind here.
func expHistogramResetMaskStage(input chplan.Node, aggs []chplan.AggFunc, keyAliases []string, densified bool) chplan.Node {
	projs := make([]chplan.Projection, 0, len(keyAliases)+len(aggs)+1)
	for _, name := range keyAliases {
		projs = append(projs, chplan.Projection{Expr: &chplan.ColumnRef{Name: name}, Alias: name})
	}
	for _, agg := range aggs {
		projs = append(projs, chplan.Projection{Expr: &chplan.ColumnRef{Name: agg.Alias}, Alias: agg.Alias})
	}
	return &chplan.Project{
		Input: input,
		Projections: append(projs, chplan.Projection{
			Expr:  expHistogramResetMaskExpr(densified),
			Alias: hqWindowResetsAlias,
		}),
	}
}

// expHistogramResetMaskExpr renders the mask itself over the groupArrays
// the per-series grouping collected.
//
// The rows are put in timestamp order once, as a permutation of POSITIONS
// rather than by sorting each list: `arraySort((rp, rt) -> rt,
// arrayEnumerate(<ts>), <ts>)` sorts the row positions by the very key
// array [counterIncreaseFold] sorts its own values by, so the two orders
// are the same permutation by construction — CH derives the permutation
// from the lambda's values alone, which are that key array in both cases.
// Every SCALAR list is then read through those positions, which keeps a
// parallel sort per list out of the emitted SQL.
//
// The two BUCKET ladders are the exception: they are sorted directly and
// handed to the pair lambda as ARGUMENTS, because a subscript of an
// Array(Array) column from inside that lambda is a capture ClickHouse
// rebuilds once per pair — see [expHistogramPairBucketLadderArgs]. A
// scalar list captured the same way costs one machine word per pair and
// is not worth a sort.
//
// The mask's j-th element compares the pair (positions[j], positions[j+1])
// — hence popBack against popFront, the same pairing the fold applies to
// its own time-sorted values.
func expHistogramResetMaskExpr(densified bool) chplan.Expr {
	tsList := chplan.Expr(&chplan.ColumnRef{Name: hqWindowTsListAlias})
	orderedRows := &chplan.FuncCall{Fn: chplan.FnArraySort, Args: []chplan.Expr{
		&chplan.Lambda{
			Params: []string{paramResetRowPos, paramResetRowTime},
			Body:   &chplan.BareIdent{Name: paramResetRowTime},
		},
		&chplan.FuncCall{Fn: chplan.FnArrayEnumerate, Args: []chplan.Expr{tsList}},
		tsList,
	}}

	ladderParams, ladderArgs := expHistogramPairBucketLadderArgs()
	return hqLet(paramResetOrderedRows, orderedRows, func(rows chplan.Expr) chplan.Expr {
		args := []chplan.Expr{
			&chplan.Lambda{
				Params: append([]string{paramResetPrevRow, paramResetCurrRow}, ladderParams...),
				Body:   expHistogramResetVerdictExpr(densified),
			},
			&chplan.FuncCall{Fn: chplan.FnArrayPopBack, Args: []chplan.Expr{rows}},
			&chplan.FuncCall{Fn: chplan.FnArrayPopFront, Args: []chplan.Expr{rows}},
		}
		return &chplan.FuncCall{Fn: chplan.FnArrayMap, Args: append(args, ladderArgs...)}
	})
}

// expHistogramResetVerdictExpr renders `DetectReset` for ONE consecutive
// pair, with the pair's two row positions bound to paramResetPrevRow /
// paramResetCurrRow by the caller's lambda.
//
// The bucket comparison rescales both rows onto the coarser of the
// PAIR's own two scales — the CURRENT row's wherever the comparison can
// decide anything — never onto the window's merged
// (coarsest-of-everyone) scale, matching reference's own "prev
// reconciled to the current schema" (cerberus issue #2095). A LATER sample coarsening the window's
// merged scale therefore cannot hide an EARLIER pair's fine-scale
// regression: each pair answers entirely from its own two rows, so it
// cannot see any OTHER row's scale at all.
func expHistogramResetVerdictExpr(densified bool) chplan.Expr {
	prev := chplan.Expr(&chplan.BareIdent{Name: paramResetPrevRow})
	curr := chplan.Expr(&chplan.BareIdent{Name: paramResetCurrRow})

	// One list read at both of the pair's positions, compared with `op`.
	pairwise := func(list chplan.Expr, op chplan.BinaryOp) chplan.Expr {
		return &chplan.Binary{
			Op:    op,
			Left:  &chplan.Subscript{Container: list, Key: curr},
			Right: &chplan.Subscript{Container: list, Key: prev},
		}
	}
	regressed := func(alias string) chplan.Expr {
		return pairwise(&chplan.ColumnRef{Name: alias}, chplan.OpLt)
	}

	return orAllExpr(
		regressed(hqWindowCountArrayAlias),
		regressed(hqWindowZeroCountsArrayAlias),
		// A resolution INCREASE only: a coarser schema is a lossless
		// fold of a finer one and happens without a restart, which is
		// why reference tests `>` rather than `!=`.
		pairwise(&chplan.ColumnRef{Name: hqAggScalesArrayAlias}, chplan.OpGt),
		expHistogramResetPairBucketRegressedExpr(
			hqAggPosOffsetsArrayAlias, paramPairPrevPosBuckets, paramPairCurrPosBuckets, prev, curr, densified,
		),
		expHistogramResetPairBucketRegressedExpr(
			hqAggNegOffsetsArrayAlias, paramPairPrevNegBuckets, paramPairCurrNegBuckets, prev, curr, densified,
		),
	)
}

// orAllExpr disjoins its arguments, which are the independent conditions
// `DetectReset` returns true on. It takes at least one so the fold has a
// seed and the "no conditions at all" shape — which would have to mean a
// constant — cannot be spelled.
func orAllExpr(first chplan.Expr, rest ...chplan.Expr) chplan.Expr {
	out := first
	for _, e := range rest {
		out = &chplan.Binary{Op: chplan.OpOr, Left: out, Right: e}
	}
	return out
}

// expHistogramResetPairBucketRegressedExpr reports whether ANY bucket of
// one signed ladder — its per-row offsets named by offArrAlias, its two
// bucket arrays bound to the pair lambda's prevBucParam / currBucParam by
// [expHistogramPairBucketLadderArgs] — regressed between prev and curr,
// comparing the two rows
// at the coarser of the PAIR's own two scales — CURR's wherever the
// comparison can decide anything, which is reference's "prev reconciled
// to the current schema" (cerberus issue #2095; see
// expHistogramResetVerdictExpr, and the `pairScale` binding below for
// why the clamp is spelled rather than assumed).
//
// Unlike the mask's other components, this reads only the ONE pair's own
// two rows rather than a per-series array, so it needs its own small
// bound + rescale exactly like [expHistogramMergeBucketsExpr] computes
// for a whole group — but over an ad hoc 2-row "group" built from prev
// and curr alone via `array(prevX, currX)`. Sharing
// [expHistogramMergeBucketsBoundsExpr] and [expHistogramBucketRowContribExpr]
// with the merge and the window fold is what keeps all three agreeing
// bucket-for-bucket about which stored position lands where.
//
// This is linear in the PAIR's own bucket span, not the window's: for N
// rows there are N-1 pairs, each rescaling only its own two rows, so the
// total cost is the same order as the single window-wide rescale the
// mask used before — never a rows × rows × buckets blowup.
//
// `densified` selects between the two renderings of that comparison —
// the dense per-target arrays this file's header describes, or the
// per-target-bucket picker + Kahan fold they replaced. The two fold the
// identical slices; see [ExpHistogramResetMaskLowerer] for why the
// superseded one is kept.
func expHistogramResetPairBucketRegressedExpr(
	offArrAlias, prevBucParam, currBucParam string, prev, curr chplan.Expr, densified bool,
) chplan.Expr {
	scalesArr := chplan.Expr(&chplan.ColumnRef{Name: hqAggScalesArrayAlias})
	offArr := chplan.Expr(&chplan.ColumnRef{Name: offArrAlias})

	at := func(list, pos chplan.Expr) chplan.Expr {
		return &chplan.Subscript{Container: list, Key: pos}
	}
	prevScale, currScale := at(scalesArr, prev), at(scalesArr, curr)
	prevOff, currOff := at(offArr, prev), at(offArr, curr)
	// The pair's two bucket ladders arrive as lambda ARGUMENTS, not as a
	// subscript of the group's per-row array — see
	// [expHistogramPairBucketLadderArgs].
	prevBuc := chplan.Expr(&chplan.BareIdent{Name: prevBucParam})
	currBuc := chplan.Expr(&chplan.BareIdent{Name: currBucParam})

	// The scale both rows are reconciled onto is the COARSER of the pair,
	// not curr's alone. For the pair this comparison can decide it is
	// curr's — reference reconciles "prev to the current schema", and a
	// pair reaching a verdict here has prev at least as coarse as curr, so
	// least(prev, curr) IS curr. It differs only where curr is FINER than
	// prev, and that pair is already condemned unconditionally by the
	// `scales[curr] > scales[prev]` term this expression is OR-ed with
	// (see expHistogramResetVerdictExpr), so no verdict moves.
	//
	// What does move is whether the pair can be EVALUATED at all.
	// Downscaling is only ever lossless downward, so every shift in
	// expHistogramBucketSliceBoundsExpr and
	// expHistogramMergeBucketsBoundsExpr is `rowScale - mergedScale` and
	// ClickHouse rejects a negative shift outright ("The number of shift
	// positions needs to be a non-negative value"). Reconciling onto
	// curr's scale alone spells exactly that shift for a resolution
	// INCREASE, leaving the mask's evaluability resting on ClickHouse
	// masking the condemned element out under short_circuit_function_
	// evaluation — which it does for some renderings of this expression
	// and not others. Taking the coarser scale removes the negative shift
	// from the expression instead of relying on it never being reached.
	pairScale := leastExpr(prevScale, currScale)

	pairArray := func(a, b chplan.Expr) chplan.Expr {
		return &chplan.FuncCall{Fn: chplan.FnArray, Args: []chplan.Expr{a, b}}
	}
	// `start` is read by the length AND by both of the two contributions
	// compared below, and those two sit inside the per-target-bucket
	// arrayExists lambda — so an unbound `start` would be re-evaluated
	// twice per target index. Binding it once outside the lambda, via
	// expHistogramOverMergedBucketRangeExpr, makes all three reads a bare
	// identifier.
	return expHistogramOverMergedBucketRangeExpr(
		pairArray(prevScale, currScale), pairArray(prevOff, currOff), pairArray(prevBuc, currBuc),
		pairScale,
		func(start, length chplan.Expr) chplan.Expr {
			if densified {
				// Both sides become dense per-target arrays built OUTSIDE
				// any per-target lambda, so the comparison below reads
				// them as arrayExists ARGUMENTS and captures nothing. See
				// expHistogramDenseContribsExpr for the measurement that
				// makes this the shipped rendering.
				dense := func(rowScale, rowOff, rowBuc chplan.Expr) chplan.Expr {
					return expHistogramDenseContribsExpr(rowScale, rowOff, rowBuc, pairScale, start, length)
				}
				return &chplan.FuncCall{Fn: chplan.FnArrayExists, Args: []chplan.Expr{
					&chplan.Lambda{
						Params: []string{paramResetDenseCurr, paramResetDensePrev},
						Body: &chplan.Binary{
							Op:    chplan.OpLt,
							Left:  &chplan.BareIdent{Name: paramResetDenseCurr},
							Right: &chplan.BareIdent{Name: paramResetDensePrev},
						},
					},
					dense(currScale, currOff, currBuc),
					dense(prevScale, prevOff, prevBuc),
				}}
			}
			// One row's contribution at target absolute index `start + rk`,
			// rescaled from that row's OWN scale to currScale. Binding
			// (paramExpRowScale, paramExpRowOffset, paramExpRowBuckets) via
			// hqLet — rather than an arrayMap over a parallel array, which is
			// what every other caller of expHistogramBucketRowContribExpr does —
			// evaluates the ONE row's contribution exactly once per target
			// index, matching hqLet's own "one evaluation" contract.
			contribAt := func(rowScale, rowOff, rowBuc chplan.Expr) chplan.Expr {
				return hqLet(paramExpRowScale, rowScale, func(chplan.Expr) chplan.Expr {
					return hqLet(paramExpRowOffset, rowOff, func(chplan.Expr) chplan.Expr {
						return hqLet(paramExpRowBuckets, rowBuc, func(chplan.Expr) chplan.Expr {
							return expHistogramBucketRowContribExpr(pairScale, start, paramResetTargetBucket)
						})
					})
				})
			}

			return &chplan.FuncCall{Fn: chplan.FnArrayExists, Args: []chplan.Expr{
				&chplan.Lambda{
					Params: []string{paramResetTargetBucket},
					Body: &chplan.Binary{
						Op:    chplan.OpLt,
						Left:  contribAt(currScale, currOff, currBuc),
						Right: contribAt(prevScale, prevOff, prevBuc),
					},
				},
				&chplan.FuncCall{Fn: chplan.FnRange, Args: []chplan.Expr{
					&chplan.FuncCall{Fn: chplan.FnToUInt64, Args: []chplan.Expr{length}},
				}},
			}}
		},
	)
}

// ExpHistogramResetMaskLowerer decides how the per-pair bucket-regression
// half of the counter-reset mask is rendered.
//
// Like [ExpHistogramWindowFoldLowerer], and unlike the operator-facing
// strategies in [RangeLowerers], this one is not a capability and carries
// no chopt feature id: both arms fold the identical stored-count slices
// (see [expHistogramDenseContribsExpr]), so there is nothing for a
// deployment to choose. It exists so a test can run one query through
// both renderings and compare — see
// [DensifiedExpHistogramResetMaskLowerer] and
// [PerTargetExpHistogramResetMaskLowerer].
type ExpHistogramResetMaskLowerer interface {
	// DensifiedResetMaskEligible reports whether the mask may build both
	// sides of each pair comparison as dense per-target arrays outside
	// the comparison lambda.
	DensifiedResetMaskEligible() bool
}

// DensifiedExpHistogramResetMaskLowerer is the DEFAULT: densify both
// sides of every pair comparison through
// [expHistogramDenseContribsExpr].
type DensifiedExpHistogramResetMaskLowerer struct{}

// DensifiedResetMaskEligible returns true.
func (DensifiedExpHistogramResetMaskLowerer) DensifiedResetMaskEligible() bool { return true }

// PerTargetExpHistogramResetMaskLowerer keeps the per-target-bucket
// picker + Kahan fold ([expHistogramBucketRowContribExpr]) the densified
// rendering replaced.
//
// It is the differential oracle, not a fallback: no deployment wires it,
// because there is no shape the two arms answer differently. A test
// selects it to obtain the same query rendered the old way.
type PerTargetExpHistogramResetMaskLowerer struct{}

// DensifiedResetMaskEligible returns false.
func (PerTargetExpHistogramResetMaskLowerer) DensifiedResetMaskEligible() bool { return false }

// expHistogramDensifiedResetMaskEligible reads the ExpHistogramResetMask
// strategy off a lowering table, treating an UNRESOLVED table — one whose
// caller never ran [RangeLowerers.withDefaults] — as the per-target
// reading rather than dereferencing a nil interface. This mirrors
// [expHistogramClosedFormEligible]'s own posture, for the same reason:
// several unit tests reach the window-input builders through a hand-built
// lowerCtx carrying no table at all.
func expHistogramDensifiedResetMaskEligible(l RangeLowerers) bool {
	if l.ExpHistogramResetMask == nil {
		return false
	}
	return l.ExpHistogramResetMask.DensifiedResetMaskEligible()
}
