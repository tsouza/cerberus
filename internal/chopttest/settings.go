//go:build integration

package chopttest

import (
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/schema"
)

// BuildSettingsRules builds the per-query engine.SettingsRules an integration
// test's mounted handler must carry to behave the way a real deployment's
// does, from a resolved chopt.EnabledSet (ResolveEnabledSet). It is the
// SettingsRules-axis sibling of BuildRangeLowerers, and mirrors
// cmd/cerberus/main.go's own settingsRules field-for-field for every rule the
// EnabledSet decides.
//
// Why this exists: prom.New / tempo.New leave Engine.Settings at its zero
// value, which applies NOTHING — every SettingsRules mechanism
// (optimize_aggregation_in_order, use_query_condition_cache,
// max_bytes_before_external_join, min_table_rows_to_use_projection_index) is
// therefore unreachable in a bare integration harness regardless of the
// connected server's version. That gap is what left cerberus issue #2779's
// join_spill memory guardrail with zero sentinel coverage (issue #2820); it
// is closed HERE, once, rather than per-lane.
//
// The OPERATOR-configured fields cmd/cerberus reads off config.Config rather
// than off the EnabledSet — LogCommentShape, ResultCacheIngestLag,
// ResultCacheTTL, QueryWorkload — are deliberately left at their zero values:
// they are not capability decisions, so a test that wants one sets it on the
// returned value explicitly rather than inheriting a hidden default. The three
// schema instances ARE required, because the aggregation-in-order and
// trace-id-bitmap-filter eligibility checks map a scanned table name to its
// sort-key prefix through them; passing the zero value would silently make
// those rules unable to fire.
//
// Note on ResultCache: it rides the EnabledSet faithfully, but
// ResolveEnabledSet does not run chclient.ProbeResultCacheCapability, so
// chopt's RequiresResultCacheCapability gate keeps result_cache OUT of every
// set this package resolves (Capability's conservative zero value). A caller
// that measures per-query cost must assert that for itself — a served-from-
// cache repeat costs almost nothing and would silently hollow out a max-of-N
// memory measurement.
func BuildSettingsRules(set chopt.EnabledSet, metrics schema.Metrics, traces schema.Traces, logs schema.Logs) engine.SettingsRules {
	return engine.SettingsRules{
		OptimizeAggregationInOrder: set.Has(chopt.FeatureAggregationInOrder),
		ConditionCache:             set.Has(chopt.FeatureConditionCache),
		JoinSpill:                  set.Has(chopt.FeatureJoinSpill),
		TraceIDBitmapFilter:        set.Has(chopt.FeatureTraceIDBitmapFilter),
		ResultCache:                set.Has(chopt.FeatureResultCache),
		LazyMaterialization:        set.Has(chopt.FeatureLazyMaterialization),
		Metrics:                    metrics,
		Traces:                     traces,
		Logs:                       logs,
	}
}
