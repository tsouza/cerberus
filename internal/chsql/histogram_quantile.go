package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// Helper columns the classic-quantile emission materialises, one per
// derived-query stage, so ClickHouse evaluates each array walk once per
// row instead of once per use site.
//
// The interpolation reads the Float64-cast bucket array, the coalesced
// bound array, the coalesced cumulative ladder, the observation total
// and the rank-walk stop index between four and eight times each. Left
// inline, every one of those references re-renders the whole underlying
// expression tree and the query only stays affordable because
// ClickHouse's query analyzer folds the repeats into a single
// evaluation. That fold is not a guarantee the emitter is entitled to:
// the analyzer can be switched off per query (see
// internal/engine/query_settings_rules.go), and with it off the
// repeated `arrayMap` / `arrayCumSum` / `arrayFirstIndex` walks are
// evaluated for real — the exact shape that exhausted the server's
// memory limit on the native (exponential) quantile path. Materialising
// each quantity into its own stage makes the single evaluation a
// property of the emitted SQL rather than of an optimiser setting.
const (
	hqClassicKeptIdxColumn      = "_cerb_hqc_kept_idx"
	hqClassicBucketsColumn      = "_cerb_hqc_buckets"
	hqClassicBoundsColumn       = "_cerb_hqc_bounds"
	hqClassicCumColumn          = "_cerb_hqc_cum"
	hqClassicObservationsColumn = "_cerb_hqc_observations"
	hqClassicIdxColumn          = "_cerb_hqc_idx"
)

// hqClassicHelperColumns names the already-materialised helper columns a
// writer set may read instead of re-deriving. An empty field means the
// quantity has not been staged yet and the writer renders it inline —
// which is what each materialising stage itself needs, since a stage can
// only read the columns the stages beneath it produced.
type hqClassicHelperColumns struct {
	keptIdx      string
	buckets      string
	bounds       string
	cum          string
	observations string
	idx          string
}

// emitHistogramQuantile renders a chplan.HistogramQuantile against the
// OTel-CH classic histogram schema (parallel BucketCounts × ExplicitBounds
// arrays per row).
//
// The CH expression chain — for each row out of the inner subquery —
// computes the interpolated quantile:
//
//  1. cum = arrayCumSum(BucketCounts) — running totals across buckets.
//  2. total = cum[length(cum)] — total observations (last bucket is +Inf,
//     so this captures every observation).
//  3. target = phi * total — the desired cumulative-count cutoff.
//  4. idx = arrayFirstIndex(c -> c >= target, cum) — the 1-based bucket
//     index whose cumulative first crosses target. Zero means no bucket
//     reaches target (only possible when phi <= 0 + total > 0; the phi
//     guard handles it).
//  5. Linear interpolation between the bucket's lower and upper bounds
//     using the cumulative counts at each edge. The lower edge of bucket
//     1 is (bound=0, cum=0); subsequent buckets read from ExplicitBounds
//     and cum at idx-1. The trailing +Inf bucket (idx == length(cum))
//     returns the highest explicit bound — matching upstream Prometheus.
//
// Steps 1-4 are each materialised into their own derived-query stage
// (see the helper-column block above); step 5's interpolation reads them
// as plain columns.
//
// Prom edge cases mirrored (`bucketQuantile`'s domain guards in
// quantile.go):
//
//   - total = 0 (empty histogram) → NaN.
//   - phi < 0 → -Inf (out of domain).
//   - phi > 1 → +Inf (out of domain).
//   - phi == 1 → highest explicit bound (in domain; the idx == length(cum)
//     guard reads the last finite edge, not +Inf).
//   - phi == 0 → lowest explicit bound (in domain; idx == 1 lower edge).
//   - Any other phi → linear interpolation per the steps above.
//
// The outer QueryBuilder projects the GroupBy columns aliased per
// GroupByAliases, then the interpolated quantile as the `Value` column,
// matching the Sample contract the lowering's wrapping Project consumes.
func (e *emitter) emitHistogramQuantile(h *chplan.HistogramQuantile) error {
	// UseNativeQuantileAggregate is set exactly once, at lowering time, by
	// the boot-wired promql.QuantileRankWalkLowerer strategy — see
	// chplan.HistogramQuantile's own doc. This dispatch is the ONLY reader:
	// no feature-flag or server-version conditional lives in the emitter.
	if h.UseNativeQuantileAggregate {
		return e.emitHistogramQuantileRankWalkNative(h)
	}
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

	// Stage 1 — the two derivations that read only base columns: the
	// coalescing index walk over ExplicitBounds and the Float64 cast of
	// BucketCounts. Both coalesced arrays are built from the index walk,
	// so staging it first keeps its arrayFilter to a single evaluation.
	rawW := newHQClassicWriters(h, hqClassicHelperColumns{})
	scanned := NewQuery().
		Select(
			Star(),
			As(rawW.keptBoundIdx(), hqClassicKeptIdxColumn),
			As(rawW.buckets(), hqClassicBucketsColumn),
		).
		From(sub)

	// Stage 2 — the coalesced bound array and the coalesced cumulative
	// ladder, both over the staged index walk and the staged bucket array.
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

	// Stage 3 — the observation total. The cumulative-input form reads the
	// ladder's top rung, so it has to sit above the ladder stage.
	ladder := scan
	ladder.bounds = hqClassicBoundsColumn
	ladder.cum = hqClassicCumColumn
	counted := NewQuery().
		Select(Star(), As(newHQClassicWriters(h, ladder).observations(), hqClassicObservationsColumn)).
		From(Subquery(coalesced))

	// Stage 4 — the rank-walk stop index, which needs both the ladder and
	// the total (target = phi * observations).
	ranked := ladder
	ranked.observations = hqClassicObservationsColumn
	indexed := NewQuery().
		Select(Star(), As(newHQClassicWriters(h, ranked).idx(), hqClassicIdxColumn)).
		From(Subquery(counted))

	helpers := ranked
	helpers.idx = hqClassicIdxColumn

	sb := NewQuery().From(Subquery(indexed))
	for i, g := range h.GroupBy {
		expr := g
		alias := ""
		if i < len(h.GroupByAliases) {
			alias = h.GroupByAliases[i]
		}
		sb.SelectAs(func(b *Builder) { _ = b.Expr(expr) }, alias)
	}
	sb.SelectAs(histogramQuantileValueFrag(h, helpers), "Value")
	return e.emitSelect(sb)
}

