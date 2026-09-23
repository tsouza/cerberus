package chclient

// SettingUseQueryConditionCache is ClickHouse's query-condition-cache switch.
// The engine stamps it to 1 on predicate-stable reads when the condition_cache
// feature resolves in; the client stamps it to 0 on EVERY data-plane query
// while SetQueryConditionCacheDisabled(true) is in force.
const SettingUseQueryConditionCache = "use_query_condition_cache"

// SetQueryConditionCacheDisabled turns the client-wide condition-cache
// override on or off. While on, every query dispatched through queryContext
// carries use_query_condition_cache=0, applied after the per-query settings so
// no plan-shape rule can re-enable the cache.
//
// Leaving the setting unstamped is not enough to keep a query off the cache:
// ClickHouse enables it by default (and a server profile may too), so a query
// that merely omits it still reads — and writes — cached granule verdicts.
// cmd/cerberus turns the override on exactly when the probed build falls
// inside chopt.FeatureConditionCache's UnsafeBuilds, where a cached verdict
// written by another query can make a later read silently drop rows, and
// re-evaluates it on every capability re-probe.
//
// The setting does not exist below ClickHouse 25.3, so the override must stay
// off against an older server; every known-unsafe build is far above that.
//
// The switch is shared by every ForHead view of the client.
func (c *Client) SetQueryConditionCacheDisabled(disabled bool) {
	c.conditionCacheDisabled.Store(disabled)
}

// QueryConditionCacheDisabled reports whether the override is in force. A
// Client built without New (a zero value in a unit test) has no switch and
// reports false.
func (c *Client) QueryConditionCacheDisabled() bool {
	return c.conditionCacheDisabled != nil && c.conditionCacheDisabled.Load()
}
