package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// range_window_fixed_accumulator.go implements
// chplan.RangeWindow.FixedAccumulatorExtrapolated (cerberus issue #2760): a
// fixed-size per-anchor accumulator decomposition for the EXTRAPOLATED
// window family — rate() / increase() / delta() — replacing the
// groupArray + arraySort + arrayFilter array-fold fan-out
// (emitWindowedArrayExtrapolatedMatrix) for the query_range MATRIX shape
// (OuterRange > 0).
//
// The array-fold path needs the FIRST and LAST sample of each anchor's
// window, not merely adjacent pairs — issue #2759's lagInFrame adjacency
// shape (range_window_lag_adjacency.go) cannot serve rate/increase/delta
// directly for that reason. This file's decomposition instead reads:
//
//   - argMin(value, ts) / argMax(value, ts) for the window's first/last
//     sample (Prom's samples.Floats[0] / samples.Floats[numSamplesMinusOne]),
//     plus min(ts) / max(ts) for their timestamps.
//   - count() for the >= 2 in-window-sample rule (Prom's len(samples.Floats)).
//   - for rate()/increase() only (kind.isCounter()): a counter-reset
//     correction term, reusing #2759's own lagInFrame validity check and
//     reset kernel (lagAdjacencyValidPrevFrag / lagAdjResetsKernel) — see
//     fixedAccumCounterDeltaFrag's doc below for the telescoping-sum algebra
//     that makes `(last_val - first_val) + reset_sum` exactly equal
//     CounterOrDeltaSum's array-fold CUMULATIVE answer, and sum(value) -
//     first_val its DELTA answer.
//
// Every quantity is a ClickHouse aggregate function evaluated once per
// (series, anchor) GROUP BY — no array is ever materialized for the
// window itself, so peak memory tracks group COUNT (series x anchors), not
// group CONTENTS (the argMax-class admission the issue's "10x admission gap
// in lwr_fanout_bound.go" note refers to), unlike the array-fold path's
// groupArray-class state.
//
// # Duplicate-timestamp handling
//
// Unlike changes/resets/irate/idelta (issue #2759), the array-fold path this
// file replaces DOES deduplicate same-(series,ts) rows before computing
// first/last/count (dedupWindowPairsByTsFrag), keeping the MAX-VALUE row of
// each tied timestamp — an OTel/ClickHouse ingestion artifact
// (dedupWindowPairsByTsFrag's own doc), not a Prometheus semantic, but this
// feature must reproduce it exactly to stay a bit-identical fallback. A
// naive single-pass `argMin(value, ts)` does NOT reproduce it: ClickHouse
// only guarantees ONE tied row wins, not which (and a signed-tuple key such
// as `argMin(value, tuple(ts, -value))` risks NaN-comparison divergence from
// arraySort's own tie order). fixedAccumDedupLayer instead collapses ties
// RELATIONALLY first, via one forward-looking leadInFrame pass that keeps
// exactly the max-value row of each timestamp run (the same
// last-of-run-in-(ts,value)-ascending-order test #2759's own
// lagAdjIsLastOfRunAlias flag uses, just for a different purpose there).
// Once every row's timestamp is unique within its series, first/last/count
// need no further tie-break: argMin/argMax/min/max/count over a
// unique-timestamp group are unambiguous regardless of NaN payloads.
//
// # Temporality-bearing counters (rate() / increase() over an OTel Sum table)
//
// A window whose TemporalityColumn is set needs CounterOrDeltaSum's runtime
// branch reproduced: DELTA-temporality samples are pre-differenced (the raw
// per-window sum minus the window's own first sample, no reset-correction
// walk at all — fixedAccumCounterDeltaFrag's DELTA arm), while CUMULATIVE (or
// unset) reads the counter-reset-aware form this file already builds. Both
// arms are computed as ordinary fixed accumulators (sum() alongside
// argMin/argMax), so the runtime If() branch costs nothing extra in memory
// shape — see fixedAccumCounterDeltaFrag.
//
// A temporality-bearing counter ADDITIONALLY needs the counter's
// zero-crossing clamp (extrapolatedValueExpr's own durStart branch) to read a
// RECONSTRUCTED first_val for a DELTA series: a single DELTA sample is an
// increment, not a running total, so feeding it directly into "did the
// counter start inside this window" arithmetic answers the wrong question.
// That reconstruction is one of two, mutually exclusive mechanisms — both
// REUSED UNCHANGED from range_window.go, carrying this file's own scalar
// accumulator columns as their passthrough instead of a window_pairs array
// (fixedAccumDeltaLevelSource / the deltaMatrixLevelSourceAggregate call in
// fixedAccumRegroupLayer are the pass-through siblings that wire this in):
//
//   - the default, BOUNDED-LOOKBACK approximation: a running per-anchor level
//     built from deltaPrefixPairsAlias / deltaPrefixSumFrag,
//     deltaMatrixLevelSource / deltaFirstValFrag (bounded by
//     e.deltaPrefixLookbackNS, not the window itself).
//   - cerberus issue #2389's EXACT, retention-independent DELTA-prefix
//     aggregate mechanism, selected only when both
//     r.DeltaPrefixAggregateInput != nil AND e.deltaPrefixReadEnabled
//     (useAggregateDeltaPrefix, mirroring
//     emitWindowedArrayExtrapolatedMatrix's own gate exactly):
//     deltaMatrixLevelSourceAggregate reads a schema-provisioned
//     DeltaPrefixTable instead of an unbounded/lookback-bounded raw scan.
//
// Getting there needs the array-fold's OWN sample-anchor fan-out
// (windowedMatrixFanoutAnchorTsFrag) and scan bound
// (applyMatrixFanoutScanBound / applyMatrixFanoutScanBoundAggregate for the
// useAggregateDeltaPrefix arm), reused UNCHANGED: a temporality-bearing
// fan-out assigns a raw sample to EITHER its covering anchors' actual
// windows OR (for a DELTA sample outside every window) a "prefix" anchor
// supplying history for the reconstruction — the two are mutually exclusive
// per (row, anchor) instance by construction (see windowedMatrixFanoutAnchorTsFrag's
// own doc), so every fixed accumulator below folds in an extra
// `TimeUnix > anchor_ts - range` condition (via ClickHouse's universal `-If`
// aggregate combinator) whenever a temporality-bearing window is in play,
// to exclude prefix-only row instances from the window's own first/last/
// count/reset-sum — the array-fold's equivalent split is the SAME condition,
// applied to which of two arrays (window_pairs / delta_prefix_pairs) a
// fanned-out row's arrayJoin instance lands in.
//
// # Scope (this cut)
//
// Every RangeWindow shape the array-fold path supports for the EXTRAPOLATED
// matrix family is now eligible (fixedAccumulatorEligible,
// internal/promql/lower_strategy.go), including both DELTA-prefix
// reconstruction mechanisms above (cerberus issue #2797). The plain fan-out
// remains the permanent, fully-functional fallback for the excluded shapes
// (rw.Identity, the INSTANT single-anchor shape, rw.Variants != nil).
const (
	// fixedAccumFirstTsAlias / fixedAccumLastTsAlias / fixedAccumFirstValAlias
	// deliberately match the array-fold path's own "first_ts" / "last_ts" /
	// "first_val" aliases (range_window.go's mid-layer SELECT), so this file
	// can call that file's sampledIntervalFrag / durationToStartRawFrag /
	// durationToEndRawFrag helpers UNCHANGED — those already render
	// `BareIdent("first_ts")` / `BareIdent("last_ts")` internally.
	fixedAccumFirstTsAlias  = "first_ts"
	fixedAccumLastTsAlias   = "last_ts"
	fixedAccumFirstValAlias = "first_val"
	fixedAccumLastValAlias  = "last_val"
	fixedAccumCountAlias    = "n"
	fixedAccumResetSumAlias = "reset_sum"
	// fixedAccumSumValAlias is the raw per-window value sum — only computed
	// for a temporality-bearing counter window, feeding
	// fixedAccumCounterDeltaFrag's DELTA arm (CounterOrDeltaSum's own
	// `arraySum(values) - firstVal`).
	fixedAccumSumValAlias          = "sum_val"
	fixedAccumSampledIntervalAlias = "sampled_interval"
	fixedAccumDurationToStartAlias = "duration_to_start"
	fixedAccumDurationToEndAlias   = "duration_to_end"
)

