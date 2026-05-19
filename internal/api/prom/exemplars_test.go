package prom_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/api/prom"
)

// exemplarsResponse mirrors prom.Response specialised to the
// query_exemplars data shape so the test decoder doesn't have to walk
// through `any`.
type exemplarsResponse struct {
	Status    string                `json:"status"`
	Data      []prom.ExemplarSeries `json:"data"`
	ErrorType string                `json:"errorType"`
	Error     string                `json:"error"`
}

// TestQueryExemplars — table-test for /api/v1/query_exemplars.
//
// The current schema doesn't expose exemplars so the success path always
// returns `data:[]`; the cases below pin every other observable: input
// validation (missing / unparseable / bogus-time params), HTTP-method
// support (GET + POST), and the empty-array envelope shape. When the
// schema gains an exemplars column, the fixtures here can grow to cover
// single-series + multi-series results without changing the input wiring.
func TestQueryExemplars(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		method     string
		query      url.Values
		wantStatus int
		wantErrKey string
	}{
		{
			name:       "empty result — GET happy path",
			method:     http.MethodGet,
			query:      url.Values{"query": {"up"}, "start": {"1717995600"}, "end": {"1717999200"}},
			wantStatus: http.StatusOK,
		},
		{
			name:       "empty result — POST happy path",
			method:     http.MethodPost,
			query:      url.Values{"query": {"http_request_duration_seconds_bucket"}, "start": {"1717995600"}, "end": {"1717999200"}},
			wantStatus: http.StatusOK,
		},
		{
			name:       "single-series matcher — happy path",
			method:     http.MethodGet,
			query:      url.Values{"query": {`http_request_duration_seconds_bucket{job="api"}`}, "start": {"1717995600"}, "end": {"1717999200"}},
			wantStatus: http.StatusOK,
		},
		{
			name:       "multi-series matchers — happy path",
			method:     http.MethodGet,
			query:      url.Values{"query": {`http_request_duration_seconds_bucket{job=~"api|db"}`}, "start": {"1717995600"}, "end": {"1717999200"}},
			wantStatus: http.StatusOK,
		},
		{
			name:       "missing query",
			method:     http.MethodGet,
			query:      url.Values{"start": {"1717995600"}, "end": {"1717999200"}},
			wantStatus: http.StatusBadRequest,
			wantErrKey: prom.ErrBadData,
		},
		{
			name:       "unparseable promql",
			method:     http.MethodGet,
			query:      url.Values{"query": {"up +"}, "start": {"1717995600"}, "end": {"1717999200"}},
			wantStatus: http.StatusBadRequest,
			wantErrKey: prom.ErrBadData,
		},
		{
			name:       "missing start",
			method:     http.MethodGet,
			query:      url.Values{"query": {"up"}, "end": {"1717999200"}},
			wantStatus: http.StatusBadRequest,
			wantErrKey: prom.ErrBadData,
		},
		{
			name:       "missing end",
			method:     http.MethodGet,
			query:      url.Values{"query": {"up"}, "start": {"1717995600"}},
			wantStatus: http.StatusBadRequest,
			wantErrKey: prom.ErrBadData,
		},
		{
			name:       "bogus start",
			method:     http.MethodGet,
			query:      url.Values{"query": {"up"}, "start": {"yesterday"}, "end": {"1717999200"}},
			wantStatus: http.StatusBadRequest,
			wantErrKey: prom.ErrBadData,
		},
		{
			name:       "end before start",
			method:     http.MethodGet,
			query:      url.Values{"query": {"up"}, "start": {"1717999200"}, "end": {"1717995600"}},
			wantStatus: http.StatusBadRequest,
			wantErrKey: prom.ErrBadData,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			q := &stubQuerier{}
			srv := newServer(q)
			t.Cleanup(srv.Close)

			var resp *http.Response
			var err error
			switch tc.method {
			case http.MethodGet:
				resp, err = http.Get(srv.URL + "/api/v1/query_exemplars?" + tc.query.Encode())
			case http.MethodPost:
				resp, err = http.Post(
					srv.URL+"/api/v1/query_exemplars",
					"application/x-www-form-urlencoded",
					strings.NewReader(tc.query.Encode()),
				)
			default:
				t.Fatalf("unsupported method %q", tc.method)
			}
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			body := readBody(t, resp)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status: got %d, want %d; body=%s", resp.StatusCode, tc.wantStatus, body)
			}

			if tc.wantStatus != http.StatusOK {
				var got prom.Response
				if err := json.Unmarshal([]byte(body), &got); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				if got.Status != "error" {
					t.Fatalf("status: got %q, want error", got.Status)
				}
				if got.ErrorType != tc.wantErrKey {
					t.Fatalf("errorType: got %q, want %q", got.ErrorType, tc.wantErrKey)
				}
				return
			}

			var parsed exemplarsResponse
			if err := json.Unmarshal([]byte(body), &parsed); err != nil {
				t.Fatalf("unmarshal: %v\nbody=%s", err, body)
			}
			if parsed.Status != "success" {
				t.Fatalf("status: got %q, want success; err=%s", parsed.Status, parsed.Error)
			}
			// The empty-data path returns an empty slice, not a null. Both decode to len(Data)==0 in Go, so verify the raw
			// JSON shape too.
			if len(parsed.Data) != 0 {
				t.Fatalf("expected empty data slice, got %d entries", len(parsed.Data))
			}
			if !strings.Contains(body, `"data":[]`) {
				t.Errorf("expected JSON to contain `\"data\":[]`; got %s", body)
			}

			// The wired handler now reaches CH and runs the EmitQueryExemplars
			// SQL; the stub Querier returns zero rows so `data` stays empty,
			// which is the empty-result happy-path contract.
			if q.lastSQL == "" {
				t.Errorf("exemplars handler did not reach CH; lastSQL is empty")
			}
		})
	}
}

