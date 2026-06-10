package tempo_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/tsouza/cerberus/internal/chclient"
)

// TestSearchRecent_MemoryLimit422 — when ClickHouse aborts a
// data-plane query with MEMORY_LIMIT_EXCEEDED (code 241; the
// per-query CERBERUS_CH_QUERY_MAX_MEMORY cap or a server-side limit),
// the Tempo head must answer 422 (a per-query resource rejection,
// peer to the sample-budget rejection) — never a 5xx: ClickHouse is
// healthy when it enforces a cap, and the chclient breaker treats
// code 241 as a success for the same reason.
func TestSearchRecent_MemoryLimit422(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{err: &chclient.MemoryLimitError{
		Limit: 1 << 30,
		Cause: &clickhouse.Exception{
			Code:    241,
			Name:    "MEMORY_LIMIT_EXCEEDED",
			Message: "Memory limit (total) exceeded: would use 2.12 GiB, maximum: 1.80 GiB",
		},
	}}
	srv := newServer(q, "v-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/search/recent")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d (body %q), want 422", resp.StatusCode, body)
	}
	if !strings.Contains(body, "memory limit exceeded") {
		t.Fatalf("body %q does not carry the memory-limit message", body)
	}
	if !strings.Contains(body, "1073741824 bytes") {
		t.Fatalf("body %q does not name the configured per-query cap", body)
	}
}
