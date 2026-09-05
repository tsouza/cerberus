package tempo_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/tsouza/cerberus/internal/chclient"
)

// TestSearchRecent_ShardUnavailable503 — when ClickHouse aborts a
// Distributed query with ALL_CONNECTION_TRIES_FAILED (code 279 — every
// replica of at least one data shard is unreachable, cerberus's own
// skip_unavailable_shards=0 pin refusing to silently answer from partial
// data), the Tempo head must answer 503 (ErrClassUnavailable — the same
// class as a tripped circuit breaker or a timed-out backend) rather than
// the 422 a per-query resource rejection gets, or a 5xx internal fault.
func TestSearchRecent_ShardUnavailable503(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{err: &chclient.ShardUnavailableError{
		Cause: &clickhouse.Exception{
			Code:    279,
			Name:    "ALL_CONNECTION_TRIES_FAILED",
			Message: "All connection tries failed. Log: \n\nCode: 210. DB::NetException: ...\n",
		},
	}}
	srv := newServer(q, "v-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/search/recent")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d (body %q), want 503", resp.StatusCode, body)
	}
	if !strings.Contains(body, "reach any replica") {
		t.Fatalf("body %q does not carry the shard-unavailable message", body)
	}
}

// TestSearchRecent_StaleReplicaFallbackDenied503 — when ClickHouse aborts a
// Distributed query with ALL_REPLICAS_ARE_STALE (code 369 — every
// reachable replica of at least one data shard is stale, and cerberus's
// own fallback_to_stale_replicas_for_distributed_queries=0 pin refuses to
// silently serve one anyway), the Tempo head must answer 503.
func TestSearchRecent_StaleReplicaFallbackDenied503(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{err: &chclient.StaleReplicaFallbackDeniedError{
		Cause: &clickhouse.Exception{
			Code:    369,
			Name:    "ALL_REPLICAS_ARE_STALE",
			Message: "Could not find enough connections to up-to-date replicas. Got: 0, needed: 1",
		},
	}}
	srv := newServer(q, "v-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/search/recent")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d (body %q), want 503", resp.StatusCode, body)
	}
	if !strings.Contains(body, "stale") {
		t.Fatalf("body %q does not carry the stale-replica-fallback message", body)
	}
}
