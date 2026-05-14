package loki_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
)

type stubQuerier struct {
	samples  []chclient.Sample
	err      error
	lastSQL  string
	lastArgs []any

	// /index/stats canned response (zero value is fine for the
	// existing /query / /query_range tests that never call it).
	statsRow chclient.IndexStatsRow
	statsErr error

	// /index/volume canned response.
	volumeRows []chclient.IndexVolumeRow
	volumeErr  error

	// /labels, /label/{name}/values, /detected_fields share a
	// single-column string-row result shape; reuse the same channel.
	stringRows []string
	stringsErr error

	// /series canned label-set rows.
	labelSets    []map[string]string
	labelSetsErr error
}

func (s *stubQuerier) Query(_ context.Context, sql string, args ...any) ([]chclient.Sample, error) {
	s.lastSQL = sql
	s.lastArgs = args
	if s.err != nil {
		return nil, s.err
	}
	return s.samples, nil
}

func (s *stubQuerier) QueryIndexStats(_ context.Context, sql string, args ...any) (chclient.IndexStatsRow, error) {
	s.lastSQL = sql
	s.lastArgs = args
	if s.statsErr != nil {
		return chclient.IndexStatsRow{}, s.statsErr
	}
	return s.statsRow, nil
}

func (s *stubQuerier) QueryIndexVolume(_ context.Context, sql string, args ...any) ([]chclient.IndexVolumeRow, error) {
	s.lastSQL = sql
	s.lastArgs = args
	if s.volumeErr != nil {
		return nil, s.volumeErr
	}
	return s.volumeRows, nil
}

func (s *stubQuerier) QueryStrings(_ context.Context, sql string, args ...any) ([]string, error) {
	s.lastSQL = sql
	s.lastArgs = args
	if s.stringsErr != nil {
		return nil, s.stringsErr
	}
	return s.stringRows, nil
}

func (s *stubQuerier) QueryLabelSets(_ context.Context, sql string, args ...any) ([]map[string]string, error) {
	s.lastSQL = sql
	s.lastArgs = args
	if s.labelSetsErr != nil {
		return nil, s.labelSetsErr
	}
	return s.labelSets, nil
}

func newServer(q loki.Querier) *httptest.Server {
	h := loki.New(q, schema.DefaultOTelLogs(), nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	return httptest.NewServer(mux)
}

// queryResponse mirrors loki.Response for tests that need access to the
// QueryData shape.
type queryResponse struct {
	Status string         `json:"status"`
	Data   loki.QueryData `json:"data"`
	Error  string         `json:"error"`
}

// TestQuery_Streams covers the raw-log query path: a `{job="api"}`
// selector returns a "streams" result with the log lines.
func TestQuery_Streams(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	q := &stubQuerier{
		samples: []chclient.Sample{
			// MetricName is hijacked to carry the log-line body for stream
			// queries (see wrapWithLogSampleProjection).
			{MetricName: "request started", Labels: map[string]string{"job": "api"}, Timestamp: ts},
			{MetricName: "request done", Labels: map[string]string{"job": "api"}, Timestamp: ts.Add(time.Second)},
		},
	}

	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + `/loki/api/v1/query?query=%7Bjob%3D%22api%22%7D`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	var parsed queryResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if parsed.Data.ResultType != "streams" {
		t.Fatalf("resultType: got %q, want streams", parsed.Data.ResultType)
	}
	raw, _ := json.Marshal(parsed.Data.Result)
	var streams []loki.Stream
	if err := json.Unmarshal(raw, &streams); err != nil {
		t.Fatalf("decode streams: %v", err)
	}
	if len(streams) != 1 {
		t.Fatalf("expected 1 stream, got %d", len(streams))
	}
	if len(streams[0].Values) != 2 {
		t.Fatalf("expected 2 values in stream, got %d", len(streams[0].Values))
	}
}

// TestQuery_MetricVector covers the metric-form query path: rate(...)
// returns a "vector" result.
func TestQuery_MetricVector(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	q := &stubQuerier{
		samples: []chclient.Sample{
			{Labels: map[string]string{}, Timestamp: ts, Value: 0.5},
		},
	}

	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + `/loki/api/v1/query?query=rate(%7Bjob%3D%22api%22%7D%5B5m%5D)`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	var parsed queryResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if parsed.Data.ResultType != "vector" {
		t.Fatalf("resultType: got %q, want vector", parsed.Data.ResultType)
	}
}

// TestResponseHeaders_EngineInstrumentation covers the R7.7 contract on
// the Loki head: every successful /query response carries the three
// canonical X-Cerberus-* headers populated by engine.Engine.
func TestResponseHeaders_EngineInstrumentation(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	q := &stubQuerier{
		samples: []chclient.Sample{
			{MetricName: "hello", Labels: map[string]string{"job": "api"}, Timestamp: ts},
		},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + `/loki/api/v1/query?query=%7Bjob%3D%22api%22%7D`)
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
}

// TestQueryRange_BadInput covers the validation contract on
// /loki/api/v1/query_range.
func TestQueryRange_BadInput(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		url  string
	}{
		{"missing start", `/loki/api/v1/query_range?query=%7Bjob%3D%22api%22%7D&end=1717999200&step=60`},
		{"missing end", `/loki/api/v1/query_range?query=%7Bjob%3D%22api%22%7D&start=1717995600&step=60`},
		{"end before start", `/loki/api/v1/query_range?query=%7Bjob%3D%22api%22%7D&start=1717999200&end=1717995600&step=60`},
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
				t.Fatalf("expected 400, got %d", resp.StatusCode)
			}
		})
	}
}
