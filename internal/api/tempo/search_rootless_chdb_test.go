//go:build chdb

// chDB-backed pins for /api/search over a trace whose root span was never
// ingested: every span names a parent, and one names a parent no row carries.
package tempo_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
)

// rootlessSeed: trace ...1 has a root; trace ...2 has none — its entry span
// names parent 4000000000000099, which no row carries, and a child of that
// entry span starts after it.
//
//	trace ...1  root 'GET /a' (frontend)  0ms +100ms   child 'db' (backend) 10ms +20ms
//	trace ...2  'orphan.entry' (orphan)   1s  +200ms   child 'orphan.child' (orphan) 1s+50ms +50ms
const rootlessSeed = `INSERT INTO otel_traces VALUES
    ('d0000000000000000000000000000001', '4000000000000011', '',                 'GET /a',       'Server', 100000000, toDateTime64('2026-05-01 11:00:00.000', 9), 'Unset', '', '', '', map(), map('service.name', 'frontend'), 'frontend'),
    ('d0000000000000000000000000000001', '4000000000000012', '4000000000000011', 'db',           'Client', 20000000,  toDateTime64('2026-05-01 11:00:00.010', 9), 'Unset', '', '', '', map(), map('service.name', 'backend'), 'backend'),
    ('d0000000000000000000000000000002', '4000000000000021', '4000000000000099', 'orphan.entry', 'Server', 200000000, toDateTime64('2026-05-01 11:00:01.000', 9), 'Unset', '', '', '', map(), map('service.name', 'orphan'), 'orphan'),
    ('d0000000000000000000000000000002', '4000000000000022', '4000000000000021', 'orphan.child', 'Client', 50000000,  toDateTime64('2026-05-01 11:00:01.050', 9), 'Unset', '', '', '', map(), map('service.name', 'orphan'), 'orphan');`

// rootlessTraceDurationMs is the rootless trace's extent: its entry span.
const rootlessTraceDurationMs = 200

func newRootlessChDBServer(t *testing.T) *httptest.Server {
	t.Helper()
	c := chclienttest.NewChDB(t)
	c.Seed(t, regroupDDL)
	c.Seed(t, rootlessSeed)
	h := tempo.New(c, schema.DefaultOTelTraces(), "v-test", nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestSearch_RootlessTrace_ChDB pins that a trace with no root span reports
// what reference Tempo does — no rootTraceName and the "<root span not yet
// received>" rootServiceName — whichever of its spans the query matched,
// while the rooted trace keeps its root.
func TestSearch_RootlessTrace_ChDB(t *testing.T) {
	srv := newRootlessChDBServer(t)
	for _, q := range []string{
		`{ }`,
		`{ name = "orphan.child" || name = "db" }`,
		`{ } | count() > 0`,
		`{ } | by(resource.service.name)`,
	} {
		t.Run(q, func(t *testing.T) {
			got := runRegroupSearch(t, srv, q, 20)
			if len(got) != 2 {
				t.Fatalf("got %d traces, want 2: %+v", len(got), got)
			}
			rootless, rooted := got[0], got[1]
			if rootless.TraceID != "d0000000000000000000000000000002" {
				t.Fatalf("newest trace = %s, want the rootless d…2", rootless.TraceID)
			}
			if rootless.RootTraceName != "" || rootless.RootServiceName != "<root span not yet received>" {
				t.Errorf("rootless trace = {rootTraceName %q, rootServiceName %q}, want {\"\", \"<root span not yet received>\"}",
					rootless.RootTraceName, rootless.RootServiceName)
			}
			if rootless.DurationMs != rootlessTraceDurationMs {
				t.Errorf("rootless trace durationMs = %d, want %d", rootless.DurationMs, rootlessTraceDurationMs)
			}
			if rooted.RootTraceName != "GET /a" || rooted.RootServiceName != "frontend" {
				t.Errorf("rooted trace = {rootTraceName %q, rootServiceName %q}, want {\"GET /a\", \"frontend\"}",
					rooted.RootTraceName, rooted.RootServiceName)
			}
		})
	}
}

// TestSearchRecent_RootlessTrace_ChDB pins the same root fields on
// /api/search/recent, which shapes its summaries through the same steps.
func TestSearchRecent_RootlessTrace_ChDB(t *testing.T) {
	srv := newRootlessChDBServer(t)
	resp, err := http.Get(srv.URL + "/api/search/recent?start=1777593600&end=1777680000")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var sr tempo.SearchResponse
	if err := json.Unmarshal([]byte(body), &sr); err != nil {
		t.Fatalf("decode: %v\nbody: %s", err, body)
	}
	byID := map[string]tempo.TraceSummary{}
	for _, tr := range sr.Traces {
		byID[tr.TraceID] = tr
	}
	if got := byID["d0000000000000000000000000000002"]; got.RootTraceName != "" || got.RootServiceName != "<root span not yet received>" {
		t.Errorf("rootless trace = {rootTraceName %q, rootServiceName %q}, want {\"\", \"<root span not yet received>\"}",
			got.RootTraceName, got.RootServiceName)
	}
	if got := byID["d0000000000000000000000000000001"]; got.RootTraceName != "GET /a" || got.RootServiceName != "frontend" {
		t.Errorf("rooted trace = {rootTraceName %q, rootServiceName %q}, want {\"GET /a\", \"frontend\"}",
			got.RootTraceName, got.RootServiceName)
	}
}
