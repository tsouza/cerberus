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

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}
