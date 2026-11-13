package choptwire_test

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/choptwire"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestSettingsRules_HistogramAdaptiveAggregatorOffFollowsTheResolvedSet pins
// that SettingsRules carries the histogram_adaptive_aggregator_off verdict:
// auto turns it on from 26.8, the first release with
// enable_adaptive_aggregator, and leaves it off below, where the server
// would reject the setting; a listing without the feature leaves it off.
func TestSettingsRules_HistogramAdaptiveAggregatorOffFollowsTheResolvedSet(t *testing.T) {
	for _, tc := range []struct {
		selection string
		server    chopt.Version
		want      bool
	}{
		{chopt.SelectionAuto, chopt.Version{Major: 26, Minor: 6}, false},
		{chopt.SelectionAuto, chopt.Version{Major: 26, Minor: 7}, false},
		{chopt.SelectionAuto, chopt.Version{Major: 26, Minor: 8}, true},
		{chopt.SelectionAuto, chopt.Version{Major: 26, Minor: 9}, true},
		{chopt.FeatureExpHistogramTwoLevel, chopt.Version{Major: 26, Minor: 8}, false},
		{chopt.FeatureHistogramAdaptiveAggregatorOff, chopt.Version{Major: 26, Minor: 8}, true},
	} {
		set, _, err := chopt.Resolve(chopt.Config{Optimizations: tc.selection}, tc.server)
		if err != nil {
			t.Fatalf("chopt.Resolve(%q) on %v: %v", tc.selection, tc.server, err)
		}
		rules := choptwire.SettingsRules(set, schema.DefaultOTelMetrics(), schema.DefaultOTelTraces(), schema.DefaultOTelLogs())
		if rules.HistogramAdaptiveAggregatorOff != tc.want {
			t.Errorf("selection %q on %v: HistogramAdaptiveAggregatorOff = %v; want %v",
				tc.selection, tc.server, rules.HistogramAdaptiveAggregatorOff, tc.want)
		}
	}
}

// TestSettingsRules_NativeHistogramAnalyzerStampFollowsTheResolvedSet pins
// the release lines on which a native-histogram plan carries enable_analyzer=0:
// below 26.8 the stamp still pays off and is applied; from 26.8 the analyzer
// is faster on every measured shape, so it is withheld; from 26.9 the server
// refuses it. Every selection resolves it alike, because the stamp follows the
// build, not the operator's choice. The plan runs through the same settings
// composition the dispatch uses (engine.ProbeQuerySettings).
func TestSettingsRules_NativeHistogramAnalyzerStampFollowsTheResolvedSet(t *testing.T) {
	plan := &chplan.HistogramQuantileNative{Input: &chplan.Scan{Table: "otel_metrics_exponential_histogram"}}
	for _, tc := range []struct {
		server  chopt.Version
		stamped bool
	}{
		{chopt.Version{Major: 24, Minor: 8}, true},
		{chopt.Version{Major: 26, Minor: 6, Patch: 8, Build: 7}, true},
		{chopt.Version{Major: 26, Minor: 7, Patch: 13, Build: 12}, true},
		{chopt.Version{Major: 26, Minor: 8}, false},
		{chopt.Version{Major: 26, Minor: 8, Patch: 36, Build: 3}, false},
		{chopt.Version{Major: 26, Minor: 9, Patch: 25, Build: 15}, false},
	} {
		for _, selection := range []string{chopt.SelectionAuto, "off"} {
			set, _, err := chopt.Resolve(chopt.Config{Optimizations: selection}, tc.server)
			if err != nil {
				t.Fatalf("chopt.Resolve(%q) on %v: %v", selection, tc.server, err)
			}
			rules := choptwire.SettingsRules(set, schema.DefaultOTelMetrics(), schema.DefaultOTelTraces(), schema.DefaultOTelLogs())
			settings := engine.ProbeQuerySettings(plan, rules, nativeHistogramProbeMemCap, chsql.SchemaStrategies{}).Settings
			got, ok := settings[enableAnalyzerSetting]
			if tc.stamped && (!ok || got != 0) {
				t.Errorf("selection %q on %v: enable_analyzer = %v (present %v); want 0", selection, tc.server, got, ok)
			}
			if !tc.stamped && ok && got == 0 {
				t.Errorf("selection %q on %v: enable_analyzer = 0 stamped; want the analyzer left on", selection, tc.server)
			}
		}
	}
}

// enableAnalyzerSetting is the ClickHouse setting the native-histogram
// analyzer fix stamps, written out because the engine's constant is unexported.
const enableAnalyzerSetting = "enable_analyzer"

// nativeHistogramProbeMemCap is the statement memory cap the probe sizes its
// spill settings from; the analyzer stamp does not read it.
const nativeHistogramProbeMemCap = int64(1 << 30)
