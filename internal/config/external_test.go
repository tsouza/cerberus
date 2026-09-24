package config

import (
	"sort"
	"testing"

	"github.com/tsouza/cerberus/internal/actuals"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/solver"
)

// ownedExternalSettings is every out-of-loader CERBERUS_* setting as its OWNER
// names it. The production list cannot reference these constants (arch-lint
// keeps internal/config from importing the owners), so this test is what
// makes the restated strings in externalSettings a mirror rather than a copy.
func ownedExternalSettings() []string {
	return []string{
		promql.EnvHistogramMergeMaxCostUnits,
		promql.EnvClassicBucketMergeMaxCostUnits,
		promql.EnvExpHistogramWindowMaxCostUnits,

		engine.EnvRangeBucketFanoutMaxRows,
		engine.EnvRangeLWRFanoutMaxRows,
		engine.EnvRateWindowFanoutMaxRows,
		engine.EnvMaxEmittedSQLBytes,
		engine.EnvRangeBucketFanoutFoldCostMaxUnits,

		solver.EnvRoute,
		solver.EnvMinFanout,
		solver.EnvMinAnchorPairs,
		solver.EnvMaxK,
		solver.EnvMinAnchorsPerSlice,
		solver.EnvParallel,
		solver.EnvTimeout,
		solver.EnvMaxOutputRows,
		solver.EnvAdaptiveEnabled,
		solver.EnvLegacyRouteMemoEnabled,
		solver.EnvRouteMemoEntryTTL,
		solver.EnvRouteMemoRevalFrac,
		solver.EnvEstimateNearEmptyRowFloor,
		solver.EnvMaxKWithEstimate,
		solver.EnvEstimateMinRowsPerAdditionalShard,

		actuals.EnvEnabled,
		actuals.EnvDriftLowerRatio,
		actuals.EnvDriftUpperRatio,
		actuals.EnvMinObservations,
		actuals.EnvEMAAlpha,
		actuals.EnvEntryTTL,
		actuals.EnvQueryLogPollInterval,
		actuals.EnvQueryLogLookback,
		actuals.EnvQueryLogSettleDelay,
	}
}

// TestExternalSettings_MatchTheirOwners pins externalSettings against the
// owners' exported constants in both directions. A knob added to an owner
// without an entry here is a knob a cerberus.yaml would reject as unknown; an
// entry here whose owner no longer parses it is a key the file accepts and
// nothing applies — the exact silence the membership check exists to end.
func TestExternalSettings_MatchTheirOwners(t *testing.T) {
	want := ownedExternalSettings()
	sort.Strings(want)
	got := append([]string(nil), externalSettings...)
	sort.Strings(got)

	if len(got) != len(want) {
		t.Fatalf("externalSettings has %d entries, the owners export %d:\n  registry: %v\n  owners:   %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("externalSettings[%d] = %s, owners' sorted constant is %s", i, got[i], want[i])
		}
	}
}

// TestExternalSettings_EveryEntryIsAKnownFlatKey pins the wiring the
// membership check rests on: each external setting must be accepted by
// translateDoc as a flat key, or the owner's file support is unreachable.
func TestExternalSettings_EveryEntryIsAKnownFlatKey(t *testing.T) {
	for _, key := range externalSettings {
		doc := map[string]any{key: "1"}
		out, err := translateDoc("cerberus.yaml", doc)
		if err != nil {
			t.Errorf("%s: translateDoc rejected the flat key: %v", key, err)
			continue
		}
		if out[key] != "1" {
			t.Errorf("%s: translateDoc rendered %q, want the value as written", key, out[key])
		}
	}
}
