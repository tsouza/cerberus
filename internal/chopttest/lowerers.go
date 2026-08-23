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
const AllNativeOptimizations = "auto," + chopt.FeatureTSGridChanges

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

	if set.Has(chopt.FeatureTSGridRange) {
		l.Rate = promql.NativeRateLowerer{
			Fallback:   promql.FanoutRateLowerer{},
			Recollapse: set.Has(chopt.FeatureTSGridRecollapse),
		}
	} else {
		l.Rate = promql.FanoutRateLowerer{}
	}
	if set.Has(chopt.FeatureTSGridResample) {
		l.Staleness = promql.NativeStalenessLowerer{Fallback: promql.FanoutStalenessLowerer{}}
	} else {
		l.Staleness = promql.FanoutStalenessLowerer{}
	}
	if set.Has(chopt.FeatureTSGridChanges) {
		l.Changes = promql.NativeChangesLowerer{Fallback: promql.FanoutChangesLowerer{}}
	} else {
		l.Changes = promql.FanoutChangesLowerer{}
	}
	if set.Has(chopt.FeatureTSGridResets) {
		l.Resets = promql.NativeResetsLowerer{Fallback: promql.FanoutResetsLowerer{}}
	} else {
		l.Resets = promql.FanoutResetsLowerer{}
	}
	if set.Has(chopt.FeatureTSGridDeriv) {
		l.Deriv = promql.NativeDerivLowerer{Fallback: promql.FanoutDerivLowerer{}}
	} else {
		l.Deriv = promql.FanoutDerivLowerer{}
	}
	if set.Has(chopt.FeatureTSGridPredictLinear) {
		l.PredictLinear = promql.NativePredictLinearLowerer{Fallback: promql.FanoutPredictLinearLowerer{}}
	} else {
		l.PredictLinear = promql.FanoutPredictLinearLowerer{}
	}

	// The anchor-injection window-slide mechanism has no chopt feature gate
	// (chopt.AlwaysAvailable), so it is wired unconditionally as the link
	// between the rate-only native ladder and the fan-out fallback — see
	// main.go's nativeRangeLowerers doc for the full formula.
	windowSlideOrFanout := promql.ClassicHistogramWindowLowerer(promql.WindowSlideClassicHistogramWindowLowerer{
		Fallback: promql.FanoutClassicHistogramWindowLowerer{},
	})
	if set.Has(chopt.FeatureTSGridHistogram) {
		l.ClassicHistogram = promql.NativeClassicHistogramWindowLowerer{Fallback: windowSlideOrFanout}
	} else {
		l.ClassicHistogram = windowSlideOrFanout
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
