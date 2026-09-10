package engine

import (
	"reflect"
	"slices"
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
)

func TestProbeQuerySettings(t *testing.T) {
	const (
		queryLimit = int64(20)
		memoryCap  = int64(1 << 30)
	)
	plan := tempoSearchRecentPlan(queryLimit)
	rules := SettingsRules{LazyMaterialization: true}
	probe := ProbeQuerySettings(plan, rules, memoryCap)
	if got := probe.Settings[settingQueryPlanOptimizeLazyMaterialization]; got != 1 {
		t.Errorf("lazy materialization = %v, want 1", got)
	}
	if got := probe.Settings[settingQueryPlanMaxLimitForLazyMaterialization]; got != queryLimit {
		t.Errorf("lazy materialization limit = %v, want %d", got, queryLimit)
	}
	if !slices.Contains(probe.EnabledOpts, "lazy_materialization") {
		t.Errorf("enabled options = %v, missing lazy_materialization", probe.EnabledOpts)
	}
	if got := probe.Settings[settingMaxBytesBeforeExternalGroupBy]; got != memoryCap/spillCapDenominator {
		t.Errorf("group-by spill threshold = %v, want %d", got, memoryCap/spillCapDenominator)
	}
	if got := ProbeQuerySettings(plan, SettingsRules{}, memoryCap).Settings[settingQueryPlanOptimizeLazyMaterialization]; got != nil {
		t.Errorf("disabled lazy materialization = %v, want absent", got)
	}
	if got := ProbeQuerySettings(&chplan.Scan{Table: "otel_traces"}, rules, memoryCap).Settings[settingQueryPlanOptimizeLazyMaterialization]; got != nil {
		t.Errorf("ineligible lazy materialization = %v, want absent", got)
	}
	t.Run("native grid setting", func(t *testing.T) {
		native := &chplan.RangeWindowGridNative{Input: &chplan.Scan{Table: "otel_metrics_sum"}}
		if got := ProbeQuerySettings(native, SettingsRules{}, memoryCap).Settings[chclient.SettingExperimentalTSGridAggregate]; got != 1 {
			t.Errorf("native grid setting = %v, want 1", got)
		}
	})
	t.Run("independent result maps", func(t *testing.T) {
		want := ProbeQuerySettings(plan, rules, memoryCap)
		probe.Settings[settingQueryPlanOptimizeLazyMaterialization] = 0
		if got := ProbeQuerySettings(plan, rules, memoryCap); !reflect.DeepEqual(got, want) {
			t.Errorf("probe mutation leaked: got %+v, want %+v", got, want)
		}
	})
}
