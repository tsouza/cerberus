package chopt

import (
	"fmt"
	"slices"
)

// Settings that version-gated features stamp and that only select an
// execution strategy or a memory bound. The engine stamps the
// result-equivalent ones through these constants. It declares the three
// memory bounds (the adaptive-aggregator switch and the two spill thresholds)
// as its own literals, which the perf-sentinel gate classifies;
// internal/engine's TestStampedSettings_AreEachClassifiedOnce reads every
// setting the engine stamps and fails unless each one, these literals among
// them, is in exactly one class: this table, GateSettings, or the engine's
// function-required or floor-known sets.
const (
	SettingEnableAdaptiveAggregator                = "enable_adaptive_aggregator"
	SettingMaxBytesBeforeExternalJoin              = "max_bytes_before_external_join"
	SettingMaxBytesBeforeExternalDistinct          = "max_bytes_before_external_distinct"
	SettingMinTableRowsToUseProjectionIndex        = "min_table_rows_to_use_projection_index"
	SettingQueryPlanOptimizeLazyMaterialization    = "query_plan_optimize_lazy_materialization"
	SettingQueryPlanMaxLimitForLazyMaterialization = "query_plan_max_limit_for_lazy_materialization"
)

// DroppableSetting is a setting a version-gated feature stamps that a query
// returns the same rows without: it selects an execution strategy or a memory
// bound, never the result. A server older than Since does not know it and
// rejects the whole query with UNKNOWN_SETTING (code 115). Such a server knows
// no droppable setting whose Since is at or above the rejected one's, so a
// query it rejected may be sent again without exactly those: the droppable
// settings it does know still ride.
//
// The feature never resolves in against such a server (Since is at or below
// the feature's MinVersion), so a node rejects the setting only when the
// fleet holds a node older than the build the set in force was resolved
// against: a rolling rollback, or an older node joining.
//
// A setting that is also a GateSettings entry is droppable only at OnlyAt,
// the value that merely enables what the feature turns on; at any other value
// it is the gate's stamp against a wrong-result defect and is never dropped.
// Neither is a function, which is part of the statement.
type DroppableSetting struct {
	Name    string
	Feature string
	// Since is the first ClickHouse release line that knows Name.
	Since Version
	// OnlyAt, when non-nil, restricts the entry to a stamp of this value.
	OnlyAt any
}

// DroppableAt reports whether the setting, sent at value, may be dropped.
func (d DroppableSetting) DroppableAt(value any) bool {
	return d.OnlyAt == nil || fmt.Sprint(value) == fmt.Sprint(d.OnlyAt)
}

// droppableSettings is the table. The release that first knows each setting
// is verified against real servers by test/chserver's droppable-setting
// probe.
var droppableSettings = []DroppableSetting{
	// A memory bound for exponential-histogram counter windows.
	{Name: SettingEnableAdaptiveAggregator, Feature: FeatureHistogramAdaptiveAggregatorOff, Since: Version{Major: 26, Minor: 8}},
	// A spill threshold: a join that crosses it spills instead of aborting.
	{Name: SettingMaxBytesBeforeExternalJoin, Feature: FeatureJoinSpill, Since: Version{Major: 26, Minor: 4}},
	// A spill threshold: a DISTINCT that crosses it spills instead of aborting.
	{Name: SettingMaxBytesBeforeExternalDistinct, Feature: FeatureDistinctSpill, Since: Version{Major: 26, Minor: 9}},
	// An index-selection threshold.
	{Name: SettingMinTableRowsToUseProjectionIndex, Feature: FeatureTraceIDBitmapFilter, Since: Version{Major: 25, Minor: 11}},
	// A read-order choice, and the LIMIT bound it engages under.
	{Name: SettingQueryPlanOptimizeLazyMaterialization, Feature: FeatureLazyMaterialization, Since: Version{Major: 25, Minor: 4}},
	{Name: SettingQueryPlanMaxLimitForLazyMaterialization, Feature: FeatureLazyMaterialization, Since: Version{Major: 25, Minor: 4}},
	// The query condition cache, switched on. Switched off (0) it is the
	// gate against the cache's wrong-result builds, and stays.
	{Name: SettingUseQueryConditionCache, Feature: FeatureConditionCache, Since: Version{Major: 25, Minor: 3}, OnlyAt: conditionCacheOn},
}

// conditionCacheOn is the use_query_condition_cache value condition_cache
// stamps.
const conditionCacheOn = 1

// DroppableSettings returns a copy of the table.
func DroppableSettings() []DroppableSetting {
	return slices.Clone(droppableSettings)
}

// LookupDroppableSetting returns the entry for the setting called name.
func LookupDroppableSetting(name string) (DroppableSetting, bool) {
	for _, s := range droppableSettings {
		if s.Name == name {
			return s, true
		}
	}
	return DroppableSetting{}, false
}