// TestQueryExemplars_EnvelopeShape — pin the data array shape so a
// future implementation can't drift. The empty-data path serialises as
// `data:[]`, and the field-name vocabulary (`seriesLabels` /
// `exemplars` / `labels` / `value` / `timestamp`) matches Prom's
// documented response shape verbatim.
func TestQueryExemplars_EnvelopeShape(t *testing.T) {
	t.Parallel()

	// Marshal a hand-built ExemplarSeries to assert the field names
	// match Prom's wire format. Done in-process — no HTTP roundtrip
	// needed for this assertion.
	in := []prom.ExemplarSeries{
		{
			SeriesLabels: map[string]string{"__name__": "http_request_duration_seconds_bucket", "job": "api"},
			Exemplars: []prom.Exemplar{
				{
					Labels:    map[string]string{"trace_id": "abc123", "span_id": "def456"},
					Value:     0.0125,
					Timestamp: 1717999199.5,
				},
			},
		},
	}
	out, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{
		`"seriesLabels"`,
		`"exemplars"`,
		`"labels"`,
		`"value"`,
		`"timestamp"`,
		`"trace_id"`,
	} {
		if !strings.Contains(string(out), key) {
			t.Errorf("expected wire-format key %s; got %s", key, string(out))
		}
	}
	// Numeric value (not stringified) — distinguishes exemplar wire
	// shape from Sample, which stringifies for precision.
	if !strings.Contains(string(out), `"value":0.0125`) {
		t.Errorf("expected numeric value field; got %s", string(out))
	}
}

// TestQueryExemplars_Route — sanity check that the route is wired,
// independent of the body assertions. Hits the handler with a no-arg
// request and expects the canonical 400/bad_data envelope (not a 404).
func TestQueryExemplars_Route(t *testing.T) {
	t.Parallel()

	srv := newServer(&stubQuerier{})
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/query_exemplars")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		t.Fatalf("route not mounted — got 404; body=%s", readBody(t, resp))
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", resp.StatusCode, readBody(t, resp))
	}
	// Header middleware should still apply.
	if got := resp.Header.Get("X-Prometheus-API-Version"); got != "v1" {
		t.Errorf("X-Prometheus-API-Version: got %q, want v1", got)
	}
	// Discard body to satisfy the request lifecycle.
	_ = fmt.Sprintf("%v", readBody(t, resp))
}
