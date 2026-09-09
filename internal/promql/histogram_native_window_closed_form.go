package promql

import (
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_window_closed_form.go replaces the per-target-bucket
// counter fold for rate()/increase() over an exponential histogram with
// its closed form, so the window's per-bucket work stops being linear in
// the number of in-window SAMPLES.
//
// # The cost this exists to remove (cerberus issue #3178)
//
// [expHistogramWindowBucketsExpr] evaluates its fold once per target
// bucket, and [counterIncreaseFold] reads EVERY in-window sample's
// contribution at that bucket. The per-(row x target) product is what
// ClickHouse charges for, essentially regardless of what the per-element
// body contains: measured against a real ClickHouse 26.6.4 on cerberus's
// own `cerberus_queries_duration_exp_hist` telemetry shape (7 series of
// 105-155 stored buckets, 21 samples per 5m window, a 21-anchor
// query_range), peak memory is 1684.6 MiB — over the 1 GiB
// CERBERUS_CH_QUERY_MAX_MEMORY default, which is what made the compose
// self-monitoring dashboard's own P95 panel answer 422. That cost is
// linear in the sample count (5 samples 309.1 MiB, 10 samples 676.8 MiB,
// 21 samples 1684.6 MiB) and does NOT respond to restructuring the body:
// transposing the loops, densifying the per-row ladder in linear time,
// and dropping the Kahan compensation each measured within 0.2% of
// baseline. Narrowing the ROW SET is what moves this fold — a throwaway
// two-rows-per-anchor probe measured 85.2 MiB, a 19.8x drop.
//
// Every one of those body rewrites kept the TARGET loop on the outside,
// which is why none of them moved: what ClickHouse charges for is the
// per-row arrays being CAPTURED by that loop, not the arithmetic in it.
// Moving the loop inside — folding each row's whole ladder in one
// `arrayReduceInRanges` call that takes the row as an argument — does
// move it, by the same lever [expHistogramDenseContribsExpr] found for
// the reset mask. See [expHistogramWindowClosedFormBucketsExpr], which is
// what the ladders render through today.
//
// # Correction: that 19.8x is this fold's own, not the query's
//
// An earlier revision of this paragraph said row-set narrowing was "the
// ONLY lever that moves it", with the 19.8x as the whole query's. Both
// readings were wrong, and the wrongness was load-bearing: after this
// file shipped, the panel it was written for STILL answered 422. Two
// corrections, each measured against the same ClickHouse 26.6.4 and the
// same live telemetry:
//
//   - The 19.8x is this fold's, not the query's. The residual cost was
//     never in the ladders at all — it was the counter-reset mask
//     (histogram_native_reset.go), a SEPARATE layer this fold's
//     coefficients read but do not render. With that mask replaced by a
//     constant the query peaked at 152 MiB; with it, 912 MiB.
//   - "The only lever" does not generalise past this fold. The mask's
//     own cost did not respond to row-set narrowing (it must see every
//     pair to decide which rows are retainable here) and did not respond
//     to cardinality either: shrinking its per-pair target range from
//     ~110 elements to FOUR left it at 898 MiB. What moved it was
//     removing ARRAY CAPTURES from its innermost lambda at unchanged
//     cardinality — see [expHistogramDenseContribsExpr].
//
// Both levers are live and both are needed: with the mask densified,
// rendering these ladders through [counterIncreaseFold] instead of the
// closed form took the same panel query from 251 MiB back to 958 MiB.
//
// # Why both temporality branches are linear in the per-row values
//
// [counterIncreaseFold] answers with one of two shapes, chosen by the
// series' own AggregationTemporality:
//
//	CUMULATIVE:  sum over consecutive pairs of (reset ? v_{j+1} : v_{j+1} - v_j)
//	DELTA:       sum(v) - v_earliest
//
// Both are LINEAR COMBINATIONS of the same per-row values, so both are
// `sum_j c_j * v_j` for a coefficient vector that depends only on the
// row ORDER, the reset mask and the temporality — never on which bucket
// is being folded. Telescoping the cumulative shape gives
//
//	c_i = [i = n] - [i = 1] + [the pair opening at i is a reset]
//
// (a reset at the FIRST pair cancels the -1, which is correct: reference
// then answers with v_n alone), and the delta shape gives c_i = [i != 1].
//
// So the whole coefficient vector is a per-SERIES quantity —
// [expHistogramWindowCoefficientStage] computes it once per group, in a
// layer of its own, exactly as the reset mask and the extrapolation
// factor already are. Every row whose coefficient is zero contributes
// nothing to any bucket and is dropped there too, which takes the row
// count the ladders fold from "every in-window sample" down to two plus
// one per reset for a cumulative counter — the overwhelmingly common
// shape.
//
// # Why this is bit-identical, not merely equal
//
// The values folded here are the stored UInt64 bucket counts
// ([expHistogramWindowAggs] collects them straight off the physical
// column), so every partial sum on either side of the rewrite is an exact
// integer in Float64 and re-associating the sum cannot change a bit. That
// is the same exactness argument this package already relies on wherever
// it folds stored counts, and it is why this rewrite is confined to the
// BUCKET ladders: Count, Sum and ZeroCount keep [counterIncreaseFold]
// verbatim. Sum in particular is a genuine float, where re-association
// would be observable, and those three are per-row scalars anyway — they
// cost nothing per bucket, which is the only cost this file is about.
//
// The DELTA branch is preserved rather than dropped: it is the same
// coefficient mechanism with its own vector, so a delta-temporality
// series still reads every sample it needs. Its vector is non-zero on all
// but the earliest row, which is not an implementation gap — a delta
// sample carries the increment itself, so there is nothing to cancel and
// nothing to narrow. What that costs is a separate question from what it
// retains, and the answer is now "nothing":
// [expHistogramWindowClosedFormBucketsExpr] loops over ROWS rather than
// over target buckets, so the ladders no longer pay per row per target
// for either vector (cerberus issue #3234).

const (
	// hqWindowCoeffsAlias holds the per-retained-row counter-fold
	// coefficients — see this file's header for the two vectors and why
	// they are a per-series quantity.
	hqWindowCoeffsAlias = "_hq_win_coeffs"

	// hqWindowNarrowedPrefix prefixes the narrowed copy of each per-row
	// groupArray column the bucket ladders read.
	//
	// They are projected as COLUMNS rather than derived where they are
	// read: the read sits inside the per-target-bucket arrayMap, so an
	// inline `arrayMap(p -> full[p], keep)` would be re-evaluated once per
	// target bucket — re-materialising the FULL array it was supposed to
	// narrow, at exactly the cardinality this whole file exists to avoid.
	// Measured: narrowing inline left peak memory at 1686.2 MiB, i.e.
	// entirely unchanged from the 1684.6 MiB baseline.
	hqWindowNarrowedPrefix = "_hq_win_"
)

// expHistogramWindowNarrowedAliases names the per-row groupArray columns
// the bucket ladders read, and therefore the ones
// [expHistogramWindowCoefficientStage] projects a narrowed copy of.
func expHistogramWindowNarrowedAliases() []string {
	return []string{
		hqAggScalesArrayAlias,
		hqAggPosOffsetsArrayAlias,
		hqAggPosBucketsArrayAlias,
		hqAggNegOffsetsArrayAlias,
		hqAggNegBucketsArrayAlias,
	}
}

// expHistogramWindowNarrowedAlias is the narrowed counterpart of one
// per-row column's alias.
func expHistogramWindowNarrowedAlias(alias string) string {
	return hqWindowNarrowedPrefix + alias
}

// Lambda parameter names for the coefficient construction. All of them
// bind per-ROW quantities, at O(in-window samples) cardinality.
const (
	paramWinOrderPos   = "wcp"
	paramWinOrderTime  = "wct"
	paramWinOrderIndex = "wci"
	paramWinResetTerm  = "wcr"
	paramWinCoeffs     = "wcc"
	paramWinCoeff      = "wcx"
	paramWinKeepPos    = "wck"
)

// expHistogramWindowClosedFormApplies reports whether this file's rewrite
// covers windowFn.
//
// rate/increase only: they are the shapes whose fold is
// [counterIncreaseFold] under a per-target-bucket arrayMap. irate/idelta
// reduce through [instantHistogramFold] (a different, already
// two-sample shape), delta through [gaugeEndpointDeltaFold], and
// sum_over_time / the bare-selector sentinel are not counters at all.
//
// resets is [expHistogramResetMaskFor]'s answer for the same windowFn:
// nil means no mask layer was built, and the coefficient vector's reset
// term has nothing to read.
func expHistogramWindowClosedFormApplies(windowFn string, resets chplan.Expr) bool {
	if resets == nil {
		return false
	}
	return windowFn == rateWindowFn || windowFn == increaseWindowFn
}

// expHistogramWindowCoefficientExpr renders the per-row coefficient
// vector, in timestamp order, for the window's rows.
//
// orderedRows is the row-position permutation that puts the groupArray
// columns in timestamp order — the SAME permutation
// [expHistogramResetMaskExpr] derives, from the same timestamp list, so
// element i of the mask answers for the pair opening at element i here.
func expHistogramWindowCoefficientExpr(orderedRows, resets, temporality chplan.Expr) chplan.Expr {
	rowCount := &chplan.FuncCall{Fn: chplan.FnLength, Args: []chplan.Expr{orderedRows}}
	index := &chplan.BareIdent{Name: paramWinOrderIndex}
	isFirst := &chplan.Binary{Op: chplan.OpEq, Left: index, Right: &chplan.LitInt{V: 1}}
	isLast := &chplan.Binary{Op: chplan.OpEq, Left: index, Right: rowCount}
	oneIf := func(cond chplan.Expr) chplan.Expr {
		return &chplan.FuncCall{Fn: chplan.FnIf, Args: []chplan.Expr{cond, &chplan.LitInt{V: 1}, &chplan.LitInt{V: 0}}}
	}

	// The mask holds one verdict per PAIR, so it is one element short of
	// the row count; padding it with a trailing zero aligns element i with
	// row i and leaves the final row (which opens no pair) with no reset
	// term, without an indexed read that would replicate the mask per
	// element.
	resetTerm := &chplan.FuncCall{Fn: chplan.FnArrayConcat, Args: []chplan.Expr{
		&chplan.FuncCall{Fn: chplan.FnArrayMap, Args: []chplan.Expr{
			&chplan.Lambda{Params: []string{paramWinResetTerm}, Body: oneIf(&chplan.BareIdent{Name: paramWinResetTerm})},
			resets,
		}},
		&chplan.FuncCall{Fn: chplan.FnArray, Args: []chplan.Expr{&chplan.LitInt{V: 0}}},
	}}

	cumulative := &chplan.FuncCall{Fn: chplan.FnArrayMap, Args: []chplan.Expr{
		&chplan.Lambda{
			Params: []string{paramWinOrderIndex, paramWinResetTerm},
			Body: addExpr(
				subExpr(oneIf(isLast), oneIf(isFirst)),
				&chplan.BareIdent{Name: paramWinResetTerm},
			),
		},
		&chplan.FuncCall{Fn: chplan.FnArrayEnumerate, Args: []chplan.Expr{orderedRows}},
		resetTerm,
	}}
	if temporality == nil {
		return cumulative
	}

	delta := &chplan.FuncCall{Fn: chplan.FnArrayMap, Args: []chplan.Expr{
		&chplan.Lambda{Params: []string{paramWinOrderIndex}, Body: oneIf(&chplan.FuncCall{Fn: chplan.FnNot, Args: []chplan.Expr{isFirst}})},
		&chplan.FuncCall{Fn: chplan.FnArrayEnumerate, Args: []chplan.Expr{orderedRows}},
	}}
	// A scalar condition over two per-row vectors: the branch choice
	// happens once per group here, where counterIncreaseFold made it once
	// per target bucket.
	return &chplan.FuncCall{Fn: chplan.FnIf, Args: []chplan.Expr{
		&chplan.Binary{
			Op:    chplan.OpEq,
			Left:  temporality,
			Right: &chplan.LitInt{V: schema.AggregationTemporalityDelta},
		},
		delta,
		cumulative,
	}}
}

// expHistogramWindowCoefficientAliases names the columns
// [expHistogramWindowCoefficientStage] adds on top of the ones it
// forwards, so a caller can extend its own forwarding list without
// running the stage to find out. The stage returns the identical list —
// keeping the two in one function is what stops them drifting.
func expHistogramWindowCoefficientAliases(forwarded []string) []string {
	added := append([]string{}, forwarded...)
	for _, alias := range expHistogramWindowNarrowedAliases() {
		added = append(added, expHistogramWindowNarrowedAlias(alias))
	}
	return append(added, hqWindowCoeffsAlias)
}

// expHistogramWindowCoefficientStage projects [hqWindowCoeffsAlias] and
// [hqWindowKeepAlias] beside everything the layers above it read,
// forwarding the grouping's own key columns and every aggregate by name —
// the same shape [expHistogramResetMaskStage] uses, and it composes above
// that stage because the coefficients read its mask.
//
// It returns the aliases it added so the caller can carry them through
// [expHistogramWindowFactorStage]'s own forwarding list.
func expHistogramWindowCoefficientStage(
	input chplan.Node,
	aggs []chplan.AggFunc,
	keyAliases []string,
	extraAliases []string,
	resets, temporality chplan.Expr,
) (chplan.Node, []string) {
	tsList := chplan.Expr(&chplan.ColumnRef{Name: hqWindowTsListAlias})
	orderedRows := &chplan.FuncCall{Fn: chplan.FnArraySort, Args: []chplan.Expr{
		&chplan.Lambda{
			Params: []string{paramWinOrderPos, paramWinOrderTime},
			Body:   &chplan.BareIdent{Name: paramWinOrderTime},
		},
		&chplan.FuncCall{Fn: chplan.FnArrayEnumerate, Args: []chplan.Expr{tsList}},
		tsList,
	}}

	projs := make([]chplan.Projection, 0, len(keyAliases)+len(aggs)+len(extraAliases)+2)
	for _, name := range keyAliases {
		projs = append(projs, chplan.Projection{Expr: &chplan.ColumnRef{Name: name}, Alias: name})
	}
	for _, agg := range aggs {
		projs = append(projs, chplan.Projection{Expr: &chplan.ColumnRef{Name: agg.Alias}, Alias: agg.Alias})
	}
	for _, name := range extraAliases {
		projs = append(projs, chplan.Projection{Expr: &chplan.ColumnRef{Name: name}, Alias: name})
	}

	// The ordered permutation and the coefficients are each read by every
	// projection below, so both are bound once per group rather than
	// re-rendered per reader.
	withKeepAndCoeffs := func(body func(keep, coeffs chplan.Expr) chplan.Expr) chplan.Expr {
		return hqLet(paramWinOrderPos, orderedRows, func(ordered chplan.Expr) chplan.Expr {
			return hqLet(paramWinCoeffs, expHistogramWindowCoefficientExpr(ordered, resets, temporality), func(cs chplan.Expr) chplan.Expr {
				nonZero := func(param string) chplan.Expr {
					return &chplan.Binary{
						Op:    chplan.OpNe,
						Left:  &chplan.BareIdent{Name: param},
						Right: &chplan.LitInt{V: 0},
					}
				}
				keep := &chplan.FuncCall{Fn: chplan.FnArrayFilter, Args: []chplan.Expr{
					&chplan.Lambda{
						Params: []string{paramWinKeepPos, paramWinCoeff},
						Body:   nonZero(paramWinCoeff),
					},
					ordered,
					cs,
				}}
				kept := &chplan.FuncCall{Fn: chplan.FnArrayFilter, Args: []chplan.Expr{
					&chplan.Lambda{Params: []string{paramWinCoeff}, Body: nonZero(paramWinCoeff)},
					cs,
				}}
				return body(keep, kept)
			})
		})
	}

	for _, alias := range expHistogramWindowNarrowedAliases() {
		narrowedAlias := expHistogramWindowNarrowedAlias(alias)
		projs = append(projs, chplan.Projection{
			Alias: narrowedAlias,
			Expr: withKeepAndCoeffs(func(keep, _ chplan.Expr) chplan.Expr {
				return &chplan.FuncCall{Fn: chplan.FnArrayMap, Args: []chplan.Expr{
					&chplan.Lambda{
						Params: []string{paramWinKeepPos},
						Body: &chplan.Subscript{
							Container: &chplan.ColumnRef{Name: alias},
							Key:       &chplan.BareIdent{Name: paramWinKeepPos},
						},
					},
					keep,
				}}
			}),
		})
	}
	projs = append(projs, chplan.Projection{
		Alias: hqWindowCoeffsAlias,
		Expr:  withKeepAndCoeffs(func(_, coeffs chplan.Expr) chplan.Expr { return coeffs }),
	})
	return &chplan.Project{Input: input, Projections: projs},
		expHistogramWindowCoefficientAliases(extraAliases)
}

// expHistogramWindowClosedFormBucketsExpr renders ONE series'
// window-reduced bucket ladder at the group's merged scale, in closed
// form: the coefficient-weighted sum of the RETAINED rows' dense
// per-target contributions, scaled by rate/increase's own boundary-
// extrapolation factor exactly as [histogramWindowFold]'s rate branch
// scales [counterIncreaseFold]'s result.
//
// It replaces [expHistogramWindowBucketsExpr] for the closed form rather
// than supplying it a `fold`, because the two differ in which axis is the
// OUTER loop, not merely in how the inner values combine.
//
// # Why the per-target reading was not enough (cerberus issue #3234)
//
// [expHistogramWindowBucketsExpr] loops over TARGET buckets and, inside
// that loop, maps over the group's per-row scale/offset/bucket arrays.
// Those arrays are CAPTURED by the per-target lambda, and ClickHouse
// materialises a captured column once per element of the enclosing loop —
// the same cost [expHistogramDenseContribsExpr] was written to remove
// from the counter-reset mask. Narrowing the row set hid it for a
// CUMULATIVE counter, where the coefficient vector keeps two rows of ~20;
// it does nothing for a DELTA one, whose vector is non-zero on every row
// but the earliest.
//
// Measured against a real ClickHouse 26.6.4 on cerberus's own
// `cerberus_queries_duration_exp_hist` telemetry (7 series, ~110-155
// stored buckets, ~20 samples per 5m window), the panel query
// `histogram_quantile(0.95, sum by (cerberus_ql) (rate(…[5m])))` emitted
// by each rendering and then spliced so ONLY the temporality test
// changes — every other byte of the two queries being what cerberus
// itself emitted — with `read_rows` 16,384 on all eight runs:
//
//	step=15, 21 anchors    per-target      this reading
//	  cumulative branch      269.32 MiB      269.10 MiB
//	  delta branch forced    985.38 MiB      268.96 MiB
//
//	step=5, 61 anchors     per-target      this reading
//	  cumulative branch      905.56 MiB      905.33 MiB
//	  delta branch forced      3.33 GiB      905.20 MiB
//
// So the delta branch stops being a special case: the ladders now cost
// the same whether the coefficient vector retained two rows or all but
// one, and the residual — unchanged by this rewrite, and the same on both
// branches — belongs to a different layer. All four result sets are
// byte-identical to their per-target counterparts.
//
// # Why the answers do not move
//
// Each row's dense contribution folds exactly the slices
// [expHistogramBucketSliceBoundsExpr] names — the same slices the
// per-target reading hands to `arraySlice` — and the values are stored
// UInt64 bucket counts in the Float64 domain, so every partial sum on
// either side is an exact integer and re-associating them cannot change a
// bit. That is this file's header argument, applied to the second axis.
//
// The bounds still come from the FULL per-row arrays, exactly as
// [expHistogramWindowBucketsExpr] documents: this ladder's own offset is
// published separately by [expHistogramMergeOffsetExpr] over those same
// full arrays, so deriving the target range from the narrowed row set
// would shift the ladder against its own offset.
func expHistogramWindowClosedFormBucketsExpr(
	offArrAlias, bucArrAlias, scalesArrAlias, mergedScaleAlias string,
	factor chplan.Expr,
) chplan.Expr {
	mergedScale := chplan.Expr(&chplan.ColumnRef{Name: mergedScaleAlias})
	narrowed := func(alias string) chplan.Expr {
		return &chplan.ColumnRef{Name: expHistogramWindowNarrowedAlias(alias)}
	}
	return expHistogramOverMergedBucketRangeExpr(
		&chplan.ColumnRef{Name: scalesArrAlias},
		&chplan.ColumnRef{Name: offArrAlias},
		&chplan.ColumnRef{Name: bucArrAlias},
		mergedScale,
		func(mergedStart, mergedLength chplan.Expr) chplan.Expr {
			// One retained row's dense contribution, already multiplied by
			// that row's coefficient. Every lambda below binds per-ROW
			// quantities and captures only scalars.
			weightedRows := &chplan.FuncCall{Fn: chplan.FnArrayMap, Args: []chplan.Expr{
				&chplan.Lambda{
					Params: []string{paramExpRowScale, paramExpRowOffset, paramExpRowBuckets, paramWinCoeff},
					Body: scaleArrayExpr(
						paramWinCoeffScaled,
						&chplan.BareIdent{Name: paramWinCoeff},
						expHistogramDenseContribsExpr(
							&chplan.BareIdent{Name: paramExpRowScale},
							&chplan.BareIdent{Name: paramExpRowOffset},
							&chplan.BareIdent{Name: paramExpRowBuckets},
							mergedScale, mergedStart, mergedLength,
						),
					),
				},
				narrowed(scalesArrAlias), narrowed(offArrAlias), narrowed(bucArrAlias),
				&chplan.ColumnRef{Name: hqWindowCoeffsAlias},
			}}
			// sumForEach adds the retained rows position by position. Its
			// result is as long as the longest array it folded, so it is
			// EMPTY for a group that retained no row at all; arrayResize
			// pins the ladder to the target range the offset beside it was
			// published for, which is this projection's contract rather
			// than something to infer from how many rows survived.
			summed := &chplan.FuncCall{Fn: chplan.FnArrayResize, Args: []chplan.Expr{
				&chplan.FuncCall{Fn: chplan.FnArrayReduce, Args: []chplan.Expr{
					&chplan.LitString{V: expHistogramWindowDenseSumAggName},
					weightedRows,
				}},
				&chplan.FuncCall{Fn: chplan.FnToUInt64, Args: []chplan.Expr{mergedLength}},
				toFloat64Expr(&chplan.LitInt{V: 0}),
			}}
			return scaleArrayExpr(paramWinFactorScaled, factor, summed)
		},
	)
}

// expHistogramWindowDenseSumAggName is the ClickHouse aggregate
// [expHistogramWindowClosedFormBucketsExpr] combines the retained rows'
// dense contributions with: `sum` under the `-ForEach` combinator, which
// applies it position by position across an array of arrays. Plain sum,
// not the compensated reducer, for the reason
// [expHistogramDenseContribsExpr] gives.
const expHistogramWindowDenseSumAggName = expHistogramDenseSumAggName + "ForEach"

// The two element parameters [scaleArrayExpr] is called with. They are
// distinct names for two scalings that nest — the coefficient one sits
// inside the array argument of the factor one — so that the emitted SQL
// never reads as if an inner `wcs` referred to an outer binding, the
// hazard [expHistogramWindowBucketsExpr]'s own `tb` comment describes.
const (
	paramWinCoeffScaled  = "wcs"
	paramWinFactorScaled = "wcf"
)

// scaleArrayExpr multiplies every element of arr by the scalar `by`,
// binding each element to `param`.
func scaleArrayExpr(param string, by, arr chplan.Expr) chplan.Expr {
	return &chplan.FuncCall{Fn: chplan.FnArrayMap, Args: []chplan.Expr{
		&chplan.Lambda{
			Params: []string{param},
			Body:   mulExpr(&chplan.BareIdent{Name: param}, by),
		},
		arr,
	}}
}

// ExpHistogramWindowFoldLowerer decides whether an exponential-histogram
// rate()/increase() window folds its bucket ladders in closed form.
//
// Unlike the other lowerer strategies in [RangeLowerers], this one is not
// an operator-facing capability and carries no chopt feature id: both
// arms compute the same values bit-for-bit over the stored integer bucket
// counts, so there is nothing for a deployment to choose. It exists so a
// test can run one query through both renderings and compare — see
// [ClosedFormExpHistogramWindowFoldLowerer] and
// [TelescopingExpHistogramWindowFoldLowerer].
type ExpHistogramWindowFoldLowerer interface {
	// ClosedFormWindowFoldEligible reports whether a window whose
	// extrapolation factor was hoisted may use the closed-form fold.
	// Returning true is permission, not a demand: the reshape still
	// declines for any shape the closed form does not cover (see
	// [expHistogramWindowClosedFormApplies]) and for a window whose
	// factor could not be hoisted, since the closed form scales by that
	// factor's own column.
	ClosedFormWindowFoldEligible() bool
}

// ClosedFormExpHistogramWindowFoldLowerer is the DEFAULT: fold the bucket
// ladders through the coefficient vector, reading only the rows whose
// coefficient is non-zero.
type ClosedFormExpHistogramWindowFoldLowerer struct{}

// ClosedFormWindowFoldEligible returns true.
func (ClosedFormExpHistogramWindowFoldLowerer) ClosedFormWindowFoldEligible() bool { return true }

// TelescopingExpHistogramWindowFoldLowerer keeps the shared
// per-consecutive-pair fold ([counterIncreaseFold]) for the bucket
// ladders — the rendering the closed form replaced.
//
// It is the differential oracle, not a fallback: no deployment wires it,
// because there is no shape the closed form answers differently. A test
// selects it to obtain the same query rendered the old way.
type TelescopingExpHistogramWindowFoldLowerer struct{}

// ClosedFormWindowFoldEligible returns false.
func (TelescopingExpHistogramWindowFoldLowerer) ClosedFormWindowFoldEligible() bool { return false }

// expHistogramClosedFormEligible reads the ExpHistogramWindowFold
// strategy off a lowering table, treating an UNRESOLVED table — one whose
// caller never ran [RangeLowerers.withDefaults] — as the conservative
// telescoping reading rather than dereferencing a nil interface.
//
// Every other strategy field is read on the assumption withDefaults
// already ran, which holds at the real lowering entries. This one is read
// from the window-input builders, which several unit tests reach through
// a hand-built lowerCtx carrying no table at all, so the assumption does
// not hold there. Answering "telescoping" is the same posture
// [ResourceBounds.withDefaults]'s own doc takes for an unresolved bound:
// an unresolved knob resolves to the SAFE reading, never to a panic and
// never to the permissive one.
func expHistogramClosedFormEligible(l RangeLowerers) bool {
	if l.ExpHistogramWindowFold == nil {
		return false
	}
	return l.ExpHistogramWindowFold.ClosedFormWindowFoldEligible()
}
