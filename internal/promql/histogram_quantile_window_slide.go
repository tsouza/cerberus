package promql

import (
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// The boot-wired strategy seam for the anchor-injection window-slide
// mechanism (issue #2408 follow-up): the second ClickHouse-native lowering
// of the per-series window stage under
// `histogram_quantile(phi, <agg> by(le) (<windowFn>(<bucket>[range])))` in
// range mode, alongside [NativeClassicHistogramWindowLowerer]'s `rate`
// ladder. It follows the identical contract as that seam and the
// timeSeries*ToGrid family's: the native/window-slide/fan-out choice is
// made ONCE at boot from the resolved chopt.EnabledSet (or, for this
// mechanism — chopt.AlwaysAvailable, see windowSlideEligible's doc —
// unconditionally) and injected as a strategy; the per-query path calls
// the interface method with no feature-flag or version read, and DELEGATES
// to its embedded Fallback for any shape it cannot express, so the method
// always returns a valid lowering.

// WindowSlideClassicHistogramWindowLowerer is the boot-wired
// ClassicHistogramWindowLowerer that emits the anchor-injection window
// aggregate (a chplan.RangeBucketWindowSlide) for shape-eligible windows.
// Unlike [NativeClassicHistogramWindowLowerer] it needs no chopt feature
// gate — the mechanism uses only chopt.AlwaysAvailable ClickHouse window
// functions (issue #2408's Task-1 spike) — but cmd/cerberus still wires it
// as a distinct link in the chain (ahead of the fan-out fallback, behind
// the rate-only native ladder) so a shape neither native lowering handles
// still reaches the fan-out. It embeds a concrete Fallback: a shape it
// cannot handle delegates rather than returning nil.
type WindowSlideClassicHistogramWindowLowerer struct {
	// Fallback is the concrete lowerer for shapes this mechanism cannot
	// handle. cmd/cerberus wires it to FanoutClassicHistogramWindowLowerer{}
	// directly, or — when the rate-only native ladder is ALSO enabled — sits
	// behind it as NativeClassicHistogramWindowLowerer's own Fallback chain
	// link, per the plan's Task 5 composition.
	Fallback ClassicHistogramWindowLowerer
}

// LowerClassicHistogramWindow returns the anchor-injection window aggregate
// for an eligible window, or delegates to the embedded Fallback otherwise.
//
// Temporality split mirrors [NativeClassicHistogramWindowLowerer] exactly:
// a schema that declares an AggregationTemporality column splits into
// COMPLEMENTARY arms, because the anchor-injection aggregate's sumForEach
// fold has no DELTA semantics of its own (see windowSlideEligible's
// windowFn clause) — DELTA-temporality rows keep the fan-out's own branch
// while every other row rides the window-slide ladder. Both arms publish
// the identical `(anchor_ts, Attributes, BucketCounts, ExplicitBounds)`
// column list in the identical order, so the positional UnionAll between
// them is sound.
func (w WindowSlideClassicHistogramWindowLowerer) LowerClassicHistogramWindow(in classicHistogramWindowInput) chplan.Node {
	if !windowSlideEligible(in) {
		return w.Fallback.LowerClassicHistogramWindow(in)
	}
	if in.s.AggregationTemporalityColumn == "" {
		return windowSlideClassicHistogramNode(in, in.pred)
	}
	cumulative := windowSlideClassicHistogramNode(in,
		andPredicate(in.pred, temporalityPredicate(in.s.AggregationTemporalityColumn, chplan.OpNe)))
	delta := in
	delta.pred = andPredicate(in.pred, temporalityPredicate(in.s.AggregationTemporalityColumn, chplan.OpEq))
	return &chplan.UnionAll{Inputs: []chplan.Node{
		cumulative,
		w.Fallback.LowerClassicHistogramWindow(delta),
	}}
}

// windowSlideMinLookbackStepRatio is the minimum Lookback/Step ratio a
// window must clear before the anchor-injection mechanism is worth its own
// correctness surface, resource-bound and maintenance cost over the
// existing fan-out — issue #2408's Task-1 spike measured the mechanism's
// real-world speedup TRACKS this ratio directly and is NOT flat:
//
//	Lookback/Step ratio   measured speedup
//	         5 (5m/1m)         1.12x   — the MODAL Grafana panel shape
//	        10 (5m/30s)        1.70x
//	        20 (5m/15s)        2.65x
//	       120 (30m/15s)      10-14x
//
// 5 — the modal panel shape — is explicitly the case this design is NOT
// meant to serve (see the plan's "Scope for this phase": a marginal win
// does not justify a permanent new code path). 10 is the first point on
// the measured curve that clears a REAL margin over the fan-out (a 70%
// reduction, not a rounding-error-adjacent 12%), so it is the threshold:
// any window at or above a 10:1 Lookback/Step ratio (a 5-minute window at
// 30-second step or coarser-relative-to-step) is eligible; anything finer
// stays on the fan-out, where its own cost is already acceptable.
//
// Caveat, added by a code review after the initial implementation: the
// curve above was measured against the Task-1 spike's own SQL shape, which
// predates two correctness/perf fixes review found in the shipped emitter —
// a wrong per-anchor bound set (now has-gated, exact-membership contribution
// — see chsql's emitRangeBucketWindowSlide doc) and an O(rows^2)-per-series
// canonical-bound computation (now a linear per-series GROUP BY joined back
// in). Both fixes change the emitter's absolute cost by a constant-ish
// factor (one extra JOIN + one extra linear aggregate pass) that does NOT
// scale with the Lookback/Step ratio itself, so the RATIO-DEPENDENT shape of
// the curve — the reason higher ratios pay off more — should still hold
// directionally; the ABSOLUTE speedup at each point on it has not been
// re-measured against the current, fixed emitter. Recalibrate this constant
// against a fresh benchmark of the shipped shape before leaning on the exact
// numbers above.
const windowSlideMinLookbackStepRatio = 10

// windowSlideDisabledPending2511 gates the whole anchor-injection mechanism
// off, unconditionally, until issue #2511 is root-caused. A real
// chDB/testcontainers benchmark against production-shaped data (run as part
// of issue #2408's own follow-up impact measurement) found this mechanism
// OOMs (100% memory, HTTP 422) for sum_over_time at EVERY tested
// Lookback/Step ratio from windowSlideMinLookbackStepRatio (10) through
// 120 — the entire eligible range — reading roughly 5x more bytes than the
// FanoutClassicHistogramWindowLowerer path it exists to improve on, which
// itself completes cleanly (or fails fast and cheaply via its own existing
// guard) at every one of those same ratios. No safe ratio threshold was
// found within realistic bounds, so this disables the mechanism entirely
// rather than raising windowSlideMinLookbackStepRatio to an unproven value.
// Flip back to false once #2511's root cause is fixed and re-verified
// against a real benchmark.
const windowSlideDisabledPending2511 = true

// windowSlideMaxLookbackMs is the largest Range issue #2408's Task-1 spike
// confirmed the ceil-to-ms encoding + numeric RANGE frame can express
// against real ClickHouse: the frame offset cap
// (chsql.RangeBetweenPrecedingAndCurrentRow's doc) is 2147483646, so the
// largest whole-millisecond lookback the frame's own `lookbackMs - 1`
// argument can carry is one more than that.
const windowSlideMaxLookbackMs = 2147483647

// windowSlideEligible gates windowSlideShapeEligible behind
// windowSlideDisabledPending2511 — see that constant's own doc for why the
// mechanism is currently disabled unconditionally.
func windowSlideEligible(in classicHistogramWindowInput) bool {
	if windowSlideDisabledPending2511 {
		return false
	}
	return windowSlideShapeEligible(in)
}

// windowSlideShapeEligible is the intrinsic SHAPE check, mirroring
// nativeClassicHistogramEligible's own structure exactly. Each clause
// names a semantic the anchor-injection aggregate cannot reproduce or a
// scope boundary the plan draws deliberately, never a feature flag:
//
//   - windowFn must be `sum_over_time`. chsql's emitter computes the
//     window fold as a plain `sumForEach` — an unweighted elementwise sum
//     of the contributing rows' own (reprojected) bucket arrays, with no
//     counter-reset repair and no rate/increase extrapolation. That is
//     EXACTLY what histogramWindowFold's default branch (sum_over_time's
//     own fold) computes — see internal/promql/histogram_quantile_window.go
//     — and is NOT what `rate` / `increase` / `delta` / `irate` / `idelta`
//     mean: those need the reset-aware `counterIncreaseFold` this
//     mechanism does not implement. `rate` already has its own native
//     ladder ([NativeClassicHistogramWindowLowerer]); every OTHER windowFn
//     (increase, delta, irate, idelta) has no native ladder of any kind and
//     stays on the array-expression fan-out, exactly as it did before this
//     node existed — this scope boundary is deliberate, not an
//     approximation of those semantics.
//   - the grid must be MATERIALISED (Step > 0 with both bounds pinned),
//     identically to nativeClassicHistogramEligible: the sentinel side's
//     anchor grid is built from constant (Start, End, Step) parameters at
//     emit time, so there is nothing to hand it in instant mode.
//   - a subquery inner's epoch-aligned grid (ctx.stepAligned) is left on
//     the fan-out for the identical reason nativeClassicHistogramEligible
//     excludes it: chplan.RangeBucketWindowSlide carries no StepAlign
//     field.
//   - the grid step, the window and the offset must each be a WHOLE
//     number of MILLISECONDS (not seconds, unlike the timeSeries*ToGrid
//     family) — the ceil-to-ms encoding (chsql's
//     windowSlideMsEncodeFrag) has no finer resolution, so a sub-
//     millisecond shape would silently round. windowSlideMaxLookbackMs
//     additionally bounds Lookback itself to ClickHouse's own numeric
//     RANGE frame offset headroom (~24.86 days) — a longer lookback stays
//     on the fan-out.
//   - the Lookback/Step ratio must clear windowSlideMinLookbackStepRatio
//     — see that constant's own doc for the measured curve this threshold
//     is picked from.
func windowSlideShapeEligible(in classicHistogramWindowInput) bool {
	if in.shape.windowFn != sumOverTimeWindowFn {
		return false
	}
	// chsql's emitter hard-codes windowSlideMinSamples = 1 (chsql cannot
	// import this package to read histogramWindowMinSamples directly — see
	// that const's own doc). This defends the cross-package assumption: if
	// histogramWindowMinSamples(sumOverTimeWindowFn) ever stopped returning
	// stalenessMinSamples, this node's real-row-count gate would silently
	// answer the wrong MinSamples floor rather than failing loudly, so this
	// case falls back to the fan-out (which reads shape.minSamples() fresh
	// on every query) instead.
	if in.shape.minSamples() != stalenessMinSamples {
		return false
	}
	if in.ctx.step <= 0 || in.ctx.start.IsZero() || in.ctx.end.IsZero() {
		return false
	}
	if in.ctx.stepAligned {
		return false
	}
	if in.win.lookback <= 0 {
		return false
	}
	if !wholeMilliseconds(in.ctx.step) || !wholeMilliseconds(in.win.lookback) || !wholeMilliseconds(in.win.offset) {
		return false
	}
	lookbackMs := in.win.lookback.Milliseconds()
	if lookbackMs > windowSlideMaxLookbackMs {
		return false
	}
	return in.win.lookback >= windowSlideMinLookbackStepRatio*in.ctx.step
}

// wholeMilliseconds reports whether d is an exact multiple of one
// millisecond — the resolution windowSlideMsEncodeFrag's ceil-to-ms
// encoding is proven exact at (issue #2408's Task-1 spike).
func wholeMilliseconds(d time.Duration) bool {
	return d%time.Millisecond == 0
}

// windowSlideClassicHistogramNode builds the anchor-injection window
// aggregate over the matchers-filtered, `le`-restricted scan. pred is the
// predicate for THIS arm (the caller folds the temporality split into it),
// mirroring nativeClassicHistogramNode exactly — same scan/predicate/`le`
// restriction/GroupBy construction, only the produced node kind differs.
func windowSlideClassicHistogramNode(in classicHistogramWindowInput, pred chplan.Expr) chplan.Node {
	var rawSide chplan.Node = in.scan
	if pred != nil {
		rawSide = &chplan.Filter{Input: in.scan, Predicate: pred}
	}
	// `le` matcher restriction (#1478) — narrows BucketCounts /
	// ExplicitBounds to the retained rungs BEFORE the window fold reads
	// them, so the ladder is built over the restricted layout exactly as
	// the fan-out's aggregation is. No-op when leMatchers is empty.
	rawSide = classicBucketLeRestriction(rawSide, in.leMatchers, in.s)

	return &chplan.RangeBucketWindowSlide{
		Input: rawSide,
		Start: in.ctx.start.UTC(),
		End:   in.ctx.end.UTC(),
		Step:  in.ctx.step,
		Range: in.win.lookback,
		// The fan-out's window is `(anchor - Offset - Range, anchor - Offset]`
		// and the window-slide aggregate's is the same span, so Offset
		// threads through unchanged — see chplan.RangeBucketWindowSlide.Offset.
		Offset: in.win.offset,
		// The grouping keys read the raw table columns, so this is a
		// series-identity BINDING site — see [canonicalGroupKeyExpr].
		GroupBy:           canonicalGroupKeyExprs([]chplan.Expr{histogramIdentityExpr(in.s)}, in.s),
		GroupByAliases:    []string{in.s.AttributesColumn},
		AnchorAlias:       stepGridAnchorColumn,
		TimestampCol:      in.s.TimestampColumn,
		BucketCountsCol:   in.s.BucketCountsColumn,
		ExplicitBoundsCol: in.s.ExplicitBoundsColumn,
	}
}
