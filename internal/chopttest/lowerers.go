//go:build integration

package chopttest

import (
	"context"
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/promql"
)

// AllNativeOptimizations is the CERBERUS_CH_OPTIMIZATIONS-shaped selection
// string this package resolves against. "auto" alone is not enough:
// chopt.FeatureTSGridChanges is the one ts_grid_* family member with
// AutoSelect: false (its registry entry explains why — the builtin changes()
// diverges from reference Prometheus on NaN-adjacent windows, #1721), so an
// operator who wants every native family activated has to opt in
// explicitly via CERBERUS_CH_OPTIMIZATIONS=auto,ts_grid_changes. This
// package's whole point is exercising every family, so it resolves against
// the identical explicit union rather than "auto" alone.
// chopt.FeatureTSGridVectorAgg joins it for the same reason: it shares
// ts_grid_range's own 25.9 floor (not a higher one — unlike ts_grid_instant
// / quantile_prom_histogram / ts_grid_last_over_time below, which stay OUT
// of this union for exactly that reason), but is AutoSelect: false as a
// brand-new, not-yet-fielded code path (cerberus issue #2763).
const AllNativeOptimizations = "auto," + chopt.FeatureTSGridChanges + "," + chopt.FeatureTSGridVectorAgg

// ResolveEnabledSet probes client's connected server version and ts_grid
// experimental capability, then resolves optimizations against them in
// chopt.Enforcing mode — the same probe -> chopt.Resolve sequence
// cmd/cerberus's own boot-time resolveCHOptimizations runs, reused here so an
// integration test's activation decision is made the identical way a real
// deployment's is. Enforcing mode means a version or capability shortfall
// fails the test loudly (t.Fatalf) instead of silently degrading to
// fan-out and leaving a caller's activation assertions vacuously
// meaningless — the "hollow green" failure this package exists to prevent
// (see the package doc and issue #2487).
func ResolveEnabledSet(ctx context.Context, t testing.TB, client *chclient.Client, optimizations string) chopt.EnabledSet {
	t.Helper()
	version, err := client.ProbeVersion(ctx)
	if err != nil {
		t.Fatalf("chopttest: probe clickhouse version: %v", err)
	}
	capability := client.ProbeTSGridCapability(ctx)
	set, warnings, err := chopt.Resolve(chopt.Config{
		Optimizations: optimizations,
		Mode:          chopt.Enforcing,
		Capability:    capability,
	}, version)
	if err != nil {
		t.Fatalf("chopttest: resolve clickhouse optimizations %q: %v", optimizations, err)
	}
	for _, w := range warnings {
		t.Logf("chopttest: ch_opt: %s", w)
	}
	t.Logf("chopttest: probed clickhouse %s, ts_grid capability %s, enabled=%v",
		version.String(), capability.String(), set.IDs())
	return set
}

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

// WireAllNativeLowerers is the one-call convenience an integration test
// wants: resolve every native ts_grid_* family against client's real
// connected server (AllNativeOptimizations) and build the full lowering
// table from the result. Returns the EnabledSet too so a caller can assert
// which families the server's own probed version actually enabled before
// trusting a per-family activation assertion against it.
func WireAllNativeLowerers(ctx context.Context, t testing.TB, client *chclient.Client) (promql.RangeLowerers, chopt.EnabledSet) {
	t.Helper()
	set := ResolveEnabledSet(ctx, t, client, AllNativeOptimizations)
	return BuildRangeLowerers(set), set
}
