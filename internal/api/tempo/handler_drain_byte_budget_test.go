package tempo_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
)

// TestSearch_DrainByteBudget422 — when the wide-projection byte budget aborts the
// span drain, the Tempo HTTP head must answer 422 (a resource rejection peer to
// the sample budget), never a 5xx. The charge itself is proven on the production
// rowsCursor in chclient; this pins the handler's error classification.
func TestSearch_DrainByteBudget422(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{err: &chclient.DrainByteBudgetError{Limit: 256 << 20}}
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
	if !strings.Contains(body, "budget") {
		t.Fatalf("body %q does not carry the byte-budget message", body)
	}
}
