package engine

import (
	"context"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// filterScan builds Filter(Predicate) over Scan(table) — a read path with an
// actual WHERE predicate, the shape the condition cache can help.
func filterScan(table string) chplan.Node {
	return &chplan.Filter{
		Input:     &chplan.Scan{Table: table},
		Predicate: &chplan.LitString{V: "x"},
	}
}

// conditionCacheRules returns SettingsRules with ConditionCache ON and the
// default schema wired (so the predicate-stable check runs against live shapes).
func conditionCacheRules() SettingsRules {
	return SettingsRules{
		ConditionCache: true,
		Metrics:        schema.DefaultOTelMetrics(),
		Traces:         schema.DefaultOTelTraces(),
		Logs:           schema.DefaultOTelLogs(),
	}
}

// TestApply_StampsConditionCache_OnPredicateStablePath confirms that with the
// ConditionCache rule on (which only resolves in on server >= 25.3) and a read
// path that carries a WHERE predicate, apply stamps use_query_condition_cache=1.
func TestApply_StampsConditionCache_OnPredicateStablePath(t *testing.T) {
	plan := filterScan("otel_metrics_sum")
	ctx := conditionCacheRules().apply(context.Background(), plan)
	if got := settingValue(ctx, settingUseQueryConditionCache); got != 1 {
		t.Errorf("ConditionCache on + predicate-stable: setting = %v; want 1", got)
	}
}

// TestApply_ConditionCache_OffByDefault confirms the rule is DARK when the
// feature did not resolve in (e.g. server < 25.3, where condition_cache is
// absent from the EnabledSet so ConditionCache is false): nothing is stamped.
func TestApply_ConditionCache_OffByDefault(t *testing.T) {
	plan := filterScan("otel_metrics_sum")
	off := SettingsRules{Metrics: schema.DefaultOTelMetrics()}.apply(context.Background(), plan)
	if got := settingValue(off, settingUseQueryConditionCache); got != nil {
		t.Errorf("ConditionCache off (server < 25.3): setting = %v; want absent", got)
	}
}

// TestApply_ConditionCache_NotStampedWithoutPredicate confirms the conservative
// gate: a bare scan with no WHERE predicate gains nothing from the condition
// cache, so the setting is not stamped even with the rule on.
func TestApply_ConditionCache_NotStampedWithoutPredicate(t *testing.T) {
	plan := chplan.Node(&chplan.Scan{Table: "otel_metrics_sum"})
	ctx := conditionCacheRules().apply(context.Background(), plan)
	if got := settingValue(ctx, settingUseQueryConditionCache); got != nil {
		t.Errorf("bare scan (no predicate): setting = %v; want absent (conservative gate)", got)
	}
}

// TestPredicateStableForConditionCache covers the predicate-stable gate
// directly: a Filter-over-Scan qualifies; a bare Scan or a predicate-less plan
// does not.
func TestPredicateStableForConditionCache(t *testing.T) {
	if !predicateStableForConditionCache(filterScan("otel_traces")) {
		t.Error("Filter over Scan: want predicate-stable")
	}
	if predicateStableForConditionCache(&chplan.Scan{Table: "otel_traces"}) {
		t.Error("bare Scan: want NOT predicate-stable")
	}
}
