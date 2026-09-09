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
// baseline. Narrowing the ROW SET is the only lever that moves it — a
// throwaway two-rows-per-anchor probe measured 85.2 MiB, a 19.8x drop.
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
// nothing to any bucket and is dropped there too, which is what takes the
// per-bucket row count from "every in-window sample" down to two plus one
// per reset for a cumulative counter — the overwhelmingly common shape.
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
// series still reads every sample it needs (its coefficients are non-zero
// on all but the earliest row) and simply does not benefit from the
// narrowing.

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
	paramWinValue      = "wcv"
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

// expHistogramWindowClosedFormFold is the per-target-bucket fold this
// file's rewrite installs: the coefficient-weighted sum of the retained
// rows' contributions, scaled by rate/increase's own boundary-
// extrapolation factor exactly as [histogramWindowFold]'s rate branch
// scales [counterIncreaseFold]'s result.
//
// `order` is ignored: the row order is already encoded in the coefficient
// vector, and the timestamps the extrapolation factor needs are read at
// FULL length from the factor's own layer rather than from anything this
// fold narrowed. Keeping those two apart is what makes the factor exact —
// a fold that derived the factor from the narrowed rows would compute it
// over the wrong window edges.
func expHistogramWindowClosedFormFold(in histogramWindowInputs) histogramWindowTimeFold {
	return func(values, _ chplan.Expr) chplan.Expr {
		weighted := &chplan.FuncCall{Fn: chplan.FnArraySum, Args: []chplan.Expr{
			&chplan.FuncCall{Fn: chplan.FnArrayMap, Args: []chplan.Expr{
				&chplan.Lambda{
					Params: []string{paramWinCoeff, paramWinValue},
					Body: &chplan.Binary{
						Op:    chplan.OpMul,
						Left:  &chplan.BareIdent{Name: paramWinCoeff},
						Right: &chplan.BareIdent{Name: paramWinValue},
					},
				},
				&chplan.ColumnRef{Name: hqWindowCoeffsAlias},
				values,
			}},
		}}
		return &chplan.Binary{Op: chplan.OpMul, Left: weighted, Right: in.hoistedFactor}
	}
}

// expHistogramWindowRowSource decides which rows a per-target-bucket
// contribution reads: every row the window grouping collected, or only
// the rows [expHistogramWindowCoefficientStage] kept.
//
// It is a type rather than a bool so the two readings are named at the
// call site and a future third one (an irate/idelta two-sample narrowing,
// say) has somewhere to go that is not another boolean parameter.
type expHistogramWindowRowSource struct {
	narrowed bool
}

// expHistogramWindowFullArrays reads every row, which is what every
// window shape outside this file's rewrite does.
func expHistogramWindowFullArrays() expHistogramWindowRowSource {
	return expHistogramWindowRowSource{}
}

// expHistogramWindowNarrowedArrays reads only the rows whose counter-fold
// coefficient is non-zero.
func expHistogramWindowNarrowedArrays() expHistogramWindowRowSource {
	return expHistogramWindowRowSource{narrowed: true}
}

// array renders one of the window grouping's per-row groupArray columns
// under this source's reading.
func (r expHistogramWindowRowSource) array(alias string) chplan.Expr {
	if r.narrowed {
		return &chplan.ColumnRef{Name: expHistogramWindowNarrowedAlias(alias)}
	}
	return &chplan.ColumnRef{Name: alias}
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
