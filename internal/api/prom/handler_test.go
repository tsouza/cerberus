package prom_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
)

type stubQuerier struct {
	samples   []chclient.Sample
	strings   []string
	labelSets []map[string]string
	metaRows  []chclient.MetricMetaRow
	err       error
	lastSQL   string
	lastArgs  []any
}

func (s *stubQuerier) Query(_ context.Context, sql string, args ...any) ([]chclient.Sample, error) {
	s.lastSQL = sql
	s.lastArgs = args
	if s.err != nil {
		return nil, s.err
	}
	return s.samples, nil
}

func (s *stubQuerier) QueryCursor(_ context.Context, sql string, args ...any) (chclient.Cursor, error) {
	s.lastSQL = sql
	s.lastArgs = args
	if s.err != nil {
		return nil, s.err
	}
	return newSliceCursor(s.samples), nil
}

func (s *stubQuerier) QueryStrings(_ context.Context, sql string, args ...any) ([]string, error) {
	s.lastSQL = sql
	s.lastArgs = args
	if s.err != nil {
		return nil, s.err
	}
	return s.strings, nil
}

func (s *stubQuerier) QueryLabelSets(_ context.Context, sql string, args ...any) ([]map[string]string, error) {
	s.lastSQL = sql
	s.lastArgs = args
	if s.err != nil {
		return nil, s.err
	}
	return s.labelSets, nil
}

func (s *stubQuerier) QueryMetricMeta(_ context.Context, sql, _ string, args ...any) ([]chclient.MetricMetaRow, error) {
	s.lastSQL = sql
	s.lastArgs = args
	if s.err != nil {
		return nil, s.err
	}
	return s.metaRows, nil
}

func newServer(q prom.Querier) *httptest.Server {
	h := prom.New(q, schema.DefaultOTelMetrics(), nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	return httptest.NewServer(mux)
}

// queryResponse mirrors prom.Response for tests that need access to the
// QueryData shape (ResultType + Result). Since prom.Response.Data is now
// `any` to accommodate metadata endpoints, tests decode into this typed
// shape directly.
type queryResponse struct {
	Status    string         `json:"status"`
	Data      prom.QueryData `json:"data"`
	ErrorType string         `json:"errorType"`
	Error     string         `json:"error"`
}

func TestQuery_Vector(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	q := &stubQuerier{
		samples: []chclient.Sample{
			{MetricName: "up", Labels: map[string]string{"job": "api"}, Timestamp: ts, Value: 1.0},
			{MetricName: "up", Labels: map[string]string{"job": "db"}, Timestamp: ts, Value: 0.0},
		},
	}

	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/query?query=up&time=1717999200")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}

	var parsed queryResponse
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("unmarshal: %v\nbody=%s", err, body)
	}
	if parsed.Status != "success" {
		t.Fatalf("status: got %q, want success; err=%s", parsed.Status, parsed.Error)
	}
	if parsed.Data.ResultType != "vector" {
		t.Fatalf("resultType: got %q, want vector", parsed.Data.ResultType)
	}

	// Re-marshal then unmarshal the result into the typed shape.
	rawResult, _ := json.Marshal(parsed.Data.Result)
	var vec []prom.VectorSample
	if err := json.Unmarshal(rawResult, &vec); err != nil {
		t.Fatalf("decode vector: %v", err)
	}
	if len(vec) != 2 {
		t.Fatalf("expected 2 series, got %d", len(vec))
	}
	for _, v := range vec {
		if v.Metric["__name__"] != "up" {
			t.Errorf("missing __name__ in %+v", v.Metric)
		}
		if _, ok := v.Metric["job"]; !ok {
			t.Errorf("missing job label in %+v", v.Metric)
		}
	}

	// The lowered SQL should reference the gauge table and bind the metric
	// name as an arg.
	if !strings.Contains(q.lastSQL, "otel_metrics_gauge") {
		t.Errorf("expected SQL to mention otel_metrics_gauge; got %q", q.lastSQL)
	}
	if len(q.lastArgs) == 0 || q.lastArgs[len(q.lastArgs)-1] != "up" {
		t.Errorf("expected last arg %q, got %v", "up", q.lastArgs)
	}
}

