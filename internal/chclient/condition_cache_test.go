package chclient

import (
	"context"
	"sync/atomic"
	"testing"
)

// TestQuerySettings_ConditionCacheOverride pins the client-wide
// use_query_condition_cache override: absent while off (the server default and
// the engine's per-query rule decide), stamped to 0 while on — overriding a
// per-query 1 an engine rule attached — and shared by every ForHead view.
func TestQuerySettings_ConditionCacheOverride(t *testing.T) {
	t.Parallel()

	def, registry := buildBreakers(false, 0, 0, 0, nil)
	c := &Client{br: def, breakers: registry, conditionCacheDisabled: new(atomic.Bool)}
	view := c.ForHead(HeadLoki)
	engineStamped := WithQuerySetting(context.Background(), SettingUseQueryConditionCache, 1)

	if _, ok := c.querySettings(context.Background())[SettingUseQueryConditionCache]; ok {
		t.Errorf("override off: %s stamped on a query that did not ask for it", SettingUseQueryConditionCache)
	}
	if got := c.querySettings(engineStamped)[SettingUseQueryConditionCache]; got != 1 {
		t.Errorf("override off: %s = %v; want the engine's per-query 1", SettingUseQueryConditionCache, got)
	}

	c.SetQueryConditionCacheDisabled(true)
	for name, client := range map[string]*Client{"client": c, "view": view} {
		for ctxName, ctx := range map[string]context.Context{"bare": context.Background(), "engine-stamped": engineStamped} {
			if got := client.querySettings(ctx)[SettingUseQueryConditionCache]; got != 0 {
				t.Errorf("override on, %s, %s query: %s = %v; want 0", name, ctxName, SettingUseQueryConditionCache, got)
			}
		}
	}

	c.SetQueryConditionCacheDisabled(false)
	if _, ok := view.querySettings(context.Background())[SettingUseQueryConditionCache]; ok {
		t.Errorf("override lifted: the view still stamps %s", SettingUseQueryConditionCache)
	}
}
