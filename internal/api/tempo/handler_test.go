package tempo_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/tempo"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
)

type stubQuerier struct {
	samples  []chclient.Sample
	strings  []string
	err      error
	lastSQL  string
	lastArgs []any
	// stringsBySQL lets tests stub multiple back-to-back QueryStrings
	// calls (e.g. /search/tags issues one query per attribute map);
	// when set, the longest substring match against the incoming SQL
	// picks the row set, with the bare `strings` field acting as the
	// default fallback.
	stringsBySQL map[string][]string
	// samplesBySQL lets tests stub multiple back-to-back Query calls
	// against different SQL shapes (e.g. /api/metrics/query_range
	// issues one matrix-shape query and one exemplars-shape query); the
	// first substring match against the incoming SQL picks the row set,
	// with the bare `samples` field acting as the default fallback.
	samplesBySQL map[string][]chclient.Sample
	// queriedSQLs records every SQL string Query was invoked with, in
	// arrival order. Lets tests assert that BOTH the matrix and the
	// exemplars queries actually fired.
	queriedSQLs []string
}

func (s *stubQuerier) Query(_ context.Context, sql string, args ...any) ([]chclient.Sample, error) {
	s.lastSQL = sql
	s.lastArgs = args
	s.queriedSQLs = append(s.queriedSQLs, sql)
	if s.err != nil {
		return nil, s.err
	}
	for needle, rows := range s.samplesBySQL {
		if strings.Contains(sql, needle) {
			return rows, nil
		}
	}
	return s.samples, nil
}

func (s *stubQuerier) QueryStrings(_ context.Context, sql string, args ...any) ([]string, error) {
	s.lastSQL = sql
	s.lastArgs = args
	if s.err != nil {
		return nil, s.err
	}
	for needle, rows := range s.stringsBySQL {
		if strings.Contains(sql, needle) {
			return rows, nil
		}
	}
	return s.strings, nil
}

func newServer(q tempo.Querier, version string) *httptest.Server {
	h := tempo.New(q, schema.DefaultOTelTraces(), version, nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	return httptest.NewServer(mux)
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func TestEcho(t *testing.T) {
	t.Parallel()
	srv := newServer(&stubQuerier{}, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/echo")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK || body != "echo" {
		t.Fatalf("status=%d body=%q want 200 \"echo\"", resp.StatusCode, body)
	}
}

func TestVersion(t *testing.T) {
	t.Parallel()
	srv := newServer(&stubQuerier{}, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/status/version")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var v tempo.VersionResponse
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v.Version != "v1.0.0-test" || v.GoVersion == "" {
		t.Fatalf("unexpected version body: %+v", v)
	}
}

func TestSearch_Empty(t *testing.T) {
	t.Parallel()
	srv := newServer(&stubQuerier{}, "v1.0.0-test")
	t.Cleanup(srv.Close)

	// Grafana datasource health-check sometimes hits /api/search with no q.
	resp, err := http.Get(srv.URL + "/api/search")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var sr tempo.SearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(sr.Traces) != 0 {
		t.Fatalf("expected empty traces, got %d", len(sr.Traces))
	}
}

func TestSearch_Query(t *testing.T) {
	t.Parallel()
	// hex-TraceID smuggled through the wrap projection via the reserved
	// __cerberus_traceID label key — toTraceSummaries reads it out so
	// the response surfaces real per-trace identity (32 hex chars)
	// rather than the legacy SpanName|Timestamp synthetic key.
	const wantTraceID = "0123456789abcdef0123456789abcdef"
	q := &stubQuerier{
		samples: []chclient.Sample{
			{
				MetricName: "GET /api/users",
				Labels: map[string]string{
					"service.name":       "frontend",
					"__cerberus_traceID": wantTraceID,
				},
				Timestamp: time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC),
				Value:     150_000_000, // 150ms in nanoseconds
			},
		},
	}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/search?q=%7B%20resource.service.name%20%3D%20%22frontend%22%20%7D")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	var sr tempo.SearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(sr.Traces) != 1 {
		t.Fatalf("expected 1 trace, got %d", len(sr.Traces))
	}
	if sr.Traces[0].TraceID != wantTraceID {
		t.Errorf("TraceID: got %q, want %q (32 hex chars from the search projection)",
			sr.Traces[0].TraceID, wantTraceID)
	}
	if len(sr.Traces[0].TraceID) != 32 {
		t.Errorf("TraceID length: got %d, want 32 (hex-encoded 16-byte trace id)",
			len(sr.Traces[0].TraceID))
	}
	if !isHexLower(sr.Traces[0].TraceID) {
		t.Errorf("TraceID format: got %q, want lowercase hex", sr.Traces[0].TraceID)
	}
	if sr.Traces[0].RootServiceName != "frontend" {
		t.Errorf("expected frontend service, got %q", sr.Traces[0].RootServiceName)
	}
	if sr.Traces[0].DurationMs != 150 {
		t.Errorf("expected 150ms, got %d", sr.Traces[0].DurationMs)
	}
}

// TestSearch_GroupsByTraceID asserts that multiple span rows sharing a
// real TraceID collapse into a single per-trace summary; this is the
// core behaviour change behind switching from the synthetic
// (SpanName | Timestamp) key to the real hex(TraceId).
func TestSearch_GroupsByTraceID(t *testing.T) {
	t.Parallel()
	const traceA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const traceB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	q := &stubQuerier{
		samples: []chclient.Sample{
			// Two spans on trace A — should collapse into one summary;
			// DurationMs reflects the max span duration; StartTimeUnixNano
			// the earliest span timestamp.
			{
				MetricName: "POST /api/orders",
				Labels: map[string]string{
					"service.name":       "checkout",
					"__cerberus_traceID": traceA,
				},
				Timestamp: time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC),
				Value:     200_000_000,
			},
			{
				MetricName: "db.query",
				Labels: map[string]string{
					"service.name":       "checkout",
					"__cerberus_traceID": traceA,
				},
				Timestamp: time.Date(2026, 5, 12, 10, 0, 1, 0, time.UTC),
				Value:     50_000_000,
			},
			// One span on trace B — separate summary.
			{
				MetricName: "GET /healthz",
				Labels: map[string]string{
					"service.name":       "frontend",
					"__cerberus_traceID": traceB,
				},
				Timestamp: time.Date(2026, 5, 12, 10, 0, 5, 0, time.UTC),
				Value:     5_000_000,
			},
		},
	}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/search?q=%7B%7D")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	var sr tempo.SearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(sr.Traces) != 2 {
		t.Fatalf("expected 2 distinct traces (grouped by real TraceID), got %d", len(sr.Traces))
	}
	// Results sort ascending by TraceID.
	if sr.Traces[0].TraceID != traceA {
		t.Errorf("[0] TraceID: got %q, want %q", sr.Traces[0].TraceID, traceA)
	}
	if sr.Traces[1].TraceID != traceB {
		t.Errorf("[1] TraceID: got %q, want %q", sr.Traces[1].TraceID, traceB)
	}
	// DurationMs is the max of {200, 50}ms = 200ms across trace A's two spans.
	if sr.Traces[0].DurationMs != 200 {
		t.Errorf("[0] DurationMs: got %d, want 200 (max across grouped spans)",
			sr.Traces[0].DurationMs)
	}
}

