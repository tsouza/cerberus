package promql

import (
	"github.com/tsouza/cerberus/internal/chplan"
)

// exp_histogram_window_sample_bound.go answers cerberus issue #3252: an
// exponential-histogram WINDOW fold is unbounded in samples-per-series-per-
// window, the one axis no plan-shape gate, anchor count or series count
// reveals.
//
// # What is actually expensive
//
// The issue frames the residue as the per-series `groupArray` state. It is
// not. Measured on real ClickHouse 26.6.4.55 against the compose stack's
// `e2e_latency_exp_hist` (one series, 20 Hz, 6000 samples per 5-minute
// window, 8 stored positive buckets), with six anchors:
//
//	the collapse ALONE, all nine arrays consumed:      15.21 MiB
//	the same grouping, full query:                  >10800 MiB (capped)
//
// A ratio above 700. The arrays are cheap to BUILD; what is expensive is
// the array fold above them ([expHistogramWindowReshape] and the merge it
// drives), which for rate/increase/sum_over_time visits the WHOLE window
// because reset detection reads every consecutive pair
// ([histogramWindowSelectionFor] answers [histogramWindowAllSamples], so
// [selectExpHistogramWindowSamples] narrows nothing).
//
// So this guard is not a bound on the stage it rides. It is a
// PRE-REJECTION: it sits directly above the reduction that builds the
// arrays and below the fold that consumes them, so a group it refuses
// never reaches the fold at all. Measured on the same rig, injected into
// cerberus's own emitted statement: firing at 6000 samples peaks at
// 27.94 MiB instead of ~38 GiB, and not firing costs nothing
// (602.40 MiB against 602.38 MiB unguarded — inside noise).
//
// # The cost model
//
// Peak per (anchor, series) group, fitted across 21 measured
// configurations spanning two real metrics and a synthetic width sweep:
//
//	bytes  <=  36 * S * W * (S + W)
//
// where S is the samples the group's window holds and W the widest stored
// bucket array in it. The shape is quadratic in S at fixed W (measured
// log-log exponents 2.18, 2.23, 2.01, 2.03 across a 300..3000 sweep, and
// 3.93x / 3.98x per doubling of the range independently) and linear in W
// at fixed S. Group count multiplies it linearly, on both the anchor axis
// and the series axis, which is why the ceiling below is calibrated
// against a whole query's cap rather than a single group's.
//
// A single monomial does not fit: `S^2 * W` alone under-predicts the real
// small-S/wide-W corner by 5.9x, and `S^2` alone ignores a 15x width
// effect. The two-term envelope above is >= the measured peak at every one
// of the 21 points, with headroom 1.04x .. 2.94x.
//
// The 36 in that envelope is the measured bytes one cost unit stands
// for, and it is the envelope constant rather than the average: the
// tightest measured point (one real series of
// `cerberus_queries_duration_exp_hist`, 30 samples of 122-wide buckets)
// sits at 1.04x, so it is a floor rather than a comfortable margin.
// Zero-padded synthetic widths are ~2.5x CHEAPER than real varied bucket
// content at matched (S, W), so a calibration drawn only from synthetic
// data would be optimistic. It is folded into
// [expHistogramWindowCostUnitsPerGiB] below rather than named here,
// because that ceiling is chosen conservatively against a whole query's
// cap and is not a pure division of it.
const (
	// expHistogramWindowCostUnitsPerGiB is the ceiling granted per GiB of
	// CERBERUS_CH_QUERY_MAX_MEMORY, and it folds the group count into the
	// calibration rather than pretending a single group may spend the
	// whole cap. The reference grid is the one the issue measured and the
	// one an ordinary Grafana panel produces: a 5-minute `query_range` at
	// step=60, six covering anchors, one series.
	//
	// Two independent derivations at a 1 GiB cap on that grid: the
	// measured largest S that stays under 1 GiB is ~980 at W=8, which is
	// 7.7M units; the envelope above, divided by the same six groups, is
	// 5.0M. This takes the conservative one. At W=8 it admits S ~= 786
	// where ~980 was measured safe — about 20% tight, which is the right
	// direction for a safety rail and the same posture
	// [rbgnDensityUnitsForMemory]'s own calibration takes.
	expHistogramWindowCostUnitsPerGiB int64 = 5_000_000

	// expHistogramWindowCostUnitsFloor is the ceiling a sub-GiB cap still
	// gets, so a deliberately small cap narrows the guard rather than
	// closing it: at W=8 it still admits ~425 samples per window, which is
	// a 1 Hz series over seven minutes. Mirrors
	// [rbgnDensityUnitsFloor]'s own role.
	expHistogramWindowCostUnitsFloor int64 = 1_500_000
)