func TestQueryRange_Matrix(t *testing.T) {
	t.Parallel()

	// Three samples spaced 30s apart in the requested [start, end] window;
	// step=60s should produce 5 evaluation points (start, +60, +120, +180, end).
	start := time.Unix(1717995600, 0).UTC()
	end := start.Add(4 * time.Minute) // 1717995840
	q := &stubQuerier{
		samples: []chclient.Sample{
			{MetricName: "up", Labels: map[string]string{"job": "api"}, Timestamp: start, Value: 1.0},
			{MetricName: "up", Labels: map[string]string{"job": "api"}, Timestamp: start.Add(90 * time.Second), Value: 2.0},
			{MetricName: "up", Labels: map[string]string{"job": "api"}, Timestamp: start.Add(180 * time.Second), Value: 3.0},
		},
	}

	srv := newServer(q)
	t.Cleanup(srv.Close)

	url := fmt.Sprintf("%s/api/v1/query_range?query=up&start=%d&end=%d&step=60",
		srv.URL, start.Unix(), end.Unix())
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}

	var parsed queryResponse
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed.Data.ResultType != "matrix" {
		t.Fatalf("resultType: got %q, want matrix", parsed.Data.ResultType)
	}

	rawResult, _ := json.Marshal(parsed.Data.Result)
	var matrix []prom.MatrixSample
	if err := json.Unmarshal(rawResult, &matrix); err != nil {
		t.Fatalf("decode matrix: %v", err)
	}
	if len(matrix) != 1 {
		t.Fatalf("expected 1 series, got %d", len(matrix))
	}
	// 5 step points expected for [start..start+4m] step=60s.
	if got := len(matrix[0].Values); got != 5 {
		t.Fatalf("expected 5 sample points in matrix, got %d: %+v", got, matrix[0].Values)
	}
	// Latest-at-step values: [1, 1, 2, 3, 3] for steps [0, 60, 120, 180, 240].
	wantValues := []string{"1", "1", "2", "3", "3"}
	for i, want := range wantValues {
		if got := matrix[0].Values[i][1]; got != want {
			t.Errorf("step %d: got value %q, want %q", i, got, want)
		}
	}
}

func TestQueryRange_BadInput(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		url  string
	}{
		{"missing start", "/api/v1/query_range?query=up&end=1717999200&step=60"},
		{"missing end", "/api/v1/query_range?query=up&start=1717995600&step=60"},
		{"missing step", "/api/v1/query_range?query=up&start=1717995600&end=1717999200"},
		{"end before start", "/api/v1/query_range?query=up&start=1717999200&end=1717995600&step=60"},
		{"zero step", "/api/v1/query_range?query=up&start=1717995600&end=1717999200&step=0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(&stubQuerier{})
			t.Cleanup(srv.Close)
			resp, err := http.Get(srv.URL + tc.url)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d body=%s", resp.StatusCode, readBody(t, resp))
			}
		})
	}
}

func TestQuery_BadInput(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		url    string
		status int
		errKey string
	}{
		{"missing query", "/api/v1/query", http.StatusBadRequest, prom.ErrBadData},
		{"bad time", "/api/v1/query?query=up&time=tomorrow", http.StatusBadRequest, prom.ErrBadData},
		{"invalid promql", "/api/v1/query?query=up%20%2B", http.StatusBadRequest, prom.ErrBadData},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(&stubQuerier{})
			t.Cleanup(srv.Close)
			resp, err := http.Get(srv.URL + tc.url)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			body := readBody(t, resp)
			if resp.StatusCode != tc.status {
				t.Fatalf("status: got %d, want %d; body=%s", resp.StatusCode, tc.status, body)
			}
			var parsed prom.Response
			if err := json.Unmarshal([]byte(body), &parsed); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if parsed.Status != "error" || parsed.ErrorType != tc.errKey {
				t.Fatalf("got status=%q errorType=%q, want error/%s", parsed.Status, parsed.ErrorType, tc.errKey)
			}
		})
	}
}

func TestResponseHeaders_PromVersionAndCHMillis(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{
		samples: []chclient.Sample{
			{MetricName: "up", Labels: map[string]string{"job": "api"}, Value: 1.0},
		},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/query?query=up")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("X-Prometheus-API-Version"); got != "v1" {
		t.Errorf("X-Prometheus-API-Version: got %q, want v1", got)
	}
	if got := resp.Header.Get("X-Cerberus-CH-Millis"); got == "" {
		t.Errorf("X-Cerberus-CH-Millis: missing")
	}
	if got := resp.Header.Get("X-Cerberus-Strategy"); got != "native" {
		t.Errorf("X-Cerberus-Strategy: got %q, want native", got)
	}
	if got := resp.Header.Get("X-Cerberus-Plan-Nodes"); got == "" {
		t.Errorf("X-Cerberus-Plan-Nodes: missing")
	}
}

// TestQuery_ScalarFold — Grafana's `?query=1+1` health probe. The fold
// runs in Go and short-circuits CH; the stub Querier must never see
// the query.
func TestQuery_ScalarFold(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/query?query=1%2B1")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}

	var parsed queryResponse
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("unmarshal: %v\nbody=%s", err, body)
	}
	if parsed.Status != "success" {
		t.Fatalf("status: got %q, want success; err=%s", parsed.Status, parsed.Error)
	}
	if parsed.Data.ResultType != "scalar" {
		t.Fatalf("resultType: got %q, want scalar", parsed.Data.ResultType)
	}

	// Result is [<ts_float>, "<value_string>"]; verify the folded value.
	rawResult, _ := json.Marshal(parsed.Data.Result)
	var point [2]any
	if err := json.Unmarshal(rawResult, &point); err != nil {
		t.Fatalf("decode scalar: %v", err)
	}
	if got := point[1]; got != "2" {
		t.Errorf("folded value: got %v, want \"2\"", got)
	}

	// Crucially, the CH stub must NOT have been invoked.
	if q.lastSQL != "" {
		t.Errorf("scalar fold reached CH: lastSQL=%q", q.lastSQL)
	}
}

