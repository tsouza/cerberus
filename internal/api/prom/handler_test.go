package prom_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
)

type stubQuerier struct {
	samples      []chclient.Sample
	strings      []string
	labelSets    []map[string]string
	metaRows     []chclient.MetricMetaRow
	exemplarRows []chclient.ExemplarRow
	err          error
	exemplarsErr error
	lastSQL      string
	lastArgs     []any
	// allSQL records every SQL statement issued through this stub, in
	// order. lastSQL only keeps the final statement; the batched metadata
	// endpoints issue several (per table group / per metadata arm), so a
	// scan-bound assertion that must hold for EVERY arm reads allSQL.
	allSQL []string

	// metaCalls records every QueryMetricMeta invocation in order so
	// metadata tests can assert the per-table fan-out (which SQL was
	// issued under which reported metric type).
	metaCalls []metaCall
	// metaRowsFn, when set, answers each QueryMetricMeta call from the
	// recorded call instead of the static metaRows slice — lets a test
	// return different rows per fan-out arm.
	metaRowsFn func(call metaCall) []chclient.MetricMetaRow
}

// metaCall is one recorded QueryMetricMeta invocation.
type metaCall struct {
	sql  string
	kind string
	args []any
}

func (s *stubQuerier) Query(_ context.Context, sql string, args ...any) ([]chclient.Sample, error) {
	s.lastSQL = sql
	s.allSQL = append(s.allSQL, sql)
	s.lastArgs = args
	if s.err != nil {
		return nil, s.err
	}
	return s.samples, nil
}

func (s *stubQuerier) QueryCursor(_ context.Context, sql string, args ...any) (chclient.Cursor, error) {
	s.lastSQL = sql
	s.allSQL = append(s.allSQL, sql)
	s.lastArgs = args
	if s.err != nil {
		return nil, s.err
	}
	return newSliceCursor(s.samples), nil
}

func (s *stubQuerier) QueryStrings(_ context.Context, sql string, args ...any) ([]string, error) {
	s.lastSQL = sql
	s.allSQL = append(s.allSQL, sql)
	s.lastArgs = args
	if s.err != nil {
		return nil, s.err
	}
	return s.strings, nil
}

func (s *stubQuerier) QueryLabelSets(_ context.Context, sql string, args ...any) ([]map[string]string, error) {
	s.lastSQL = sql
	s.allSQL = append(s.allSQL, sql)
	s.lastArgs = args
	if s.err != nil {
		return nil, s.err
	}
	return s.labelSets, nil
}

func (s *stubQuerier) QueryMetricMeta(_ context.Context, sql, metricType string, args ...any) ([]chclient.MetricMetaRow, error) {
	s.lastSQL = sql
	s.allSQL = append(s.allSQL, sql)
	s.lastArgs = args
	call := metaCall{sql: sql, kind: metricType, args: args}
	s.metaCalls = append(s.metaCalls, call)
	if s.err != nil {
		return nil, s.err
	}
	if s.metaRowsFn != nil {
		return s.metaRowsFn(call), nil
	}
	return s.metaRows, nil
}

func (s *stubQuerier) QueryExemplars(_ context.Context, sql string, args ...any) ([]chclient.ExemplarRow, error) {
	s.lastSQL = sql
	s.lastArgs = args
	if s.exemplarsErr != nil {
		return nil, s.exemplarsErr
	}
	if s.err != nil {
		return nil, s.err
	}
	return s.exemplarRows, nil
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
	// name as an arg. The bare-selector path appends LWR + eval-ts /
	// staleness predicates after the MetricName equality, so "up" lands
	// somewhere in lastArgs rather than at the tail.
	if !strings.Contains(q.lastSQL, "otel_metrics_gauge") {
		t.Errorf("expected SQL to mention otel_metrics_gauge; got %q", q.lastSQL)
	}
	foundUp := false
	for _, a := range q.lastArgs {
		if a == "up" {
			foundUp = true
			break
		}
	}
	if !foundUp {
		t.Errorf("expected %q among bound args, got %v", "up", q.lastArgs)
	}
}