// bytesPerGiB is the divisor [ExpHistogramWindowCostUnitsForMemory] reads
// the cap in. internal/promql may not import internal/config
// (.go-arch-lint.yml), so the constant is restated here rather than
// shared.
const bytesPerGiB int64 = 1 << 30

// ExpHistogramWindowCostUnitsForMemory derives the samples-per-window cost
// ceiling from a ClickHouse per-query memory cap, in the sentinel-0 shape
// [rbgnDensityUnitsForMemory] established and for the identical reason:
// the units count a proxy for BYTES, and the byte ceiling is the
// operator's own configurable dial, so a fixed number is wrong at every
// cap but one.
//
// A non-positive cap means CERBERUS_CH_QUERY_MAX_MEMORY is unset — cerberus
// stamps no `max_memory_usage` at all then, so ClickHouse's own server
// limit is the only ceiling and cerberus has no number to divide. The
// per-GiB rate applied once is the honest answer there: it is the same
// bound a 1 GiB deployment gets, which is the shipped product default.
func ExpHistogramWindowCostUnitsForMemory(chQueryMaxMemory int64) int64 {
	if chQueryMaxMemory <= 0 {
		return expHistogramWindowCostUnitsPerGiB
	}
	units := (chQueryMaxMemory / bytesPerGiB) * expHistogramWindowCostUnitsPerGiB
	if rem := chQueryMaxMemory % bytesPerGiB; rem > 0 {
		// Proportional remainder, so a 1.5 GiB cap is not rounded down to
		// a 1 GiB one.
		units += (rem * expHistogramWindowCostUnitsPerGiB) / bytesPerGiB
	}
	if units < expHistogramWindowCostUnitsFloor {
		return expHistogramWindowCostUnitsFloor
	}
	return units
}

// expHistogramWindowSamplesExpr is S: how many samples the group's window
// holds.
//
// It reads `length(<the timestamp groupArray>)` rather than
// `uniqExact(TimeUnix)`, and the difference is load-bearing rather than
// stylistic. The arrays this guard is protecting hold one element per ROW,
// so `length` is exactly their size; `uniqExact` counts distinct
// timestamps and undercounts a group holding two rows at one instant —
// which is precisely the group that costs the most per timestamp. The
// fan-out's MinSamples HAVING keeps using `uniqExact`, because the
// question there really is how many scrapes the window spans.
func expHistogramWindowSamplesExpr() chplan.Expr {
	return &chplan.FuncCall{
		Fn:   chplan.FnLength,
		Args: []chplan.Expr{&chplan.ColumnRef{Name: hqWindowTsListAlias}},
	}
}

// expHistogramWindowWidthParam is the `arrayMap` lambda's parameter in
// [expHistogramWindowWidthExpr] — one bucket array of the group. It is
// named here rather than inline so
// TestExpHistogramWindowGuard_LambdaParamIsBare can assert the emitted
// spelling against the same identifier the emitter uses.
const expHistogramWindowWidthParam = "bw"

// expHistogramWindowWidthExpr is W: the widest stored bucket array in the
// group, positive and negative summed.
//
// Both halves are read off the groupArray-of-arrays the reduction already
// collected, so the guard adds no aggregate to the reduction and cannot
// perturb the agg list the fold's own projections are derived from. The
// per-element work is a `length` over an already-materialised array, so
// the guard costs O(S) against the O(S^2 * W) it gates.
//
// The widest STORED row, not the merged target-bucket span. On the
// measured data the merged span never exceeded the stored width (it stayed
// at 6 while S went from 311 to 6000, so the quadratic in S is intrinsic
// to the fold rather than a widening-bucket artefact), but a group whose
// rows span many scales could merge wider than any row stores. That
// regime does not exist in the measured data and is not modelled here;
// what this expression claims is a lower bound on the true width, which
// makes the guard conservative in the admitting direction for it.
func expHistogramWindowWidthExpr() chplan.Expr {
	// The body refers to the lambda's own parameter, so it is a
	// [chplan.BareIdent] and not a [chplan.ColumnRef]. The distinction is
	// not cosmetic: ColumnRef renders backtick-quoted, which spells the
	// parameter as a base-column lookup, and every ColumnRef-walking
	// analysis in the tree — projection derivation, the containment
	// guards, the chdb fan-out self-sufficiency check — then counts `bw`
	// as a column the scan must supply. [chplan.BareIdent] exists for
	// exactly this, and [chplan.RangeWindowGridNative.RecollapseReadColumns]
	// states the invariant it relies on.
	widest := func(alias string) chplan.Expr {
		return &chplan.FuncCall{
			Fn: chplan.FnArrayMax,
			Args: []chplan.Expr{&chplan.FuncCall{
				Fn: chplan.FnArrayMap,
				Args: []chplan.Expr{
					&chplan.Lambda{
						Params: []string{expHistogramWindowWidthParam},
						Body: &chplan.FuncCall{
							Fn:   chplan.FnLength,
							Args: []chplan.Expr{&chplan.BareIdent{Name: expHistogramWindowWidthParam}},
						},
					},
					&chplan.ColumnRef{Name: alias},
				},
			}},
		}
	}
	return &chplan.Binary{
		Op:    chplan.OpAdd,
		Left:  widest(hqAggPosBucketsArrayAlias),
		Right: widest(hqAggNegBucketsArrayAlias),
	}
}

