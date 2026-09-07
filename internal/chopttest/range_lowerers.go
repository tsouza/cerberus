package chopttest

import (
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/promql"
)

// This file carries no build tag on purpose: BuildRangeLowerers depends only
// on chopt and promql, and the untagged unit lane's solver-decision ratchet
// (test/perf/solver_decision_ratchet_test.go) consumes it as THE one
// test-side copy of cmd/cerberus's nativeRangeLowerers composition, instead
// of carrying a third hand-rolled mirror of it. The rest of this package
// stays behind the integration tag because it needs a live ClickHouse.

// BuildRangeLowerers builds the FULL promql.RangeLowerers dispatch table from
// set, field-for-field identical to cmd/cerberus/main.go's own
// nativeRangeLowerers — see that function's doc comment for the formula
// (a feature-gated native impl always embeds the NEXT link as its own
// Fallback, never the bare fan-out impl directly). It duplicates that
// function's body rather than importing it: nativeRangeLowerers is
// unexported in `package main` (cmd/cerberus) and so cannot be imported by
// any test package. Recollapse tracks chopt.FeatureTSGridRecollapse exactly
// as main.go's does.
func BuildRangeLowerers(set chopt.EnabledSet) promql.RangeLowerers {
	var l promql.RangeLowerers

	// arg_and_max_fusion is a plain emission-detail bit, not a swappable
	// strategy (RangeLowerers.ArgAndMaxFusion's own doc) — mirrors
	// cmd/cerberus/main.go's nativeRangeLowerers exactly. It feeds both the
	// VectorJoin site (read directly off this table) and the RangeLWR site
	// via FanoutStalenessLowerer below.
	argAndMaxFusion := set.Has(chopt.FeatureArgAndMaxFusion)
	l.ArgAndMaxFusion = argAndMaxFusion

	// ts_grid_vector_agg lives on RangeLowerers itself (never nested inside
	// a `set.Has(chopt.FeatureTSGridRange)` branch) — mirrors
	// cmd/cerberus/main.go's nativeRangeLowerers exactly; see
	// promql.RangeLowerers.VectorAgg's own doc for why.
	l.VectorAgg = set.Has(chopt.FeatureTSGridVectorAgg)

	// ts_grid_group_array is, like VectorAgg immediately above, a plain
	// narrowing bit rather than a per-function Lowerer swap — mirrors
	// cmd/cerberus/main.go's nativeRangeLowerers exactly (cerberus issue
	// #2749).
	l.NativeGroupArray = set.Has(chopt.FeatureTSGridGroupArray)

	// ts_grid_instant is NOT part of AllNativeOptimizations (new 26.5 floor,
	// AutoSelect=false — see chopt.FeatureTSGridInstant's own doc), the same
	// posture quantile_prom_histogram / ts_grid_last_over_time already take
	// below: callers who want it activated resolve it explicitly against a
	// >= 26.5 server and pass the resulting set here. It is a pure narrowing
	// of each of rate/changes/resets/deriv/predict_linear's own matrix
	// feature (never independently reachable), mirroring
	// cmd/cerberus/main.go's nativeRangeLowerers exactly.
	tsGridInstant := set.Has(chopt.FeatureTSGridInstant)

	// fixed_accumulator_extrapolated layers BENEATH rate/increase/delta's own
	// native ts_grid strategy, exactly like laginframe_adjacency layers
	// inside irate/idelta below — mirrors cmd/cerberus/main.go's
	// nativeRangeLowerers exactly (cerberus issue #2760).
	var rateFallback promql.RateLowerer = promql.FanoutRateLowerer{}
	var increaseFallback promql.IncreaseLowerer = promql.FanoutIncreaseLowerer{}
	var deltaFallback promql.DeltaLowerer = promql.FanoutDeltaLowerer{}
	if set.Has(chopt.FeatureFixedAccumulatorExtrapolated) {
		rateFallback = promql.FixedAccumulatorRateLowerer{Fallback: rateFallback}
		increaseFallback = promql.FixedAccumulatorIncreaseLowerer{Fallback: increaseFallback}
		deltaFallback = promql.FixedAccumulatorDeltaLowerer{Fallback: deltaFallback}
	}
	if set.Has(chopt.FeatureTSGridRange) {
		l.Rate = promql.NativeRateLowerer{
			Fallback:   rateFallback,
			Recollapse: set.Has(chopt.FeatureTSGridRecollapse),
			Instant:    tsGridInstant,
		}
	} else {
		l.Rate = rateFallback
	}
	if set.Has(chopt.FeatureTSGridIncrease) {
		l.Increase = promql.NativeIncreaseLowerer{Fallback: increaseFallback}
	} else {
		l.Increase = increaseFallback
	}
	if set.Has(chopt.FeatureTSGridResample) {
		l.Staleness = promql.NativeStalenessLowerer{Fallback: promql.FanoutStalenessLowerer{ArgAndMaxFusion: argAndMaxFusion}}
	} else {
		l.Staleness = promql.FanoutStalenessLowerer{ArgAndMaxFusion: argAndMaxFusion}
	}

	// changes/resets: laginframe_adjacency layers BENEATH their own native
	// ts_grid strategy, exactly like it does for irate/idelta below —
	// mirrors cmd/cerberus/main.go's nativeRangeLowerers exactly (cerberus
	// issue #2759).
	var changesFallback promql.ChangesLowerer = promql.FanoutChangesLowerer{}
	var resetsFallback promql.ResetsLowerer = promql.FanoutResetsLowerer{}
	if set.Has(chopt.FeatureLagInFrameAdjacency) {
		changesFallback = promql.LagAdjacencyChangesLowerer{Fallback: changesFallback}
		resetsFallback = promql.LagAdjacencyResetsLowerer{Fallback: resetsFallback}
	}
	if set.Has(chopt.FeatureTSGridChanges) {
		l.Changes = promql.NativeChangesLowerer{Fallback: changesFallback, Instant: tsGridInstant}
	} else {
		l.Changes = changesFallback
	}
	if set.Has(chopt.FeatureTSGridResets) {
		l.Resets = promql.NativeResetsLowerer{Fallback: resetsFallback, Instant: tsGridInstant}
	} else {
		l.Resets = resetsFallback
	}
	if set.Has(chopt.FeatureTSGridDeriv) {
		l.Deriv = promql.NativeDerivLowerer{Fallback: promql.FanoutDerivLowerer{}, Instant: tsGridInstant}
	} else {
		l.Deriv = promql.FanoutDerivLowerer{}
	}
	if set.Has(chopt.FeatureTSGridPredictLinear) {
		l.PredictLinear = promql.NativePredictLinearLowerer{Fallback: promql.FanoutPredictLinearLowerer{}, Instant: tsGridInstant}
	} else {
		l.PredictLinear = promql.FanoutPredictLinearLowerer{}
	}
	if set.Has(chopt.FeatureTSGridDelta) {
		l.Delta = promql.NativeDeltaLowerer{Fallback: deltaFallback}
	} else {
		l.Delta = deltaFallback
	}

	// irate/idelta: laginframe_adjacency layers BENEATH their own native
	// ts_grid strategy, mirroring cmd/cerberus's own nativeRangeLowerers
	// (cerberus issue #2746).
	var irateFallback promql.IrateLowerer = promql.FanoutIrateLowerer{}
	var ideltaFallback promql.IdeltaLowerer = promql.FanoutIdeltaLowerer{}
	if set.Has(chopt.FeatureLagInFrameAdjacency) {
		irateFallback = promql.LagAdjacencyIrateLowerer{Fallback: irateFallback}
		ideltaFallback = promql.LagAdjacencyIdeltaLowerer{Fallback: ideltaFallback}
	}
	if set.Has(chopt.FeatureTSGridIrate) {
		l.Irate = promql.NativeIrateLowerer{Fallback: irateFallback}
	} else {
		l.Irate = irateFallback
	}
	if set.Has(chopt.FeatureTSGridIdelta) {
		l.Idelta = promql.NativeIdeltaLowerer{Fallback: ideltaFallback}
	} else {
		l.Idelta = ideltaFallback
	}

	// The anchor-injection window-slide mechanism was removed by #2511's
	// root-cause investigation (structural over-read, see main.go's
	// nativeRangeLowerers doc) — fan-out is the sole fallback below the
	// rate-only native ladder.
	if set.Has(chopt.FeatureTSGridHistogram) {
		l.ClassicHistogram = promql.NativeClassicHistogramWindowLowerer{Fallback: promql.FanoutClassicHistogramWindowLowerer{}}
	} else {
		l.ClassicHistogram = promql.FanoutClassicHistogramWindowLowerer{}
	}

	// quantile_prom_histogram is NOT part of AllNativeOptimizations (floor
	// 25.10, above the 25.9-alpine substrate most callers of this function
	// probe against under Enforcing mode — listing it there would fail every
	// such caller loudly rather than silently degrading). Callers that want
	// it activated resolve it explicitly against a >= 25.10 server and pass
	// the resulting set here; this branch only ever fires for those callers.
	if set.Has(chopt.FeatureQuantilePromHistogram) {
		l.QuantileRankWalk = promql.NativeQuantileRankWalkLowerer{}
	} else {
		l.QuantileRankWalk = promql.FanoutQuantileRankWalkLowerer{}
	}

	// ts_grid_last_over_time is NOT part of AllNativeOptimizations (floor
	// 26.6, above the 25.9-alpine substrate most callers of this function
	// probe against under Enforcing mode — listing it there would fail every
	// such caller loudly rather than silently degrading). Callers that want
	// it activated resolve it explicitly against a >= 26.6 server and pass
	// the resulting set here; this branch only ever fires for those callers.
	if set.Has(chopt.FeatureTSGridLastOverTime) {
		l.LastOverTime = promql.NativeLastOverTimeLowerer{Fallback: promql.FanoutLastOverTimeLowerer{}}
	} else {
		l.LastOverTime = promql.FanoutLastOverTimeLowerer{}
	}

	// downsample_tier WRAPS whichever irate/idelta/last_over_time strategy
	// was just resolved above — mirrors cmd/cerberus/main.go's
	// nativeRangeLowerers exactly (cerberus issue #2751). rate()/increase()/
	// delta() have no such wrapping (chopt.FeatureDownsampleTier's own doc).
	if set.Has(chopt.FeatureDownsampleTier) {
		l.Irate = promql.DownsampleTierIrateLowerer{Fallback: l.Irate}
		l.Idelta = promql.DownsampleTierIdeltaLowerer{Fallback: l.Idelta}
		l.LastOverTime = promql.DownsampleTierLastOverTimeLowerer{Fallback: l.LastOverTime}
	}

	// sorted_slab_over_time (issue #2761, widened by issue #2804) has no
	// native timeSeries*ToGrid competitor of its own — it is wired directly
	// with its plain fan-out as its own embedded Fallback, mirroring
	// cmd/cerberus/main.go's nativeRangeLowerers exactly.
	if set.Has(chopt.FeatureSortedSlabOverTime) {
		l.OverTime = promql.SortedSlabOverTimeLowerer{Fallback: promql.FanoutOverTimeLowerer{}}
	} else {
		l.OverTime = promql.FanoutOverTimeLowerer{}
	}

	// classic_bucket_merge_summap has no version floor to probe — mirrors
	// cmd/cerberus/main.go's nativeRangeLowerers exactly (cerberus issue
	// #2756; AutoSelect stays false per #2923's measured negative result,
	// see classic_bucket_merge_summap.go's header). Concrete fan-out impl
	// when the feature is off; this field is never nil on the lowering
	// path (promql.RangeLowerers.ClassicBucketMerge's own doc), so leaving
	// it unset here would panic any test exercising a classic-bucket-merge
	// shape rather than merely leaving the feature inert.
	if set.Has(chopt.FeatureClassicBucketMergeSumMap) {
		l.ClassicBucketMerge = promql.NativeClassicBucketMergeLowerer{Fallback: promql.FanoutClassicBucketMergeLowerer{}}
	} else {
		l.ClassicBucketMerge = promql.FanoutClassicBucketMergeLowerer{}
	}

	// exp_histogram_merge_summap has no version floor to probe either —
	// mirrors cmd/cerberus/main.go's nativeRangeLowerers exactly (cerberus
	// issue #2757). Same never-nil contract as ClassicBucketMerge above
	// (promql.RangeLowerers.ExpHistogramMerge's own doc).
	if set.Has(chopt.FeatureExpHistogramMergeSumMap) {
		l.ExpHistogramMerge = promql.NativeExpHistogramMergeLowerer{}
	} else {
		l.ExpHistogramMerge = promql.FanoutExpHistogramMergeLowerer{}
	}

	return l
}