func TestQueryRange_Matrix(t *testing.T) {
	t.Parallel()

	// Post Pool-AK rework: matrix pivot is a row → sample copy keyed
	// by canonical series. The SQL handed back is responsible for the
	// per-step LWR (bare selector path) or per-anchor windowing
	// (matrix RangeWindow path) — the pivot no longer iterates the
	// step grid or carries values forward. Stub three rows at distinct
	// step anchors and assert the matrix surfaces them verbatim, in
	// timestamp order, with no carry-forward of stale values.
	start := time.Unix(1717995600, 0).UTC()
	end := start.Add(4 * time.Minute) // 1717995840
	q := &stubQuerier{
		samples: []chclient.Sample{
			{MetricName: "up", Labels: map[string]string{"job": "api"}, Timestamp: start, Value: 1.0},
			{MetricName: "up", Labels: map[string]string{"job": "api"}, Timestamp: start.Add(2 * time.Minute), Value: 2.0},
			{MetricName: "up", Labels: map[string]string{"job": "api"}, Timestamp: start.Add(3 * time.Minute), Value: 3.0},
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
	if got := len(matrix[0].Values); got != 3 {
		t.Fatalf("expected 3 sample points (one per stubbed row), got %d: %+v",
			got, matrix[0].Values)
	}
	// Values appear in the order the SQL returned them — the rows the
	// stub yields land at start + (0, 2, 3) minutes with values
	// (1, 2, 3) — verbatim.
	wantValues := []string{"1", "2", "3"}
	for i, want := range wantValues {
		if got := matrix[0].Values[i][1]; got != want {
			t.Errorf("row %d: got value %q, want %q", i, got, want)
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

// TestQueryRange_ResolutionCap pins the upstream-Prometheus resolution
// cap on /api/v1/query_range: a (start, end, step) grid that exceeds
// 11,000 points per timeseries is rejected with 400 bad_data and the
// exact upstream message (web/api/v1.queryRange in
// prometheus/prometheus), while a grid at exactly 11,000 points is
// accepted. The scalar fast-path (`1+1`) must be capped too — upstream
// runs the check before the engine is consulted.
func TestQueryRange_ResolutionCap(t *testing.T) {
	t.Parallel()

	const capMsg = "exceeded maximum resolution of 11,000 points per timeseries. Try decreasing the query resolution (?step=XX)"

	// step=60s: 11,000 points span 660,000s; one extra step exceeds.
	const (
		start    = 1717995600
		step     = 60
		endAtCap = start + 11000*step // (end-start)/step == 11000 → allowed
		endOver  = endAtCap + step    // (end-start)/step == 11001 → rejected
	)

	rejected := []struct {
		name  string
		query string
	}{
		{"selector over cap", "up"},
		{"scalar fast-path over cap", "1%2B1"},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := &stubQuerier{}
			srv := newServer(q)
			t.Cleanup(srv.Close)

			url := fmt.Sprintf("%s/api/v1/query_range?query=%s&start=%d&end=%d&step=%d",
				srv.URL, tc.query, start, endOver, step)
			resp, err := http.Get(url)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			body := readBody(t, resp)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status: got %d, want 400; body=%s", resp.StatusCode, body)
			}
			var parsed prom.Response
			if err := json.Unmarshal([]byte(body), &parsed); err != nil {
				t.Fatalf("unmarshal: %v\nbody=%s", err, body)
			}
			if parsed.Status != "error" || parsed.ErrorType != prom.ErrBadData {
				t.Fatalf("got status=%q errorType=%q, want error/%s", parsed.Status, parsed.ErrorType, prom.ErrBadData)
			}
			if parsed.Error != capMsg {
				t.Fatalf("error message: got %q, want %q", parsed.Error, capMsg)
			}
			// The cap must fire before any ClickHouse round-trip.
			if q.lastSQL != "" {
				t.Errorf("over-cap query_range reached CH: lastSQL=%q", q.lastSQL)
			}
		})
	}

	t.Run("exactly 11000 points passes", func(t *testing.T) {
		t.Parallel()
		srv := newServer(&stubQuerier{})
		t.Cleanup(srv.Close)

		url := fmt.Sprintf("%s/api/v1/query_range?query=up&start=%d&end=%d&step=%d",
			srv.URL, start, endAtCap, step)
		resp, err := http.Get(url)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status: got %d, want 200; body=%s", resp.StatusCode, body)
		}
		var parsed queryResponse
		if err := json.Unmarshal([]byte(body), &parsed); err != nil {
			t.Fatalf("unmarshal: %v\nbody=%s", err, body)
		}
		if parsed.Status != "success" {
			t.Fatalf("status: got %q, want success; err=%s", parsed.Status, parsed.Error)
		}
		if parsed.Data.ResultType != "matrix" {
			t.Fatalf("resultType: got %q, want matrix", parsed.Data.ResultType)
		}
	})
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

// Inspected returns the count of Next() calls that returned true (idx
// clamped to the slice length, since idx overshoots by one once Next
// returns false). Mirrors the production cursor's drain count.
func (c *sliceCursor) Inspected() int64 {
	if c.idx > len(c.samples) {
		return int64(len(c.samples))
	}
	return int64(c.idx)
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

// TestQuery_StringLiteral pins the resultType "string" wire shape for
// a top-level string literal on /api/v1/query — reference Prometheus
// answers `"a string literal"` with [<ts>, <value>] (the
// rejection-parity layer proved the old 422 was a wrong rejection).
// No ClickHouse round-trip happens (the stub records no SQL).
func TestQuery_StringLiteral(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/query?query=" + url.QueryEscape(`"a string literal"`) + "&time=1717999200")
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
	if parsed.Data.ResultType != "string" {
		t.Fatalf("resultType: got %q, want string", parsed.Data.ResultType)
	}
	raw, _ := json.Marshal(parsed.Data.Result)
	var pair []any
	if err := json.Unmarshal(raw, &pair); err != nil {
		t.Fatalf("decode string result: %v", err)
	}
	if len(pair) != 2 || pair[1] != "a string literal" {
		t.Fatalf("string result = %v, want [<ts>, \"a string literal\"]", pair)
	}
	if q.lastSQL != "" {
		t.Fatalf("string literal must not reach ClickHouse; got SQL %q", q.lastSQL)
	}
}

// TestQuery_TopLevelMatrixSelector pins the resultType "matrix" pivot
// for `up[5m]` on /api/v1/query: every returned sample lands at its own
// timestamp, grouped per series — the upstream wire shape for a
// top-level range-vector selector.
func TestQuery_TopLevelMatrixSelector(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 5, 11, 11, 58, 0, 0, time.UTC)
	q := &stubQuerier{
		samples: []chclient.Sample{
			{MetricName: "up", Labels: map[string]string{"job": "api"}, Timestamp: base, Value: 1},
			{MetricName: "up", Labels: map[string]string{"job": "api"}, Timestamp: base.Add(time.Minute), Value: 2},
		},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/query?query=" + url.QueryEscape(`up[5m]`) + "&time=1717999200")
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
	if parsed.Data.ResultType != "matrix" {
		t.Fatalf("resultType: got %q, want matrix; body=%s", parsed.Data.ResultType, body)
	}
	raw, _ := json.Marshal(parsed.Data.Result)
	var mat []prom.MatrixSample
	if err := json.Unmarshal(raw, &mat); err != nil {
		t.Fatalf("decode matrix: %v", err)
	}
	if len(mat) != 1 || len(mat[0].Values) != 2 {
		t.Fatalf("matrix shape: got %d series / %v values, want 1 series with 2 values", len(mat), mat)
	}
}

// TestQueryRange_RejectsNonVectorTypes pins the upstream expression
// type gate on /api/v1/query_range: matrix- and string-typed
// expressions are 400 bad_data ("must be Scalar or instant Vector"),
// matching web/api/v1's invalidExprError — NOT a 2xx with rows and NOT
// a lowering 422.
func TestQueryRange_RejectsNonVectorTypes(t *testing.T) {
	t.Parallel()

	srv := newServer(&stubQuerier{})
	t.Cleanup(srv.Close)

	for _, query := range []string{`up[5m]`, `up[5m:1m]`, `"a string"`} {
		resp, err := http.Get(srv.URL + "/api/v1/query_range?query=" + url.QueryEscape(query) +
			"&start=1717999200&end=1717999800&step=60")
		if err != nil {
			t.Fatalf("GET(%q): %v", query, err)
		}
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("query %q: status=%d, want 400; body=%s", query, resp.StatusCode, body)
		}
		if !strings.Contains(body, "must be Scalar or instant Vector") {
			t.Fatalf("query %q: body %q missing the upstream type-gate message", query, body)
		}
	}
}
