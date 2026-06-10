package tempo_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
)

// TestSearchRecent_SampleBudget422 — when the per-query sample budget
// (CERBERUS_QUERY_MAX_SAMPLES) aborts the result-set drain, the Tempo
// head must answer 422 (an over-broad query, peer to the lower-stage
// rejections) — never a 5xx: ClickHouse is healthy and the chclient
// breaker never counts drain errors as failures.
func TestSearchRecent_SampleBudget422(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{err: &chclient.TooManySamplesError{Limit: 3}}
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
	if !strings.Contains(body, "sample budget exceeded") {
		t.Fatalf("body %q does not carry the budget message", body)
	}
}