// fixedAccumulatorMatrixShapeCheck validates the RangeWindow fields this
// emitter needs, mirroring lagAdjacencyMatrixShapeCheck. r.DeltaPrefixAggregateInput
// is intentionally NOT rejected here (cerberus issue #2797) — a set value is
// a valid, eligible shape now; emitFixedAccumulatorExtrapolatedMatrix reads
// it (gated behind e.deltaPrefixReadEnabled, the same runtime knob
// emitWindowedArrayExtrapolatedMatrix consults) to select
// deltaMatrixLevelSourceAggregate instead of the bounded-lookback
// approximation.
func fixedAccumulatorMatrixShapeCheck(r *chplan.RangeWindow) error {
	if r.TimestampColumn == "" {
		return fmt.Errorf("%w: RangeWindow.TimestampColumn unset", ErrUnsupported)
	}
	if r.ValueColumn == "" {
		return fmt.Errorf("%w: RangeWindow.ValueColumn unset", ErrUnsupported)
	}
	if r.OuterRange <= 0 || r.Step <= 0 {
		return fmt.Errorf("%w: RangeWindow.FixedAccumulatorExtrapolated requires a matrix (OuterRange > 0, Step > 0) window", ErrUnsupported)
	}
	return nil
}

// fixedAccumDedupLayer collapses duplicate-(series,ts) raw samples down to
// one row per distinct timestamp, keeping the MAX-VALUE row of each tied
// timestamp run — see this file's doc comment for why. A row is the keeper
// of its timestamp iff no LATER row, in the same (ts, value) ascending order
// groupArrayPairFrag's arraySort establishes, shares its timestamp: exactly
// #2759's own lagAdjIsLastOfRunAlias test (`leadInFrame(ts) != ts` under a
// forward-admitting frame), reused here to dedupe rather than to survivor-pick
// a window's last two samples.
//
// The scan is bounded here, at the EARLIEST point over raw innerSub —
// mirroring lagAdjacencyAnnotateLayer's own pushdown, so ClickHouse prunes
// granules before any window function runs. A temporality-bearing window
// (needsDeltaFirstLevel) needs the WIDER bound applyMatrixFanoutScanBound (or,
// when useAggregateDeltaPrefix selects the exact #2389 mechanism,
// applyMatrixFanoutScanBoundAggregate) already computes; that bound is a pure
// function of the raw row's own srcTs (never of which anchor it will later be
// assigned to), so applying it here rather than after the fan-out is
// equivalent and prunes earlier.
func (e *emitter) fixedAccumDedupLayer(
	r *chplan.RangeWindow, innerSub Frag, groupFrags []Frag, srcTs string,
	end Frag, stepNS, rangeNS, numAnchors int64, needsDeltaFirstLevel, useAggregateDeltaPrefix bool,
) (*QueryBuilder, error) {
	orderBy := []OrderKey{{Expr: Col(srcTs)}, {Expr: Col(r.ValueColumn)}}
	leadFrame := RowsCurrentRowToUnboundedFollowing()
	hasTemporality := windowTemporalityProjected(r)

	tag := NewQuery().From(innerSub)
	tag.Select(groupFrags...)
	tag.Select(Col(srcTs))
	tag.Select(Col(r.ValueColumn))
	if hasTemporality {
		tag.Select(Col(r.TemporalityColumn))
	}
	tag.Select(As(
		Neq(WindowFrame(Call("leadInFrame", Col(srcTs)), groupFrags, orderBy, leadFrame), Col(srcTs)),
		lagAdjIsLastOfRunAlias,
	))
	if useAggregateDeltaPrefix {
		if err := e.applyMatrixFanoutScanBoundAggregate(tag, r, srcTs, end, stepNS, rangeNS, numAnchors); err != nil {
			return nil, err
		}
	} else {
		e.applyMatrixFanoutScanBound(tag, r, srcTs, end, stepNS, rangeNS, numAnchors, needsDeltaFirstLevel)
	}

	dedup := NewQuery().From(tag.Frag())
	dedup.Select(groupFrags...)
	dedup.Select(Col(srcTs))
	dedup.Select(Col(r.ValueColumn))
	if hasTemporality {
		dedup.Select(Col(r.TemporalityColumn))
	}
	dedup.Where(Col(lagAdjIsLastOfRunAlias))
	return dedup, nil
}

