package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// hqRankWalkLeColumn / hqRankWalkCumColumn name the ARRAY JOIN aliases the
// native rank-walk emission unnests the (BOUNDED window of the coalesced
// ExplicitBounds, coalesced cumulative ladder) pair into, one row per
// surviving rung — see hqRankWalkIdxColumn's doc for why the window is
// bounded rather than the whole ladder (cerberus issue #2790).
const (
	hqRankWalkLeColumn  = "_cerb_hqn_le"
	hqRankWalkCumColumn = "_cerb_hqn_cum"

	// hqRankWalkCumTermColumn is the terminal-appended cumulative ladder
	// (the FULL-length cum array the pre-#2790 emission ARRAY JOINed
	// directly) — read twice below (the bounded index search and the
	// window's own length / terminal-membership test), so it is
	// materialised once rather than re-derived at each read site.
	hqRankWalkCumTermColumn = "_cerb_hqn_cumterm"

	// hqRankWalkIdxColumn is the bounded rank walk's own stop index: the
	// 1-based position in hqRankWalkCumTermColumn where the cumulative
	// count first reaches phi's target rank — the SAME arrayFirstIndex
	// search histogramQuantileValueFrag's w.idx() already performs for
	// the legacy fan-out emitter (chplan.HistogramQuantile's other
	// emission), just against the terminal-appended ladder instead of the
	// bare one, so the found index is directly usable to slice
	// hqRankWalkCumTermColumn itself. Materialised because the window
	// construction below reads it three times (the lower bound, the
	// upper bound, and the terminal-membership test).
	hqRankWalkIdxColumn = "_cerb_hqn_idx"

	// hqRankWalkSliceLeColumn / hqRankWalkSliceCumColumn are the
	// AT-MOST-TWO-ELEMENT window (hqRankWalkIdxColumn and its immediate
	// predecessor) sliced from the terminal-appended ladder — read twice
	// below (the two arms of the conditional terminal append), so the
	// arraySlice itself is materialised rather than re-evaluated per arm.
	hqRankWalkSliceLeColumn  = "_cerb_hqn_slice_le"
	hqRankWalkSliceCumColumn = "_cerb_hqn_slice_cum"
)

