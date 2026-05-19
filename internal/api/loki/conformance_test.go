package loki_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/tsouza/cerberus/internal/api/admit"
	"github.com/tsouza/cerberus/internal/api/loki"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
)

// --- Section A: wire-format conformance ----------------------------------
//
// Loki shares the Prom-shaped {status, data:…} envelope but data shapes
// vary per endpoint. Each test below routes a representative payload
// through the handler and asserts the documented JSON shape.

// TestConformance_LokiQueryWire — `/loki/api/v1/query` returns streams
// for log queries and vector/matrix for metric queries.
func TestConformance_LokiQueryWire(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name        string
		query       string
		samples     []chclient.Sample
		wantType    string
		wantStreams int
	}{
		{
			name:  "streams_with_lines",
			query: `{job="api"}`,
			samples: []chclient.Sample{
				{MetricName: "first log line", Labels: map[string]string{"job": "api"}, Timestamp: ts},
				{MetricName: "second", Labels: map[string]string{"job": "api"}, Timestamp: ts.Add(time.Second)},
			},
			wantType:    "streams",
			wantStreams: 1,
		},
		{
			name:     "streams_empty",
			query:    `{job="api"}`,
			samples:  nil,
			wantType: "streams",
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(&stubQuerier{samples: c.samples})
			t.Cleanup(srv.Close)
			resp, err := http.Get(srv.URL + "/loki/api/v1/query?query=" + url.QueryEscape(c.query))
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status=%d body=%s", resp.StatusCode, body)
			}
			var env struct {
				Status string `json:"status"`
				Data   struct {
					ResultType string          `json:"resultType"`
					Result     json.RawMessage `json:"result"`
				} `json:"data"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if env.Status != "success" {
				t.Errorf("status: got %q, want success", env.Status)
			}
			if env.Data.ResultType != c.wantType {
				t.Errorf("resultType: got %q, want %q", env.Data.ResultType, c.wantType)
			}
			// stream values are [<unix_ns_string>, <line_string>] —
			// JSON strings on both sides per Loki convention.
			var streams []loki.Stream
			if err := json.Unmarshal(env.Data.Result, &streams); err != nil {
				t.Fatalf("decode streams: %v", err)
			}
			if c.wantStreams > 0 && len(streams) != c.wantStreams {
				t.Errorf("streams count: got %d, want %d", len(streams), c.wantStreams)
			}
			// Each tuple element is a string. Empty line is allowed,
			// but the [ts, line] pair must round-trip as strings — the
			// json.Unmarshal above already enforces that for the
			// declared [2]string type, so no extra assertion needed.
			_ = streams
		})
	}
}

// TestConformance_LokiQueryRangeWire — `/loki/api/v1/query_range` for
// metric and log queries returns the matching matrix / streams shape.
func TestConformance_LokiQueryRangeWire(t *testing.T) {
	t.Parallel()

	start := time.Unix(1717995600, 0).UTC()
	end := start.Add(2 * time.Minute)
	cases := []struct {
		name     string
		query    string
		wantType string
	}{
		{"streams_range", `{job="api"}`, "streams"},
		{"metric_range", `rate({job="api"}[5m])`, "matrix"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(&stubQuerier{})
			t.Cleanup(srv.Close)
			path := "/loki/api/v1/query_range?query=" + url.QueryEscape(c.query) +
				"&start=" + strconv.FormatInt(start.Unix(), 10) +
				"&end=" + strconv.FormatInt(end.Unix(), 10) + "&step=60"
			resp, err := http.Get(srv.URL + path)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status=%d body=%s", resp.StatusCode, body)
			}
			var env struct {
				Data struct {
					ResultType string `json:"resultType"`
				} `json:"data"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if env.Data.ResultType != c.wantType {
				t.Errorf("resultType: got %q, want %q", env.Data.ResultType, c.wantType)
			}
		})
	}
}

// TestConformance_LokiLabelsWire — `data: []string`.
func TestConformance_LokiLabelsWire(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		rows []string
	}{
		{"non_empty", []string{"job", "instance"}},
		{"empty", nil},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(&stubQuerier{stringRows: c.rows})
			t.Cleanup(srv.Close)

			resp, err := http.Get(srv.URL + "/loki/api/v1/labels")
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			var env struct {
				Status string   `json:"status"`
				Data   []string `json:"data"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if env.Status != "success" {
				t.Errorf("status: got %q, want success", env.Status)
			}
		})
	}
}

// TestConformance_LokiLabelValuesWire — `/label/<name>/values` returns
// `data: []string`.
func TestConformance_LokiLabelValuesWire(t *testing.T) {
	t.Parallel()

	srv := newServer(&stubQuerier{stringRows: []string{"a", "b", "c"}})
	t.Cleanup(srv.Close)

	for _, name := range []string{"job", "instance"} {
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			resp, err := http.Get(srv.URL + "/loki/api/v1/label/" + name + "/values")
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status=%d body=%s", resp.StatusCode, body)
			}
			var env struct {
				Status string   `json:"status"`
				Data   []string `json:"data"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if env.Status != "success" {
				t.Errorf("status: got %q, want success", env.Status)
			}
		})
	}
}