// fixedAccumLagLayer tags each ALREADY-DEDUPED row (fixedAccumDedupLayer's
// output — one row per distinct timestamp within its series) with its
// immediately preceding sample's (ts, value) via lagInFrame. Running this
// over the deduped stream, rather than raw innerSub, is what makes the
// resulting prev_ts/prev_val the true previous DISTINCT-timestamp sample: a
// duplicate-ts sibling can never be mistaken for a genuine predecessor,
// because it no longer exists as a separate row by the time this pass runs.
//
// Only rate()/increase() (kind.isCounter()) call this — the counter-reset
// correction term reads prev_val/prev_ts; delta() (a gauge) never needs a
// predecessor at all, so emitFixedAccumulatorExtrapolatedMatrix skips this
// layer entirely for that kind.
func fixedAccumLagLayer(r *chplan.RangeWindow, dedupSource Frag, groupFrags []Frag, srcTs string) *QueryBuilder {
	orderBy := []OrderKey{{Expr: Col(srcTs)}}
	lagFrame := RowsUnboundedPrecedingToCurrentRow()
	hasTemporality := windowTemporalityProjected(r)

	lag := NewQuery().From(dedupSource)
	lag.Select(groupFrags...)
	lag.Select(Col(srcTs))
	lag.Select(Col(r.ValueColumn))
	if hasTemporality {
		lag.Select(Col(r.TemporalityColumn))
	}
	lag.Select(As(WindowFrame(Call("lagInFrame", Col(srcTs)), groupFrags, orderBy, lagFrame), lagAdjPrevTsAlias))
	lag.Select(As(WindowFrame(Call("lagInFrame", Col(r.ValueColumn)), groupFrags, orderBy, lagFrame), lagAdjPrevValAlias))
	return lag
}

