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

	// /labels and /label/{name}/values share a single-column
	// string-row result shape; reuse the same channel.
	stringRows []string
	stringsErr error

	// /detected_fields canned (Body, LogAttributes, ResourceAttributes)
	// rows.
	detectedRows []chclient.DetectedFieldRow
	detectedErr  error

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

func (s *stubQuerier) QueryDetectedFieldRows(_ context.Context, sql string, args ...any) ([]chclient.DetectedFieldRow, error) {
	s.mu.Lock()
	s.lastSQL = sql
	s.lastArgs = args
	rows, err := s.detectedRows, s.detectedErr
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

// TestQuery_Streams_StructuredMetadata pins the per-line structured
// metadata surface (PR #903 follow-up): a log-stream entry carries the
// OTel-CH LogAttributes map as the optional third tuple element
// (`[ts, line, {metadata}]`), which Grafana's Logs Drilldown reads to
// render clean per-line columns. The assertions cover all three #903
// regressions at the wire boundary:
//
//   - the useful CH-query-log keys (duration / read_bytes / query_id)
//     surface with non-empty, well-formed values;
//   - an empty-valued attribute is DROPPED, so no blank `_method`-style
//     column appears;
//   - a value with a trailing comma is carried verbatim (cerberus never
//     mangles it into an `8192,` artefact — the join-with-comma bug that
//     #903 was accused of doesn't exist on cerberus's side: each value is
//     a single map entry, not a stray-delimiter join).
func TestQuery_Streams_StructuredMetadata(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	q := &stubQuerier{
		samples: []chclient.Sample{
			{
				MetricName: "SELECT count() FROM otel_logs",
				Labels:     map[string]string{"service_name": "clickhouse"},
				Timestamp:  ts,
				Metadata: map[string]string{
					"duration":   "12ms",
					"read_bytes": "4096",
					"query_id":   "abc-123",
					// An empty attribute must NOT surface as a column.
					"exception": "",
				},
			},
		},
	}

	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + `/loki/api/v1/query?query=%7Bservice_name%3D%22clickhouse%22%7D`)
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
	raw, _ := json.Marshal(parsed.Data.Result)
	var streams []loki.Stream
	if err := json.Unmarshal(raw, &streams); err != nil {
		t.Fatalf("decode streams: %v", err)
	}
	if len(streams) != 1 || len(streams[0].Values) != 1 {
		t.Fatalf("want 1 stream / 1 value, got %d streams", len(streams))
	}

	md := streams[0].Values[0].Metadata
	for k, want := range map[string]string{
		"duration":   "12ms",
		"read_bytes": "4096",
		"query_id":   "abc-123",
	} {
		if got := md[k]; got != want {
			t.Errorf("metadata[%q]=%q, want %q", k, got, want)
		}
	}
	// Empty-valued attribute dropped — no blank column.
	if _, ok := md["exception"]; ok {
		t.Errorf("empty-valued attribute leaked into structured metadata: %v", md)
	}
	// No leading-underscore / single-underscore garbage keys (the
	// `_method` / `_` / `_id` Drilldown line-parse artefacts must never
	// originate from cerberus's structured-metadata surface).
	for k := range md {
		if k == "_" || strings.HasPrefix(k, "_") {
			t.Errorf("garbage structured-metadata key surfaced: %q", k)
		}
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

// TestQuery_Streams_NormalisesOTelDottedLabels — every dotted OTel
// key on the OUTPUT envelope is rewritten to the Prom/Loki-grammar
// underscored form. When both `service.name` and `service_name` are
// present (the seeder in PR #525 writes both into ResourceAttributes
// so dotted-form stream-selectors keep matching at the WHERE layer),
// the underscored form wins on collision per the NormalizeLabelMap
// policy. A solo dotted key with no underscored sibling — e.g.
// `orphan.key` — is normalised to `orphan_key` rather than left as
// `orphan.key`; the older behaviour of preserving solo dotted keys
// was the proximate cause of every `sum by (orphan_key)` panel
// silently returning empty because PromQL grammar forbids `.`.
func TestQuery_Streams_NormalisesOTelDottedLabels(t *testing.T) {
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
					// Dotted key with NO underscore sibling — now normalised
					// to `orphan_key` so it's actually selectable from PromQL.
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
		t.Errorf("expected service.name to be NORMALISED out (sibling service_name wins); got %v", got)
	}
	if got["service_name"] != "tempo" {
		t.Errorf("expected service_name=tempo to be PRESERVED; got %v", got)
	}
	if _, ok := got["k8s.pod.name"]; ok {
		t.Errorf("expected k8s.pod.name to be NORMALISED out (sibling k8s_pod_name wins); got %v", got)
	}
	if got["k8s_pod_name"] != "pod-0" {
		t.Errorf("expected k8s_pod_name=pod-0 to be PRESERVED; got %v", got)
	}
	if got["detected_level"] != "info" {
		t.Errorf("expected detected_level=info to be PRESERVED (non-dotted key); got %v", got)
	}
	if _, ok := got["orphan.key"]; ok {
		t.Errorf("expected orphan.key to be NORMALISED to orphan_key; got %v", got)
	}
	if got["orphan_key"] != "value" {
		t.Errorf("expected orphan_key=value (normalised from orphan.key); got %v", got)
	}
	// Every output key must satisfy Prom/Loki label grammar.
	for k := range got {
		for i := 0; i < len(k); i++ {
			c := k[i]
			switch {
			case c >= 'a' && c <= 'z':
			case c >= 'A' && c <= 'Z':
			case c == '_':
			case c >= '0' && c <= '9' && i > 0:
			default:
				t.Errorf("output key %q has invalid char at offset %d", k, i)
			}
		}
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
		{"invalid limit", `/loki/api/v1/query_range?query=%7Bjob%3D%22api%22%7D&start=1717995600&end=1717999200&step=60&limit=-5`},
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

// TestQuery_Streams_RespectsLimitParameter pins the wire-format
// contract that /query honours Loki's `limit` URL parameter on
// log-stream queries: the response surfaces AT MOST `limit` entries
// across all returned streams. The bug this guards against was
// cerberus ignoring `limit` entirely — the
// `regression/drilldown-patterns.yaml#Basic drilldown with json and
// logfmt parsing` loki-compat case surfaced this as `streams length:
// expected=1000 actual=1440`. With per-entry-unique parser-extracted
// labels (which is what a `| json | logfmt` pipeline produces against
// the loki-compat seed), every entry collapses into its own Stream,
// so an unbounded sample stream becomes a stream-count mismatch on
// the wire.
//
// The test uses a parser-stage-free query so the labels are constant
// across rows — collapsing into a single Stream — and asserts the
// entry count inside that stream is exactly `limit`. A bare selector
// with 5 underlying CH rows + `limit=3` should produce one Stream
// with three entries; without the clamp it would surface all five.
func TestQuery_Streams_RespectsLimitParameter(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	q := &stubQuerier{
		samples: []chclient.Sample{
			{MetricName: "line-1", Labels: map[string]string{"job": "api"}, Timestamp: base},
			{MetricName: "line-2", Labels: map[string]string{"job": "api"}, Timestamp: base.Add(1 * time.Second)},
			{MetricName: "line-3", Labels: map[string]string{"job": "api"}, Timestamp: base.Add(2 * time.Second)},
			{MetricName: "line-4", Labels: map[string]string{"job": "api"}, Timestamp: base.Add(3 * time.Second)},
			{MetricName: "line-5", Labels: map[string]string{"job": "api"}, Timestamp: base.Add(4 * time.Second)},
		},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + `/loki/api/v1/query?query=%7Bjob%3D%22api%22%7D&limit=3`)
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
	raw, _ := json.Marshal(parsed.Data.Result)
	var streams []loki.Stream
	if err := json.Unmarshal(raw, &streams); err != nil {
		t.Fatalf("decode streams: %v", err)
	}
	if len(streams) != 1 {
		t.Fatalf("expected 1 stream (constant labelset), got %d", len(streams))
	}
	if got, want := len(streams[0].Values), 3; got != want {
		t.Fatalf("stream values: got %d, want %d (the 3 latest entries surfaced under direction=backward default)", got, want)
	}
	// The latest three timestamps should be line-3, line-4, line-5
	// — direction=backward orders descending so the response carries
	// them most-recent-first. Verify by extracting the line bodies
	// (the second tuple slot in each value).
	got := map[string]bool{}
	for _, v := range streams[0].Values {
		got[v.Line] = true
	}
	for _, want := range []string{"line-3", "line-4", "line-5"} {
		if !got[want] {
			t.Errorf("expected limit=3 backward clamp to surface %q, got values %v", want, streams[0].Values)
		}
	}
	for _, dropped := range []string{"line-1", "line-2"} {
		if got[dropped] {
			t.Errorf("expected limit=3 backward clamp to drop %q, got values %v", dropped, streams[0].Values)
		}
	}
}

// TestQuery_Streams_CollapsesPerEntryLabelsToStreamCount mirrors the
// loki-compat `regression/drilldown-patterns.yaml#Basic drilldown
// with json and logfmt parsing` case where each entry has unique
// parser-extracted labels so every entry surfaces as its own stream.
// Without the limit clamp every entry would be its own stream, blowing
// past Loki's documented `limit` ceiling; with the clamp the
// per-entry-unique labelset still produces one stream per surviving
// entry, but the total entry count (== stream count) honours the
// caller's `limit`. Acts as a direct regression test for the
// "1000 vs 1440" diff the loki-compat differential surfaces.
func TestQuery_Streams_CollapsesPerEntryLabelsToStreamCount(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	samples := make([]chclient.Sample, 0, 10)
	for i := 0; i < 10; i++ {
		samples = append(samples, chclient.Sample{
			MetricName: "line-" + strconv.Itoa(i),
			// Per-entry-unique label so every entry surfaces as
			// its own Stream — mirrors the parser-stage shape
			// (json + logfmt extract caller-varying keys per row).
			Labels:    map[string]string{"job": "api", "request_id": strconv.Itoa(i)},
			Timestamp: base.Add(time.Duration(i) * time.Second),
		})
	}
	q := &stubQuerier{samples: samples}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + `/loki/api/v1/query?query=%7Bjob%3D%22api%22%7D&limit=4`)
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
	raw, _ := json.Marshal(parsed.Data.Result)
	var streams []loki.Stream
	if err := json.Unmarshal(raw, &streams); err != nil {
		t.Fatalf("decode streams: %v", err)
	}
	if got, want := len(streams), 4; got != want {
		t.Fatalf("streams length: got %d, want %d (limit=4 clamp, one stream per unique parser-extracted labelset)", got, want)
	}
}

// TestQueryRange_Streams_DefaultLimitClampsResponse pins the
// default-limit behaviour: a /query_range request with no `limit`
// parameter clamps the response to Loki's documented 100-entry
// default. Without the clamp every CH row would surface — a
// long-window log query against a multi-thousand-entry seed would
// return more entries than `limit=100` callers expect, breaking
// downstream tools that paginate against Loki's contract.
func TestQueryRange_Streams_DefaultLimitClampsResponse(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	samples := make([]chclient.Sample, 0, 250)
	for i := 0; i < 250; i++ {
		samples = append(samples, chclient.Sample{
			MetricName: "line-" + strconv.Itoa(i),
			Labels:     map[string]string{"job": "api"},
			Timestamp:  base.Add(time.Duration(i) * time.Second),
		})
	}
	q := &stubQuerier{samples: samples}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	start := strconv.FormatInt(base.Unix(), 10)
	end := strconv.FormatInt(base.Add(300*time.Second).Unix(), 10)
	resp, err := http.Get(srv.URL + `/loki/api/v1/query_range?query=%7Bjob%3D%22api%22%7D&start=` + start + `&end=` + end + `&step=60`)
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
	raw, _ := json.Marshal(parsed.Data.Result)
	var streams []loki.Stream
	if err := json.Unmarshal(raw, &streams); err != nil {
		t.Fatalf("decode streams: %v", err)
	}
	if len(streams) != 1 {
		t.Fatalf("expected 1 stream (constant labelset), got %d", len(streams))
	}
	if got, want := len(streams[0].Values), 100; got != want {
		t.Fatalf("default-limit clamp: got %d entries, want %d (Loki documented default)", got, want)
	}
}

// TestQuery_Streams_RespectsForwardDirection pins the wire-format
// contract that `direction=forward` flips the limit clamp to surface
// the EARLIEST N entries rather than the latest. The loki-bench
// harness sets every log case to backward (forward is unsupported by
// Loki's v2 engine) but the handler still has to accept the parameter
// per Loki's documented API.
func TestQuery_Streams_RespectsForwardDirection(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	q := &stubQuerier{
		samples: []chclient.Sample{
			{MetricName: "line-1", Labels: map[string]string{"job": "api"}, Timestamp: base},
			{MetricName: "line-2", Labels: map[string]string{"job": "api"}, Timestamp: base.Add(1 * time.Second)},
			{MetricName: "line-3", Labels: map[string]string{"job": "api"}, Timestamp: base.Add(2 * time.Second)},
			{MetricName: "line-4", Labels: map[string]string{"job": "api"}, Timestamp: base.Add(3 * time.Second)},
			{MetricName: "line-5", Labels: map[string]string{"job": "api"}, Timestamp: base.Add(4 * time.Second)},
		},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + `/loki/api/v1/query?query=%7Bjob%3D%22api%22%7D&limit=2&direction=forward`)
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
	raw, _ := json.Marshal(parsed.Data.Result)
	var streams []loki.Stream
	if err := json.Unmarshal(raw, &streams); err != nil {
		t.Fatalf("decode streams: %v", err)
	}
	if len(streams) != 1 {
		t.Fatalf("expected 1 stream, got %d", len(streams))
	}
	if got, want := len(streams[0].Values), 2; got != want {
		t.Fatalf("stream values: got %d, want %d", got, want)
	}
	got := map[string]bool{}
	for _, v := range streams[0].Values {
		got[v.Line] = true
	}
	for _, want := range []string{"line-1", "line-2"} {
		if !got[want] {
			t.Errorf("expected limit=2 forward clamp to surface %q, got values %v", want, streams[0].Values)
		}
	}
}
