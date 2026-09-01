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

	// ts_grid_vector_agg lives on RangeLowerers itself (never nested inside
	// a `set.Has(chopt.FeatureTSGridRange)` branch) — mirrors
	// cmd/cerberus/main.go's nativeRangeLowerers exactly; see
	// promql.RangeLowerers.VectorAgg's own doc for why.
	l.VectorAgg = set.Has(chopt.FeatureTSGridVectorAgg)

	// ts_grid_instant is NOT part of AllNativeOptimizations (new 26.5 floor,
	// AutoSelect=false — see chopt.FeatureTSGridInstant's own doc), the same
	// posture quantile_prom_histogram / ts_grid_last_over_time already take
	// below: callers who want it activated resolve it explicitly against a
	// >= 26.5 server and pass the resulting set here. It is a pure narrowing
	// of each of rate/changes/resets/deriv/predict_linear's own matrix
	// feature (never independently reachable), mirroring
	// cmd/cerberus/main.go's nativeRangeLowerers exactly.
	tsGridInstant := set.Has(chopt.FeatureTSGridInstant)

	if set.Has(chopt.FeatureTSGridRange) {
		l.Rate = promql.NativeRateLowerer{
			Fallback:   promql.FanoutRateLowerer{},
			Recollapse: set.Has(chopt.FeatureTSGridRecollapse),
			Instant:    tsGridInstant,
		}
	} else {
		l.Rate = promql.FanoutRateLowerer{}
	}
	if set.Has(chopt.FeatureTSGridIncrease) {
		l.Increase = promql.NativeIncreaseLowerer{Fallback: promql.FanoutIncreaseLowerer{}}
	} else {
		l.Increase = promql.FanoutIncreaseLowerer{}
	}
	if set.Has(chopt.FeatureTSGridResample) {
		l.Staleness = promql.NativeStalenessLowerer{Fallback: promql.FanoutStalenessLowerer{}}
	} else {
		l.Staleness = promql.FanoutStalenessLowerer{}
	}
	if set.Has(chopt.FeatureTSGridChanges) {
		l.Changes = promql.NativeChangesLowerer{Fallback: promql.FanoutChangesLowerer{}, Instant: tsGridInstant}
	} else {
		l.Changes = promql.FanoutChangesLowerer{}
	}
	if set.Has(chopt.FeatureTSGridResets) {
		l.Resets = promql.NativeResetsLowerer{Fallback: promql.FanoutResetsLowerer{}, Instant: tsGridInstant}
	} else {
		l.Resets = promql.FanoutResetsLowerer{}
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
		l.Delta = promql.NativeDeltaLowerer{Fallback: promql.FanoutDeltaLowerer{}}
	} else {
		l.Delta = promql.FanoutDeltaLowerer{}
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
