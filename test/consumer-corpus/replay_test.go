package consumercorpus

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/schema"
)

// newStubServer builds the entry's datasource handler over the named
// canned-row fixture, mirroring how internal/api/*/conformance tests
// construct handlers.
func newStubServer(t *testing.T, datasource, fixture string) *httptest.Server {
	t.Helper()
	q, err := StubFixture(fixture)
	if err != nil {
		t.Fatalf("stub fixture: %v", err)
	}
	mux := http.NewServeMux()
	switch datasource {
	case "prom":
		prom.New(q, schema.DefaultOTelMetrics(), nil).Mount(mux)
	case "loki":
		loki.New(q, schema.DefaultOTelLogs(), nil).Mount(mux)
	case "tempo":
		tempo.New(q, schema.DefaultOTelTraces(), "v0.0.0-corpus", nil).Mount(mux)
	default:
		t.Fatalf("unknown datasource %q", datasource)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestConsumerCorpus_Replay_Stub replays every corpus entry against
// stub-backed in-process handlers and asserts the wire-level consumer
// contract: HTTP status, the exact consumer decode, and the wire
// predicates. Each entry is an independent subtest, so a run always
// reports EVERY violated entry — no early exit, no tolerated
// failures.
func TestConsumerCorpus_Replay_Stub(t *testing.T) {
	t.Parallel()

	entries, err := Load(".")
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("corpus is empty")
	}
	for _, e := range entries {
		e := e
		t.Run(e.Version+"/"+e.Name, func(t *testing.T) {
			t.Parallel()
			srv := newStubServer(t, e.Datasource, e.Stub)
			for _, err := range Replay(srv.Client(), srv.URL, e, StubTokens(e.Datasource), false) {
				t.Errorf("%v", err)
			}
		})
	}
}
