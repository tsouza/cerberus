package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// hqRankWalkLeColumn / hqRankWalkCumColumn name the ARRAY JOIN aliases the
// native rank-walk emission unnests the (coalesced ExplicitBounds, coalesced
// cumulative ladder) pair into, one row per bucket rung.
const (
	hqRankWalkLeColumn  = "_cerb_hqn_le"
	hqRankWalkCumColumn = "_cerb_hqn_cum"
)

// emitHistogramQuantileRankWalkNative renders a chplan.HistogramQuantile
// whose UseNativeQuantileAggregate is set: the ClickHouse-native
// quantilePrometheusHistogram(phi)(le, cum) aggregate over an ARRAY JOIN of
// the row's (coalesced ExplicitBounds, coalesced cumulative ladder) pair,
// replacing the hand-rolled rank walk emitHistogramQuantile renders.
//
// It reuses emitHistogramQuantile's Stage 1 / Stage 2 verbatim — the
// duplicate-bound coalescing and the cumulative ladder — so the two
// emitters can never drift on what "coalesced" means. See
// chopt.FeatureQuantilePromHistogram's doc for why coalescing survives (the
// aggregate answers WRONG on raw duplicate-bound rows, confirmed against a
// real server) even though this path deletes every OTHER staging level:
// the aggregate replaces the observation total, the rank-walk index and the
// linear interpolation (steps 3-5 of emitHistogramQuantile's own doc) in
// one call, and every quantity this emission still builds (the (le, cum)
// pair) is referenced exactly ONCE — in the ARRAY JOIN clause — so there is
// no repeated-sub-expression evaluation for the analyzer's CSE fold to be
// unreliable about, unlike the legacy interpolation formula this file does
// not need.
//
// Two input-contract quirks, both confirmed against a real ClickHouse
// 25.10.7.6 rather than assumed (see the registry doc for the probes):
//
//   - The aggregate answers nan whenever no row carries le = +Inf, so a
//     terminal (+Inf, total) pair is ALWAYS appended: the genuine overflow
//     rung when the row's BucketCounts already carries one (cumCount =
//     boundCount + 1 — hasOverflowRung below), or a synthetic tie-cum entry
//     (le=+Inf, cum=<the ladder's own last entry>) when it does not. Both
//     forms were verified to reproduce the legacy emitter's answer exactly,
//     including at phi == 1. An empty (zero-bucket) row degrades to the
//     single pair (+Inf, 0), which the aggregate itself already answers nan
//     for — but histogramQuantileRankWalkNativeValueFrag keeps an explicit
//     length(BucketCounts) == 0 / observations == 0 outer check anyway: a
//     real-CH differential caught that those cases must answer nan even for
//     an OUT-OF-RANGE phi (see that function's own doc), which the
//     aggregate's internal nan cannot produce once the outer phi-range
//     branch has already redirected to a literal ±Inf.
//   - The parametric phi argument must be a compile-time-constant value in
//     [0, 1]; an aggregate's parametric argument is evaluated once for the
//     whole query regardless of which branch of an enclosing scalar if()
//     would select its result, so an out-of-range or NaN phi reaching it
//     directly throws PARAMETER_OUT_OF_BOUND and fails the WHOLE query.
//     histogramQuantileRankWalkNativeValueFrag clamps that argument
//     unconditionally and answers Prometheus's own -inf / inf / nan
//     contract in an outer branch that never touches the aggregate for an
//     out-of-domain phi.
func (e *emitter) emitHistogramQuantileRankWalkNative(h *chplan.HistogramQuantile) error {
	if h.Input == nil {
		return fmt.Errorf("%w: HistogramQuantile.Input is nil", ErrUnsupported)
	}
	if h.BucketCountsColumn == "" || h.ExplicitBoundsColumn == "" {
		return fmt.Errorf("%w: HistogramQuantile requires BucketCountsColumn and ExplicitBoundsColumn", ErrUnsupported)
	}
	sub, err := e.subqueryFrag(h.Input)
	if err != nil {
		return err
	}

	// Stage 1 / Stage 2 — byte-identical to emitHistogramQuantile: see that
	// function's own doc for the algorithm these two stages implement.
	rawW := newHQClassicWriters(h, hqClassicHelperColumns{})
	scanned := NewQuery().
		Select(
			Star(),
			As(rawW.keptBoundIdx(), hqClassicKeptIdxColumn),
			As(rawW.buckets(), hqClassicBucketsColumn),
		).
		From(sub)

	scan := hqClassicHelperColumns{
		keptIdx: hqClassicKeptIdxColumn,
		buckets: hqClassicBucketsColumn,
	}
	scanW := newHQClassicWriters(h, scan)
	coalesced := NewQuery().
		Select(
			Star(),
			As(scanW.bounds(), hqClassicBoundsColumn),
			As(scanW.cum(), hqClassicCumColumn),
		).
		From(Subquery(scanned))

	// ladder reads the two columns Stage 2 just materialized, so every
	// derivation below is a plain Col() read — no re-derivation of the
	// coalescing walk.
	ladder := scan
	ladder.bounds = hqClassicBoundsColumn
	ladder.cum = hqClassicCumColumn

	// Stage 3 — the observation total, materialized for the SAME reason
	// emitHistogramQuantile's own Stage 3 is: it is read TWICE below (the
	// outer "empty histogram" branch and the synthetic-terminal cumTerm),
	// and arraySum() over the bucket array is not free to re-derive at an
	// unbounded bucket count.
	counted := NewQuery().
		Select(Star(), As(newHQClassicWriters(h, ladder).observations(), hqClassicObservationsColumn)).
		From(Subquery(coalesced))

	final := ladder
	final.observations = hqClassicObservationsColumn
	finalW := newHQClassicWriters(h, final)

	// hasOverflowRung mirrors emitHistogramQuantile's own definition
	// exactly (histogramQuantileValueFrag's doc): cumCount != boundCount
	// iff the row's BucketCounts genuinely carries a trailing +Inf rung
	// beyond ExplicitBounds' own length.
	hasOverflowRung := Neq(finalW.cumCount(), finalW.boundCount())
	leTerm := Call("arrayPushBack", Col(hqClassicBoundsColumn), verbatim("inf"))
	cumTerm := If(
		hasOverflowRung,
		Col(hqClassicCumColumn),
		Call("arrayPushBack", Col(hqClassicCumColumn), Col(hqClassicObservationsColumn)),
	)

	sb := NewQuery().
		From(Subquery(counted)).
		ArrayJoin(As(leTerm, hqRankWalkLeColumn), As(cumTerm, hqRankWalkCumColumn))

	groupByFrags := make([]Frag, 0, len(h.GroupBy))
	for i, g := range h.GroupBy {
		expr := g
		alias := ""
		if i < len(h.GroupByAliases) {
			alias = h.GroupByAliases[i]
		}
		f := func(b *Builder) { _ = b.Expr(expr) }
		sb.SelectAs(f, alias)
		groupByFrags = append(groupByFrags, f)
	}
	sb.SelectAs(histogramQuantileRankWalkNativeValueFrag(h, final), "Value")
	sb.GroupBy(groupByFrags...)
	return e.emitSelect(sb)
}

