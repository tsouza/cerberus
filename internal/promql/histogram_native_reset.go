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
// The bucket comparison is made at EACH PAIR's own scale — the current
// row's own, matching reference's "prev reconciled to the current
// schema" — never at the window's merged (coarsest-of-everyone) scale.
// Comparing at the merged scale instead (cerberus issue #2095, fixed by
// [expHistogramResetPairBucketRegressedExpr]) let a LATER sample's
// coarser scale pull the comparison scale below an EARLIER pair's own,
// so a fine-bucket regression the coarser bucket absorbed stopped being
// visible: reference condemned the sample, the merged-scale reading did
// not. Rescaling only the pair's own two rows — not the whole window —
// is also cheaper: the cost is linear in the PAIR's bucket span, not the
// window's.
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
func expHistogramResetMaskStage(input chplan.Node, aggs []chplan.AggFunc, keyAliases []string) chplan.Node {
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
			Expr:  expHistogramResetMaskExpr(),
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
// Every list is then read through those positions, which is also what
// keeps six parallel sorts out of the emitted SQL.
//
// The mask's j-th element compares the pair (positions[j], positions[j+1])
// — hence popBack against popFront, the same pairing the fold applies to
// its own time-sorted values.
func expHistogramResetMaskExpr() chplan.Expr {
	tsList := chplan.Expr(&chplan.ColumnRef{Name: hqWindowTsListAlias})
	orderedRows := &chplan.FuncCall{Fn: chplan.FnArraySort, Args: []chplan.Expr{
		&chplan.Lambda{
			Params: []string{paramResetRowPos, paramResetRowTime},
			Body:   &chplan.BareIdent{Name: paramResetRowTime},
		},
		&chplan.FuncCall{Fn: chplan.FnArrayEnumerate, Args: []chplan.Expr{tsList}},
		tsList,
	}}

	return hqLet(paramResetOrderedRows, orderedRows, func(rows chplan.Expr) chplan.Expr {
		return &chplan.FuncCall{Fn: chplan.FnArrayMap, Args: []chplan.Expr{
			&chplan.Lambda{
				Params: []string{paramResetPrevRow, paramResetCurrRow},
				Body:   expHistogramResetVerdictExpr(),
			},
			&chplan.FuncCall{Fn: chplan.FnArrayPopBack, Args: []chplan.Expr{rows}},
			&chplan.FuncCall{Fn: chplan.FnArrayPopFront, Args: []chplan.Expr{rows}},
		}}
	})
}

// expHistogramResetVerdictExpr renders `DetectReset` for ONE consecutive
// pair, with the pair's two row positions bound to paramResetPrevRow /
// paramResetCurrRow by the caller's lambda.
//
// The bucket comparison rescales the PREVIOUS row down to the CURRENT
// row's own scale — never to the window's merged (coarsest-of-everyone)
// scale — matching reference's own "prev reconciled to the current
// schema" (cerberus issue #2095). A LATER sample coarsening the window's
// merged scale therefore cannot hide an EARLIER pair's fine-scale
// regression: each pair answers entirely from its own two rows, so it
// cannot see any OTHER row's scale at all.
func expHistogramResetVerdictExpr() chplan.Expr {
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
		expHistogramResetPairBucketRegressedExpr(hqAggPosOffsetsArrayAlias, hqAggPosBucketsArrayAlias, prev, curr),
		expHistogramResetPairBucketRegressedExpr(hqAggNegOffsetsArrayAlias, hqAggNegBucketsArrayAlias, prev, curr),
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
// one signed ladder (Positive or Negative, named by offArrAlias /
// bucArrAlias) regressed between prev and curr, comparing the two rows
// at CURR's own scale — reference's "prev reconciled to the current
// schema" (cerberus issue #2095; see expHistogramResetVerdictExpr).
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
func expHistogramResetPairBucketRegressedExpr(offArrAlias, bucArrAlias string, prev, curr chplan.Expr) chplan.Expr {
	scalesArr := chplan.Expr(&chplan.ColumnRef{Name: hqAggScalesArrayAlias})
	offArr := chplan.Expr(&chplan.ColumnRef{Name: offArrAlias})
	bucArr := chplan.Expr(&chplan.ColumnRef{Name: bucArrAlias})

	at := func(list, pos chplan.Expr) chplan.Expr {
		return &chplan.Subscript{Container: list, Key: pos}
	}
	prevScale, currScale := at(scalesArr, prev), at(scalesArr, curr)
	prevOff, currOff := at(offArr, prev), at(offArr, curr)
	prevBuc, currBuc := at(bucArr, prev), at(bucArr, curr)

	pairArray := func(a, b chplan.Expr) chplan.Expr {
		return &chplan.FuncCall{Fn: chplan.FnArray, Args: []chplan.Expr{a, b}}
	}
	start, length := expHistogramMergeBucketsBoundsExpr(
		pairArray(prevScale, currScale), pairArray(prevOff, currOff), pairArray(prevBuc, currBuc),
		currScale,
	)

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
					return expHistogramBucketRowContribExpr(currScale, start, paramResetTargetBucket)
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
}
