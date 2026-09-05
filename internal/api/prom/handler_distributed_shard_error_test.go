package prom_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/tsouza/cerberus/internal/chclient"
)

// assertDistributedShardUnavailable503 decodes a Prom error envelope and
// pins the ClickHouse Distributed-query partial-shard-failure wire
// contract (cerberus issue #3078): HTTP 503, errorType "unavailable" — the
// same class as a tripped circuit breaker, because ClickHouse itself is
// healthy and one data shard behind the Distributed table is not.
func assertDistributedShardUnavailable503(t *testing.T, resp *http.Response, wantSubstr string) {
	t.Helper()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503", resp.StatusCode)
	}
	var body queryResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	_ = resp.Body.Close()
	if body.Status != "error" {
		t.Fatalf("status field: got %q, want \"error\"", body.Status)
	}
	if body.ErrorType != "unavailable" {
		t.Fatalf("errorType: got %q, want \"unavailable\"", body.ErrorType)
	}
	if !strings.Contains(body.Error, wantSubstr) {
		t.Fatalf("error message %q does not mention %q", body.Error, wantSubstr)
	}
}

// TestQuery_ShardUnavailable503 — when ClickHouse aborts a Distributed
// query with ALL_CONNECTION_TRIES_FAILED (code 279 — every replica of at
// least one data shard is unreachable, cerberus's own
// skip_unavailable_shards=0 pin refusing to silently answer from partial
// data), the Prom head must answer 503 errorType=unavailable, never a 5xx
// internal fault or a 422 resource rejection.
func TestQuery_ShardUnavailable503(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{err: &chclient.ShardUnavailableError{
		Cause: &clickhouse.Exception{
			Code:    279,
			Name:    "ALL_CONNECTION_TRIES_FAILED",
			Message: "All connection tries failed. Log: \n\nCode: 210. DB::NetException: ...\n",
		},
	}}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/query?query=up")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	assertDistributedShardUnavailable503(t, resp, "reach any replica")
}

// TestQuery_StaleReplicaFallbackDenied503 — when ClickHouse aborts a
// Distributed query with ALL_REPLICAS_ARE_STALE (code 369 — every
// reachable replica of at least one data shard is stale, and cerberus's
// own fallback_to_stale_replicas_for_distributed_queries=0 pin refuses to
// silently serve one anyway), the Prom head must answer 503
// errorType=unavailable.
func TestQuery_StaleReplicaFallbackDenied503(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{err: &chclient.StaleReplicaFallbackDeniedError{
		Cause: &clickhouse.Exception{
			Code:    369,
			Name:    "ALL_REPLICAS_ARE_STALE",
			Message: "Could not find enough connections to up-to-date replicas. Got: 0, needed: 1",
		},
	}}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/query?query=up")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	assertDistributedShardUnavailable503(t, resp, "stale")
}
