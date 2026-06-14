package consumercorpus

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/schema"
)

// streamWith builds a one-stream, one-value streamView whose value tuple
// is the given raw JSON elements — used to drive checkStreamValueArity's
// arity / shape branches directly.
func streamWith(elems ...string) []streamView {
	val := make([]json.RawMessage, len(elems))
	for i, e := range elems {
		val[i] = json.RawMessage(e)
	}
	return []streamView{{Values: [][]json.RawMessage{val}}}
}

// TestCheckStreamValueArity pins the EXACT shape Grafana's converter
// accepts per negotiated mode — the contract whose violation produced
// both #908 shard breaks.
func TestCheckStreamValueArity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		streams    []streamView
		categorize bool
		wantErr    string // substring; "" = expect success
	}{
		{
			name:       "plain two-element ok",
			streams:    streamWith(`"1"`, `"line"`),
			categorize: false,
		},
		{
			name:       "plain three-element rejected",
			streams:    streamWith(`"1"`, `"line"`, `{"structuredMetadata":{}}`),
			categorize: false,
			wantErr:    "want exactly 2",
		},
		{
			// The #908 shard-smoke 400: a two-element value under the
			// advertised categorize-labels flag.
			name:       "categorized two-element rejected",
			streams:    streamWith(`"1"`, `"line"`),
			categorize: true,
			wantErr:    "want exactly 3",
		},
		{
			name:       "categorized three-element with empty object ok",
			streams:    streamWith(`"1"`, `"line"`, `{"structuredMetadata":{}}`),
			categorize: true,
		},
		{
			name:       "categorized three-element with metadata ok",
			streams:    streamWith(`"1"`, `"line"`, `{"structuredMetadata":{"thread":"w0"}}`),
			categorize: true,
		},
		{
			name:       "categorized empty object ok",
			streams:    streamWith(`"1"`, `"line"`, `{}`),
			categorize: true,
		},
		{
			// A bare metadata map (not wrapped in the categorized
			// envelope) is read as label-type fields, so columns never
			// surface — reject it.
			name:       "categorized bare metadata map rejected",
			streams:    streamWith(`"1"`, `"line"`, `{"thread":"w0"}`),
			categorize: true,
			wantErr:    "unexpected top-level key",
		},
		{
			name:       "categorized non-object third rejected",
			streams:    streamWith(`"1"`, `"line"`, `"oops"`),
			categorize: true,
			wantErr:    "must be a JSON object",
		},
		{
			name:       "categorized parsed-only object ok",
			streams:    streamWith(`"1"`, `"line"`, `{"parsed":{"k":"v"}}`),
			categorize: true,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := checkStreamValueArity(tt.streams, tt.categorize)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.wantErr != "" && err == nil:
				t.Fatalf("want error containing %q, got nil", tt.wantErr)
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Fatalf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}

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