// isHexLower reports whether s is a non-empty lowercase hex string.
// The OTel CH exporter writes TraceId via hex.EncodeToString, which
// produces lowercase a-f digits.
func isHexLower(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

func TestTraceByID_NotFound(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{samples: nil}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/traces/abc123")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", resp.StatusCode)
	}
	var er tempo.ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !er.Error || er.TraceID != "abc123" {
		t.Fatalf("unexpected error body: %+v", er)
	}
}

func TestTraceByID_Found(t *testing.T) {
	t.Parallel()
	q := &stubQuerier{
		samples: []chclient.Sample{
			{
				MetricName: "GET /api/users",
				Labels:     map[string]string{"service.name": "frontend"},
				Timestamp:  time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC),
				Value:      150_000_000,
			},
		},
	}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/traces/abc123")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var tr tempo.TraceByIDResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(tr.Batches) != 1 {
		t.Fatalf("expected 1 batch, got %d", len(tr.Batches))
	}
	if len(tr.Batches[0].Spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(tr.Batches[0].Spans))
	}
}

// TestResponseHeaders_EngineInstrumentation covers the Tempo head's
// response-header contract: /api/search returns Strategy=native;
// /api/traces/{id} returns Strategy=trace-by-id (engine.Meta.IsTraceByID
// short-circuits the optimizer and tags the response).
func TestResponseHeaders_EngineInstrumentation(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{
		samples: []chclient.Sample{
			{
				MetricName: "GET /api/users",
				Labels:     map[string]string{"service.name": "frontend"},
				Timestamp:  time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC),
				Value:      150_000_000,
			},
		},
	}
	srv := newServer(q, "v1.0.0-test")
	t.Cleanup(srv.Close)

	t.Run("search_native", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/api/search?q=%7B%20resource.service.name%20%3D%20%22frontend%22%20%7D")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		if got := resp.Header.Get("X-Cerberus-Strategy"); got != "native" {
			t.Errorf("X-Cerberus-Strategy: got %q, want native", got)
		}
		if got := resp.Header.Get("X-Cerberus-Plan-Nodes"); got == "" {
			t.Errorf("X-Cerberus-Plan-Nodes: missing")
		}
		if got := resp.Header.Get("X-Cerberus-CH-Millis"); got == "" {
			t.Errorf("X-Cerberus-CH-Millis: missing")
		}
	})

	t.Run("traceByID_short_circuit", func(t *testing.T) {
		resp, err := http.Get(srv.URL + "/api/traces/abc123")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		if got := resp.Header.Get("X-Cerberus-Strategy"); got != "trace-by-id" {
			t.Errorf("X-Cerberus-Strategy: got %q, want trace-by-id", got)
		}
		if got := resp.Header.Get("X-Cerberus-Plan-Nodes"); got == "" {
			t.Errorf("X-Cerberus-Plan-Nodes: missing")
		}
		if got := resp.Header.Get("X-Cerberus-CH-Millis"); got == "" {
			t.Errorf("X-Cerberus-CH-Millis: missing")
		}
	})
}