// fixedAccumCounterDeltaFrag renders rate()/increase()'s counter delta,
// mirroring CounterOrDeltaSum's own runtime branch exactly:
//
//   - temporalityRef == nil (no TemporalityColumn on this window): the
//     CUMULATIVE form alone, `(last_val - first_val) + reset_sum`.
//   - temporalityRef != nil: `if(temporality = DELTA, sum_val - first_val,
//     (last_val - first_val) + reset_sum)`.
//
// CUMULATIVE-form proof: CounterOrDeltaSum's array-fold folds
// `if(c<p, c, c-p)` over every adjacent pair. A non-reset pair contributes
// `c-p`, which telescopes across the whole window to `last_val - first_val`
// regardless of how many such pairs there are. A reset pair (`c<p`)
// contributes `c` instead of `c-p` — an EXCESS of exactly `p` (the pre-reset
// value) over what the telescoping baseline already counts. Summing that
// excess over every reset pair gives `counter_delta = (last_val - first_val)
// + sum_over_resets(prev_val)`. reset_sum (this emitter's regroup-stage
// `sumIf(prev_val, valid AND curr<prev)`) is exactly that sum, restricted —
// via lagAdjacencyValidPrevFrag — to pairs whose predecessor is inside THIS
// anchor's own window, matching CounterDelta's own pairs (which only ever
// walks the anchor's own deduped window_pairs array).
//
// DELTA-form: CounterOrDeltaSum's DELTA arm is `arraySum(values) - firstVal`
// — the raw per-window sum minus the window's own first (RAW, not
// reconstructed) sample. sum_val / first_val here are the SAME fixed
// accumulators the CUMULATIVE form and the counter zero-clamp both read.
func fixedAccumCounterDeltaFrag(temporalityRef Frag) Frag {
	cumulative := Add(
		Paren(Sub(BareIdent(fixedAccumLastValAlias), BareIdent(fixedAccumFirstValAlias))),
		Col(fixedAccumResetSumAlias),
	)
	if temporalityRef == nil {
		return cumulative
	}
	delta := Sub(BareIdent(fixedAccumSumValAlias), BareIdent(fixedAccumFirstValAlias))
	return If(Eq(temporalityRef, InlineLit(schema.AggregationTemporalityDelta)), delta, cumulative)
}

