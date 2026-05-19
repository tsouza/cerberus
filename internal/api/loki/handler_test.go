package loki_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
)

// stubQuerier is a shared in-package test fixture for the loki HTTP
// handlers. A single stub is sometimes wired into a server that fans a
// request out across parallel subtests (see TestConformance_LokiLabelValuesWire
// and TestConformance_LokiSeriesWire), so every field accessed inside a
// Query* method is guarded by mu. Tests that read lastSQL / lastArgs
// after a request returns must do so via LastSQL() / LastArgs() so the
// race detector sees the happens-before edge.
type stubQuerier struct {
	mu       sync.Mutex
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

	// /patterns canned (Timestamp, Body) tuples; drain consumes them
	// as the peek-window training set.
	tsLines    []chclient.TimestampedLine
	tsLinesErr error
}

func (s *stubQuerier) Query(_ context.Context, sql string, args ...any) ([]chclient.Sample, error) {
	s.mu.Lock()
	s.lastSQL = sql
	s.lastArgs = args
	samples, err := s.samples, s.err
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return samples, nil
}

func (s *stubQuerier) QueryIndexStats(_ context.Context, sql string, args ...any) (chclient.IndexStatsRow, error) {
	s.mu.Lock()
	s.lastSQL = sql
	s.lastArgs = args
	row, err := s.statsRow, s.statsErr
	s.mu.Unlock()
	if err != nil {
		return chclient.IndexStatsRow{}, err
	}
	return row, nil
}

func (s *stubQuerier) QueryIndexVolume(_ context.Context, sql string, args ...any) ([]chclient.IndexVolumeRow, error) {
	s.mu.Lock()
	s.lastSQL = sql
	s.lastArgs = args
	rows, err := s.volumeRows, s.volumeErr
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return rows, nil
}

func (s *stubQuerier) QueryStrings(_ context.Context, sql string, args ...any) ([]string, error) {
	s.mu.Lock()
	s.lastSQL = sql
	s.lastArgs = args
	rows, err := s.stringRows, s.stringsErr
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return rows, nil
}

func (s *stubQuerier) QueryTimestampedLines(_ context.Context, sql string, args ...any) ([]chclient.TimestampedLine, error) {
	s.mu.Lock()
	s.lastSQL = sql
	s.lastArgs = args
	rows, err := s.tsLines, s.tsLinesErr
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return rows, nil
}

func (s *stubQuerier) QueryLabelSets(_ context.Context, sql string, args ...any) ([]map[string]string, error) {
	s.mu.Lock()
	s.lastSQL = sql
	s.lastArgs = args
	rows, err := s.labelSets, s.labelSetsErr
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// LastSQL returns the SQL passed to the most recent Query* call. Locked
// because writers run on HTTP-handler goroutines and readers run on the
// test goroutine after the request returns; the race detector needs the
// explicit happens-before that the mutex provides.
func (s *stubQuerier) LastSQL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSQL
}

// LastArgs returns the positional args of the most recent Query* call.
// Returns the slice header by value (the underlying backing array is
// not mutated after a Query* call returns, so callers may iterate it
// without further locking).
func (s *stubQuerier) LastArgs() []any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastArgs
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

// TestResponseHeaders_EngineInstrumentation covers the Loki head's
// response-header contract: every successful /query response carries the
// three canonical X-Cerberus-* headers populated by engine.Engine.
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

// TestQueryRange_PushesStartEndToSQL pins the wire-format contract that
// the URL `start` / `end` parameters reach the engine and produce a
// Timestamp BETWEEN predicate in the emitted SQL. The bug this guards
// against is the pre-fix behaviour where the handler parsed those params
// for response shaping (matrix step-grid bucketing) but never threaded
// them through to the lowering — so the emitted SQL returned every
// matching log row regardless of the requested window.
func TestQueryRange_PushesStartEndToSQL(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	// Window: 2026-05-14T12:03:20Z → 2026-05-14T12:05:00Z (Unix seconds).
	// 1778760000 = 2026-05-14T12:00:00Z; +200s = 1778760200; +300s = 1778760300.
	const startSec = 1778760200
	const endSec = 1778760300
	url := srv.URL + `/loki/api/v1/query_range?query=%7Bservice_name%3D%22api%22%7D` +
		`&start=` + strconv.Itoa(startSec) + `&end=` + strconv.Itoa(endSec) + `&step=10`
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	lastSQL := q.LastSQL()
	lastArgs := q.LastArgs()
	if !strings.Contains(lastSQL, "`Timestamp` >=") || !strings.Contains(lastSQL, "`Timestamp` <=") {
		t.Fatalf("expected Timestamp BETWEEN predicate in SQL; got: %s", lastSQL)
	}
	// The bound is rendered as toDateTime64('YYYY-MM-DD HH:MM:SS.fffffffff', 9)
	// so both bound strings must appear as positional args.
	const wantStart = "2026-05-14 12:03:20.000000000"
	const wantEnd = "2026-05-14 12:05:00.000000000"
	if !containsArg(lastArgs, wantStart) {
		t.Errorf("expected args to contain start bound %q; got: %#v", wantStart, lastArgs)
	}
	if !containsArg(lastArgs, wantEnd) {
		t.Errorf("expected args to contain end bound %q; got: %#v", wantEnd, lastArgs)
	}
}