// emitHistogramQuantileRankWalkNative renders a chplan.HistogramQuantile
// whose UseNativeQuantileAggregate is set: the ClickHouse-native
// quantilePrometheusHistogram(phi)(le, cum) aggregate over an ARRAY JOIN of
// a BOUNDED window of the row's (coalesced ExplicitBounds, coalesced
// cumulative ladder) pair, replacing the hand-rolled rank walk
// emitHistogramQuantile renders.
//
// It reuses emitHistogramQuantile's Stage 1 / Stage 2 verbatim — the
// duplicate-bound coalescing and the cumulative ladder — so the two
// emitters can never drift on what "coalesced" means. See
// chopt.FeatureQuantilePromHistogram's doc for why coalescing survives (the
// aggregate answers WRONG on raw duplicate-bound rows, confirmed against a
// real server) even though this path deletes every OTHER staging level:
// the aggregate replaces the observation total, the rank-walk index and the
// linear interpolation (steps 3-5 of emitHistogramQuantile's own doc) in
// one call.
//
// # Bounded window (cerberus issue #2790)
//
// The FIRST cut of this emission (still what a reader sees in git history
// prior to #2790's PR 2) ARRAY JOINed the WHOLE terminal-appended ladder —
// every coalesced (bound, cum) rung plus the mandatory terminal. That
// multiplies row count by the ladder's own length (roughly 12-13 rungs in a
// representative real OTel export) before GROUP BY collapses it back down,
// a genuine memory blow-up at high series cardinality the issue measured at
// ~3.3x relative to the legacy fan-out walk (see chopt.
// FeatureQuantilePromHistogram's doc for the full numbers).
//
// This emission instead ARRAY JOINs AT MOST THREE rungs per row, regardless
// of ladder length: the rung the target cumulative count first reaches
// (hqRankWalkIdxColumn), its immediate predecessor, and the mandatory
// terminal (+Inf, total) pair — appended only when the found rung is not
// ALREADY that terminal. The row-count multiplier is therefore a small
// constant (2 or 3), not the ladder length, which is exactly the dimension
// #2790 measured growing the ARRAY JOIN's memory cost.
//
// Correctness rests on quantilePrometheusHistogram needing only the two
// rungs adjacent to its own internal rank search to interpolate correctly
// — confirmed empirically against a real ClickHouse 25.10.7.6 across every
// edge case chopt.FeatureQuantilePromHistogram's doc names (a normal
// interior crossing, a first-bucket non-positive-upper-bound short circuit
// at various DISTANCES from the array's own start, phi in {0, 1}, a
// genuine overflow rung, the no-overflow equal-length shape): narrowing the
// array to the found index and its predecessor plus the terminal always
// reproduced the SAME answer feeding the whole ladder would, because
// dropping rungs the target rank never reaches never changes which rung
// the search would have landed on — the aggregate's own "is this the
// array's first rung" special-casing (the non-positive-upper-bound short
// circuit) only ever fires when the search ALSO lands on that same rung,
// so it is never fooled into treating a narrowed window's own first
// element as bucket zero when it is not. The one shape this window MUST
// NOT try to save on is the terminal itself: an aggregate call missing
// le=+Inf entirely answers nan regardless of phi (see the quirk below),
// and — separately, verified by probe — DUPLICATING the terminal (once
// naturally, once appended) corrupts interior-phi answers, which is why
// the terminal is appended conditionally rather than unconditionally.
// TestHistogramQuantile_RankWalkNative_DifferentialRealCH re-proves this
// bounded shape against the SAME real-server differential the unbounded
// shape was proven against.
//
// Two input-contract quirks, both confirmed against a real ClickHouse
// 25.10.7.6 rather than assumed (see the registry doc for the probes):
//
//   - The aggregate answers nan whenever no row carries le = +Inf, so a
//     terminal (+Inf, total) pair is ALWAYS present in the window: the
//     genuine overflow rung when the row's BucketCounts already carries one
//     (cumCount = boundCount + 1 — hasOverflowRung below) and the found
//     index already reaches it, or an explicitly appended (le=+Inf,
//     cum=<observations>) pair otherwise. Both forms were verified to
//     reproduce the legacy emitter's answer exactly, including at phi == 1.
//     An empty (zero-bucket) row degrades to the single pair (+Inf, 0),
//     which the aggregate itself already answers nan for — but
//     histogramQuantileRankWalkNativeValueFrag keeps an explicit
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
//     out-of-domain phi. The SAME out-of-range phi can also leave the
//     window-construction search (which runs on the UNCLAMPED phi, ahead
//     of that outer branch) unable to find any rung at all — arrayFirstIndex
//     answers 0 for phi > 1 (target exceeds every cum, including the
//     terminal) and, for a NaN PhiExpr, every comparison is false for the
//     same reason. Both degrade the window to just the terminal pair,
//     which the aggregate answers nan for — a value the outer phi-range /
//     isNaN branches already discard in favour of their own literal
//     answer, so the degenerate window is inert rather than wrong.
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
	cumTerm := If(
		hasOverflowRung,
		Col(hqClassicCumColumn),
		Call("arrayPushBack", Col(hqClassicCumColumn), Col(hqClassicObservationsColumn)),
	)

	// Stage 4 — the terminal-appended cumulative ladder: read twice below
	// (the bounded index search and the window's own length /
	// terminal-membership test), so it is materialized once rather than
	// re-derived at each read site — see hqRankWalkCumTermColumn's own doc.
	termed := NewQuery().
		Select(Star(), As(cumTerm, hqRankWalkCumTermColumn)).
		From(Subquery(counted))

	// Stage 5 — the bounded rank walk's own stop index: the SAME
	// arrayFirstIndex search histogramQuantileValueFrag's w.idx() performs
	// for the legacy fan-out emitter (including its computed-phi 24.8
	// predicate-normalisation workaround — see that function's own doc),
	// run here against the terminal-appended ladder Stage 4 just
	// materialized instead of the bare one, so the found index directly
	// slices hqRankWalkCumTermColumn. See hqRankWalkIdxColumn's own doc.
	target := finalW.target()
	idxCmp := Gte(BareIdent("c"), target)
	idxPred := idxCmp
	if h.PhiExpr != nil {
		idxPred = Paren(Eq(If(idxCmp, InlineLit(1), InlineLit(0)), InlineLit(1)))
	}
	idxExpr := Call("arrayFirstIndex", Lambda1("c", idxPred), Col(hqRankWalkCumTermColumn))
	indexed := NewQuery().
		Select(Star(), As(idxExpr, hqRankWalkIdxColumn)).
		From(Subquery(termed))

	// Stage 6 — the AT-MOST-TWO-ELEMENT slice of the terminal-appended
	// ladder bracketing hqRankWalkIdxColumn: itself and its immediate
	// predecessor (clamped to the array's own start when the index is 1),
	// materialized because the conditional terminal append below reads it
	// in BOTH arms — see hqRankWalkSliceLeColumn's own doc.
	lowIdx := Call("greatest", InlineLit(1), Sub(Col(hqRankWalkIdxColumn), InlineLit(1)))
	windowLen := Add(Sub(Col(hqRankWalkIdxColumn), lowIdx), InlineLit(1))
	leTermFull := Call("arrayPushBack", Col(hqClassicBoundsColumn), verbatim("inf"))
	sliced := NewQuery().
		Select(
			Star(),
			As(Call("arraySlice", leTermFull, lowIdx, windowLen), hqRankWalkSliceLeColumn),
			As(Call("arraySlice", Col(hqRankWalkCumTermColumn), lowIdx, windowLen), hqRankWalkSliceCumColumn),
		).
		From(Subquery(indexed))

	// needsTerminal is false exactly when the found index ALREADY IS the
	// terminal rung — the slice above already carries (+Inf, total) in
	// that case, and appending it again would DUPLICATE it, which a probe
	// against a real server confirmed corrupts interior-phi answers (see
	// this function's own doc). Only when the found rung sits strictly
	// before the terminal is it appended, closing #2790's ARRAY JOIN
	// blow-up: the window ARRAY JOINs at most three rungs regardless of
	// the ladder's own length.
	needsTerminal := Neq(Col(hqRankWalkIdxColumn), Call("length", Col(hqRankWalkCumTermColumn)))
	windowLe := If(needsTerminal,
		Call("arrayPushBack", Col(hqRankWalkSliceLeColumn), verbatim("inf")),
		Col(hqRankWalkSliceLeColumn))
	windowCum := If(needsTerminal,
		Call("arrayPushBack", Col(hqRankWalkSliceCumColumn), Col(hqClassicObservationsColumn)),
		Col(hqRankWalkSliceCumColumn))

	sb := NewQuery().
		From(Subquery(sliced)).
		ArrayJoin(As(windowLe, hqRankWalkLeColumn), As(windowCum, hqRankWalkCumColumn))

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