// fixedAccumClampFirstValFrag renders the first_val extrapolatedValueExpr's
// own counter zero-crossing clamp should read: the RAW window-first value
// for a non-temporality-bearing or CUMULATIVE window, or the RECONSTRUCTED
// level (deltaAnchorLevelsAlias + the raw first value) for a DELTA series —
// see this file's own doc comment ("Temporality-bearing counters") for why a
// raw DELTA increment cannot answer the zero-clamp's question directly.
// Delegates the branch itself to range_window.go's deltaFirstValFrag,
// UNCHANGED.
func fixedAccumClampFirstValFrag(temporalityRef Frag) Frag {
	if temporalityRef == nil {
		return BareIdent(fixedAccumFirstValAlias)
	}
	return deltaFirstValFrag(
		temporalityRef,
		Add(BareIdent(deltaAnchorLevelsAlias), BareIdent(fixedAccumFirstValAlias)),
		BareIdent(fixedAccumFirstValAlias),
	)
}

// fixedAccumSampleCountMinusOneFrag renders `(n - 1)` — the sample-interval
// count the extrapolation threshold clamp divides by (see
// extrapThresholdClampExpr). The array-fold path's own
// numSamplesMinusOneFrag calls length() on a materialized array; this
// emitter has no array, only the `n` accumulator column, so it renders the
// subtraction directly.
func fixedAccumSampleCountMinusOneFrag() Frag {
	return Paren(Sub(Col(fixedAccumCountAlias), InlineLit(int64(1))))
}

// fixedAccumValueFrag adapts extrapolatedValueExpr (range_window.go, fully
// generic over its operand Frags — UNCHANGED, no fork) to this emitter's own
// column aliases. counterDelta is fixedAccumCounterDeltaFrag(temporalityRef)
// for a counter kind (kind.isCounter()) or unused for delta() —
// extrapolatedValueExpr's own Delta branch overwrites rawResult with
// `last_val - first_val` directly and never renders the counterDelta
// argument in that case, so temporalityRef is always nil there too (delta()
// never carries a TemporalityColumn — see chplan.RangeWindow's own doc).
func fixedAccumValueFrag(kind extrapolationKind, rangeSeconds float64, temporalityRef Frag) Frag {
	var counterDelta Frag
	if kind.isCounter() {
		counterDelta = fixedAccumCounterDeltaFrag(temporalityRef)
	}
	return extrapolatedValueExpr(
		kind, rangeSeconds,
		counterDelta,
		BareIdent(fixedAccumSampledIntervalAlias),
		fixedAccumClampFirstValFrag(temporalityRef),
		BareIdent(fixedAccumLastValAlias),
		BareIdent(fixedAccumDurationToStartAlias),
		BareIdent(fixedAccumDurationToEndAlias),
	)
}

// fixedAccumDeltaLevelSource is this file's sibling of range_window.go's
// deltaMatrixLevelSource: it attaches the reconstructed DELTA level
// (deltaAnchorLevelsAlias) before each anchor window, via the SAME running
// `sum() OVER (PARTITION BY series ORDER BY anchor_ts)` and the SAME
// deltaPrefixSumFrag(deltaPrefixPairsAlias) step — both reused UNCHANGED, see
// this file's own doc comment. The only difference from deltaMatrixLevelSource
// is WHICH columns pass through: this emitter carries its own scalar
// accumulators (passthroughCols) instead of a window_pairs array.
// deltaPrefixSumFrag is called with alreadyDeduped=false unconditionally: this
// file's own delta_prefix_pairs term (fixedAccumRegroupLayer, above) is
// always built via the hand-rolled groupArrayIf, never the native
// timeSeriesGroupArrayIf combinator range_window.go's array-fold site
// adopted for cerberus issue #2862 — see that issue and
// deltaMatrixLevelSourceAggregate's own doc for why the flag can't be
// derived from r.NativeGroupArray in a function this shared.
func fixedAccumDeltaLevelSource(regroupSource Frag, groupFrags []Frag, passthroughCols []string) Frag {
	selectPassthrough := func(q *QueryBuilder) {
		q.Select(groupFrags...)
		q.Select(Col(RangeWindowAnchorAlias))
		q.Select(Col(windowTemporalityAlias))
		for _, col := range passthroughCols {
			q.Select(Col(col))
		}
	}

	increments := NewQuery().From(regroupSource)
	selectPassthrough(increments)
	increments.Select(As(deltaPrefixSumFrag(Col(deltaPrefixPairsAlias), false), deltaPrefixStepAlias))

	levels := NewQuery().From(increments.Frag())
	selectPassthrough(levels)
	levels.Select(As(
		Window(Call("sum", Col(deltaPrefixStepAlias)), groupFrags, []OrderKey{{Expr: Col(RangeWindowAnchorAlias)}}),
		deltaAnchorLevelsAlias,
	))
	return levels.Frag()
}

