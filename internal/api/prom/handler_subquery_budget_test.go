package prom_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
)

// newServerWithAnchorBudget mounts a prom handler with the engine-level
// subquery anchor budget wired — the same value cmd/cerberus copies from
// client.MaxQuerySamples() into engine.MaxQuerySamples.
func newServerWithAnchorBudget(q prom.Querier, maxSamples int64) *httptest.Server {
	h := prom.New(q, schema.DefaultOTelMetrics(), nil)
	h.Engine.MaxQuerySamples = maxSamples
	mux := http.NewServeMux()
	h.Mount(mux)
	return httptest.NewServer(mux)
}

// TestQuery_SubqueryAnchorBudget422 — GAP-2 regression pin. An instant
// subquery whose anchor grid alone (OuterRange/Step) exceeds the per-query
// sample budget is rejected with the SAME Prometheus "too many samples" 422 as
// a result-drain overage — and crucially BEFORE any SQL is sent, so the
// millions of intermediate anchor rows never reach ClickHouse to OOM it. This
// is the layer that the original audit found missing: nothing asserted the
// query-of-death is rejected rather than executed.
//
// max_over_time(rate(m[1m])[90d:1s]) = 7,776,001 anchors > the 1M budget here.
func TestQuery_SubqueryAnchorBudget422(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{} // returns nothing; the gate must reject before reaching it
	srv := newServerWithAnchorBudget(q, 1_000_000)
	t.Cleanup(srv.Close)

	q1 := url.QueryEscape("max_over_time(rate(http_requests_total[1m])[90d:1s])")
	resp, err := http.Get(srv.URL + "/api/v1/query?query=" + q1)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	assertBudget422(t, resp)

	if q.lastSQL != "" {
		t.Fatalf("anchor gate must reject before emitting SQL; querier saw SQL: %q", q.lastSQL)
	}
}

// TestQuery_SubqueryWithinBudget_NotGated — the gate must NOT over-reject: a
// normal subquery (5m:30s = 11 anchors) sails under the budget and reaches
// execution (the querier is consulted).
func TestQuery_SubqueryWithinBudget_NotGated(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{samples: []chclient.Sample{
		{MetricName: "http_requests_total", Labels: map[string]string{"job": "api"}, Value: 1},
	}}
	srv := newServerWithAnchorBudget(q, 1_000_000)
	t.Cleanup(srv.Close)

	q1 := url.QueryEscape("max_over_time(rate(http_requests_total[1m])[5m:30s])")
	resp, err := http.Get(srv.URL + "/api/v1/query?query=" + q1)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusUnprocessableEntity {
		t.Fatalf("within-budget subquery wrongly gated (422); the anchor budget over-rejected")
	}
	if q.lastSQL == "" {
		t.Fatal("within-budget subquery never reached execution; the anchor gate over-rejected")
	}
}