// histogramQuantileValueFrag returns the Frag that renders the per-row
// quantile-interpolation expression. A literal phi is rendered as an
// inline float literal (query-shape parameter, mirrors
// holtWintersValueExpr's sf / tf treatment); a computed phi
// (h.PhiExpr != nil) renders the expression — typically a scalar
// subquery, which CH evaluates once and folds as a constant — at every
// phi position, wrapped in a leading `isNaN(phi) → nan` branch
// because a runtime phi can be NaN (PromQL `scalar()` over zero / many
// series) and Prom's bucketQuantile returns NaN there. Bound user data
// lives on the wrapping Filter / Project, not on the per-row
// arithmetic.
//
// helpers names the derived-query columns the caller already
// materialised; an empty set renders every quantity inline, which is the
// shape the materialising stages and the unit tests ask for.
//
// The expression is structured as nested if(...) clauses so the
// edge-case branches stay inlined (no CASE WHEN, no CTEs) — keeps the
// query shape stable for CH's planner.
func histogramQuantileValueFrag(h *chplan.HistogramQuantile, helpers hqClassicHelperColumns) Frag {
	w := newHQClassicWriters(h, helpers)

	// `nan` / `0.0` are CH-portable shape tokens, not data: InlineLit
	// would render `nan` as the quoted string `'nan'` and `0.0` as the
	// canonicalised `0`, so they ride verbatim (the same posture as
	// IfNonZero's `0.0` fallback in builder.go).
	nan := verbatim("nan")
	negInf := verbatim("-inf")
	posInf := verbatim("inf")
	zeroF := verbatim("0.0")
	highestBound := Subscript(w.bounds(), Call("length", w.bounds())) // coalesced ExplicitBounds' last entry

	// Lower-edge selectors branch on idx = 1 → 0.0, else the [idx-1]
	// lookup. bound_lo / cum_lo per the interpolation below.
	boundLo := func() Frag { return If(Eq(w.idx(), InlineLit(1)), zeroF, w.boundAt(true)) }
	cumLo := func() Frag { return If(Eq(w.idx(), InlineLit(1)), zeroF, w.cumAt(true)) }

	// Interpolation:
	//   bound_lo + (bound_hi - bound_lo) * (target - cum_lo) / (cum_hi - cum_lo)
	// bound_hi = ExplicitBounds[idx]; cum_hi = cum[idx]; target = phi *
	// observations. The grouping parens match the legacy emitter exactly:
	//   (bound_lo + (bound_hi - bound_lo) * (((target) - cum_lo) / (cum_hi - cum_lo)))
	//
	// Prometheus divides the rank offset before multiplying by the bucket width.
	// Keeping that order preserves its Float64 rounding for rate-derived counts.
	interp := Paren(
		Add(
			boundLo(),
			Mul(
				Paren(Sub(w.boundAt(false), boundLo())),
				Paren(Div(
					Paren(Sub(w.target(), cumLo())),
					Paren(Sub(w.cumAt(false), cumLo())),
				)),
			),
		),
	)

	// if(idx = length(cum) AND hasOverflowRung, highest_bound, <interp>) —
	// the +Inf bucket (only the trailing bucket crosses target) returns
	// the highest explicit bound.
	//
	// hasOverflowRung guards this against a row whose BucketCounts is the
	// SAME length as ExplicitBounds — the OTel-canonical shape always
	// carries one more count than bound (the overflow rung above the
	// highest boundary, folded in unconditionally by
	// classicBucketMergedLadderExpr on the cumulative path too), but the
	// corpus also seeds the equal-length shape that simply omits it (see
	// readSeededClassicHistograms). There idx landing on the LAST rung is
	// the highest FINITE bucket, not an overflow one beyond it — its
	// upper bound still has a real predecessor to interpolate from, and
	// short-circuiting to highestBound skips that interpolation and
	// answers with the raw bound instead of Prometheus's own
	// bucketQuantile result. cumCount != boundCount is exactly the
	// "genuine overflow rung present" test: the merged/cumulative path
	// always appends one (cumCount = boundCount + 1, unaffected by this
	// change) and so does any canonical per-bucket row.
	//
	// Nested inside the else: upstream's `b == 0 && buckets[0].UpperBound
	// <= 0` short-circuit. The interpolation below assumes the first
	// bucket spans `(0, ExplicitBounds[1]]`, which is only a bucket when
	// that bound is positive. OTel permits negative and zero explicit
	// bounds, and there the assumed lower edge sits ABOVE the upper one:
	// interpolating across `[0, -10]` walks a negative-width interval and
	// answers with a number no observation could have produced. Upstream
	// returns the bound itself, so every rank landing in that bucket
	// reports its upper edge. The branch order matches BucketQuantile's
	// switch exactly — a lone +Inf bucket satisfies both predicates and
	// upstream resolves it as the trailing bucket.
	lowestBound := Subscript(w.bounds(), InlineLit(1))
	firstBucketNonPositive := And(Eq(w.idx(), InlineLit(1)), Lte(lowestBound, InlineLit(0)))
	hasOverflowRung := Neq(w.cumCount(), w.boundCount())
	idxBranch := If(
		And(Eq(w.idx(), Call("length", w.cum())), hasOverflowRung),
		highestBound,
		If(firstBucketNonPositive, lowestBound, interp),
	)

	// Nested edge-case chain, outermost first:
	//   if(length(bc) = 0, nan,
	//      if(observations = 0, nan,
	//         if(phi < 0, -inf,
	//            if(phi > 1, inf, idxBranch))))
	//
	// Prometheus semantics (`bucketQuantile`'s domain guards in quantile.go):
	// phi < 0 → -Inf, phi > 1 → +Inf are OUT of domain. phi == 0 and phi == 1 are IN domain and
	// fall through to idxBranch: phi == 0 → target 0 → idx 1 → lower edge
	// 0.0; phi == 1 → target observations → idx == length(cum) → highest finite
	// bound. ClickHouse parses the bare `inf` / `-inf` tokens as Float64.
	core := If(Eq(w.lengthBC(), InlineLit(0)), nan,
		If(Eq(w.observations(), InlineLit(0)), nan,
			If(Lt(w.phi(), InlineLit(0)), negInf,
				If(Gt(w.phi(), InlineLit(1)), posInf, idxBranch))))

	if h.PhiExpr == nil {
		return core
	}
	// Computed phi can be NaN at runtime; every comparison branch above
	// evaluates false on NaN and the interpolation would index cum[0] —
	// guard with a leading isNaN → nan branch (Prom's bucketQuantile
	// NaN-phi contract). The literal path skips the wrapper so existing
	// fixtures stay byte-stable.
	return If(Call("isNaN", w.phi()), nan, core)
}