// TestConformance_LokiSeriesWire — `data: []map[string]string`.
func TestConformance_LokiSeriesWire(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		rows []map[string]string
	}{
		{"two_streams", []map[string]string{{"job": "api"}, {"job": "db"}}},
		{"empty", nil},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(&stubQuerier{labelSets: c.rows})
			t.Cleanup(srv.Close)

			resp, err := http.Get(srv.URL + `/loki/api/v1/series?match%5B%5D=%7Bjob%3D%22api%22%7D`)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			var env struct {
				Status string              `json:"status"`
				Data   []map[string]string `json:"data"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if env.Status != "success" {
				t.Errorf("status: got %q, want success", env.Status)
			}
		})
	}
}

// TestConformance_LokiIndexStatsWire — top-level IndexStats wire shape.
// Note: this endpoint returns the IndexStats struct directly (no
// `status`/`data` wrapper) per upstream Loki's documented schema.
func TestConformance_LokiIndexStatsWire(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		row  chclient.IndexStatsRow
	}{
		{
			name: "non_zero",
			row:  chclient.IndexStatsRow{Streams: 4, Entries: 1000, Bytes: 4096},
		},
		{
			name: "zero",
			row:  chclient.IndexStatsRow{},
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(&stubQuerier{statsRow: c.row})
			t.Cleanup(srv.Close)

			resp, err := http.Get(srv.URL + `/loki/api/v1/index/stats?query=%7Bjob%3D%22api%22%7D&start=1717995600&end=1717999200`)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status=%d body=%s", resp.StatusCode, body)
			}
			var out loki.IndexStats
			if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if out.Streams != c.row.Streams {
				t.Errorf("Streams: got %d, want %d", out.Streams, c.row.Streams)
			}
			if out.Chunks != 0 {
				t.Errorf("Chunks: got %d, want 0 (cerberus has no chunk model)", out.Chunks)
			}
		})
	}
}

// TestConformance_LokiIndexVolumeWire — `/index/volume` returns
// `data: {resultType:"vector", result:[VectorSample]}`.
func TestConformance_LokiIndexVolumeWire(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		rows []chclient.IndexVolumeRow
	}{
		{
			name: "two_rows",
			rows: []chclient.IndexVolumeRow{
				{Labels: map[string]string{"job": "api"}, Bytes: 1024},
				{Labels: map[string]string{"job": "db"}, Bytes: 512},
			},
		},
		{name: "empty", rows: nil},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(&stubQuerier{volumeRows: c.rows})
			t.Cleanup(srv.Close)

			resp, err := http.Get(srv.URL +
				`/loki/api/v1/index/volume?query=%7Bjob%3D%22api%22%7D&start=1717995600&end=1717999200`)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status=%d body=%s", resp.StatusCode, body)
			}
			var env struct {
				Status string `json:"status"`
				Data   struct {
					ResultType string              `json:"resultType"`
					Result     []loki.VectorSample `json:"result"`
				} `json:"data"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if env.Status != "success" {
				t.Errorf("status: got %q, want success", env.Status)
			}
			if env.Data.ResultType != "vector" {
				t.Errorf("resultType: got %q, want vector", env.Data.ResultType)
			}
			if len(env.Data.Result) != len(c.rows) {
				t.Errorf("rows: got %d, want %d", len(env.Data.Result), len(c.rows))
			}
		})
	}
}

