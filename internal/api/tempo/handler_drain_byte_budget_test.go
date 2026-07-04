package tempo_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
)

// TestSearch_AttachesByteBudget_NoBypass is the no-bypass ratchet: EVERY Tempo
// drain endpoint that projects the wide attribute maps must attach the drain
// byte budget to its query context. A new endpoint that drains wide maps without
// routing through withSpanDrainBudget would leave that path uncharged (exactly
// how /api/traces/{id} was missed in review) — this fails if any listed
// endpoint stops attaching it.
func TestSearch_AttachesByteBudget_NoBypass(t *testing.T) {
	t.Parallel()
	for _, ep := range []string{
		"/api/search?q=" + url.QueryEscape("{}"),
		"/api/search/recent",
		"/api/traces/0000000000000000000000000000000a",
	} {
		q := &stubQuerier{}
		srv := newServer(q, "v-test")
		resp, err := http.Get(srv.URL + ep)
		if err != nil {
			srv.Close()
			t.Fatalf("GET %s: %v", ep, err)
		}
		resp.Body.Close()
		srv.Close()
		if !q.sawByteBudget {
			t.Errorf("%s did not attach the wide-projection drain byte budget — a wide-map drain would run uncharged", ep)
		}
	}
}

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
