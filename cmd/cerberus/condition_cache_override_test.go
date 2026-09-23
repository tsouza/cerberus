package main

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/config"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestCHOptConsumers_ConditionCacheOverrideFollowsTheProbedBuild pins the
// client-wide use_query_condition_cache=0 override across the capability
// re-probe: it engages on a build known to return wrong results through the
// cache — even under the "off" selection, because the server's own default
// engages the cache — lifts once a probe answers from a fixed build, and is
// left untouched by a floor fallback, which reports an unreachable server
// rather than a changed one.
func TestCHOptConsumers_ConditionCacheOverrideFollowsTheProbedBuild(t *testing.T) {
	t.Parallel()

	client, err := chclient.New(chclient.Config{Addr: "127.0.0.1:1", Database: "otel"})
	if err != nil {
		t.Fatalf("chclient.New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	view := client.ForHead(chclient.HeadProm)

	cfg := config.Config{Schema: schema.DefaultOTelMetrics()}
	consumers := chOptConsumers{client: client}
	unsafeBuild := chopt.Version{Major: 26, Minor: 2, Patch: 19, Build: 43}
	fixedBuild := chopt.Version{Major: 26, Minor: 6, Patch: 1, Build: 1193}

	consumers.apply(cfg, resolutionAt(t, unsafeBuild))
	if !client.QueryConditionCacheDisabled() || !view.QueryConditionCacheDisabled() {
		t.Fatalf("override off on known-unsafe %s under the off selection (client %v, view %v)",
			unsafeBuild, client.QueryConditionCacheDisabled(), view.QueryConditionCacheDisabled())
	}

	fallback := resolutionAt(t, supportedFloorVersion, chopt.FeatureAggregationInOrder)
	fallback.VersionFallback = true
	consumers.apply(cfg, fallback)
	if !client.QueryConditionCacheDisabled() {
		t.Fatal("a floor fallback lifted the override; an unreachable probe says nothing about the build")
	}

	consumers.apply(cfg, resolutionAt(t, fixedBuild, chopt.FeatureConditionCache))
	if client.QueryConditionCacheDisabled() || view.QueryConditionCacheDisabled() {
		t.Fatalf("override still on after a probe answered from fixed %s", fixedBuild)
	}
}