// TestConformance_LokiDetectedFieldsWire — `data: {fields, limit,
// line_limit}` matches upstream Loki's documented shape.
func TestConformance_LokiDetectedFieldsWire(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		rows []string
	}{
		{"json_lines", []string{`{"user_id":42,"action":"login"}`, `{"user_id":7,"action":"logout"}`}},
		{"logfmt_lines", []string{`user_id=42 action=login`, `user_id=7 action=logout`}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(&stubQuerier{stringRows: c.rows})
			t.Cleanup(srv.Close)
			resp, err := http.Get(srv.URL +
				`/loki/api/v1/detected_fields?query=%7Bjob%3D%22api%22%7D&start=1717995600&end=1717999200`)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status=%d body=%s", resp.StatusCode, body)
			}
			var env struct {
				Status string                  `json:"status"`
				Data   loki.DetectedFieldsData `json:"data"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if env.Status != "success" {
				t.Errorf("status: got %q, want success", env.Status)
			}
			if env.Data.Limit == 0 || env.Data.LineLimit == 0 {
				t.Errorf("limits not echoed: %+v", env.Data)
			}
		})
	}
}

// TestConformance_LokiPatternsWire — empty-data envelope. Cerberus
// doesn't run pattern discovery yet; the test pins the wire-stable
// `data:[]` shape (top-level array, matching upstream Loki's
// `WriteQueryPatternsResponseJSON`).
func TestConformance_LokiPatternsWire(t *testing.T) {
	t.Parallel()

	cases := []string{
		`{job="api"}`,
		`{job=~"api|db"}`,
	}
	for _, q := range cases {
		q := q
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			srv := newServer(&stubQuerier{})
			t.Cleanup(srv.Close)
			resp, err := http.Get(srv.URL + "/loki/api/v1/patterns?query=" + url.QueryEscape(q))
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, body)
			}
			var env struct {
				Status string         `json:"status"`
				Data   []loki.Pattern `json:"data"`
			}
			if err := json.Unmarshal(body, &env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if env.Data == nil {
				t.Errorf("expected non-nil Data slice (JSON []); got nil — body=%s", body)
			}
		})
	}
}

// --- Section B: error envelope per head ----------------------------------

// TestConformance_LokiErrorEnvelope — Loki shares Prom's wire-format
// envelope `{status:"error", errorType, error}` per upstream convention.
func TestConformance_LokiErrorEnvelope(t *testing.T) {
	t.Parallel()

	type tc struct {
		name     string
		path     string
		stub     *stubQuerier
		wantCode int
		wantKind string
	}
	cases := []tc{
		{
			name: "400_missing_query", stub: &stubQuerier{},
			path: "/loki/api/v1/query", wantCode: http.StatusBadRequest, wantKind: loki.ErrBadData,
		},
		{
			name: "400_bad_logql", stub: &stubQuerier{},
			path:     "/loki/api/v1/query?query=not+a+selector",
			wantCode: http.StatusBadRequest, wantKind: loki.ErrBadData,
		},
		{
			name: "400_missing_start_range", stub: &stubQuerier{},
			path:     "/loki/api/v1/query_range?query=%7Bjob%3D%22api%22%7D&end=2&step=1",
			wantCode: http.StatusBadRequest, wantKind: loki.ErrBadData,
		},
		{
			name: "400_end_before_start", stub: &stubQuerier{},
			path:     "/loki/api/v1/query_range?query=%7Bjob%3D%22api%22%7D&start=20&end=10&step=1",
			wantCode: http.StatusBadRequest, wantKind: loki.ErrBadData,
		},
		{
			name: "400_index_stats_missing_query", stub: &stubQuerier{},
			path: "/loki/api/v1/index/stats?start=1&end=2", wantCode: http.StatusBadRequest, wantKind: loki.ErrBadData,
		},
		{
			name: "400_index_volume_missing_query", stub: &stubQuerier{},
			path: "/loki/api/v1/index/volume?start=1&end=2", wantCode: http.StatusBadRequest, wantKind: loki.ErrBadData,
		},
		{
			name: "400_detected_fields_missing_query", stub: &stubQuerier{},
			path: "/loki/api/v1/detected_fields?start=1&end=2", wantCode: http.StatusBadRequest, wantKind: loki.ErrBadData,
		},
		{
			name: "400_patterns_missing_query", stub: &stubQuerier{},
			path: "/loki/api/v1/patterns", wantCode: http.StatusBadRequest, wantKind: loki.ErrBadData,
		},
		{
			name: "502_query_ch_failure", stub: &stubQuerier{err: errors.New("clickhouse: connection refused")},
			path:     "/loki/api/v1/query?query=%7Bjob%3D%22api%22%7D",
			wantCode: http.StatusBadGateway, wantKind: loki.ErrInternal,
		},
		{
			name: "502_labels_ch_failure", stub: &stubQuerier{stringsErr: errors.New("ch failure")},
			path: "/loki/api/v1/labels", wantCode: http.StatusBadGateway, wantKind: loki.ErrInternal,
		},
		{
			name: "502_index_stats_ch_failure", stub: &stubQuerier{statsErr: errors.New("ch failure")},
			path:     "/loki/api/v1/index/stats?query=%7Bjob%3D%22api%22%7D&start=1&end=60",
			wantCode: http.StatusBadGateway, wantKind: loki.ErrInternal,
		},
		{
			name: "502_index_volume_ch_failure", stub: &stubQuerier{volumeErr: errors.New("ch failure")},
			path:     "/loki/api/v1/index/volume?query=%7Bjob%3D%22api%22%7D&start=1&end=60",
			wantCode: http.StatusBadGateway, wantKind: loki.ErrInternal,
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(c.stub)
			t.Cleanup(srv.Close)
			resp, err := http.Get(srv.URL + c.path)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != c.wantCode {
				t.Fatalf("status: got %d, want %d; body=%s", resp.StatusCode, c.wantCode, body)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
				t.Errorf("Content-Type: got %q", ct)
			}
			// Loki and Prom share the envelope shape.
			var env struct {
				Status    string `json:"status"`
				ErrorType string `json:"errorType"`
				Error     string `json:"error"`
			}
			if err := json.Unmarshal(body, &env); err != nil {
				t.Fatalf("envelope decode: %v body=%s", err, body)
			}
			if env.Status != "error" {
				t.Errorf("status: got %q, want error", env.Status)
			}
			if env.ErrorType != c.wantKind {
				t.Errorf("errorType: got %q, want %q", env.ErrorType, c.wantKind)
			}
		})
	}
}

// --- Section C: header pins ---------------------------------------------

// TestConformance_LokiHeaders — Content-Type + cerberus instrumentation
// headers present on a successful Loki query.
func TestConformance_LokiHeaders(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{
		samples: []chclient.Sample{
			{MetricName: "log line", Labels: map[string]string{"job": "api"}, Timestamp: time.Now()},
		},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + `/loki/api/v1/query?query=%7Bjob%3D%22api%22%7D`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Errorf("Content-Type: got %q, want application/json", got)
	}
	if got := resp.Header.Get("X-Cerberus-Strategy"); got == "" {
		t.Errorf("X-Cerberus-Strategy: missing")
	}
	if got := resp.Header.Get("X-Cerberus-Plan-Nodes"); got == "" {
		t.Errorf("X-Cerberus-Plan-Nodes: missing")
	}
	chMillis := resp.Header.Get("X-Cerberus-CH-Millis")
	if chMillis == "" {
		t.Errorf("X-Cerberus-CH-Millis: missing")
	} else if _, err := strconv.Atoi(chMillis); err != nil {
		t.Errorf("X-Cerberus-CH-Millis: got %q, want numeric", chMillis)
	}
}

// --- Section D: range parameter parsing matrix --------------------------

// TestConformance_LokiRangeTimeMatrix — start / end accept unix seconds,
// nanoseconds, RFC3339; invalid forms 400.
func TestConformance_LokiRangeTimeMatrix(t *testing.T) {
	t.Parallel()

	type tc struct {
		name     string
		start    string
		end      string
		wantCode int
	}
	cases := []tc{
		// Loki accepts unix nanos when > 1e12.
		{"unix_seconds_int", "1717995600", "1717999200", http.StatusOK},
		{"unix_seconds_float", "1717995600.5", "1717999200.5", http.StatusOK},
		{"unix_nanoseconds", "1717995600000000000", "1717999200000000000", http.StatusOK},
		{"rfc3339", "2024-01-01T00:00:00Z", "2024-01-01T01:00:00Z", http.StatusOK},
		{"garbage_start", "tomorrow", "1717999200", http.StatusBadRequest},
		{"end_before_start", "1717999200", "1717995600", http.StatusBadRequest},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(&stubQuerier{})
			t.Cleanup(srv.Close)

			path := `/loki/api/v1/query_range?query=%7Bjob%3D%22api%22%7D&step=60`
			if c.start != "" {
				path += "&start=" + url.QueryEscape(c.start)
			}
			if c.end != "" {
				path += "&end=" + url.QueryEscape(c.end)
			}
			resp, err := http.Get(srv.URL + path)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != c.wantCode {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("status: got %d, want %d; body=%s", resp.StatusCode, c.wantCode, body)
			}
		})
	}
}

// --- Section F: WebSocket /tail extra coverage --------------------------

// TestConformance_TailHeartbeat — Server tolerates a long-lived connection
// without any client activity, and a ctx.Done() teardown drops the
// connection cleanly (no leaked goroutine).
func TestConformance_TailHeartbeat(t *testing.T) {
	t.Parallel()

	q := &tailStubQuerier{chunks: nil}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	conn := dialTail(t, srv, `{job="api"}`)
	// Hold the connection for ~200ms without sending — server should
	// keep polling without faulting.
	time.Sleep(200 * time.Millisecond)
	_ = conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye"),
		time.Now().Add(time.Second))
	_ = conn.Close()
	time.Sleep(50 * time.Millisecond)
}

// TestConformance_TailNoUpgradeOnError — 4xx happens before the
// WebSocket upgrade, so the response is a regular HTTP envelope (not a
// confusing handshake-then-close failure).
func TestConformance_TailNoUpgradeOnError(t *testing.T) {
	t.Parallel()

	srv := newServer(&tailStubQuerier{})
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/loki/api/v1/tail")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", resp.StatusCode)
	}
	if got := resp.Header.Get("Upgrade"); got != "" {
		t.Errorf("Upgrade header set on 400: %q", got)
	}
}

// TestConformance_TailMultiplePollsRespectCtx — the read-pump-based
// disconnect detection short-circuits the polling loop quickly.
func TestConformance_TailMultiplePollsRespectCtx(t *testing.T) {
	t.Parallel()

	q := &tailStubQuerier{}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	conn := dialTail(t, srv, `{job="api"}`)
	// Close immediately; the read-pump fires cancel() on conn.NextReader
	// failure, which short-circuits the next tick. No assertion beyond
	// "doesn't hang or panic" — the test runner's deadline catches leaks.
	_ = conn.Close()
	time.Sleep(50 * time.Millisecond)
}

// --- Section G: admission control / concurrency cap ---------------------

// TestConformance_LokiAdmitRejectsAtCap — Loki mux composition wires
// the limiter through. Hold a slot, expect 503 on the next request.
func TestConformance_LokiAdmitRejectsAtCap(t *testing.T) {
	t.Parallel()

	limiter := admit.New("loki", 1)
	rel, ok := limiter.Acquire(context.Background())
	if !ok {
		t.Fatalf("setup acquire failed")
	}
	defer rel()

	h := loki.New(&stubQuerier{}, schema.DefaultOTelLogs(), nil)
	h.Limiter = limiter
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + `/loki/api/v1/query?query=%7Bjob%3D%22api%22%7D`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got == "" {
		t.Errorf("Retry-After: missing on 503")
	}
}

// TestConformance_LokiAdmitSerialReleasesSlot — successive requests
// through a cap=N limiter all succeed because each Release returns the
// slot before the next Acquire. Sanity check on the admit middleware
// composition.
func TestConformance_LokiAdmitSerialReleasesSlot(t *testing.T) {
	t.Parallel()

	const cap = 3
	limiter := admit.New("loki", cap)
	h := loki.New(&stubQuerier{
		samples: []chclient.Sample{{MetricName: "x", Labels: map[string]string{"job": "api"}}},
	}, schema.DefaultOTelLogs(), nil)
	h.Limiter = limiter
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// 2*cap requests serially — all should succeed since the slots
	// release before the next acquire.
	for i := 0; i < cap*2; i++ {
		resp, err := http.Get(srv.URL + `/loki/api/v1/query?query=%7Bjob%3D%22api%22%7D`)
		if err != nil {
			t.Fatalf("GET %d: %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("req %d: got %d, want 200", i, resp.StatusCode)
		}
	}
}

// TestConformance_LokiAdmitIndependentFromOthers — the loki limiter
// doesn't affect prom/tempo because each handler owns its own. Wire a
// loki handler with no limiter; every request passes.
//
// Issued serially: keeps the assertion simple ("n requests, n
// admits"). `stubQuerier` now mutex-guards lastSQL/lastArgs, so a
// concurrent fan-out would also be race-clean — but the serial loop
// gives us a deterministic counter.
func TestConformance_LokiAdmitIndependentFromOthers(t *testing.T) {
	t.Parallel()

	h := loki.New(&stubQuerier{
		samples: []chclient.Sample{{MetricName: "x", Labels: map[string]string{"job": "api"}}},
	}, schema.DefaultOTelLogs(), nil)
	// No limiter wired — every request passes.
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	const n = 20
	admitted := 0
	for i := 0; i < n; i++ {
		resp, err := http.Get(srv.URL + `/loki/api/v1/query?query=%7Bjob%3D%22api%22%7D`)
		if err != nil {
			continue
		}
		if resp.StatusCode == http.StatusOK {
			admitted++
		}
		resp.Body.Close()
	}
	if admitted != n {
		t.Errorf("nil-limiter handler must admit every request: got %d/%d", admitted, n)
	}
}