// fixedAccumRegroupLayer builds the dedup -> [lag] -> fan-out -> regroup ->
// [delta-level] pipeline and returns the per-(series, anchor) source the
// caller's extrap/outer layers read from. Split out of
// emitFixedAccumulatorExtrapolatedMatrix to keep that function under
// golangci-lint's funlen ceiling — mirrors range_window.go's own
// windowedMatrixFanoutAnchorTsFrag split for the identical reason.
func (e *emitter) fixedAccumRegroupLayer(
	r *chplan.RangeWindow, innerSub Frag, groupFrags []Frag, srcTs string, end Frag,
	stepNS, rangeNS, numAnchors int64, hasTemporality, needsResetTerm, needsDeltaFirstLevel, useAggregateDeltaPrefix bool,
) (Frag, error) {
	dedupQuery, err := e.fixedAccumDedupLayer(r, innerSub, groupFrags, srcTs, end, stepNS, rangeNS, numAnchors, needsDeltaFirstLevel, useAggregateDeltaPrefix)
	if err != nil {
		return nil, err
	}
	dedupSource := dedupQuery.Frag()

	preFanoutSource := dedupSource
	if needsResetTerm {
		preFanoutSource = fixedAccumLagLayer(r, dedupSource, groupFrags, srcTs).Frag()
	}

	fanout := NewQuery().From(preFanoutSource)
	fanout.Select(groupFrags...)
	fanout.Select(Col(srcTs))
	fanout.Select(Col(r.ValueColumn))
	if needsResetTerm {
		fanout.Select(Col(lagAdjPrevTsAlias))
		fanout.Select(Col(lagAdjPrevValAlias))
	}
	if hasTemporality {
		fanout.Select(Col(r.TemporalityColumn))
	}
	fanout.Select(RawAs(
		windowedMatrixFanoutAnchorTsFrag(r, end, srcTs, stepNS, rangeNS, numAnchors, needsDeltaFirstLevel, useAggregateDeltaPrefix),
		RangeWindowAnchorAlias,
	))

	windowStart := rangeStartFrag(Col(RangeWindowAnchorAlias), rangeNS)
	// inWindow excludes a DELTA-prefix-only fan-out instance (a raw sample
	// assigned to this anchor purely to feed the level reconstruction, never
	// a member of the anchor's own (anchor-range, anchor] window) from every
	// accumulator below — see this file's own doc comment. Every fanned-out
	// row is a genuine window member by construction when
	// !needsDeltaFirstLevel (the plain, non-widened fan-out never assigns a
	// prefix-only instance), so the condition is omitted there rather than
	// rendered as an always-true no-op.
	inWindow := func(cond Frag) Frag { return cond }
	if needsDeltaFirstLevel {
		inWindow = func(cond Frag) Frag { return And(Gt(Col(srcTs), windowStart), cond) }
	}

	regroup := NewQuery().From(fanout.Frag())
	regroup.Select(groupFrags...)
	regroup.Select(Col(RangeWindowAnchorAlias))
	if hasTemporality {
		regroup.Select(As(Call("any", Col(r.TemporalityColumn)), windowTemporalityAlias))
	}
	if needsDeltaFirstLevel {
		inWindowCond := Gt(Col(srcTs), windowStart)
		regroup.Select(As(Call("countIf", inWindowCond), fixedAccumCountAlias))
		regroup.Select(As(Call("minIf", Col(srcTs), inWindowCond), fixedAccumFirstTsAlias))
		regroup.Select(As(Call("argMinIf", Col(r.ValueColumn), Col(srcTs), inWindowCond), fixedAccumFirstValAlias))
		regroup.Select(As(Call("maxIf", Col(srcTs), inWindowCond), fixedAccumLastTsAlias))
		regroup.Select(As(Call("argMaxIf", Col(r.ValueColumn), Col(srcTs), inWindowCond), fixedAccumLastValAlias))
	} else {
		regroup.Select(As(Call("count"), fixedAccumCountAlias))
		regroup.Select(As(Call("min", Col(srcTs)), fixedAccumFirstTsAlias))
		regroup.Select(As(Call("argMin", Col(r.ValueColumn), Col(srcTs)), fixedAccumFirstValAlias))
		regroup.Select(As(Call("max", Col(srcTs)), fixedAccumLastTsAlias))
		regroup.Select(As(Call("argMax", Col(r.ValueColumn), Col(srcTs)), fixedAccumLastValAlias))
	}
	if needsResetTerm {
		regroup.Select(As(
			Call("sumIf", Col(lagAdjPrevValAlias), inWindow(And(
				lagAdjacencyValidPrevFrag(rangeNS),
				lagAdjResetsKernel(Col(r.ValueColumn), Col(lagAdjPrevValAlias)),
			))),
			fixedAccumResetSumAlias,
		))
	}
	if needsDeltaFirstLevel {
		regroup.Select(As(Call("sumIf", Col(r.ValueColumn), Gt(Col(srcTs), windowStart)), fixedAccumSumValAlias))
		regroup.Select(As(
			Call("arraySort", Call("groupArrayIf", Tuple(Col(srcTs), Col(r.ValueColumn)), Lte(Col(srcTs), windowStart))),
			deltaPrefixPairsAlias,
		))
	}
	regroupKeys := make([]Frag, 0, len(groupFrags)+1)
	regroupKeys = append(regroupKeys, groupFrags...)
	regroupKeys = append(regroupKeys, Col(RangeWindowAnchorAlias))
	regroup.GroupBy(regroupKeys...)

	regroupSource := regroup.Frag()
	if needsDeltaFirstLevel {
		passthrough := []string{fixedAccumCountAlias, fixedAccumFirstTsAlias, fixedAccumFirstValAlias, fixedAccumLastTsAlias, fixedAccumLastValAlias, fixedAccumSumValAlias}
		if needsResetTerm {
			passthrough = append(passthrough, fixedAccumResetSumAlias)
		}
		if useAggregateDeltaPrefix {
			// false: see fixedAccumDeltaLevelSource's own doc — this file's
			// delta_prefix_pairs term never adopts the native combinator.
			regroupSource, err = e.deltaMatrixLevelSourceAggregate(r, regroupSource, groupFrags, passthrough, false, end, stepNS, rangeNS, numAnchors)
			if err != nil {
				return nil, err
			}
		} else {
			regroupSource = fixedAccumDeltaLevelSource(regroupSource, groupFrags, passthrough)
		}
	}
	return regroupSource, nil
}

