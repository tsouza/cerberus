package chclient

import (
	"context"
	"testing"
)

// resultCacheSettingsTestTTLSeconds is an arbitrary well-formed TTL for the
// stamp under test; the assertions below never depend on its magnitude, only
// on it riding through unchanged.
const resultCacheSettingsTestTTLSeconds = 300

// TestWithResultCacheSetting_NeverStampsUseQueryCacheAlone is the unit-level
// half of the cerberus issue #2895 regression pin: use_query_cache=1 must
// never reach a query without
// query_cache_nondeterministic_function_handling riding alongside it.
//
// The coupling is the whole fix, not a detail of it. ClickHouse's server
// default for that handling knob is `throw`, so a statement its query cache
// declines to cache — anything containing a function it classifies as
// non-deterministic, `arrayJoin` among them, which is how every non-native
// range lowering fans samples across the step grid — comes back as error 704
// instead of as rows. On main that failed 21 fully-closed-window range
// queries outright and the resulting error rate tripped the chclient circuit
// breaker, cascading into 144 diverged LogQL cases and 871 diverged PromQL
// cases against the reference backends.
//
// This test pins the shape of the settings map. Its companion,
// TestResultCache_NonDeterministicQuery_RunsUncachedInsteadOfFailing in
// result_cache_nondeterministic_integration_test.go, pins the BEHAVIOUR
// against a real ClickHouse — that a statement carrying arrayJoin actually
// returns its rows under this stamp. Neither substitutes for the other: this
// one would still pass if ClickHouse changed the value's meaning, and that
// one only runs on the Docker-bearing lane.
func TestWithResultCacheSetting_NeverStampsUseQueryCacheAlone(t *testing.T) {
	settings := QuerySettingsFromContext(
		WithResultCacheSetting(context.Background(), resultCacheSettingsTestTTLSeconds),
	)

	if got, ok := settings[SettingUseQueryCache]; !ok || got != 1 {
		t.Fatalf("settings[%s] = %v (present=%t); want 1", SettingUseQueryCache, got, ok)
	}

	got, ok := settings[SettingQueryCacheNondeterministicFunctionHandling]
	if !ok {
		t.Fatalf("settings[%s] missing; use_query_cache=1 must never ride without it (#2895) — "+
			"ClickHouse's default for it is `throw`, which FAILS a query its cache declines rather than "+
			"returning the rows uncached", SettingQueryCacheNondeterministicFunctionHandling)
	}
	if got != queryCacheHandlingIgnore {
		t.Errorf("settings[%s] = %v; want %q — `throw` fails the query and `save` would cache a result "+
			"ClickHouse just said it cannot vouch for",
			SettingQueryCacheNondeterministicFunctionHandling, got, queryCacheHandlingIgnore)
	}

	if got, ok := settings[SettingQueryCacheTTL]; !ok || got != int64(resultCacheSettingsTestTTLSeconds) {
		t.Errorf("settings[%s] = %v (present=%t); want %d",
			SettingQueryCacheTTL, got, ok, resultCacheSettingsTestTTLSeconds)
	}
}