// histogramQuantileRankWalkNativeValueFrag returns the Frag rendering the
// `Value` column for the native rank-walk path: reference Prometheus's own
// classic-histogram outer contract (matching histogramQuantileValueFrag's
// identical outer chain — length(BucketCounts) == 0 -> nan, then
// observations == 0 -> nan, THEN phi < 0 / phi > 1, then isNaN(phi) for a
// computed phi) wrapping a single quantilePrometheusHistogram(phi)(le, cum)
// call whose OWN parametric phi argument is separately clamped into [0, 1]
// so it is never the value that trips the aggregate's
// PARAMETER_OUT_OF_BOUND — see this file's own doc for why that clamp is
// mandatory rather than defensive.
//
// The empty / zero-total checks stay OUTERMOST, ahead of the phi-range
// check, even though this emission's own synthetic-terminal construction
// (emitHistogramQuantileRankWalkNative) means the aggregate itself would
// otherwise answer nan for an empty row regardless of phi: a real-CH
// differential caught that an empty or all-zero-count row must answer nan
// even for an OUT-OF-RANGE phi on the classic path, matching
// histogramQuantileValueFrag's order exactly — the reverse of the
// exponential/native-histogram path, which checks phi-range first
// (cerberus issue #2067). The two histogram kinds read different
// Prometheus source functions (bucketQuantile vs BucketQuantile) with
// different edge-case orders; this node handles only the classic one.
//
// helpers must carry a materialized `observations` column (see the
// emitter's own Stage 3) so this function reads it once rather than
// re-deriving arraySum() a second time.
func histogramQuantileRankWalkNativeValueFrag(h *chplan.HistogramQuantile, helpers hqClassicHelperColumns) Frag {
	w := newHQClassicWriters(h, helpers)

	// `nan` / `-inf` / `inf` / `0.0` / `1.0` are CH-portable shape tokens,
	// not data — InlineLit would quote/canonicalise them (see
	// histogramQuantileValueFrag's identical precedent).
	nan := verbatim("nan")
	negInf := verbatim("-inf")
	posInf := verbatim("inf")
	zeroF := verbatim("0.0")
	oneF := verbatim("1.0")

	// clampedPhi is ALWAYS a finite value in [0, 1]. A NaN phi first
	// collapses to 0.0 (isNaN branch) before the greatest/least clamp,
	// because greatest/least do not resolve a NaN operand to either bound —
	// confirmed against a real server: an unclamped NaN still trips
	// PARAMETER_OUT_OF_BOUND even inside greatest(0, least(1, ...)).
	clampedPhi := Call("greatest", zeroF, Call("least", oneF, If(Call("isNaN", w.phi()), zeroF, w.phi())))
	agg := Parametric("quantilePrometheusHistogram", []Frag{clampedPhi}, Col(hqRankWalkLeColumn), Col(hqRankWalkCumColumn))

	// Out-of-domain phi is answered WITHOUT ever evaluating the aggregate's
	// true (unclamped) semantics for that row — the aggregate itself always
	// runs (ClickHouse computes every aggregate in the SELECT list for
	// every group regardless of an enclosing if() branch), but its
	// clamped-safe result is simply discarded in favour of the literal
	// -inf / inf answer.
	//
	// any(...) wraps lengthBC / observations because this SELECT sits ABOVE
	// the ARRAY JOIN + GROUP BY: every one of the group's exploded per-bucket
	// rows carries the SAME original-row value for both (they are read off
	// columns computed BEFORE the unnest), so any() — an arbitrary but
	// deterministic-enough pick — is exact, not an approximation, and is
	// what lets these two columns be referenced at all once GROUP BY has
	// collapsed the rows that carried them.
	core := If(Eq(Call("any", w.lengthBC()), InlineLit(0)), nan,
		If(Eq(Call("any", w.observations()), InlineLit(0)), nan,
			If(Lt(w.phi(), InlineLit(0)), negInf,
				If(Gt(w.phi(), InlineLit(1)), posInf, agg))))

	if h.PhiExpr == nil {
		return core
	}
	// Computed phi can be NaN at runtime (PromQL `scalar()` over a zero- or
	// multi-series vector) — guard with a leading isNaN -> nan branch,
	// Prom's bucketQuantile NaN-phi contract, matching
	// histogramQuantileValueFrag's identical guard. The literal path skips
	// it so existing fixtures stay byte-stable.
	return If(Call("isNaN", w.phi()), nan, core)
}