// expHistogramWindowCostOverBudgetExpr renders `S * W * (S + W) >
// maxCostUnits` — the envelope of this file's own doc, with the
// bytes-per-unit constant divided out into the ceiling so the emitted
// arithmetic stays in the same cost-unit vocabulary every other guard in
// this tree speaks.
func expHistogramWindowCostOverBudgetExpr(maxCostUnits int64) chplan.Expr {
	samples := expHistogramWindowSamplesExpr()
	width := expHistogramWindowWidthExpr()
	cost := &chplan.Binary{
		Op:   chplan.OpMul,
		Left: samples,
		Right: &chplan.Binary{
			Op:    chplan.OpMul,
			Left:  width,
			Right: &chplan.Binary{Op: chplan.OpAdd, Left: samples, Right: width},
		},
	}
	return gtLit(cost, maxCostUnits)
}

// wrapExpHistogramWindowSampleGuard attaches the pre-rejection directly
// above reduced — the [chplan.RangeBucketFanout] or [chplan.Aggregate]
// that built the window's groupArrays — and below the fold that consumes
// them. Mirrors [wrapExpHistogramMergeBudgetGuard]'s own placement, and
// for the same reason: the predicate reads that reduction's own aliases,
// so it can only ride directly on top of it.
//
// A non-positive ceiling means the caller resolved no bound and the guard
// is omitted rather than rendered as "reject everything" — the opposite
// of [ResourceBounds]'s other fields, whose zero value is filled by
// [ResourceBounds.withDefaults] before any guard sees it. This branch
// exists for the plan-building callers that construct a [lowerCtx]
// directly in tests.
func wrapExpHistogramWindowSampleGuard(reduced chplan.Node, maxCostUnits int64) chplan.Node {
	if maxCostUnits <= 0 {
		return reduced
	}
	return &chplan.Filter{
		Input: reduced,
		Predicate: &chplan.Binary{
			Op: chplan.OpEq,
			Left: &chplan.FuncCall{
				Fn: chplan.FnThrowIf,
				Args: []chplan.Expr{
					expHistogramWindowCostOverBudgetExpr(maxCostUnits),
					&chplan.InlineString{V: chplan.ExpHistogramWindowSampleBudgetMessage},
				},
			},
			Right: &chplan.LitInt{V: 0},
		},
	}
}

// guardExpHistogramWindowReduction is the one call every exponential-
// histogram WINDOW reduction makes, so that "which reductions carry the
// pre-rejection" is a grep rather than a reading of seven lowerings.
//
// It has to be a per-site call rather than a single shared seam: the
// FOLD family's four grid modes pass their reduction through
// [selectExpHistogramWindowSamples], but the over_time range mode and the
// fused `histogram_quantile(q, <fold>)` path hand theirs straight to
// [expHistogramWindowReshape], and the guard must ride DIRECTLY on the
// reduction whose groupArray aliases it reads. What keeps a future eighth
// site from forgetting is not this helper but
// TestExpHistogramWindowGuard_EveryWindowFoldShapeCarriesIt, which
// enumerates the query shapes rather than the call sites.
func guardExpHistogramWindowReduction(reduced chplan.Node, ctx lowerCtx) chplan.Node {
	return wrapExpHistogramWindowSampleGuard(reduced, ctx.resourceBounds.ExpHistogramWindowMaxCostUnits)
}