// hqClassicWriters bundles the per-row sub-expression Frag builders the
// classic quantile emission composes. Each field is a closure returning a
// FRESH Frag so a sub-expression rendered at several positions re-emits
// its own `?` placeholders, matching the legacy emitter's per-position
// re-emission.
type hqClassicWriters struct {
	phi          func() Frag
	keptBoundIdx func() Frag
	buckets      func() Frag
	bounds       func() Frag
	cum          func() Frag
	observations func() Frag
	target       func() Frag
	idx          func() Frag
	cumAt        func(offsetMinusOne bool) Frag
	boundAt      func(offsetMinusOne bool) Frag
	lengthBC     func() Frag
	boundCount   func() Frag
	cumCount     func() Frag
}

// newHQClassicWriters builds the writer set for one emission stage.
// helpers names the quantities already materialised into derived-query
// columns: a named column short-circuits its writer to a plain Col read,
// an empty name re-derives the expression inline. emitHistogramQuantile
// hands each stage only the columns the stages beneath it produced, so
// the inline forms are what the staging itself renders; a caller that
// passes the zero value gets the fully inlined expression.
func newHQClassicWriters(h *chplan.HistogramQuantile, helpers hqClassicHelperColumns) hqClassicWriters {
	bc := h.BucketCountsColumn
	eb := h.ExplicitBoundsColumn
	var w hqClassicWriters

	// BucketCounts is Array(UInt64) in the OTel-CH schema; arraySum /
	// arrayCumSum on it return UInt64. The downstream linear-interpolation
	// arithmetic mixes those with Float64 ExplicitBounds and the `0.0`
	// edge-case literals, which CH rejects with NO_COMMON_TYPE
	// ("some are integers and some are floating point"). Cast BucketCounts
	// to Array(Float64) once at the entry so every sum / cumsum derives
	// Float64 and the interpolation arithmetic stays in a single numeric
	// domain.
	//
	// Monotonicity invariant — why there is no counterpart to Prometheus's
	// ensureMonotonicAndIgnoreSmallDeltas here. Prometheus repairs its
	// ladder before interpolating because its input is already CUMULATIVE:
	// one independently-computed float per `le`, so a later `le` can carry
	// a smaller count than an earlier one. Cerberus's input is the opposite
	// shape — BucketCountsColumn is a PER-BUCKET, non-negative array
	// (Array(UInt64) in the OTel-CH schema, element-wise summed by
	// sumForEach when the lowering aggregates) — and the ladder is built
	// HERE by arrayCumSum. IEEE-754 addition is correctly rounded and
	// monotone, so cum[i+1] = fl(cum[i] + x[i]) >= cum[i] for every
	// x[i] >= 0: the ladder is non-decreasing by construction and
	// upstream's `curr < prev` branch has no reachable work to do. A
	// zero-count bucket yields a flat run — precisely the shape the
	// upstream repair outputs (pinned by the
	// histogram_quantile_classic_*_plateau fixtures).
	//
	// h.BucketCountsCumulative is the path that breaks that precondition:
	// the bucket-layout merge hands this node an ALREADY-cumulative,
	// independently-derived per-`le` ladder (one rung per bound in the
	// union of the group's layouts, folded across rows in the cumulative
	// domain). There arrayCumSum must NOT run — the array is the ladder —
	// and the producer owes Prometheus's ensureMonotonicAndIgnoreSmallDeltas
	// repair before this node sees it (chplan.HistogramQuantile documents
	// that debt). `observations` below is then read off the ladder's top
	// rung rather than summed, or target and ladder disagree exactly when
	// the repair fires.
	w.buckets = func() Frag {
		if helpers.buckets != "" {
			return Col(helpers.buckets)
		}
		return Call("arrayMap", Lambda1("x", Call("toFloat64", BareIdent("x"))), Col(bc))
	}
	// length(BucketCounts) reads the raw column directly: it is the
	// "row carries no buckets at all" test, and the cast preserves length.
	w.lengthBC = func() Frag { return Call("length", Col(bc)) }
	w.boundCount = func() Frag { return Call("length", Col(eb)) }
	w.cumCount = func() Frag { return Call("length", w.buckets()) }

	// Coalesce buckets that share an upper bound before interpolation. The
	// classic OTel row is per-bucket while Prometheus observes a sequence of
	// float bucket samples. Retaining the first rung of a duplicate run
	// preserves that sequence's ordering: the duplicate interval's mass is
	// carried into the next distinct rung. Retaining the last rung instead
	// incorrectly puts that mass at the repeated bound; bounds [1, 1, 5]
	// with counts [2, 3, 5, 0] then return 0.6 for phi=0.3 rather than the
	// Prometheus result 1.5.
	// The trailing +Inf rung is appended unconditionally — it has no
	// ExplicitBounds entry to be duplicated, and its cumulative value is
	// the total, which coalescing never changes.
	//
	// Duplicate-free input (every real producer) keeps every index, so the
	// coalesced arrays are element-wise identical to the raw ones and the
	// quantile is unchanged.
	//
	// keptBoundIdx: the first index of every run of equal bounds.
	// keptCumIdx extends it with every cumulative entry past the bounds
	// array, untouched. That tail is the overflow rung the schema carries
	// when BucketCounts runs one longer than ExplicitBounds; rebuilding it
	// from arraySum instead would ADD a rung whenever the two arrays are
	// the same length, so it is carried across verbatim and the ladder's
	// height is preserved exactly.
	w.keptBoundIdx = func() Frag {
		if helpers.keptIdx != "" {
			return Col(helpers.keptIdx)
		}
		return Call(
			"arrayFilter",
			Lambda1("i", Or(
				Eq(BareIdent("i"), InlineLit(1)),
				Neq(Subscript(Col(eb), BareIdent("i")), Subscript(Col(eb), Sub(BareIdent("i"), InlineLit(1)))),
			)),
			Call("range", InlineLit(1), Add(w.boundCount(), InlineLit(1))),
		)
	}
	keptCumIdx := func() Frag {
		return Call(
			"arrayConcat",
			w.keptBoundIdx(),
			Call("range", Add(w.boundCount(), InlineLit(1)), Add(w.cumCount(), InlineLit(1))),
		)
	}
	w.bounds = func() Frag {
		if helpers.bounds != "" {
			return Col(helpers.bounds)
		}
		return Call("arrayMap", Lambda1("i", Subscript(Col(eb), BareIdent("i"))), w.keptBoundIdx())
	}
	w.cum = func() Frag {
		if helpers.cum != "" {
			return Col(helpers.cum)
		}
		rawCumSum := Call("arrayCumSum", w.buckets())
		if h.BucketCountsCumulative {
			rawCumSum = w.buckets()
		}
		return Call("arrayMap", Lambda1("i", Subscript(rawCumSum, BareIdent("i"))), keptCumIdx())
	}

	// observations is Prometheus's `observations` (quantile.go): the
	// ladder's top rung. Per-bucket input reaches it by summing the array;
	// cumulative input already carries it as the last coalesced entry, and
	// summing there would total the ladder instead of reading it. Both
	// forms are the same value in the per-bucket mode, so the sum form
	// stays there and existing fixtures are byte-stable.
	w.observations = func() Frag {
		if helpers.observations != "" {
			return Col(helpers.observations)
		}
		if h.BucketCountsCumulative {
			return Subscript(w.cum(), Call("length", w.cum()))
		}
		return Call("arraySum", w.buckets())
	}

	// phi renders the phi parameter: the computed expression when PhiExpr
	// is set, the inline float literal (query-shape param, mirrors
	// holtWintersValueExpr's sf / tf) otherwise. Re-invoked at each phi
	// position so each carries its own `?` placeholder for the PhiExpr
	// case — matching the legacy emitter's per-position re-emission.
	w.phi = func() Frag {
		if h.PhiExpr != nil {
			return func(b *Builder) { _ = b.Expr(h.PhiExpr) }
		}
		return InlineLit(h.Phi)
	}
	// target = (phi * observations), the desired cumulative-count cutoff.
	w.target = func() Frag { return Paren(Mul(w.phi(), w.observations())) }

	// idx = arrayFirstIndex(c -> c >= target, cum). Computed phi:
	// ClickHouse 24.8 rejects a scalar subquery anywhere in
	// arrayFirstIndex's argument tree with ILLEGAL_COLUMN ("Unexpected
	// type of filter column") — the lambda's comparison result stops
	// being the plain UInt8 filter column the higher-order filter
	// machinery expects (newer CH accepts it). Wrapping the predicate as
	// `(if(<cmp>, 1, 0) = 1)` restores the constant-folded UInt8 the 24.8
	// filter path requires. The literal path keeps the bare comparison.
	w.idx = func() Frag {
		if helpers.idx != "" {
			return Col(helpers.idx)
		}
		cmp := Gte(BareIdent("c"), w.target())
		pred := cmp
		if h.PhiExpr != nil {
			pred = Paren(Eq(If(cmp, InlineLit(1), InlineLit(0)), InlineLit(1)))
		}
		return Call("arrayFirstIndex", Lambda1("c", pred), w.cum())
	}
	// idxAtOffset renders `<idx>` or `<idx> - 1` (the `idx - 1` lower-edge
	// lookups). offsetMinusOne selects the `- 1` form.
	idxAtOffset := func(offsetMinusOne bool) Frag {
		if offsetMinusOne {
			return Sub(w.idx(), InlineLit(1))
		}
		return w.idx()
	}
	w.cumAt = func(offsetMinusOne bool) Frag {
		return Subscript(w.cum(), idxAtOffset(offsetMinusOne))
	}
	w.boundAt = func(offsetMinusOne bool) Frag {
		return Subscript(w.bounds(), idxAtOffset(offsetMinusOne))
	}

	return w
}
