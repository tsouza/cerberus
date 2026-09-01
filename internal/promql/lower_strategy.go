package promql

import (
	"math"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// This file defines the BOOT-WIRED POLYMORPHIC lowering seam for the
// ClickHouse-native timeSeries*ToGrid family. The decision of WHICH lowering
// to use (native aggregate vs. the generic SQL fan-out) is made ONCE at boot —
// from the already-resolved chopt.EnabledSet — and injected as a RangeLowerers
// table. The per-query lowering path then calls through that table with NO
// feature-flag or server-version conditional, AND NO nil/presence check on the
// strategy: every field is ALWAYS a concrete non-nil impl, so the dispatch site
// is a plain interface method call. The only per-query decisions are AST
// node-type dispatch and query-SHAPE eligibility, which live INSIDE the chosen
// strategy: a native impl that cannot handle a shape DELEGATES to its embedded
// fan-out fallback (never returns nil), so the interface method ALWAYS returns
// a valid lowering.
//
// Why a per-FUNCTION table rather than one global bool: the features are
// independent (native rate may be on while native resample is off, and vice
// versa), so the wiring composes per-function. cmd/cerberus builds the concrete
// strategies once from EnabledSet.Has(chopt.FeatureTSGridRange) /
// .Has(chopt.FeatureTSGridResample) and threads the table down through the prom
// handler -> lang adapter -> LowerOpts. The promql package cannot import chopt
// (the dependency-cone rule), so the strategy TYPES live here and the
// feature/version READ lives at boot — exactly where the rule requires it.
//
// Wiring shape (at boot, the ONLY place the feature read happens):
//
//	rate = enabled ? NativeRateLowerer{Fallback: FanoutRateLowerer{}} : FanoutRateLowerer{}
//
// The fan-out impl is the concrete DEFAULT; it is never nil. The zero value of
// RangeLowerers carries nil fields, which is the "no caller opted in" sentinel
// resolved to the all-fan-out table by [RangeLowerers.withDefaults] at the
// single lowering-entry seam — never at the per-query dispatch site.

// RateLowerer lowers a range-mode rate RangeWindow to a chplan node. It ALWAYS
// returns a valid lowering: the native impl emits the native node for a
// shape-eligible window and delegates to its embedded fan-out fallback for any
// other shape; the fan-out impl returns the generic RangeWindow directly. The
// shape eligibility (rate-over-counter with a materialised grid and a plain
// Scan/Filter input) is intrinsic and lives inside the implementation; it is
// NOT a feature-flag branch.
type RateLowerer interface {
	// LowerRate returns the chplan node for rw — the native RangeWindowGridNative
	// for a shape the impl handles, or the fan-out lowering otherwise. It never
	// returns nil.
	LowerRate(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node
}

// IncreaseLowerer lowers a range-mode increase(<counter>[range]) RangeWindow
// to a chplan node. It ALWAYS returns a valid lowering: the native impl
// reuses rate's own timeSeriesRateToGrid aggregate (multiplied back by the
// window seconds at emit time — see chsql.nativeGridValueExpr) for a
// shape-eligible window and delegates to its embedded fan-out fallback for
// any other shape; the fan-out impl returns the generic RangeWindow directly.
// The shape eligibility (increase-over-counter with a materialised grid and a
// plain Scan/Filter input) is intrinsic and lives inside the implementation;
// it is NOT a feature-flag branch. Mirrors [RateLowerer] rather than
// [ChangesLowerer] because increase() shares rate's DELTA-temporality
// runtime-branch guard (see [NativeIncreaseLowerer.LowerIncrease]).
type IncreaseLowerer interface {
	// LowerIncrease returns the chplan node for rw — the native
	// RangeWindowGridNative for a shape the impl handles, or the fan-out
	// lowering otherwise. It never returns nil.
	LowerIncrease(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node
}

// StalenessLowerer lowers a range-mode bare instant-vector selection (the
// staleness shape) to a chplan node. It ALWAYS returns a valid lowering: the
// native impl emits the native resample node and the fan-out impl emits the
// generic RangeLWR. The build closure carries the already-resolved
// scan/pred/anchor/grid for the selector; the strategy reads only intrinsic
// shape from it (never a feature flag).
type StalenessLowerer interface {
	// LowerStaleness returns the chplan node for in — the native
	// RangeWindowStaleResample for a shape the impl handles, or the fan-out RangeLWR
	// otherwise. It never returns nil.
	LowerStaleness(in stalenessLowerInput) chplan.Node
}

// stalenessLowerInput carries the resolved inputs the range-mode staleness wrap
// has already computed (the matchers-filtered scan, the eval grid, the offset,
// and the schema column names), so a strategy can build the resample / RangeLWR
// node without re-deriving them. It is the intrinsic SHAPE description — no
// feature/version state rides here.
type stalenessLowerInput struct {
	// input is the matchers-filtered scan (Scan, Filter-over-Scan, or the
	// gauge+sum merge / companion union) exposing the canonical column names.
	input chplan.Node
	// start / end / step define the materialised query_range grid; lookback is
	// the staleness horizon (instantLookback). offset folds the PromQL offset
	// onto the membership window.
	start, end             time.Time
	step, lookback, offset time.Duration

	// stepAligned mirrors lowerCtx.stepAligned: the caller resolved this
	// staleness wrap over a subquery inner's epoch-aligned (phase 0) grid,
	// not the outer request's own [start, end]. It threads onto
	// chplan.RangeLWR.StepAlign so chplan.ReanchorRange re-derives each
	// shard's grid from epoch 0 instead of assigning the shard's raw
	// bounds verbatim.
	stepAligned bool

	// sampleTimestamp asks for the selected sample's OWN timestamp to be
	// published alongside the per-anchor value, as
	// chplan.RangeLWRSampleTimestampColumn. Only the range-mode
	// `timestamp(<vector-selector>)` lowering sets it: that is the one PromQL
	// shape whose result is the sample's time rather than the evaluation
	// step. It is intrinsic query SHAPE, not feature state — which is why a
	// strategy is allowed to read it.
	sampleTimestamp bool

	metricNameCol, attributesCol string
	timestampCol, valueCol       string
}

// ChangesLowerer lowers a range-mode changes(<v>[range]) RangeWindow to a
// chplan node. It ALWAYS returns a valid lowering: the native impl emits the
// native RangeWindowGridNative (Func="changes" -> timeSeriesChangesToGrid) for a
// shape-eligible window and delegates to its embedded fan-out fallback for any
// other shape; the fan-out impl returns the generic RangeWindow directly. The
// shape eligibility is intrinsic and lives inside the implementation; it is NOT
// a feature-flag branch.
type ChangesLowerer interface {
	// LowerChanges returns the chplan node for rw — the native
	// RangeWindowGridNative for a shape the impl handles, or the fan-out lowering
	// otherwise. It never returns nil.
	LowerChanges(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node
}

// ResetsLowerer lowers a range-mode resets(<counter>[range]) RangeWindow to a
// chplan node, mirroring [ChangesLowerer]: native impl emits RangeWindowGridNative
// (Func="resets" -> timeSeriesResetsToGrid) for an eligible window, fan-out
// fallback otherwise. It never returns nil.
type ResetsLowerer interface {
	// LowerResets returns the chplan node for rw — the native RangeWindowGridNative
	// for a shape the impl handles, or the fan-out lowering otherwise. It never
	// returns nil.
	LowerResets(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node
}

// IrateLowerer lowers a range-mode irate(<counter>[range]) RangeWindow to a
// chplan node, mirroring [ChangesLowerer]: the native impl emits a
// RangeWindowGridNative (Func="irate" -> timeSeriesInstantRateToGrid, the
// trailing-pair counter-reset-corrected instantaneous rate) for an eligible
// window, layered above the lagInFrame annotation shape (cerberus issue
// #2759, chplan.RangeWindow.LagAdjacency) as its own fallback's fallback, and
// the array-fold fan-out (window_pairs[length]/[length-1]) beneath that. It
// ALWAYS returns a valid lowering — every impl in the chain delegates to its
// embedded fallback for a shape it cannot handle, and the fan-out impl
// returns rw unchanged. Never nil.
type IrateLowerer interface {
	// LowerIrate returns the chplan node for rw. Never nil.
	LowerIrate(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node
}

// IdeltaLowerer is IrateLowerer's sibling for idelta(<gauge>[range]) (native
// timeSeriesInstantDeltaToGrid — the trailing-pair difference, NO
// counter-reset correction). Never nil.
type IdeltaLowerer interface {
	// LowerIdelta returns the chplan node for rw. Never nil.
	LowerIdelta(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node
}

// LastOverTimeLowerer lowers a range-mode last_over_time(<v>[range])
// RangeWindow to a chplan node. It ALWAYS returns a valid lowering: the
// native impl emits chplan.RangeWindowStaleResample — the SAME native node
// [StalenessLowerer]'s native impl builds, reusing
// timeSeriesResampleToGridWithStaleness with the matrix [range] as the
// staleness parameter in place of the bare-selector shape's fixed 5m
// instantLookback — for a shape-eligible window, and delegates to its
// embedded fan-out fallback for any other shape; the fan-out impl returns rw
// unchanged. The shape eligibility (last_over_time func, materialised grid,
// plain Scan/Filter input, the fixed [Attributes] grouping key
// RangeWindowStaleResample requires) is intrinsic and lives inside the
// implementation; it is NOT a feature-flag branch.
//
// Unlike every other RangeLowerers member, the two functions
// [rangeFnPreservesName] names (last_over_time, first_over_time) keep
// `__name__` on their output — so LowerLastOverTime's caller
// (lowerRangeVectorCall) treats a returned node that differs from the input
// rw as ALREADY the canonical named shape (see nativeLastOverTimeNode's own
// doc) rather than routing it through the fan-out's name-synthesis wrap.
// first_over_time has no native sibling — the aggregate carries the LATEST
// in-window sample forward, never the earliest — so it stays on the fan-out
// unconditionally and never reaches this interface at all.
type LastOverTimeLowerer interface {
	// LowerLastOverTime returns the chplan node for rw — the native
	// RangeWindowStaleResample for a shape the impl handles, or the fan-out
	// lowering otherwise. It never returns nil.
	LowerLastOverTime(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node
}

// OverTimeLowerer lowers a range-mode sum_over_time(...) / avg_over_time(...)
// RangeWindow to a chplan node (cerberus issue #2761). Like IrateLowerer there
// is no ClickHouse-native timeSeries*ToGrid member for the *_over_time
// family — the only alternative to the array-fold fan-out
// (emitWindowedArrayMatrix) is the sorted-slab shape (a single per-series
// groupArray sliced once per anchor with arrayFilter, see
// chsql/range_window_sorted_slab.go), so this interface has exactly one
// non-fan-out impl. It ALWAYS returns a valid lowering: the sorted-slab impl
// emits RangeWindow{SortedSlabOverTime: true} for a shape-eligible window and
// delegates to its embedded fan-out fallback otherwise; the fan-out impl
// returns rw unchanged. Never nil.
type OverTimeLowerer interface {
	// LowerOverTime returns the chplan node for rw. Never nil.
	LowerOverTime(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node
}

// DerivLowerer lowers a range-mode deriv(<gauge>[range]) RangeWindow to a
// chplan node, mirroring [ChangesLowerer]: the native impl emits a
// RangeWindowGridNative (Func="deriv" -> timeSeriesDerivToGrid, the per-window
// simple-linear-regression slope) for an eligible window, the fan-out fallback
// otherwise. It never returns nil.
type DerivLowerer interface {
	// LowerDeriv returns the chplan node for rw — the native RangeWindowGridNative
	// for a shape the impl handles, or the fan-out lowering otherwise. It never
	// returns nil.
	LowerDeriv(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node
}

// DeltaLowerer lowers a range-mode delta(<gauge>[range]) RangeWindow to a
// chplan node. TWO non-fan-out impls layer beneath the fan-out, mirroring
// how Rate/Increase layer native ts_grid beneath fixed-accumulator: the
// native impl emits a RangeWindowGridNative (Func="delta" ->
// timeSeriesDeltaToGrid, the per-window non-counter-corrected extrapolated
// difference, server >= 25.9) for a shape-eligible window, falling back to
// the fixed-accumulator impl (RangeWindow{FixedAccumulatorExtrapolated:
// true}, cerberus issue #2760, no version floor) for a shape the native
// aggregate cannot serve, which itself falls back to the plain fan-out.
// Unlike Rate/Increase there is no AggregationTemporality union-split:
// delta() is gauge-only in PromQL (it never counter-repairs, matching Prom's
// extrapolatedRate(isCounter=false, isRate=false) — see
// chsql.emitRangeWindowDelta / extrapolationKindDelta), so the
// DELTA-vs-CUMULATIVE runtime branch rate/increase need never applies here.
// It never returns nil.
type DeltaLowerer interface {
	// LowerDelta returns the chplan node for rw — the native RangeWindowGridNative
	// for a shape the impl handles, or the fan-out lowering otherwise. It never
	// returns nil.
	LowerDelta(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node
}

// PredictLinearLowerer lowers a range-mode predict_linear(<gauge>[range], t)
// RangeWindow to a chplan node, mirroring [ChangesLowerer]: the native impl
// emits a RangeWindowGridNative (Func="predict_linear" ->
// timeSeriesPredictLinearToGrid, the per-window slope*t + intercept forecast)
// for an eligible window, the fan-out fallback otherwise. Only a single
// whole-second literal horizon t is native-eligible — the aggregate's 5th
// parametric arg is a constant, so computed / fractional horizons delegate to
// the fan-out. It never returns nil.
type PredictLinearLowerer interface {
	// LowerPredictLinear returns the chplan node for rw — the native
	// RangeWindowGridNative for a shape the impl handles, or the fan-out lowering
	// otherwise. It never returns nil.
	LowerPredictLinear(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node
}

// RangeLowerers is the boot-wired dispatch table the lowering reads. Each field
// is the CONCRETE strategy for one promql function family, decided once at
// boot. Every field is ALWAYS non-nil on the lowering path — a fan-out-only
// deployment wires the concrete fan-out impl, NOT nil. The zero value (nil
// fields) is the "no caller opted in" sentinel, resolved to the all-fan-out
// table by [withDefaults] at the single lowering-entry seam; the per-query
// dispatch then calls the interface method directly, with no nil/presence
// check.
type RangeLowerers struct {
	// Rate handles range-mode rate(...) shapes. Concrete fan-out impl when the
	// native path is off; never nil on the lowering path.
	Rate RateLowerer
	// Increase handles range-mode increase(...) shapes (native
	// timeSeriesRateToGrid multiplied back by the window seconds, server >=
	// 25.9). Concrete fan-out impl when the native path is off; never nil on
	// the lowering path.
	Increase IncreaseLowerer
	// Staleness handles range-mode bare instant-vector selection (staleness)
	// shapes. Concrete fan-out impl when the native path is off; never nil on
	// the lowering path.
	Staleness StalenessLowerer
	// Changes handles range-mode changes(...) shapes (native
	// timeSeriesChangesToGrid, server >= 25.9). Concrete fan-out impl when the
	// native path is off; never nil on the lowering path.
	Changes ChangesLowerer
	// Resets handles range-mode resets(...) shapes (native
	// timeSeriesResetsToGrid, server >= 25.9). Concrete fan-out impl when the
	// native path is off; never nil on the lowering path.
	Resets ResetsLowerer
	// Deriv handles range-mode deriv(...) shapes (native timeSeriesDerivToGrid,
	// server >= 25.9). Concrete fan-out impl when the native path is off; never
	// nil on the lowering path.
	Deriv DerivLowerer
	// PredictLinear handles range-mode predict_linear(..., t) shapes (native
	// timeSeriesPredictLinearToGrid, server >= 25.9). Concrete fan-out impl when
	// the native path is off; never nil on the lowering path.
	PredictLinear PredictLinearLowerer
	// Delta handles range-mode delta(...) shapes: native timeSeriesDeltaToGrid
	// (server >= 25.9) falling back to the fixed-accumulator decomposition
	// (fixed_accumulator_extrapolated, no version floor), falling back to the
	// plain fan-out. Concrete fan-out impl when neither feature is on; never
	// nil on the lowering path.
	Delta DeltaLowerer

	// ClassicHistogram handles the per-series `rate` window stage under the
	// range-mode classic-histogram quantile idiom (native ladder aggregate,
	// server >= 25.9). Concrete fan-out impl when the native path is off;
	// never nil on the lowering path.
	ClassicHistogram ClassicHistogramWindowLowerer

	// QuantileRankWalk handles the classic-histogram-quantile rank walk
	// itself (native quantilePrometheusHistogram aggregate, server >= 25.10)
	// — distinct from ClassicHistogram, which handles only the range-mode
	// per-series rate WINDOW stage that feeds this one's Input. Concrete
	// fan-out impl when the native path is off; never nil on the lowering
	// path.
	QuantileRankWalk QuantileRankWalkLowerer

	// Irate handles range-mode irate(...) shapes (native
	// timeSeriesInstantRateToGrid, server >= 25.9, layered above the
	// lagInFrame annotation, laginframe_adjacency — no version floor).
	// Concrete fan-out impl when both features are off; never nil on the
	// lowering path.
	Irate IrateLowerer
	// Idelta handles range-mode idelta(...) shapes (native
	// timeSeriesInstantDeltaToGrid, server >= 25.9, layered above the
	// lagInFrame annotation, laginframe_adjacency — no version floor).
	// Concrete fan-out impl when both features are off; never nil on the
	// lowering path.
	Idelta IdeltaLowerer

	// LastOverTime handles range-mode last_over_time(...) shapes (native
	// timeSeriesResampleToGridWithStaleness, server >= 26.6 — the SAME
	// aggregate ts_grid_resample rides). Concrete fan-out impl when the
	// native path is off; never nil on the lowering path.
	LastOverTime LastOverTimeLowerer

	// OverTime handles range-mode sum_over_time(...) / avg_over_time(...)
	// shapes (sorted-slab groupArray, index-math-sliced per anchor,
	// sorted_slab_over_time — no version floor). Concrete fan-out impl when
	// the feature is off; never nil on the lowering path.
	OverTime OverTimeLowerer

	// ClassicBucketMerge handles the aggregated classic-histogram-quantile
	// cross-series merge stage (sumMap + arrayCumSum for the SUM fold,
	// classic_bucket_merge_summap.go — no version floor,
	// classic_bucket_merge_summap chopt feature). Concrete fan-out impl
	// (the groupArray + per-rung fold) when the feature is off; never nil
	// on the lowering path.
	ClassicBucketMerge ClassicBucketMergeLowerer

	// ExpHistogramMerge handles the across-series exponential-histogram
	// merge stage (two-pass sumMap keying for the instant, single-group,
	// SUM fold shape, exp_histogram_merge_summap.go — no version floor,
	// exp_histogram_merge_summap chopt feature). Concrete fan-out impl
	// (the existing groupArray + picker fold) when the feature is off, or
	// for every ineligible shape (avg, any by/without grouping, range
	// mode); never nil on the lowering path.
	ExpHistogramMerge ExpHistogramMergeLowerer

	// ArgAndMaxFusion is the resolved chopt.FeatureArgAndMaxFusion verdict
	// (server >= 25.11, cerberus issue #2764), threaded to
	// internal/promql/binary.go's vector-vector join lowering so it can set
	// chplan.VectorJoin.ArgAndMaxFusion. Unlike every other field on this
	// struct it is a plain bool, not a swappable Lowerer: there is no
	// alternate NODE shape to select between (RangeWindow's own
	// SortedSlabOverTime / FixedAccumulator strategies pick between
	// genuinely different SQL SHAPES), only a single emission-detail bit
	// the chsql emitter reads directly off the SAME node — so a Lowerer
	// interface would add indirection with nothing to dispatch on.
	// cmd/cerberus's nativeRangeLowerers sets it from the same
	// optSet.Has(chopt.FeatureArgAndMaxFusion) read it also threads into
	// FanoutStalenessLowerer.ArgAndMaxFusion for the RangeLWR site (see
	// that field's own doc for why RangeLWR needs its own copy rather than
	// reading this one: FanoutStalenessLowerer.LowerStaleness has no
	// access to the enclosing RangeLowerers value). False (the default)
	// keeps every deployment below the version floor on the pre-fusion
	// SQL, byte-unchanged.
	ArgAndMaxFusion bool

	// VectorAgg is the resolved chopt.FeatureTSGridVectorAgg verdict
	// (cerberus issue #2763), read directly by lowerAggregate. Like
	// ArgAndMaxFusion it is a plain bool, not a swappable Lowerer: there is
	// no per-range-function strategy to select between here, only whether
	// lowerAggregate — a DIFFERENT lowering stage than any of the Native*
	// Lowerer fields above, invoked AFTER a range function has already
	// lowered to a possibly-native node — may fold an eligible outer
	// sum/min/max/avg/count by/without directly into that node's own
	// pre-explode grid via chplan.RangeWindowGridNativeVectorAgg, instead of
	// building the ordinary exploded-then-grouped chplan.Aggregate.
	//
	// It is a pure narrowing of ts_grid_range in the SAME sense
	// ts_grid_recollapse is: lowerAggregate only ever consults it AFTER
	// confirming its own input is already a *chplan.RangeWindowGridNative
	// (which can only exist when ts_grid_range fired for that node), so
	// "VectorAgg true but no native grid beneath it" is unreachable by
	// construction rather than a case this field's zero value has to guard
	// against. cmd/cerberus's nativeRangeLowerers sets it from
	// optSet.Has(chopt.FeatureTSGridVectorAgg), unconditionally (unlike
	// ts_grid_recollapse, which is threaded ONLY inside the
	// "ts_grid_range enabled" branch that builds NativeRateLowerer) because
	// this field lives on RangeLowerers itself rather than on any single
	// Native*Lowerer — mirroring FeatureTSGridInstant's own single-flag,
	// governs-every-function shape rather than nine per-function siblings.
	// False (the default) keeps every deployment on the unchanged
	// exploded-then-grouped Aggregate shape.
	VectorAgg bool

	// NativeGroupArray is the resolved chopt.FeatureTSGridGroupArray verdict
	// (cerberus issue #2749), threaded onto rate/increase/delta's own
	// chplan.RangeWindow node by lowerRangeVectorCallFanout. Like
	// ArgAndMaxFusion / VectorAgg it is a plain bool, not a swappable
	// Lowerer: it never decides WHICH of rate/increase/delta's own Native /
	// FixedAccumulator / Fanout tiers fires (that stays this field's own
	// three-tier composition), only whether the array-fold tier — reachable
	// as any of those tiers' ultimate fallback, and the fused subquery-
	// interior slab — assembles its per-series sample array via the native
	// timeSeriesGroupArray aggregate instead of groupArray + arraySort
	// (+dedup). cmd/cerberus's nativeRangeLowerers sets it from
	// optSet.Has(chopt.FeatureTSGridGroupArray), unconditionally, mirroring
	// VectorAgg's own "lives on RangeLowerers itself, not on any single
	// Native*Lowerer" posture — it is inert on every func this codebase
	// does not read chplan.RangeWindow.NativeGroupArray for (changes /
	// resets / deriv / irate / idelta / last_over_time / sum_over_time /
	// avg_over_time all keep their own, unrelated fan-out shapes). False
	// (the default) keeps every deployment on the unchanged
	// groupArray+arraySort(+dedup) array-fold assembly.
	NativeGroupArray bool
}

// withDefaults returns a copy of l with any nil strategy field filled with its
// concrete fan-out impl. This is the SINGLE normalization seam (called once at
// the lowering entry, never per query) that turns the zero-value
// "no caller opted in" sentinel into the all-fan-out table. After this, every
// field is a concrete non-nil impl and the per-query dispatch is a plain
// interface method call with no nil check.
func (l RangeLowerers) withDefaults() RangeLowerers {
	if l.Rate == nil {
		l.Rate = FanoutRateLowerer{}
	}
	if l.Increase == nil {
		l.Increase = FanoutIncreaseLowerer{}
	}
	if l.Staleness == nil {
		l.Staleness = FanoutStalenessLowerer{}
	}
	if l.Changes == nil {
		l.Changes = FanoutChangesLowerer{}
	}
	if l.Resets == nil {
		l.Resets = FanoutResetsLowerer{}
	}
	if l.Deriv == nil {
		l.Deriv = FanoutDerivLowerer{}
	}
	if l.PredictLinear == nil {
		l.PredictLinear = FanoutPredictLinearLowerer{}
	}
	if l.Delta == nil {
		l.Delta = FanoutDeltaLowerer{}
	}
	if l.ClassicHistogram == nil {
		l.ClassicHistogram = FanoutClassicHistogramWindowLowerer{}
	}
	if l.QuantileRankWalk == nil {
		l.QuantileRankWalk = FanoutQuantileRankWalkLowerer{}
	}
	if l.Irate == nil {
		l.Irate = FanoutIrateLowerer{}
	}
	if l.Idelta == nil {
		l.Idelta = FanoutIdeltaLowerer{}
	}
	if l.LastOverTime == nil {
		l.LastOverTime = FanoutLastOverTimeLowerer{}
	}
	if l.OverTime == nil {
		l.OverTime = FanoutOverTimeLowerer{}
	}
	if l.ClassicBucketMerge == nil {
		l.ClassicBucketMerge = FanoutClassicBucketMergeLowerer{}
	}
	if l.ExpHistogramMerge == nil {
		l.ExpHistogramMerge = FanoutExpHistogramMergeLowerer{}
	}
	return l
}

// FanoutRateLowerer is the concrete DEFAULT RateLowerer: it returns the generic
// fan-out RangeWindow unchanged. It is the fallback the native impl embeds AND
// the strategy a fan-out-only deployment wires directly, so the dispatch site
// never needs a nil check.
type FanoutRateLowerer struct{}

// LowerRate returns the fan-out RangeWindow rw unchanged.
func (FanoutRateLowerer) LowerRate(rw *chplan.RangeWindow, _ schema.Metrics) chplan.Node {
	return rw
}

// NativeRateLowerer is the boot-wired RateLowerer that emits the native
// timeSeriesRateToGrid lowering (a chplan.RangeWindowGridNative) for shape-eligible
// rate range-windows. cmd/cerberus wires it ONLY when chopt resolved the
// ts_grid_range feature at boot. It embeds a concrete Fallback (the fan-out
// impl): a shape it cannot handle delegates to Fallback rather than returning
// nil, so the interface method ALWAYS yields a valid lowering and the dispatch
// site stays branch-free.
type NativeRateLowerer struct {
	// Fallback is the concrete lowerer for shapes the native path cannot
	// handle. Boot wires it to FanoutRateLowerer{}.
	Fallback RateLowerer

	// Recollapse additionally defers the label-shaping Project past the native
	// aggregate on inputs that carry a hoistable one, so the OTel → Prometheus
	// attribute reshape runs once per raw series instead of once per raw row.
	// cmd/cerberus sets it ONLY when chopt resolved the ts_grid_recollapse
	// feature at boot; it is carried on the strategy rather than read per query
	// for the same reason the native/fan-out choice is. A node the deferral
	// does not apply to keeps the unchanged two-level shape — this never
	// changes WHETHER the native lowering fires, only which shape it emits.
	Recollapse bool

	// Instant additionally opts an eligible INSTANT (gridSingleAnchor, Step ==
	// 0) rate window onto chplan.RangeWindowGridNativeInstant — the degenerate
	// one-point-grid sibling of the matrix RangeWindowGridNative above (cerberus
	// issue #2748). cmd/cerberus sets it ONLY when chopt ALSO resolved
	// ts_grid_instant at boot, inside this SAME "ts_grid_range enabled" branch:
	// it is a pure narrowing of ts_grid_range (mirrors how Recollapse narrows
	// it), never independently reachable. False keeps every instant rate()
	// query on the unchanged fan-out, exactly like before this field existed.
	Instant bool
}

// LowerRate returns a RangeWindowGridNative for an eligible range-mode rate shape,
// or delegates to the embedded Fallback otherwise. A temporality-bearing window
// splits into complementary CUMULATIVE-native and DELTA-fan-out arms: the
// native aggregate has no DELTA semantics, while the fan-out emitter does.
// The eligibility predicate is the intrinsic SHAPE check (rate func,
// materialised grid, plain Scan/Filter input) — see nativeTSGridRateNode.
//
// For an INSTANT window (gridSingleAnchor) the matrix check above always
// misses (nativeTSGridRateNode requires Step > 0), so — when n.Instant is set
// — this additionally tries nativeTSGridInstantNode before falling to
// Fallback. The temporality union split does NOT (yet) extend to the instant
// arm: nativeTSGridInstantNode carries the same TemporalityColumn guard
// nativeTSGridMatrixNode does, so a temporality-bearing instant window stays
// on the fan-out unconditionally — a real, deliberate scope narrowing (not an
// oversight), tracked as cerberus issue #2843: on the real default schema
// (schema.DefaultOTelMetrics always sets AggregationTemporalityColumn), an
// instant rate() query therefore does not yet reach the native path at all.
// Every OTHER combination this feature covers (a schema without that column,
// or any of changes/resets/deriv/predict_linear, none of which is ever
// temporality-gated — see counterTemporalityRangeFn) is unaffected.
func (n NativeRateLowerer) LowerRate(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node {
	if rw.TemporalityColumn != "" {
		cumulative := *rw
		cumulative.Input = nativeTemporalityFilter(rw.Input, rw.TemporalityColumn)
		// The native aggregate is safe only after DELTA rows are excluded.
		cumulative.TemporalityColumn = ""
		if native := nativeTSGridRateNode(&cumulative, s, n.Recollapse); native != nil {
			delta := *rw
			delta.Input = temporalityFilter(rw.Input, rw.TemporalityColumn, chplan.OpEq)
			return derivedRateArm(&chplan.UnionAll{Inputs: []chplan.Node{
				native,
				&delta,
			}}, s)
		}
	}
	if native := nativeTSGridRateNode(rw, s, n.Recollapse); native != nil {
		return native
	}
	if n.Instant {
		if native := nativeTSGridInstantNode(rw, "rate", s); native != nil {
			return native
		}
	}
	return n.Fallback.LowerRate(rw, s)
}

// derivedRateArm restores the derived metric name the complementary range arms
// expose to downstream PromQL nodes. Projecting once above their positional
// union avoids repeating identical shaping work in both arms. Shared by
// NativeRateLowerer and NativeIncreaseLowerer's temporality-union arms: both
// rate() and increase() drop source __name__, so the restored value is the
// empty literal for either caller.
func derivedRateArm(input chplan.Node, s schema.Metrics) *chplan.Project {
	return &chplan.Project{
		Input: input,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: &chplan.ColumnRef{Name: chplan.RangeWindowAnchorColumn}, Alias: chplan.RangeWindowAnchorColumn},
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
		},
	}
}

// temporalityFilter preserves the selector's row shape while partitioning its
// samples by the OTLP DELTA enum. OpNe admits CUMULATIVE and UNSPECIFIED values.
// When the selector is a Project, place the predicate below it so the rejected
// arm never pays for per-row label-map reconstruction.
func temporalityFilter(input chplan.Node, column string, op chplan.BinaryOp) chplan.Node {
	predicate := &chplan.Binary{
		Op:    op,
		Left:  &chplan.ColumnRef{Name: column},
		Right: &chplan.LitInt{V: schema.AggregationTemporalityDelta},
	}
	project, ok := input.(*chplan.Project)
	if !ok {
		return fuseTemporalityFilter(input, predicate)
	}
	projectCopy := *project
	projectCopy.Input = fuseTemporalityFilter(project.Input, predicate)
	return &projectCopy
}

func fuseTemporalityFilter(input chplan.Node, predicate chplan.Expr) chplan.Node {
	filter, ok := input.(*chplan.Filter)
	if !ok {
		return &chplan.Filter{Input: input, Predicate: predicate}
	}
	filterCopy := *filter
	filterCopy.Predicate = &chplan.Binary{
		Op:    chplan.OpAnd,
		Left:  filter.Predicate,
		Right: predicate,
	}
	return &filterCopy
}

// nativeTemporalityFilter removes the fan-out-only temporality projection from
// the native arm, while placing the filter beneath selector shaping so the
// resulting input still satisfies nativeTSGridMatrixNode's four-column shape.
func nativeTemporalityFilter(input chplan.Node, column string) chplan.Node {
	project, ok := input.(*chplan.Project)
	if !ok {
		return temporalityFilter(input, column, chplan.OpNe)
	}
	filter := temporalityFilter(project.Input, column, chplan.OpNe)
	projectCopy := *project
	projectCopy.Projections = make([]chplan.Projection, 0, len(project.Projections)-1)
	for _, projection := range project.Projections {
		if projection.Alias != column {
			projectCopy.Projections = append(projectCopy.Projections, projection)
		}
	}
	projectCopy.Input = filter
	return &projectCopy
}

// FanoutIncreaseLowerer is the concrete DEFAULT IncreaseLowerer: it returns
// the generic fan-out RangeWindow (emitWindowedArrayExtrapolatedMatrix's
// undivided extrapolated increase) unchanged. It is the fallback the native
// impl embeds AND the strategy a fan-out-only deployment wires directly.
type FanoutIncreaseLowerer struct{}

// LowerIncrease returns the fan-out RangeWindow rw unchanged.
func (FanoutIncreaseLowerer) LowerIncrease(rw *chplan.RangeWindow, _ schema.Metrics) chplan.Node {
	return rw
}

// NativeIncreaseLowerer is the boot-wired IncreaseLowerer that emits the
// native timeSeriesRateToGrid lowering — multiplied back by the window
// seconds at emit time (chsql.nativeGridValueExpr) — for shape-eligible
// increase range-windows. cmd/cerberus wires it ONLY when chopt resolved the
// ts_grid_increase feature at boot. It embeds a concrete Fallback (the
// fan-out impl): a shape it cannot handle delegates to Fallback rather than
// returning nil, so the interface method ALWAYS yields a valid lowering and
// the dispatch site stays branch-free.
//
// Unlike NativeRateLowerer it carries no Recollapse field: no chopt feature
// defers the label-shaping hoist past a native increase grid in this cut (see
// nativeTSGridFn's "increase" entry doc in internal/chsql), so every eligible
// node renders the plain two-level shape.
type NativeIncreaseLowerer struct {
	// Fallback is the concrete lowerer for shapes the native path cannot
	// handle. Boot wires it to FanoutIncreaseLowerer{}.
	Fallback IncreaseLowerer
}

// LowerIncrease returns a RangeWindowGridNative for an eligible range-mode
// increase shape, or delegates to the embedded Fallback otherwise. Mirrors
// [NativeRateLowerer.LowerRate] exactly, including the DELTA-temporality
// union split: a temporality-bearing window splits into complementary
// CUMULATIVE-native and DELTA-fan-out arms, because the native aggregate has
// no DELTA semantics while the fan-out emitter does. The eligibility
// predicate is the intrinsic SHAPE check (increase func, materialised grid,
// plain Scan/Filter input) — see nativeTSGridMatrixNode.
func (n NativeIncreaseLowerer) LowerIncrease(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node {
	if rw.TemporalityColumn != "" {
		cumulative := *rw
		cumulative.Input = nativeTemporalityFilter(rw.Input, rw.TemporalityColumn)
		// The native aggregate is safe only after DELTA rows are excluded.
		cumulative.TemporalityColumn = ""
		if native := nativeTSGridMatrixNode(&cumulative, "increase", s, noRecollapse); native != nil {
			delta := *rw
			delta.Input = temporalityFilter(rw.Input, rw.TemporalityColumn, chplan.OpEq)
			return derivedRateArm(&chplan.UnionAll{Inputs: []chplan.Node{
				native,
				&delta,
			}}, s)
		}
	}
	if native := nativeTSGridMatrixNode(rw, "increase", s, noRecollapse); native != nil {
		return native
	}
	return n.Fallback.LowerIncrease(rw, s)
}

// This section wires the fixed-accumulator decomposition (cerberus issue
// #2760): per-(series, anchor) count/min/max/argMin/argMax/sumIf aggregates,
// in place of the groupArray + arraySort + arrayFilter array fold, for
// rate() / increase() / delta(). Unlike the timeSeries*ToGrid family it needs
// no server-version floor or experimental setting — see
// chopt.FeatureFixedAccumulatorExtrapolated — so cmd/cerberus wires it purely
// off the resolved EnabledSet. Every one of the three functions layers it
// BENEATH their existing native ts_grid strategy (Native{Fallback:
// FixedAccumulator{Fallback: Fanout{}}}), same embed pattern
// laginframe_adjacency uses for changes/resets — delta gained its own native
// ts_grid_delta competitor (cerberus issue #2745), so it is no longer the
// exception PredictLinearLowerer's siblings are.
//
// Scope: a temporality-bearing rate()/increase() window IS eligible — see
// chsql/range_window_fixed_accumulator.go's own doc comment
// ("Temporality-bearing counters") for the DELTA/CUMULATIVE runtime branch
// and the reconstructed counter zero-clamp this needs, including BOTH the
// bounded-lookback approximation and issue #2389's EXACT, retention-independent
// DELTA-prefix aggregate mechanism (rw.DeltaPrefixAggregateInput != nil) —
// cerberus issue #2797 closed the gap that used to exclude the latter.

// FixedAccumulatorRateLowerer is the boot-wired RateLowerer that emits
// RangeWindow{FixedAccumulatorExtrapolated: true} for a shape-eligible rate
// range-window. cmd/cerberus wires it ONLY when chopt resolved
// fixed_accumulator_extrapolated at boot, embedded beneath NativeRateLowerer
// so ts_grid_range (when also enabled) still takes priority.
type FixedAccumulatorRateLowerer struct {
	// Fallback is the concrete lowerer for shapes the fixed-accumulator path
	// cannot handle. Boot wires it to FanoutRateLowerer{}.
	Fallback RateLowerer
}

// LowerRate returns rw with FixedAccumulatorExtrapolated set for an eligible
// window, or delegates to the embedded Fallback otherwise.
func (l FixedAccumulatorRateLowerer) LowerRate(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node {
	if fixedAccumulatorEligible(rw) {
		out := *rw
		out.FixedAccumulatorExtrapolated = true
		return &out
	}
	return l.Fallback.LowerRate(rw, s)
}

// FixedAccumulatorIncreaseLowerer mirrors [FixedAccumulatorRateLowerer] for
// increase(). cmd/cerberus wires it ONLY when chopt resolved
// fixed_accumulator_extrapolated at boot, embedded beneath
// NativeIncreaseLowerer so ts_grid_increase (when also enabled) still takes
// priority.
type FixedAccumulatorIncreaseLowerer struct {
	// Fallback is the concrete lowerer for shapes the fixed-accumulator path
	// cannot handle. Boot wires it to FanoutIncreaseLowerer{}.
	Fallback IncreaseLowerer
}

// LowerIncrease returns rw with FixedAccumulatorExtrapolated set for an
// eligible window, or delegates to the embedded Fallback otherwise.
func (l FixedAccumulatorIncreaseLowerer) LowerIncrease(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node {
	if fixedAccumulatorEligible(rw) {
		out := *rw
		out.FixedAccumulatorExtrapolated = true
		return &out
	}
	return l.Fallback.LowerIncrease(rw, s)
}

// FixedAccumulatorDeltaLowerer is the boot-wired DeltaLowerer that emits
// RangeWindow{FixedAccumulatorExtrapolated: true} for a shape-eligible delta
// range-window. cmd/cerberus wires it ONLY when chopt resolved
// fixed_accumulator_extrapolated at boot, embedded beneath NativeDeltaLowerer
// so ts_grid_delta (when also enabled) still takes priority — mirroring
// [FixedAccumulatorRateLowerer] / [FixedAccumulatorIncreaseLowerer] exactly.
type FixedAccumulatorDeltaLowerer struct {
	// Fallback is the concrete lowerer for shapes the fixed-accumulator path
	// cannot handle. Boot wires it to FanoutDeltaLowerer{}.
	Fallback DeltaLowerer
}

// LowerDelta returns rw with FixedAccumulatorExtrapolated set for an eligible
// window, or delegates to the embedded Fallback otherwise. delta() carries no
// TemporalityColumn concern (it is a gauge function — see
// fixedAccumulatorEligible), so its only exclusions are the shared
// shape/Variants/DeltaPrefixAggregateInput guards.
func (l FixedAccumulatorDeltaLowerer) LowerDelta(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node {
	if fixedAccumulatorEligible(rw) {
		out := *rw
		out.FixedAccumulatorExtrapolated = true
		return &out
	}
	return l.Fallback.LowerDelta(rw, s)
}

// fixedAccumulatorEligible is the intrinsic query-SHAPE eligibility predicate
// for the fixed-accumulator decomposition — reads NO feature flag or server
// version, exactly like lagAdjacencyEligible above (whose first three clauses
// this mirrors verbatim). Every clause that fails sends the query down the
// unchanged fan-out path:
//
//   - rw.Identity must be false — the bare-vector subquery no-op path is not
//     one of the three owning functions.
//   - The window must be the materialised MATRIX grid: OuterRange > 0,
//     Step > 0, and both Start and End pinned — the fixed-accumulator
//     emitter's own dedup/lag passes need the same scan-prune bound
//     lagAdjacencyEligible's identical clause exists for.
//   - rw.Variants must be empty: the fused multi-arm shape has its own
//     emitter and does not participate in this decomposition.
//
// rw.TemporalityColumn is deliberately NOT excluded: a temporality-bearing
// rate()/increase() window IS eligible (see this file's earlier doc comment
// and chsql/range_window_fixed_accumulator.go's own "Temporality-bearing
// counters" section) — the DELTA/CUMULATIVE runtime branch and the
// reconstructed counter zero-clamp are both decomposed into fixed
// accumulators too, not merely the no-temporality case. rw.DeltaPrefixAggregateInput
// is likewise NOT excluded (cerberus issue #2797): the emitter reads it
// (gated behind e.deltaPrefixReadEnabled, resolved from the runtime context,
// not this shape predicate) to select issue #2389's exact reconstruction
// mechanism instead of the bounded-lookback approximation.
func fixedAccumulatorEligible(rw *chplan.RangeWindow) bool {
	if rw.Identity {
		return false
	}
	if rw.OuterRange <= 0 || rw.Step <= 0 || rw.Start.IsZero() || rw.End.IsZero() {
		return false
	}
	return len(rw.Variants) == 0
}

// FanoutOverTimeLowerer is the concrete DEFAULT OverTimeLowerer: it returns
// the generic fan-out RangeWindow (emitWindowedArrayMatrix's array-fold
// fan-out) unchanged. It is the fallback the sorted-slab impl embeds AND the
// strategy a fan-out-only deployment wires directly.
type FanoutOverTimeLowerer struct{}

// LowerOverTime returns the fan-out RangeWindow rw unchanged.
func (FanoutOverTimeLowerer) LowerOverTime(rw *chplan.RangeWindow, _ schema.Metrics) chplan.Node {
	return rw
}

// SortedSlabOverTimeLowerer is the boot-wired OverTimeLowerer that emits
// RangeWindow{SortedSlabOverTime: true} for a shape-eligible sum_over_time /
// avg_over_time range-window (cerberus issue #2761). cmd/cerberus wires it
// ONLY when chopt resolved sorted_slab_over_time at boot.
type SortedSlabOverTimeLowerer struct {
	// Fallback is the concrete lowerer for shapes the sorted-slab path
	// cannot handle. Boot wires it to FanoutOverTimeLowerer{}.
	Fallback OverTimeLowerer
}

// LowerOverTime returns rw with SortedSlabOverTime set for an eligible
// window, or delegates to the embedded Fallback otherwise.
func (l SortedSlabOverTimeLowerer) LowerOverTime(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node {
	if sortedSlabOverTimeEligible(rw) {
		out := *rw
		out.SortedSlabOverTime = true
		return &out
	}
	return l.Fallback.LowerOverTime(rw, s)
}

// sortedSlabOverTimeEligible is the intrinsic query-SHAPE eligibility
// predicate for the sorted-slab decomposition — reads NO feature flag or
// server version, mirroring fixedAccumulatorEligible's shape clauses (there
// is no DeltaPrefixAggregateInput concern here: that field is populated only
// by the rate/increase/delta DELTA-prefix lowering, never by *_over_time).
// Every clause that fails sends the query down the unchanged fan-out path:
//
//   - rw.Identity must be false — the bare-vector subquery no-op path is not
//     a reducer.
//   - The window must be the materialised MATRIX grid: OuterRange > 0,
//     Step > 0, and both Start and End pinned — the sorted-slab emitter's
//     per-series groupArray needs the same scan-prune bound
//     fixedAccumulatorEligible's identical clause exists for.
//   - rw.Variants must be empty: the fused multi-arm shape has its own
//     emitter and does not participate in this decomposition.
func sortedSlabOverTimeEligible(rw *chplan.RangeWindow) bool {
	if rw.Identity {
		return false
	}
	if rw.OuterRange <= 0 || rw.Step <= 0 || rw.Start.IsZero() || rw.End.IsZero() {
		return false
	}
	return len(rw.Variants) == 0
}

// FanoutStalenessLowerer is the concrete DEFAULT StalenessLowerer: it builds the
// generic fan-out RangeLWR from the resolved staleness input. It is the
// fallback the native impl embeds AND the strategy a fan-out-only deployment
// wires directly.
type FanoutStalenessLowerer struct {
	// ArgAndMaxFusion is the resolved chopt.FeatureArgAndMaxFusion verdict
	// (server >= 25.11, cerberus issue #2764), set onto every RangeLWR this
	// lowerer builds. It is INERT unless the built node also has
	// SampleTimestamp set — the fusion collapses the argMax(Value,
	// TimeUnix) + max(TimeUnix) pair that pairing exists ONLY under, see
	// [chplan.RangeLWR.ArgAndMaxFusion]'s own doc. cmd/cerberus's
	// nativeRangeLowerers sets this from the same
	// optSet.Has(chopt.FeatureArgAndMaxFusion) read that feeds
	// RangeLowerers.ArgAndMaxFusion (VectorJoin's copy of the same
	// verdict) — this lowerer carries its own copy because it has no
	// access to the enclosing RangeLowerers value LowerStaleness is called
	// through.
	ArgAndMaxFusion bool
}

// LowerStaleness builds the fan-out RangeLWR node from in.
func (l FanoutStalenessLowerer) LowerStaleness(in stalenessLowerInput) chplan.Node {
	return &chplan.RangeLWR{
		Input:           in.input,
		Start:           in.start,
		End:             in.end,
		Step:            in.step,
		Lookback:        in.lookback,
		Offset:          in.offset,
		StepAlign:       in.stepAligned,
		SampleTimestamp: in.sampleTimestamp,
		ArgAndMaxFusion: l.ArgAndMaxFusion,

		MetricNameCol: in.metricNameCol,
		AttributesCol: in.attributesCol,
		TimestampCol:  in.timestampCol,
		ValueCol:      in.valueCol,
	}
}

// NativeStalenessLowerer is the boot-wired StalenessLowerer that emits the
// native timeSeriesResampleToGridWithStaleness lowering (a
// chplan.RangeWindowStaleResample). cmd/cerberus wires it ONLY when chopt resolved
// the ts_grid_resample feature at boot. It embeds a concrete Fallback (the
// fan-out impl) for future shape carve-outs, so the interface method always
// yields a valid lowering and the dispatch site stays branch-free.
type NativeStalenessLowerer struct {
	// Fallback is the concrete lowerer for shapes the native path cannot
	// handle. Boot wires it to FanoutStalenessLowerer{}.
	Fallback StalenessLowerer
}

// LowerStaleness returns a RangeWindowStaleResample for the range-mode staleness
// input, or delegates to the embedded Fallback for the one shape the native
// aggregate cannot express.
//
// That carve-out is `in.sampleTimestamp`.
// timeSeriesResampleToGridWithStaleness returns ONLY the resampled VALUE per
// grid point (an Array(Nullable(Float64))); the timestamp of the sample each
// grid point carried forward is not among its outputs, and no member of the
// timeSeries*ToGrid family exposes it. The fan-out RangeLWR can express it —
// its collapse is an explicit GROUP BY over the fanned rows, so a second
// aggregate over the same bucket recovers the sample's own time — so the
// carve-out delegates rather than emitting an answer the native shape would
// have to fake from the anchor. Both paths therefore agree on
// `timestamp(<selector>)`; they simply reach the agreement on the fan-out.
func (n NativeStalenessLowerer) LowerStaleness(in stalenessLowerInput) chplan.Node {
	if in.sampleTimestamp {
		return n.Fallback.LowerStaleness(in)
	}
	return &chplan.RangeWindowStaleResample{
		Input:         in.input,
		Start:         in.start,
		End:           in.end,
		Step:          in.step,
		Lookback:      in.lookback,
		Offset:        in.offset,
		MetricNameCol: in.metricNameCol,
		AttributesCol: in.attributesCol,
		TimestampCol:  in.timestampCol,
		ValueCol:      in.valueCol,
	}
}

// FanoutChangesLowerer is the concrete DEFAULT ChangesLowerer: it returns the
// generic fan-out RangeWindow (the arrayPopBack/arrayPopFront `c != p` count)
// unchanged. It is the fallback the native impl embeds AND the strategy a
// fan-out-only deployment wires directly.
type FanoutChangesLowerer struct{}

// LowerChanges returns the fan-out RangeWindow rw unchanged.
func (FanoutChangesLowerer) LowerChanges(rw *chplan.RangeWindow, _ schema.Metrics) chplan.Node {
	return rw
}

// NativeChangesLowerer is the boot-wired ChangesLowerer that emits the native
// timeSeriesChangesToGrid lowering (a chplan.RangeWindowGridNative with
// Func="changes") for shape-eligible changes range-windows. cmd/cerberus wires
// it ONLY when chopt resolved the ts_grid_changes feature (server >= 25.9) at
// boot. It embeds a concrete Fallback (the fan-out impl): a shape it cannot
// handle delegates to Fallback rather than returning nil, so the interface
// method ALWAYS yields a valid lowering and the dispatch site stays branch-free.
type NativeChangesLowerer struct {
	// Fallback is the concrete lowerer for shapes the native path cannot
	// handle. Boot wires it to FanoutChangesLowerer{}.
	Fallback ChangesLowerer

	// Instant additionally opts an eligible INSTANT (gridSingleAnchor) changes
	// window onto chplan.RangeWindowGridNativeInstant (cerberus issue #2748).
	// cmd/cerberus sets it ONLY when chopt ALSO resolved ts_grid_instant at
	// boot, inside this SAME "ts_grid_changes enabled" branch — a pure
	// narrowing, never independently reachable. Since ts_grid_changes itself
	// is opt-in-only (#1721, the NaN-adjacent-window overcount), an operator
	// who has not explicitly listed ts_grid_changes never reaches this field
	// at all, so the instant arm inherits that same explicit-opt-in posture
	// automatically rather than needing its own carve-out.
	Instant bool
}

// LowerChanges returns a RangeWindowGridNative for an eligible range-mode changes
// shape, or delegates to the embedded Fallback otherwise. The eligibility
// predicate is the intrinsic SHAPE check (changes func, materialised grid,
// plain Scan/Filter input) — see nativeTSGridMatrixNode. When n.Instant is
// set, an eligible INSTANT window (nativeTSGridInstantNode) is tried next,
// before Fallback.
func (n NativeChangesLowerer) LowerChanges(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node {
	if native := nativeTSGridMatrixNode(rw, "changes", s, noRecollapse); native != nil {
		return native
	}
	if n.Instant {
		if native := nativeTSGridInstantNode(rw, "changes", s); native != nil {
			return native
		}
	}
	return n.Fallback.LowerChanges(rw, s)
}

// FanoutResetsLowerer is the concrete DEFAULT ResetsLowerer: it returns the
// generic fan-out RangeWindow (the arrayPopBack/arrayPopFront `c < p` count)
// unchanged. It is the fallback the native impl embeds AND the strategy a
// fan-out-only deployment wires directly.
type FanoutResetsLowerer struct{}

// LowerResets returns the fan-out RangeWindow rw unchanged.
func (FanoutResetsLowerer) LowerResets(rw *chplan.RangeWindow, _ schema.Metrics) chplan.Node {
	return rw
}

// NativeResetsLowerer is the boot-wired ResetsLowerer that emits the native
// timeSeriesResetsToGrid lowering (a chplan.RangeWindowGridNative with
// Func="resets") for shape-eligible resets range-windows. cmd/cerberus wires it
// ONLY when chopt resolved the ts_grid_resets feature (server >= 25.9) at boot.
// It embeds a concrete Fallback for shapes it cannot handle, so the interface
// method always yields a valid lowering and the dispatch site stays branch-free.
type NativeResetsLowerer struct {
	// Fallback is the concrete lowerer for shapes the native path cannot
	// handle. Boot wires it to FanoutResetsLowerer{}.
	Fallback ResetsLowerer

	// Instant additionally opts an eligible INSTANT (gridSingleAnchor) resets
	// window onto chplan.RangeWindowGridNativeInstant (cerberus issue #2748).
	// cmd/cerberus sets it ONLY when chopt ALSO resolved ts_grid_instant at
	// boot, inside this SAME "ts_grid_resets enabled" branch — a pure
	// narrowing, mirroring NativeChangesLowerer.Instant.
	Instant bool
}

// LowerResets returns a RangeWindowGridNative for an eligible range-mode resets
// shape, or delegates to the embedded Fallback otherwise. Same intrinsic SHAPE
// check as changes (resets func, materialised grid, plain Scan/Filter input).
// When n.Instant is set, an eligible INSTANT window is tried next, before
// Fallback.
func (n NativeResetsLowerer) LowerResets(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node {
	if native := nativeTSGridMatrixNode(rw, "resets", s, noRecollapse); native != nil {
		return native
	}
	if n.Instant {
		if native := nativeTSGridInstantNode(rw, "resets", s); native != nil {
			return native
		}
	}
	return n.Fallback.LowerResets(rw, s)
}

// FanoutDerivLowerer is the concrete DEFAULT DerivLowerer: it returns the
// generic fan-out RangeWindow (the simpleLinearRegression slope) unchanged. It
// is the fallback the native impl embeds AND the strategy a fan-out-only
// deployment wires directly.
type FanoutDerivLowerer struct{}

// LowerDeriv returns the fan-out RangeWindow rw unchanged.
func (FanoutDerivLowerer) LowerDeriv(rw *chplan.RangeWindow, _ schema.Metrics) chplan.Node {
	return rw
}

// NativeDerivLowerer is the boot-wired DerivLowerer that emits the native
// timeSeriesDerivToGrid lowering (a chplan.RangeWindowGridNative with Func="deriv")
// for shape-eligible deriv range-windows. cmd/cerberus wires it ONLY when the
// chopt resolved the ts_grid_deriv feature (server >= 25.9) at boot. It embeds
// a concrete Fallback for shapes it cannot handle, so the interface method
// always yields a valid lowering and the dispatch site stays branch-free.
type NativeDerivLowerer struct {
	// Fallback is the concrete lowerer for shapes the native path cannot
	// handle. Boot wires it to FanoutDerivLowerer{}.
	Fallback DerivLowerer

	// Instant additionally opts an eligible INSTANT (gridSingleAnchor) deriv
	// window onto chplan.RangeWindowGridNativeInstant (cerberus issue #2748).
	// cmd/cerberus sets it ONLY when chopt ALSO resolved ts_grid_instant at
	// boot, inside this SAME "ts_grid_deriv enabled" branch — a pure
	// narrowing, mirroring NativeChangesLowerer.Instant.
	Instant bool
}

// LowerDeriv returns a RangeWindowGridNative for an eligible range-mode deriv
// shape, or delegates to the embedded Fallback otherwise. Same intrinsic SHAPE
// check as changes/resets (deriv func, materialised grid, plain Scan/Filter
// input) — deriv takes no scalar, so no extra parameter gate applies. When
// n.Instant is set, an eligible INSTANT window is tried next, before
// Fallback.
func (n NativeDerivLowerer) LowerDeriv(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node {
	if native := nativeTSGridMatrixNode(rw, "deriv", s, noRecollapse); native != nil {
		return native
	}
	if n.Instant {
		if native := nativeTSGridInstantNode(rw, "deriv", s); native != nil {
			return native
		}
	}
	return n.Fallback.LowerDeriv(rw, s)
}

// FanoutPredictLinearLowerer is the concrete DEFAULT PredictLinearLowerer: it
// returns the generic fan-out RangeWindow (the simpleLinearRegression
// intercept + slope*t forecast) unchanged. It is the fallback the native impl
// embeds AND the strategy a fan-out-only deployment wires directly.
type FanoutPredictLinearLowerer struct{}

// LowerPredictLinear returns the fan-out RangeWindow rw unchanged.
func (FanoutPredictLinearLowerer) LowerPredictLinear(rw *chplan.RangeWindow, _ schema.Metrics) chplan.Node {
	return rw
}

// NativePredictLinearLowerer is the boot-wired PredictLinearLowerer that emits
// the native timeSeriesPredictLinearToGrid lowering (a chplan.RangeWindowGridNative
// with Func="predict_linear") for shape-eligible predict_linear range-windows.
// cmd/cerberus wires it ONLY when the chopt resolved the ts_grid_predict_linear
// feature (server >= 25.9) at boot. It embeds a concrete Fallback for shapes it
// cannot handle, so the interface method always yields a valid lowering and the
// dispatch site stays branch-free.
type NativePredictLinearLowerer struct {
	// Fallback is the concrete lowerer for shapes the native path cannot
	// handle. Boot wires it to FanoutPredictLinearLowerer{}.
	Fallback PredictLinearLowerer

	// Instant additionally opts an eligible INSTANT (gridSingleAnchor)
	// predict_linear window onto chplan.RangeWindowGridNativeInstant
	// (cerberus issue #2748). cmd/cerberus sets it ONLY when chopt ALSO
	// resolved ts_grid_instant at boot, inside this SAME
	// "ts_grid_predict_linear enabled" branch — a pure narrowing, mirroring
	// NativeChangesLowerer.Instant. The horizon-literal eligibility gate
	// (nativePredictLinearHorizonEligible) applies identically to the instant
	// arm: it is a statement about the SCALAR shape, not the grid shape.
	Instant bool
}

// LowerPredictLinear returns a RangeWindowGridNative for an eligible range-mode
// predict_linear shape, or delegates to the embedded Fallback otherwise. On top
// of the shared shape check (predict_linear func, materialised grid, plain
// Scan/Filter input) the horizon t must be a single whole-second literal:
// timeSeriesPredictLinearToGrid takes the offset as its 5th parametric arg (a
// constant), so a computed horizon (ScalarExprs) or a fractional t cannot ride
// the native aggregate and stays on the exact fan-out arithmetic.
//
// For an INSTANT window (gridSingleAnchor) the matrix check always misses
// (nativeTSGridMatrixNode requires Step > 0), so — when n.Instant is set and
// the horizon is still eligible — this additionally tries
// nativeTSGridInstantNode before falling to Fallback.
func (n NativePredictLinearLowerer) LowerPredictLinear(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node {
	if nativePredictLinearHorizonEligible(rw) {
		if native := nativeTSGridMatrixNode(rw, "predict_linear", s, noRecollapse); native != nil {
			return native
		}
		if n.Instant {
			if native := nativeTSGridInstantNode(rw, "predict_linear", s); native != nil {
				return native
			}
		}
	}
	return n.Fallback.LowerPredictLinear(rw, s)
}

// nativePredictLinearHorizonEligible reports whether rw's predict_linear
// horizon t can be threaded into timeSeriesPredictLinearToGrid's 5th parametric
// arg: exactly one literal scalar (no computed ScalarExprs) whose value is a
// non-negative whole number of seconds. A fractional or computed horizon is
// byte-exact only on the fan-out's `intercept + slope*t` Float64 arithmetic, so
// it delegates. A negative t (legal PromQL backward projection) also delegates:
// the aggregate's predict_offset parameter is not verified to accept a signed
// offset on the >= 25.9 substrate, and the fan-out's `intercept + slope*t`
// evaluates negative t exactly, so the native path stays inside the verified
// non-negative domain rather than risk a signed-literal rejection at query time.
func nativePredictLinearHorizonEligible(rw *chplan.RangeWindow) bool {
	if len(rw.ScalarExprs) != 0 || len(rw.Scalars) != 1 {
		return false
	}
	t := rw.Scalars[0]
	return t >= 0 && t == math.Trunc(t)
}

// FanoutDeltaLowerer is the concrete DEFAULT DeltaLowerer: it returns the
// generic fan-out RangeWindow (emitWindowedArrayExtrapolated's non-counter-
// corrected extrapolated difference) unchanged. It is the fallback the native
// impl embeds AND the strategy a fan-out-only deployment wires directly.
type FanoutDeltaLowerer struct{}

// LowerDelta returns the fan-out RangeWindow rw unchanged.
func (FanoutDeltaLowerer) LowerDelta(rw *chplan.RangeWindow, _ schema.Metrics) chplan.Node {
	return rw
}

// NativeDeltaLowerer is the boot-wired DeltaLowerer that emits the native
// timeSeriesDeltaToGrid lowering (a chplan.RangeWindowGridNative with
// Func="delta") for shape-eligible delta range-windows. cmd/cerberus wires it
// ONLY when chopt resolved the ts_grid_delta feature (server >= 25.9) at
// boot. It embeds a concrete Fallback for shapes it cannot handle, so the
// interface method always yields a valid lowering and the dispatch site
// stays branch-free.
type NativeDeltaLowerer struct {
	// Fallback is the concrete lowerer for shapes the native path cannot
	// handle. Boot wires it to FanoutDeltaLowerer{}.
	Fallback DeltaLowerer
}

// LowerDelta returns a RangeWindowGridNative for an eligible range-mode delta
// shape, or delegates to the embedded Fallback otherwise. Same intrinsic
// SHAPE check as changes/resets/deriv (delta func, materialised grid, plain
// Scan/Filter input) — delta takes no scalar, so no extra parameter gate
// applies, and (like changes/resets/deriv) it carries no -State/-Merge
// combinator pair, so nativeTSGridMatrixNode is always called with
// noRecollapse.
func (n NativeDeltaLowerer) LowerDelta(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node {
	if native := nativeTSGridMatrixNode(rw, "delta", s, noRecollapse); native != nil {
		return native
	}
	return n.Fallback.LowerDelta(rw, s)
}

// This section wires the lagInFrame annotation shape (cerberus issue #2759):
// a single sorted lagInFrame/leadInFrame pass plus fixed-size per-anchor
// accumulators, in place of the array-fold fan-out, for changes / resets /
// irate / idelta. Unlike the timeSeries*ToGrid family above it needs no
// server-version floor or experimental setting — see chopt.
// FeatureLagInFrameAdjacency — so cmd/cerberus wires it purely off the
// resolved EnabledSet with no capability probe involved. All four functions
// layer it BENEATH their own native ts_grid strategy (Native{Fallback:
// LagAdjacency{Fallback: Fanout{}}}), same embed pattern throughout this
// file: irate/idelta gained their own native timeSeriesInstantRateToGrid /
// timeSeriesInstantDeltaToGrid competitor (cerberus issue #2746), so as of
// that feature all four of changes / resets / irate / idelta share the
// identical three-tier composition (see NativeIrateLowerer /
// NativeIdeltaLowerer below).

// FanoutIrateLowerer is the concrete DEFAULT IrateLowerer: it returns the
// generic fan-out RangeWindow (window_pairs[length]/[length-1]) unchanged.
type FanoutIrateLowerer struct{}

// LowerIrate returns the fan-out RangeWindow rw unchanged.
func (FanoutIrateLowerer) LowerIrate(rw *chplan.RangeWindow, _ schema.Metrics) chplan.Node {
	return rw
}

// FanoutIdeltaLowerer is IdeltaLowerer's Fanout sibling.
type FanoutIdeltaLowerer struct{}

// LowerIdelta returns the fan-out RangeWindow rw unchanged.
func (FanoutIdeltaLowerer) LowerIdelta(rw *chplan.RangeWindow, _ schema.Metrics) chplan.Node {
	return rw
}

// LagAdjacencyChangesLowerer is the boot-wired ChangesLowerer that emits
// RangeWindow{LagAdjacency: true} for a shape-eligible changes range-window.
// cmd/cerberus wires it ONLY when chopt resolved laginframe_adjacency at
// boot, embedded beneath NativeChangesLowerer so ts_grid_changes (when also
// enabled) still takes priority.
type LagAdjacencyChangesLowerer struct {
	// Fallback is the concrete lowerer for shapes lag-adjacency cannot
	// handle. Boot wires it to FanoutChangesLowerer{}.
	Fallback ChangesLowerer
}

// LowerChanges returns rw with LagAdjacency set for an eligible window, or
// delegates to the embedded Fallback otherwise.
func (l LagAdjacencyChangesLowerer) LowerChanges(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node {
	if lagAdjacencyEligible(rw) {
		out := *rw
		out.LagAdjacency = true
		return &out
	}
	return l.Fallback.LowerChanges(rw, s)
}

// LagAdjacencyResetsLowerer mirrors [LagAdjacencyChangesLowerer] for resets.
type LagAdjacencyResetsLowerer struct {
	// Fallback is the concrete lowerer for shapes lag-adjacency cannot
	// handle. Boot wires it to FanoutResetsLowerer{}.
	Fallback ResetsLowerer
}

// LowerResets returns rw with LagAdjacency set for an eligible window, or
// delegates to the embedded Fallback otherwise.
func (l LagAdjacencyResetsLowerer) LowerResets(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node {
	if lagAdjacencyEligible(rw) {
		out := *rw
		out.LagAdjacency = true
		return &out
	}
	return l.Fallback.LowerResets(rw, s)
}

// LagAdjacencyIrateLowerer is the boot-wired (and only non-fan-out) IrateLowerer.
// cmd/cerberus wires it ONLY when chopt resolved laginframe_adjacency at boot.
type LagAdjacencyIrateLowerer struct {
	// Fallback is the concrete lowerer for shapes lag-adjacency cannot
	// handle. Boot wires it to FanoutIrateLowerer{}.
	Fallback IrateLowerer
}

// LowerIrate returns rw with LagAdjacency set for an eligible window, or
// delegates to the embedded Fallback otherwise.
func (l LagAdjacencyIrateLowerer) LowerIrate(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node {
	if lagAdjacencyEligible(rw) {
		out := *rw
		out.LagAdjacency = true
		return &out
	}
	return l.Fallback.LowerIrate(rw, s)
}

// LagAdjacencyIdeltaLowerer mirrors [LagAdjacencyIrateLowerer] for idelta.
type LagAdjacencyIdeltaLowerer struct {
	// Fallback is the concrete lowerer for shapes lag-adjacency cannot
	// handle. Boot wires it to FanoutIdeltaLowerer{}.
	Fallback IdeltaLowerer
}

// LowerIdelta returns rw with LagAdjacency set for an eligible window, or
// delegates to the embedded Fallback otherwise.
func (l LagAdjacencyIdeltaLowerer) LowerIdelta(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node {
	if lagAdjacencyEligible(rw) {
		out := *rw
		out.LagAdjacency = true
		return &out
	}
	return l.Fallback.LowerIdelta(rw, s)
}

// lagAdjacencyEligible is the intrinsic query-SHAPE eligibility predicate for
// the lagInFrame annotation path — reads NO feature flag or server version,
// exactly like nativeTSGridMatrixNode above. Every clause that fails sends
// the query down the unchanged fan-out path:
//
//   - rw.Identity must be false — the bare-vector subquery no-op path is not
//     one of the four owning functions.
//   - The window must be the materialised MATRIX grid: OuterRange > 0,
//     Step > 0, and both Start and End pinned. maybePushInnerScanTimeBounds
//     (the annotation pass's scan-prune bound, and the widening the
//     slice-invariance argument in internal/chplan/sliceinvariant.go rests
//     on) is itself gated on Start/End being set, so a subquery-internal
//     window (Start/End zero, OuterRange/Step-only) stays on the unchanged
//     fan-out rather than run lagInFrame over an unbounded partition scan.
//   - rw.DeltaPrefixAggregateInput must be nil: none of the four owning
//     functions reconstruct a DELTA counter's absolute level (that is
//     rate/increase/delta's concern), so a populated side-scan is a shape
//     this path has never been proven against and it declines rather than
//     silently ignoring the side-scan.
//   - rw.Variants must be empty: the fused multi-arm shape reduces a shared
//     per-window array across several Func arms at once (range_window_
//     fused.go); the annotation path has its own single-Func emitter and
//     does not participate in that fusion.
func lagAdjacencyEligible(rw *chplan.RangeWindow) bool {
	if rw.Identity {
		return false
	}
	if rw.OuterRange <= 0 || rw.Step <= 0 || rw.Start.IsZero() || rw.End.IsZero() {
		return false
	}
	if rw.DeltaPrefixAggregateInput != nil {
		return false
	}
	return len(rw.Variants) == 0
}

// This section wires the native timeSeriesInstantRateToGrid /
// timeSeriesInstantDeltaToGrid aggregates for irate()/idelta() (cerberus
// issue #2746), completing the family: irate/idelta previously had no
// timeSeries*ToGrid member and their only non-fan-out strategy was the
// lagInFrame annotation above. Both aggregates shipped in the same v25.6
// release as timeSeriesRateToGrid/timeSeriesDeltaToGrid and share the
// family's 25.9 floor (RequiresExperimentalTSGrid, the left-open window
// fix) — see chopt.FeatureTSGridIrate / chopt.FeatureTSGridIdelta.
//
// A chDB differential sweep against a real 26.5.1.1 substrate (cerberus
// issue #2746) settled the two open questions the issue named: irate DOES
// counter-reset-correct the trailing pair, exactly like the fan-out's
// CounterOrDeltaPairDelta CUMULATIVE branch (a strictly-decreasing pair
// 100 -> 10, 60s apart, returned the REPAIRED 10/60 = 0.1(6), never the raw
// -1.5); idelta applies NO correction (the identical pair returned the raw
// -90). See internal/chsql.nativeTSGridFn's own "irate"/"idelta" entries for
// the full sweep, including the duplicate-timestamp / window-membership
// findings and the pre-existing #2798 gap they reproduce.

// NativeIrateLowerer is the boot-wired IrateLowerer that emits the native
// timeSeriesInstantRateToGrid lowering (a chplan.RangeWindowGridNative with
// Func="irate") for shape-eligible irate range-windows. cmd/cerberus wires it
// ONLY when chopt resolved the ts_grid_irate feature (server >= 25.9) at
// boot. It embeds a concrete Fallback for shapes it cannot handle, so the
// interface method always yields a valid lowering and the dispatch site
// stays branch-free.
type NativeIrateLowerer struct {
	// Fallback is the concrete lowerer for shapes the native path cannot
	// handle. Boot wires it to LagAdjacencyIrateLowerer{Fallback:
	// FanoutIrateLowerer{}} when laginframe_adjacency is also enabled,
	// FanoutIrateLowerer{} otherwise — mirroring NativeChangesLowerer's own
	// embed of the improved fan-out.
	Fallback IrateLowerer
}

// LowerIrate returns a RangeWindowGridNative for an eligible range-mode
// irate shape, or delegates to the embedded Fallback otherwise. Same
// intrinsic SHAPE check as delta (irate func, materialised grid, plain
// Scan/Filter input). irate needs no dedicated DELTA/CUMULATIVE union split
// of its own — nativeTSGridMatrixNode's existing TemporalityColumn guard
// already sends any DELTA-temporality counter to the Fallback
// unconditionally, exactly like every other native ts_grid member, so the
// counter-reset correction the trailing-pair aggregate applies is only ever
// reached for a CUMULATIVE (or temporality-less) counter. irate takes no
// scalar and carries no -State/-Merge combinator pair, so
// nativeTSGridMatrixNode is always called with noRecollapse.
func (n NativeIrateLowerer) LowerIrate(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node {
	if native := nativeTSGridMatrixNode(rw, "irate", s, noRecollapse); native != nil {
		return native
	}
	return n.Fallback.LowerIrate(rw, s)
}

// NativeIdeltaLowerer mirrors [NativeIrateLowerer] for idelta, emitting
// timeSeriesInstantDeltaToGrid (Func="idelta") — the trailing-pair
// difference with NO counter-reset correction, matching PromQL's
// funcIdelta.
type NativeIdeltaLowerer struct {
	// Fallback is the concrete lowerer for shapes the native path cannot
	// handle. Boot wires it to LagAdjacencyIdeltaLowerer{Fallback:
	// FanoutIdeltaLowerer{}} when laginframe_adjacency is also enabled,
	// FanoutIdeltaLowerer{} otherwise.
	Fallback IdeltaLowerer
}

// LowerIdelta returns a RangeWindowGridNative for an eligible range-mode
// idelta shape, or delegates to the embedded Fallback otherwise. Same
// intrinsic SHAPE check as irate; idelta takes no scalar and carries no
// -State/-Merge combinator pair.
func (n NativeIdeltaLowerer) LowerIdelta(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node {
	if native := nativeTSGridMatrixNode(rw, "idelta", s, noRecollapse); native != nil {
		return native
	}
	return n.Fallback.LowerIdelta(rw, s)
}

// FanoutLastOverTimeLowerer is the concrete DEFAULT LastOverTimeLowerer: it
// returns the generic fan-out RangeWindow (the windowed-array
// `window_vals[length(window_vals)]` reducer, overTimeArrayValueFrag's
// lastWindowValOrNaNFrag) unchanged. It is the fallback the native impl
// embeds AND the strategy a fan-out-only deployment wires directly.
type FanoutLastOverTimeLowerer struct{}

// LowerLastOverTime returns the fan-out RangeWindow rw unchanged.
func (FanoutLastOverTimeLowerer) LowerLastOverTime(rw *chplan.RangeWindow, _ schema.Metrics) chplan.Node {
	return rw
}

// NativeLastOverTimeLowerer is the boot-wired LastOverTimeLowerer that emits
// the native timeSeriesResampleToGridWithStaleness lowering (a
// chplan.RangeWindowStaleResample, reusing the SAME node
// [NativeStalenessLowerer] builds) for shape-eligible last_over_time
// range-windows. cmd/cerberus wires it ONLY when chopt resolved the
// ts_grid_last_over_time feature at boot. It embeds a concrete Fallback (the
// fan-out impl): a shape it cannot handle delegates to Fallback rather than
// returning nil, so the interface method ALWAYS yields a valid lowering and
// the dispatch site stays branch-free.
type NativeLastOverTimeLowerer struct {
	// Fallback is the concrete lowerer for shapes the native path cannot
	// handle. Boot wires it to FanoutLastOverTimeLowerer{}.
	Fallback LastOverTimeLowerer
}

// LowerLastOverTime returns a RangeWindowStaleResample for an eligible
// range-mode last_over_time shape (see nativeLastOverTimeNode's own doc for
// the eligibility predicate — the shape check, not a feature/version read),
// or delegates to the embedded Fallback otherwise. The returned node's
// Lookback is rw.Range — last_over_time's matrix [range] literal — in place
// of the bare-selector staleness shape's fixed 5m instantLookback; every
// other field mirrors [NativeStalenessLowerer.LowerStaleness].
func (n NativeLastOverTimeLowerer) LowerLastOverTime(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node {
	if native := nativeLastOverTimeNode(rw, s); native != nil {
		return native
	}
	return n.Fallback.LowerLastOverTime(rw, s)
}

// This section wires the operator opt-in downsampled long-range tier
// (cerberus issue #2751): reading the operator-provisioned
// schema.DownsampleTierTable (a materialized view folding raw samples into
// a persisted timeSeriesLastTwoSamples aggregate state per bucket) instead
// of scanning the full-resolution raw table. Unlike every strategy above,
// this is NOT a stateless read-time function swap over the SAME table — it
// reads a SEPARATE, pre-populated table an operator must provision and
// backfill first (internal/schema/ddl, internal/downsampletier) — and it
// wires only irate() / idelta() / last_over_time(): see
// chopt.FeatureDownsampleTier's own doc for why rate() / increase() /
// delta() are structurally never eligible (a hard scope boundary, not a
// version gate).

// downsampleTierEligible is the SHAPE + RESOLUTION-AWARE eligibility
// predicate shared by every DownsampleTier*Lowerer below — the single
// source of "is this the ONE query shape the tier can answer exactly"
// (cerberus issue #2751's own scope decision, see
// schema.DownsampleTierBucket's doc for why v1 supports only ONE fixed
// bucket rather than a configurable one):
//
//   - rw.DownsampleTierInput must be populated — attachDownsampleTierArm
//     (internal/promql/lower.go) only does that for an unambiguous
//     Sum/Histogram-table scan of an eligible Func (downsampleTierEligibleFunc).
//   - The window must be a materialised query_range grid: Step > 0, Start
//     and End pinned — mirroring nativeTSGridMatrixNode's own instant
//     exclusion. OuterRange is NOT checked: applyStepGridFanout
//     (internal/promql/modifiers.go) sets it to End-Start for EVERY
//     fan-out shape, ordinary query_range included, not only a literal
//     PromQL subquery `[range:step]` — so it carries no subquery-specific
//     signal at this layer to exclude on.
//   - rw.Offset must be zero — an offset shifts the window off the tier's
//     own bucket grid; this mechanism does not attempt to re-derive an
//     offset-shifted alignment.
//   - rw.Range must be a POSITIVE INTEGER MULTIPLE of
//     schema.DownsampleTierBucket (cerberus issue #2857 — the single-bucket
//     rw.Range == bucket case #2751 shipped is the N==1 degenerate case of
//     this same check). Every wider multiple merges the N covering tier
//     rows via timeSeriesLastTwoSamplesMerge and re-filters the merged
//     result to the exact `(anchor-range, anchor]` window — see
//     internal/chsql's emitRangeWindowDownsampleTier, mirroring the
//     reasoning internal/schema/ddl's downsampleTierBucketEndExpr doc
//     comment lays out for the single-bucket case. A range narrower than
//     one bucket, or not an integer multiple of it, cannot be answered from
//     whole bucket-granularity rows at all.
//   - rw.Step must be a POSITIVE INTEGER MULTIPLE of the bucket — this is
//     the "step >= bucket" resolution-awareness the issue requires (a step
//     smaller than the bucket can never divide it evenly, so the modulo
//     check alone rejects every step-too-fine shape; a 15s-step query
//     never touches the 5-minute tier).
//   - rw.Start must land exactly on a bucket boundary (absolute Unix-epoch
//     alignment, matching internal/schema/ddl's downsampleTierBucketEndExpr
//     — ClickHouse's toStartOfInterval with no origin argument is itself
//     epoch-aligned). Combined with the Step-multiple check above, EVERY
//     anchor Start+k*Step is then also bucket-aligned, so this one check
//     covers the whole grid.
func downsampleTierEligible(rw *chplan.RangeWindow) bool {
	if rw.DownsampleTierInput == nil {
		return false
	}
	if rw.Identity || rw.Step <= 0 || rw.Start.IsZero() || rw.End.IsZero() {
		return false
	}
	if rw.Offset != 0 {
		return false
	}
	if len(rw.Variants) > 0 {
		return false
	}
	bucketNS := schema.DownsampleTierBucket.Nanoseconds()
	if rw.Range <= 0 || rw.Range.Nanoseconds()%bucketNS != 0 {
		return false
	}
	if rw.Step.Nanoseconds()%bucketNS != 0 {
		return false
	}
	return rw.Start.UnixNano()%bucketNS == 0
}

// downsampleTierNode builds the eligible node: a shallow copy of rw with
// DownsampleTier set, telling the emitter (internal/chsql's
// emitRangeWindowDownsampleTier) to render DownsampleTierInput's bucketed
// state instead of windowing Input's raw rows. Returns nil when
// downsampleTierEligible(rw) is false, so every caller below follows the
// same `if node := downsampleTierNode(rw); node != nil { return node }`
// shape the native strategies above use.
func downsampleTierNode(rw *chplan.RangeWindow) *chplan.RangeWindow {
	if !downsampleTierEligible(rw) {
		return nil
	}
	out := *rw
	out.DownsampleTier = true
	return &out
}

// DownsampleTierIrateLowerer is the boot-wired IrateLowerer that routes an
// eligible range-mode irate() shape onto the downsampled long-range tier.
// cmd/cerberus wires it ONLY when chopt resolved the downsample_tier
// feature (server >= 25.9, opt-in) at boot, WRAPPING whatever IrateLowerer
// the family's own ts_grid_irate / laginframe_adjacency wiring already
// resolved — so a query this strategy declines still gets the family's own
// best available raw-scan strategy, never a hand-rolled fallback.
type DownsampleTierIrateLowerer struct {
	// Fallback is the concrete lowerer for shapes the tier cannot handle —
	// the family's own already-resolved IrateLowerer (Native, LagAdjacency,
	// or Fanout), threaded so the raw-read path stays reachable forever.
	Fallback IrateLowerer
}

// LowerIrate returns the tier-routed node for an eligible shape, or
// delegates to the embedded Fallback otherwise.
func (l DownsampleTierIrateLowerer) LowerIrate(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node {
	if node := downsampleTierNode(rw); node != nil {
		return node
	}
	return l.Fallback.LowerIrate(rw, s)
}

// DownsampleTierIdeltaLowerer mirrors [DownsampleTierIrateLowerer] for
// idelta().
type DownsampleTierIdeltaLowerer struct {
	// Fallback is the concrete lowerer for shapes the tier cannot handle —
	// the family's own already-resolved IdeltaLowerer.
	Fallback IdeltaLowerer
}

// LowerIdelta returns the tier-routed node for an eligible shape, or
// delegates to the embedded Fallback otherwise.
func (l DownsampleTierIdeltaLowerer) LowerIdelta(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node {
	if node := downsampleTierNode(rw); node != nil {
		return node
	}
	return l.Fallback.LowerIdelta(rw, s)
}

// DownsampleTierLastOverTimeLowerer mirrors [DownsampleTierIrateLowerer] for
// last_over_time().
type DownsampleTierLastOverTimeLowerer struct {
	// Fallback is the concrete lowerer for shapes the tier cannot handle —
	// the family's own already-resolved LastOverTimeLowerer.
	Fallback LastOverTimeLowerer
}

// LowerLastOverTime returns the tier-routed node for an eligible shape, or
// delegates to the embedded Fallback otherwise.
func (l DownsampleTierLastOverTimeLowerer) LowerLastOverTime(rw *chplan.RangeWindow, s schema.Metrics) chplan.Node {
	if node := downsampleTierNode(rw); node != nil {
		return node
	}
	return l.Fallback.LowerLastOverTime(rw, s)
}