// TestQuery_PushesInstantWindowToSQL pins the same contract on the
// instant `/query` path. The handler collapses a single `time` param
// into a [time - 5m, time] envelope (per Loki's instant-lookback
// convention) so the emitted SQL doesn't pull every matching row in
// the table.
func TestQuery_PushesInstantWindowToSQL(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	// time = 2026-05-14T12:05:00Z; window = [12:00:00, 12:05:00].
	const tsSec = 1778760300
	url := srv.URL + `/loki/api/v1/query?query=%7Bservice_name%3D%22api%22%7D&time=` + strconv.Itoa(tsSec)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	lastSQL := q.LastSQL()
	lastArgs := q.LastArgs()
	if !strings.Contains(lastSQL, "`Timestamp` >=") || !strings.Contains(lastSQL, "`Timestamp` <=") {
		t.Fatalf("expected Timestamp BETWEEN predicate in SQL; got: %s", lastSQL)
	}
	const wantStart = "2026-05-14 12:00:00.000000000"
	const wantEnd = "2026-05-14 12:05:00.000000000"
	if !containsArg(lastArgs, wantStart) {
		t.Errorf("expected args to contain start bound %q; got: %#v", wantStart, lastArgs)
	}
	if !containsArg(lastArgs, wantEnd) {
		t.Errorf("expected args to contain end bound %q; got: %#v", wantEnd, lastArgs)
	}
}

// containsArg reports whether any positional arg equals the given
// string. Helper for asserting bound rendering without coupling to
// arg ordering (the chsql emitter may reorder predicates by sort-key
// rank).
func containsArg(args []any, want string) bool {
	for _, a := range args {
		if s, ok := a.(string); ok && s == want {
			return true
		}
	}
	return false
}

// TestQuery_Streams_DropsOTelDottedLabels — when the CH row carries
// both the OTel-form `service.name` AND the Loki-form `service_name`
// (the canonical wire-format key), the Stream envelope cerberus
// returns must only surface the normalised underscore form. Without
// the filter the response is a superset of what reference Loki emits,
// and the loki-compat differential harness rejects the row with
// `streams[0] labels differ: ... actual=map[... service.name:tempo
// service_name:tempo]`. The seeder in PR #525 intentionally writes
// both forms into ResourceAttributes so dotted-form stream-selectors
// continue to match at the WHERE layer; only the OUTPUT envelope
// needs to drop the redundant sibling.
func TestQuery_Streams_DropsOTelDottedLabels(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	q := &stubQuerier{
		samples: []chclient.Sample{
			{
				MetricName: "row 1",
				Labels: map[string]string{
					"service.name":   "tempo",
					"service_name":   "tempo",
					"k8s.pod.name":   "pod-0",
					"k8s_pod_name":   "pod-0",
					"detected_level": "info",
					// Dotted key with NO underscore sibling — must pass through
					// (the only signal that a sibling exists in the map is the
					// underscore form, so without it we can't tell whether the
					// dot is OTel-form or a legitimately dotted name).
					"orphan.key": "value",
				},
				Timestamp: ts,
			},
		},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + `/loki/api/v1/query?query=%7Bservice_name%3D%22tempo%22%7D`)
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
		t.Fatalf("resultType=%q, want streams", parsed.Data.ResultType)
	}
	raw, _ := json.Marshal(parsed.Data.Result)
	var streams []loki.Stream
	if err := json.Unmarshal(raw, &streams); err != nil {
		t.Fatalf("decode streams: %v", err)
	}
	if len(streams) != 1 {
		t.Fatalf("expected 1 stream, got %d: %+v", len(streams), streams)
	}
	got := streams[0].Stream
	if _, ok := got["service.name"]; ok {
		t.Errorf("expected service.name to be DROPPED from output (sibling service_name present); got %v", got)
	}
	if got["service_name"] != "tempo" {
		t.Errorf("expected service_name=tempo to be PRESERVED; got %v", got)
	}
	if _, ok := got["k8s.pod.name"]; ok {
		t.Errorf("expected k8s.pod.name to be DROPPED (sibling k8s_pod_name present); got %v", got)
	}
	if got["k8s_pod_name"] != "pod-0" {
		t.Errorf("expected k8s_pod_name=pod-0 to be PRESERVED; got %v", got)
	}
	if got["detected_level"] != "info" {
		t.Errorf("expected detected_level=info to be PRESERVED (non-dotted key); got %v", got)
	}
	if got["orphan.key"] != "value" {
		t.Errorf("expected orphan.key=value to be PRESERVED (no underscore sibling); got %v", got)
	}
}

// TestQuery_Streams_OTelFilterDoesNotAffectSelector — the dotted-form
// filter applies strictly to the OUTPUT label map. LogQL itself
// restricts label identifiers to [a-zA-Z_][a-zA-Z0-9_]*, so dotted
// selectors are syntactic 400s at the parser layer (not a behavior
// the filter could change). The harness seeder writes both
// `service.name` and `service_name` into ResourceAttributes so the
// canonical underscore-form selector still resolves through the
// Map[..] subscript at the CH layer. This test pins that path.
func TestQuery_Streams_OTelFilterDoesNotAffectSelector(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + `/loki/api/v1/query?query=%7Bservice_name%3D%22tempo%22%7D`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if got := q.LastSQL(); !strings.Contains(got, "ResourceAttributes") {
		t.Errorf("expected WHERE clause to project against ResourceAttributes; got SQL: %q", got)
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