// TestFormatQuery — Grafana's query-editor "Format query" button.
// Round-trips the input through Prom's parser; output is the
// pretty-printed canonical form.
func TestFormatQuery(t *testing.T) {
	t.Parallel()

	srv := newServer(&stubQuerier{})
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/format_query?query=up%2Bup")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}

	var parsed struct {
		Status string `json:"status"`
		Data   string `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("unmarshal: %v\nbody=%s", err, body)
	}
	if parsed.Status != "success" {
		t.Fatalf("status: got %q, want success", parsed.Status)
	}
	if !strings.Contains(parsed.Data, "up") {
		t.Errorf("expected formatted query to contain `up`; got %q", parsed.Data)
	}
}

// TestParseQuery — `/api/v1/parse_query` returns the AST type +
// stringified node. Minimal shape; enough for Grafana's inline
// syntax check.
func TestParseQuery(t *testing.T) {
	t.Parallel()

	srv := newServer(&stubQuerier{})
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/parse_query?query=up")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}

	var parsed struct {
		Status string `json:"status"`
		Data   struct {
			Type string `json:"type"`
			Node string `json:"node"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("unmarshal: %v\nbody=%s", err, body)
	}
	if parsed.Status != "success" {
		t.Fatalf("status: got %q, want success", parsed.Status)
	}
	if parsed.Data.Type == "" {
		t.Errorf("expected non-empty Type; got %q", parsed.Data.Type)
	}
	if parsed.Data.Node != "up" {
		t.Errorf("expected Node=`up`; got %q", parsed.Data.Node)
	}
}

// TestFormatQuery_BadQuery — invalid PromQL returns 400 bad_data.
func TestFormatQuery_BadQuery(t *testing.T) {
	t.Parallel()

	srv := newServer(&stubQuerier{})
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/format_query?query=up%20%2B")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", resp.StatusCode, readBody(t, resp))
	}
}

func TestQuery_UpstreamError(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{err: errors.New("clickhouse: connection refused")}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/query?query=up")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d body=%s", resp.StatusCode, readBody(t, resp))
	}
}

// BenchmarkExecuteRangeStreaming exercises the streaming path against a
// 100k-sample synthetic result set spread across 100 series and reports
// allocated bytes per request. The cursor variant should keep the peak
// resident slice (the master []chclient.Sample copy) out of the
// allocation profile.
func BenchmarkExecuteRangeStreaming(b *testing.B) {
	const (
		seriesCount     = 100
		samplesPerSerie = 1000
	)
	start := time.Unix(1717995600, 0).UTC()
	step := time.Second
	end := start.Add(time.Duration(samplesPerSerie-1) * step)

	samples := make([]chclient.Sample, 0, seriesCount*samplesPerSerie)
	for i := 0; i < seriesCount; i++ {
		labels := map[string]string{"job": "api", "instance": fmt.Sprintf("host-%d", i)}
		for j := 0; j < samplesPerSerie; j++ {
			samples = append(samples, chclient.Sample{
				MetricName: "up",
				Labels:     labels,
				Timestamp:  start.Add(time.Duration(j) * step),
				Value:      float64(j),
			})
		}
	}

	q := &stubQuerier{samples: samples}
	srv := newServer(q)
	b.Cleanup(srv.Close)
	url := fmt.Sprintf("%s/api/v1/query_range?query=up&start=%d&end=%d&step=1",
		srv.URL, start.Unix(), end.Unix())

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := http.Get(url)
		if err != nil {
			b.Fatalf("GET: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
}

// sliceCursor is the in-memory chclient.Cursor used by stubQuerier so
// tests don't need a live ClickHouse for /api/v1/query_range. Mirrors
// the production cursor's lifecycle: Next advances, Sample yields the
// current row, Err reports any saved error, Close is idempotent.
type sliceCursor struct {
	samples []chclient.Sample
	idx     int
	cur     chclient.Sample
	closed  bool
}

func newSliceCursor(samples []chclient.Sample) *sliceCursor {
	return &sliceCursor{samples: samples, idx: -1}
}

func (c *sliceCursor) Next() bool {
	c.idx++
	if c.idx >= len(c.samples) {
		return false
	}
	c.cur = c.samples[c.idx]
	return true
}

func (c *sliceCursor) Sample() chclient.Sample { return c.cur }
func (c *sliceCursor) Err() error              { return nil }

func (c *sliceCursor) Close() error {
	c.closed = true
	return nil
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}
