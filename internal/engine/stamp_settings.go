package engine

import "github.com/tsouza/cerberus/internal/chclient"

// Every setting the engine stamps on a query (through
// chclient.WithQuerySetting or a chclient helper that stamps one) is in
// exactly one of four classes, by what a ClickHouse node too old for its name
// does with the query:
//
//   - a chopt.DroppableSettings entry, at a value it may be dropped at: the
//     client sends the query again without it;
//   - a chopt.GateSettings entry: the query fails with a named error;
//   - FunctionRequiredStampSettings: stamped only on a statement calling a
//     function that needs the setting, so the node does not know the function
//     either;
//   - FloorKnownStampSettings: every supported release knows it.
//
// TestStampedSettings_AreEachClassifiedOnce enforces the classification over
// the engine's source, and test/chserver's setting-floor probe verifies the
// floor-known set and the droppable and gate releases against real servers.

// FloorKnownStampSettings returns the stamped settings every supported
// ClickHouse release knows, from the 24.8 floor up.
func FloorKnownStampSettings() []string {
	return []string{
		settingOptimizeAggregationInOrder,
		settingEnableAnalyzer,
		settingLogComment,
		settingMaxBytesBeforeExternalGroupBy,
		settingMaxBytesBeforeExternalSort,
		settingMaxThreads,
		settingMaxBlockSize,
		settingGroupByTwoLevelThresholdBytes,
		chclient.SettingUseQueryCache,
		chclient.SettingQueryCacheNondeterministicFunctionHandling,
		chclient.SettingQueryCacheTTL,
		chclient.SettingWorkload,
		chclient.SettingParallelReplicas,
	}
}

// FunctionRequiredStampSettings returns the stamped settings a function the
// same statement calls requires: the time-series grid functions'
// experimental switch.
func FunctionRequiredStampSettings() []string {
	return []string{chclient.SettingExperimentalTSGridAggregate}
}
