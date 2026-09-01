//go:build integration

package chclient_test

import (
	"context"
	"testing"
	"time"

	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	"github.com/tsouza/cerberus/internal/chclient"
)

// resultCacheArrayJoinProbeSQL is a statement ClickHouse's query cache
// REFUSES to cache, reduced to the one construct that causes the refusal.
// arrayJoin is classified non-deterministic (it changes the row count), and
// it is not an incidental choice here: `arrayJoin(arrayMap(i -> ...,
// range(...)))` is how every non-native range lowering fans a query's samples
// across its step grid, so this is the shape the compatibility corpus
// actually dispatched when cerberus issue #2895 broke. The projection is a
// String so Client.QueryStrings can decode it.
const resultCacheArrayJoinProbeSQL = "SELECT toString(arrayJoin(range(3)))"

// resultCacheArrayJoinProbeRows is how many rows resultCacheArrayJoinProbeSQL
// returns — one per element of range(3). Asserting the count (not merely a
// nil error) is what separates "the query ran" from "the query ran and
// produced its result", which is the property #2895 destroyed.
const resultCacheArrayJoinProbeRows = 3

// TestResultCache_NonDeterministicQuery_RunsUncachedInsteadOfFailing is the
// behavioural regression pin for cerberus issue #2895, against a real
// ClickHouse server rather than a settings-map assertion.
//
// On main, chclient.WithResultCacheSetting stamped use_query_cache=1 without
// query_cache_nondeterministic_function_handling. That knob's SERVER DEFAULT
// is `throw`, so ClickHouse answered any statement its query cache declined
// to cache with error 704 QUERY_CACHE_USED_WITH_NONDETERMINISTIC_FUNCTIONS
// instead of with rows. Because the result_cache feature is AutoSelect, every
// deployment on the default posture hit it on every fully-closed-window range
// query whose lowering fans samples out with arrayJoin: the compatibility
// harness's ClickHouse recorded 21 such rejections, and the error rate tripped
// the chclient circuit breaker into a lane-wide regression (144 LogQL and 871
// PromQL cases diverging from the reference backends).
//
// The invariant this pins is the general one, not the narrow symptom: a
// performance optimization must never be able to turn a query that would have
// SUCCEEDED into one that fails. ClickHouse keeps its veto over statements its
// cache cannot vouch for; the veto just costs a cache miss now.
//
// Running it against the code as it stood before the fix reproduces the
// incident directly — QueryStrings returns the 704 exception and the test
// fails on the first check.
//
// Gated by the `integration` build tag (requires Docker) and run by
// `just chclient-integration`, which the required `strict-scan` lane invokes.
func TestResultCache_NonDeterministicQuery_RunsUncachedInsteadOfFailing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	container, err := tcclickhouse.Run(
		ctx,
		"clickhouse/clickhouse-server:25.9-alpine",
		tcclickhouse.WithUsername("cerberus"),
		tcclickhouse.WithPassword("cerberus"),
	)
	if err != nil {
		t.Fatalf("start clickhouse: %v", err)
	}
	t.Cleanup(func() {
		_ = container.Terminate(ctx)
	})

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("host: %v", err)
	}
	port, err := container.MappedPort(ctx, "9000/tcp")
	if err != nil {
		t.Fatalf("port: %v", err)
	}

	client, err := chclient.New(chclient.Config{
		Addr:     host + ":" + port.Port(),
		Username: "cerberus",
		Password: "cerberus",
		Database: "default",
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if capability := client.ProbeResultCacheCapability(ctx); capability.String() != "available" {
		t.Fatalf("query cache not available on this server: %v", capability)
	}

	// The exact ctx the engine's result-cache rule installs for a query whose
	// window eligibleForResultCache proved closed.
	rcCtx := chclient.WithResultCacheSetting(ctx, int64(time.Minute.Seconds()))

	rows, err := client.QueryStrings(rcCtx, resultCacheArrayJoinProbeSQL)
	if err != nil {
		t.Fatalf("arrayJoin query under the result-cache stamp: %v\n"+
			"want it to RUN (uncached); ClickHouse's query cache declines to cache arrayJoin, and under "+
			"the server's `throw` default that refusal fails the query outright — cerberus issue #2895", err)
	}
	if len(rows) != resultCacheArrayJoinProbeRows {
		t.Errorf("arrayJoin query returned %d row(s); want %d — the statement must produce its real "+
			"result, not merely avoid an error", len(rows), resultCacheArrayJoinProbeRows)
	}

	// The stamp must still be a real cache for the statements ClickHouse DOES
	// accept: `ignore` suppresses the failure without suppressing the feature.
	// Without this leg, silently dropping the whole result-cache stamp would
	// also make the assertions above pass.
	hitsBefore, _ := chclient.ResultCacheStats()
	for range 2 {
		if _, err := client.QueryStrings(rcCtx, resultCacheHitProbeSQL); err != nil {
			t.Fatalf("cacheable query under the result-cache stamp: %v", err)
		}
	}
	if hitsAfter, _ := chclient.ResultCacheStats(); hitsAfter-hitsBefore != 1 {
		t.Errorf("QueryCacheHits delta over two identical cacheable dispatches = %d; want 1 — "+
			"`ignore` must skip caching only the statements ClickHouse vetoes, never disable the cache",
			hitsAfter-hitsBefore)
	}
}