// emitFixedAccumulatorExtrapolatedMatrix is the fixed-accumulator sibling of
// emitWindowedArrayExtrapolatedMatrix (cerberus issue #2760): rate() /
// increase() / delta() over a query_range matrix window (OuterRange > 0),
// with every per-(series, anchor) quantity read as a ClickHouse aggregate
// (count/min/max/argMin/argMax/sum/sumIf, plus their -If forms for a
// temporality-bearing window) instead of a materialized
// groupArray + arraySort + arrayFilter array. See this file's own doc
// comment for the full design (duplicate-timestamp handling, the
// counter-reset telescoping-sum proof, the temporality-bearing extension,
// and this cut's scope).
//
// Unlike emitWindowedArrayExtrapolatedMatrix's fan-out this emitter does NOT
// wrap its sample-anchor fan-out in rateWindowFanoutBoundedSourceFrag's
// row-count guard: that guard bounds groupArray-CLASS memory (the fold's
// per-group array state grows with the fan-out row count), which does not
// apply here — every per-(series, anchor) reduction below is a fixed-size
// aggregate state regardless of how many rows feed it. Issue #2759's own
// lagInFrame adjacency shape (range_window_lag_adjacency.go) established this
// same precedent: neither emitLagAdjacencyChangesResets nor
// emitLagAdjacencyPairs applies the guard either.
func (e *emitter) emitFixedAccumulatorExtrapolatedMatrix(r *chplan.RangeWindow, kind extrapolationKind) error {
	if err := fixedAccumulatorMatrixShapeCheck(r); err != nil {
		return err
	}

	end := endExprFrag(r)
	rangeNS := r.Range.Nanoseconds()
	stepNS := r.Step.Nanoseconds()
	rangeSeconds := r.Range.Seconds()
	numAnchors := r.OuterRange.Nanoseconds()/stepNS + 1
	end, numAnchors = stepAlignGrid(r, end, stepNS, numAnchors)
	anchor := verbatim("anchor_ts")
	rangeStart := rangeStartFrag(anchor, rangeNS)
	groupFrags, err := e.collectGroupByFrags(r.GroupBy)
	if err != nil {
		return err
	}

	innerSub, err := e.subqueryFrag(r.Input)
	if err != nil {
		return err
	}
	innerSub, srcTs := fanoutTsSource(innerSub, r.TimestampColumn)

	hasTemporality := windowTemporalityProjected(r)
	needsResetTerm := kind.isCounter()
	// needsDeltaFirstLevel mirrors emitWindowedArrayExtrapolatedMatrix's own
	// flag exactly: a temporality-bearing counter window (delta() never
	// carries TemporalityColumn, so this is always false for that kind).
	needsDeltaFirstLevel := hasTemporality && kind.isCounter()
	// useAggregateDeltaPrefix mirrors emitWindowedArrayExtrapolatedMatrix's own
	// gate exactly (cerberus issue #2389's exact, retention-independent
	// DELTA-prefix aggregate mechanism) — see this file's own doc comment.
	useAggregateDeltaPrefix := needsDeltaFirstLevel && r.DeltaPrefixAggregateInput != nil && e.deltaPrefixReadEnabled

	regroupSource, err := e.fixedAccumRegroupLayer(r, innerSub, groupFrags, srcTs, end,
		stepNS, rangeNS, numAnchors, hasTemporality, needsResetTerm, needsDeltaFirstLevel, useAggregateDeltaPrefix)
	if err != nil {
		return err
	}

	extrap := NewQuery().From(regroupSource)
	extrap.Select(groupFrags...)
	extrap.Select(Col(RangeWindowAnchorAlias))
	extrap.Select(Col(fixedAccumCountAlias))
	extrap.Select(Col(fixedAccumFirstValAlias))
	extrap.Select(Col(fixedAccumLastValAlias))
	if needsResetTerm {
		extrap.Select(Col(fixedAccumResetSumAlias))
	}
	if needsDeltaFirstLevel {
		extrap.Select(Col(fixedAccumSumValAlias))
		extrap.Select(Col(windowTemporalityAlias))
		extrap.Select(Col(deltaAnchorLevelsAlias))
	}
	// sampledIntervalFrag / durationToStartRawFrag / durationToEndRawFrag
	// (range_window.go) render BareIdent("first_ts") / BareIdent("last_ts")
	// internally — safe to reuse unchanged because this emitter's own
	// accumulator aliases are named identically (see this file's const
	// block doc).
	extrap.Select(As(sampledIntervalFrag(), fixedAccumSampledIntervalAlias))
	nm1 := fixedAccumSampleCountMinusOneFrag()
	extrap.Select(As(
		extrapThresholdClampExpr(durationToStartRawFrag(rangeStart), BareIdent(fixedAccumSampledIntervalAlias), nm1),
		fixedAccumDurationToStartAlias,
	))
	extrap.Select(As(
		extrapThresholdClampExpr(durationToEndRawFrag(anchor), BareIdent(fixedAccumSampledIntervalAlias), nm1),
		fixedAccumDurationToEndAlias,
	))

	outer := NewQuery().From(extrap.Frag())
	outer.Select(groupFrags...)
	outer.Select(Col(RangeWindowAnchorAlias))
	projectAnchorAsTimestampColumn(outer, r)
	var temporalityRef Frag
	if hasTemporality {
		temporalityRef = BareIdent(windowTemporalityAlias)
	}
	outer.Select(As(fixedAccumValueFrag(kind, rangeSeconds, temporalityRef), r.ValueColumn))
	outer.Where(Gte(Col(fixedAccumCountAlias), InlineLit(int64(2))))

	return e.emitSelect(outer)
}
