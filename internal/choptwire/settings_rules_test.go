package choptwire_test

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/choptwire"
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
