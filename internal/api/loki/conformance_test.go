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

// TestConformance_LokiQueryConstantArithmetic_HealthProbe pins the Grafana
// Loki CheckHealth probe shape — `vector(1)+vector(1)` (and friends) —
// against the regression where the metric-branch wire-wrap blindly
// ColumnRef'd `ResourceAttributes` over a top-level VectorJoin output.
// ClickHouse returned `code: 47 Unknown expression identifier
// 'ResourceAttributes'`, which Grafana surfaced as a red "Unable to
// connect with Loki" banner on every page load.
//
// The fix extends `isVectorAggregateSampleShape` in internal/logql/binary.go
// to also recognise VectorJoin (its emitter projects under the canonical
// `Attributes` alias). Pin all four arithmetic ops + a literal-vector mix
// so a future refactor of the helper can't silently drop the branch.
func TestConformance_LokiQueryConstantArithmetic_HealthProbe(t *testing.T) {
	t.Parallel()

	cases := []string{
		`vector(1)+vector(1)`,
		`vector(1)-vector(1)`,
		`vector(2)*vector(3)`,
		`vector(8)/vector(2)`,
	}
	for _, q := range cases {
		q := q
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			srv := newServer(&stubQuerier{})
			t.Cleanup(srv.Close)
			resp, err := http.Get(srv.URL + "/loki/api/v1/query?query=" + url.QueryEscape(q))
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s — Grafana's Loki CheckHealth "+
					"probe lands here; a non-200 surfaces as 'Unable to "+
					"connect with Loki' on every Grafana page load",
					resp.StatusCode, string(body))
			}
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
					ResultType string            `json:"resultType"`
					Result     []json.RawMessage `json:"result"`
				} `json:"data"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if env.Data.ResultType != c.wantType {
				t.Errorf("resultType: got %q, want %q", env.Data.ResultType, c.wantType)
			}
			// `result` must serialise as the empty JSON array []
			// (not null) when stubQuerier returns no samples — the
			// wire contract guarantees the key is always present so
			// upstream Loki clients can iterate without nil-checking.
			if env.Data.Result == nil {
				t.Errorf("result: got null, want empty JSON array []")
			}
			if len(env.Data.Result) != 0 {
				t.Errorf("result: got %d items, want 0 (stubQuerier returns no samples)", len(env.Data.Result))
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

// TestConformance_LokiDetectedFieldsWire pins the BARE top-level wire
// shape upstream Loki serializes for /loki/api/v1/detected_fields —
// `{"fields":[{label,type,cardinality,parsers,jsonPath}],"limit":N}`
// with NO {status, data} envelope (upstream's
// WriteDetectedFieldsResponseJSON writes logproto.DetectedFieldsResponse
// verbatim; see pkg/util/marshal/marshal.go). The decode below reads
// `body.fields` exactly as Grafana's Logs Drilldown does — the
// post-incident doctrine for the "Fields: 0" bug class: a 200 with an
// enveloped body is invisible to status-code oracles, so the test IS
// the consumer.
func TestConformance_LokiDetectedFieldsWire(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		rows        []chclient.DetectedFieldRow
		wantParsers []string
	}{
		{
			"json_lines",
			[]chclient.DetectedFieldRow{
				{Line: `{"user_id":42,"action":"login"}`},
				{Line: `{"user_id":7,"action":"logout"}`},
			},
			[]string{"json"},
		},
		{
			"logfmt_lines",
			[]chclient.DetectedFieldRow{
				{Line: `user_id=42 action=login`},
				{Line: `user_id=7 action=logout`},
			},
			[]string{"logfmt"},
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(&stubQuerier{detectedRows: c.rows})
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
			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}

			// Regression pin: the {status, data} query-API envelope is
			// GONE. Grafana reads body.fields directly — an enveloped
			// 200 renders every Logs Drilldown service page with
			// "Fields: 0".
			var top map[string]json.RawMessage
			if err := json.Unmarshal(raw, &top); err != nil {
				t.Fatalf("decode top-level object: %v", err)
			}
			if _, ok := top["status"]; ok {
				t.Fatalf("response carries the query-API envelope (top-level \"status\"); upstream serializes DetectedFieldsResponse bare: %s", raw)
			}
			if _, ok := top["data"]; ok {
				t.Fatalf("response carries the query-API envelope (top-level \"data\"); upstream serializes DetectedFieldsResponse bare: %s", raw)
			}
			if _, ok := top["fields"]; !ok {
				t.Fatalf("top-level \"fields\" missing — the consumer decodes body.fields: %s", raw)
			}

			// Consumer decode: exactly what the Logs Drilldown app /
			// Loki datasource frontend reads.
			var body struct {
				Fields []loki.DetectedField `json:"fields"`
				Limit  uint32               `json:"limit"`
			}
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatalf("decode as consumer: %v", err)
			}
			if len(body.Fields) == 0 {
				t.Fatalf("fields empty — Logs Drilldown would render \"Fields: 0\": %s", raw)
			}
			if body.Limit == 0 {
				t.Errorf("limit not echoed alongside non-empty fields: %s", raw)
			}
			for _, f := range body.Fields {
				if f.Label == "" {
					t.Errorf("field with empty label: %s", raw)
				}
				if f.Type == "" {
					t.Errorf("field %q with empty type: %s", f.Label, raw)
				}
				if f.Cardinality == 0 {
					t.Errorf("field %q with zero cardinality: %s", f.Label, raw)
				}
				if len(f.Parsers) != len(c.wantParsers) || f.Parsers[0] != c.wantParsers[0] {
					t.Errorf("field %q parsers=%v want %v", f.Label, f.Parsers, c.wantParsers)
				}
			}
		})
	}
}

// TestConformance_LokiDetectedFieldsWire_StructuredMetadataParsersNull
// — fields derived from structured metadata only (LogAttributes, no
// line parse) serialize `"parsers":null`: upstream's logproto JSON tag
// has NO omitempty on parsers, and the upstream handler nils the slice
// out when no parser produced the field.
func TestConformance_LokiDetectedFieldsWire_StructuredMetadataParsersNull(t *testing.T) {
	t.Parallel()

	srv := newServer(&stubQuerier{detectedRows: []chclient.DetectedFieldRow{
		{Line: "plain unparseable text", Attributes: map[string]string{"detected_level": "warn"}},
	}})
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL +
		`/loki/api/v1/detected_fields?query=%7Bjob%3D%22api%22%7D&start=1717995600&end=1717999200`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var body struct {
		Fields []struct {
			Label   string          `json:"label"`
			Parsers json.RawMessage `json:"parsers"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Fields) != 1 || body.Fields[0].Label != "detected_level" {
		t.Fatalf("want exactly the detected_level field, got: %s", raw)
	}
	if got := strings.TrimSpace(string(body.Fields[0].Parsers)); got != "null" {
		t.Errorf("structured-metadata field parsers=%s want null on the wire", got)
	}
}

// TestConformance_LokiDetectedLabelsWire pins the
// `{"detectedLabels":[{"label":"...","cardinality":N}, ...]}` envelope
// Grafana 11.2+ expects from /loki/api/v1/detected_labels. Mirrors the
// detected_fields wire test above so a future schema tweak surfaces here
// rather than only in Grafana's UI.
func TestConformance_LokiDetectedLabelsWire(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{
		labelSets: []map[string]string{
			{"job": "api", "instance": "host-1"},
			{"job": "api", "instance": "host-2"},
		},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL +
		`/loki/api/v1/detected_labels?query=%7Bjob%3D%22api%22%7D&start=1717995600&end=1717999200`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var env loki.DetectedLabelsData
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.DetectedLabels) != 2 {
		t.Fatalf("got %d labels, want 2: %+v", len(env.DetectedLabels), env.DetectedLabels)
	}
	for _, dl := range env.DetectedLabels {
		if dl.Label == "" {
			t.Errorf("empty label name in entry %+v", dl)
		}
		if dl.Cardinality == 0 {
			t.Errorf("zero cardinality for label %q", dl.Label)
		}
	}
}

// TestConformance_LokiPatternsBasic — non-empty-data envelope. With
// the drain wire-up live (PR #517), feeding canned (Timestamp, Body)
// rows through the stub Querier produces a real cluster on the
// response. This conformance test pins the wire shape Grafana decodes:
//
//   - `data` is a top-level JSON array (NOT `{patterns:[...]}` — that
//     legacy wrapper was dropped in #514 to match upstream Loki's
//     `WriteQueryPatternsResponseJSON`),
//   - each element decodes into `loki.Pattern{Pattern, Level, Samples}`,
//   - each `samples[i]` is a 2-tuple `[unix_seconds, count]`. The first
//     slot is unix SECONDS (not ms/ns) per upstream's
//     `sample.Timestamp.Unix()` projection.
//
// The drain-internals assertions (cluster count, template content) live
// in patterns_test.go; this test focuses on the wire envelope alone so
// a future tokeniser tweak in drain doesn't churn the conformance pin.
func TestConformance_LokiPatternsBasic(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	// 5 canned (Timestamp, Body) rows feeding the stub Querier — drain
	// folds them into >=1 clusters when the handler trains on them.
	// Drain rejects lines with fewer than 4 tokens, so each body
	// includes at least four space-separated chunks (path, status,
	// latency suffix) to satisfy the minimum.
	q := &stubQuerier{
		tsLines: []chclient.TimestampedLine{
			{Timestamp: base, Body: "GET /api/foo/1 status=200 latency=5ms"},
			{Timestamp: base.Add(1 * time.Second), Body: "GET /api/foo/2 status=200 latency=7ms"},
			{Timestamp: base.Add(2 * time.Second), Body: "GET /api/foo/3 status=200 latency=4ms"},
			{Timestamp: base.Add(3 * time.Second), Body: "GET /api/foo/4 status=200 latency=11ms"},
			{Timestamp: base.Add(4 * time.Second), Body: "GET /api/foo/5 status=200 latency=9ms"},
		},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL +
		`/loki/api/v1/patterns?query=%7Bjob%3D%22api%22%7D&start=1778760000&end=1778763600`)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}

	// Decode the envelope. The crucial pin is that `data` is a top-level
	// array, not a struct — a `{patterns:[...]}` wrapper would fail this
	// decode because the field shape no longer matches.
	var env struct {
		Status string         `json:"status"`
		Data   []loki.Pattern `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode: %v body=%s", err, body)
	}
	if env.Status != "success" {
		t.Errorf("status: got %q, want success", env.Status)
	}
	if env.Data == nil {
		t.Fatalf("expected non-nil Data slice (JSON []); got nil — body=%s", body)
	}
	if len(env.Data) < 1 {
		t.Fatalf("expected >=1 cluster after 5 trained lines; got %d — body=%s",
			len(env.Data), body)
	}
	// Per-element pins: each Pattern decodes with the
	// {Pattern, Level, Samples} shape (#514) and each Samples element is
	// a 2-tuple [unix_seconds, count]. We pick a window such that every
	// sample timestamp falls in unix-seconds range — 2026-05-14T12:00:00Z
	// = 1778760000, so a sample at +/-1d is well within the second-scale
	// range that distinguishes from ms (+10^3) / ns (+10^9).
	const baseUnix = int64(1778760000)
	for i, p := range env.Data {
		if p.Pattern == "" {
			t.Errorf("env.Data[%d].Pattern is empty", i)
		}
		// Level is "" in this slice (PR B emits empty level — see
		// patterns.go doc + plan §5).
		if p.Level != "" {
			t.Errorf("env.Data[%d].Level=%q want empty (PR B does not bucket by level)", i, p.Level)
		}
		for j, s := range p.Samples {
			// unix_seconds for 2026-05-14T12:00:00Z is ~1.78e9. unix_ms
			// would be ~1.78e12, unix_ns ~1.78e18. A ±1d window catches
			// any mis-shifted unit while tolerating drain's bucketing
			// rounding (TimeResolution=10s, so a sample could trail the
			// last training timestamp by up to 10s).
			if s[0] < baseUnix-86400 || s[0] > baseUnix+86400 {
				t.Errorf("env.Data[%d].Samples[%d][0] = %d; expected unix SECONDS near %d (a ms/ns mis-shift would be 10^3/10^9 too large)",
					i, j, s[0], baseUnix)
			}
			if s[1] <= 0 {
				t.Errorf("env.Data[%d].Samples[%d][1] = %d; expected positive count", i, j, s[1])
			}
		}
	}
}

// TestConformance_LokiPatternsWire — empty-data envelope. Wired before
// drain landed; pins the wire-stable `data:[]` shape (top-level array,
// matching upstream Loki's `WriteQueryPatternsResponseJSON`) when the
// stub returns zero rows from the peek window.
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
		{
			name: "502_patterns_ch_failure", stub: &stubQuerier{tsLinesErr: errors.New("ch failure")},
			path:     "/loki/api/v1/patterns?query=%7Bjob%3D%22api%22%7D&start=1&end=60",
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

// TestConformance_LokiGrafanaMsTimestamps_ResourcesProxy is the
// request-level pin for #194 on the Loki side. Grafana 11.x's Loki
// datasource sends 13-digit ms `start` / `end` timestamps over
// `/api/datasources/uid/<ds>/resources/...`; cerberus must decode them
// as milliseconds. The pre-fix `>1e12 → ns` heuristic interpreted a
// 13-digit ms value as nanoseconds → year-58353 → ClickHouse
// `toDateTime64` overflow → HTTP 500 → empty Grafana panels.
//
// Drives ms-shaped bounds through both range-query endpoints; HTTP 200
// is the only acceptable outcome. A 500 here is the canary that the
// ms / ns split regressed.
func TestConformance_LokiGrafanaMsTimestamps_ResourcesProxy(t *testing.T) {
	t.Parallel()

	// 2025-01-26 ≈ 1_737_864_000_000 ms; 1 hour later.
	const startMs = "1737000000000"
	const endMs = "1737003600000"
	const q = `{job="api"}`

	cases := []struct {
		name string
		path string
	}{
		{"query", "/loki/api/v1/query?query=" + url.QueryEscape(q) + "&time=" + endMs},
		{
			"query-range",
			"/loki/api/v1/query_range?query=" + url.QueryEscape(q) +
				"&start=" + startMs + "&end=" + endMs + "&step=60s",
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(&stubQuerier{})
			t.Cleanup(srv.Close)
			resp, err := http.Get(srv.URL + c.path)
			if err != nil {
				t.Fatalf("GET %s: %v", c.path, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status=%d body=%s (ms→ns misroute regression)", resp.StatusCode, body)
			}
		})
	}
}

// TestConformance_LokiLogcliNanosTimestamps pins the ns branch on the
// Loki side. The `logcli` client emits 19-digit ns timestamps; the
// 1e15 split must keep routing them to time.Unix(0, n) (i.e., they
// stay ns, not get misread as ms → year-2554 → also broken).
func TestConformance_LokiLogcliNanosTimestamps(t *testing.T) {
	t.Parallel()

	const startNs = "1700000000000000000"
	const endNs = "1700000060000000000"
	const q = `{job="api"}`

	srv := newServer(&stubQuerier{})
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/loki/api/v1/query_range?query=" + url.QueryEscape(q) +
		"&start=" + startNs + "&end=" + endNs + "&step=60s")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s (ns branch regression)", resp.StatusCode, body)
	}
}

// TestConformance_LokiLabelsGrammar pins the wire-emit OTel→Loki label
// normalisation: every key returned by /loki/api/v1/labels matches the
// Prom/Loki label-name grammar `[a-zA-Z_][a-zA-Z0-9_]*`. Loki stores
// OTel-original dotted keys in `ResourceAttributes` for stream-selector
// matching; the wire envelope rewrites them so Grafana panels keying
// off `service_name` / `k8s_pod_name` don't have to second-guess
// whether the underlying column is dotted or underscored.
//
// Collision policy: when both `service.name` and `service_name` exist,
// the natural underscored form wins (the dotted form is the OTel
// telemetry alias).
func TestConformance_LokiLabelsGrammar(t *testing.T) {
	t.Parallel()

	rows := []string{
		"deployment.environment",
		"service.instance.id",
		"service.name",
		"service.version",
		"service_name", // sibling — wins on collision
		"k8s.pod.name",
		"k8s_pod_name", // sibling — wins on collision
		"detected_level",
	}
	srv := newServer(&stubQuerier{stringRows: rows})
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/loki/api/v1/labels?start=1717995600&end=1717999200")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var env struct {
		Status string   `json:"status"`
		Data   []string `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode: %v body=%s", err, body)
	}

	for _, k := range env.Data {
		for i := 0; i < len(k); i++ {
			c := k[i]
			switch {
			case c >= 'a' && c <= 'z':
			case c >= 'A' && c <= 'Z':
			case c == '_':
			case c >= '0' && c <= '9' && i > 0:
			default:
				t.Errorf("key %q has stray byte at offset %d (0x%02x); full data=%v",
					k, i, c, env.Data)
			}
		}
	}

	seen := map[string]struct{}{}
	for _, k := range env.Data {
		seen[k] = struct{}{}
	}
	for _, dotted := range []string{
		"deployment.environment", "service.instance.id",
		"service.name", "service.version", "k8s.pod.name",
	} {
		if _, ok := seen[dotted]; ok {
			t.Errorf("dotted key %q must be normalised away; got data=%v", dotted, env.Data)
		}
	}
	for _, want := range []string{
		"deployment_environment", "service_instance_id",
		"service_name", "service_version", "k8s_pod_name",
		"detected_level",
	} {
		if _, ok := seen[want]; !ok {
			t.Errorf("expected normalised key %q in result; got %v", want, env.Data)
		}
	}
}

// TestConformance_LokiFormatQueryWire pins the wire shape of
// `/loki/api/v1/format_query`. The endpoint mirrors Prom's
// /api/v1/format_query — `{status:"success", data:<formatted>}` — but
// runs the upstream LogQL syntax parser instead. Asserts the success
// envelope across a representative set of LogQL shapes (stream
// selector, log pipeline, range aggregation), plus the error path
// when the `query` parameter is missing.
func TestConformance_LokiFormatQueryWire(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		path string
	}{
		{"selector", `/loki/api/v1/format_query?query=` + url.QueryEscape(`{job="api"}`)},
		{"pipeline", `/loki/api/v1/format_query?query=` + url.QueryEscape(`{job="api"} |= "boom"`)},
		{"range_agg", `/loki/api/v1/format_query?query=` + url.QueryEscape(`rate({job="api"}[5m])`)},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(&stubQuerier{})
			t.Cleanup(srv.Close)

			resp, err := http.Get(srv.URL + c.path)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			body := readBody(t, resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, body)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
				t.Errorf("Content-Type: got %q, want application/json", ct)
			}
			var env struct {
				Status string `json:"status"`
				Data   string `json:"data"`
			}
			if err := json.Unmarshal([]byte(body), &env); err != nil {
				t.Fatalf("decode: %v body=%s", err, body)
			}
			if env.Status != "success" {
				t.Errorf("status: got %q, want success", env.Status)
			}
			if env.Data == "" {
				t.Errorf("data: empty pretty-printed string in body=%s", body)
			}
		})
	}

	// Missing-query parameter mirrors Prom's /api/v1/format_query:
	// 400 with `{status:"error", errorType:"bad_data"}`.
	t.Run("missing_query", func(t *testing.T) {
		t.Parallel()
		srv := newServer(&stubQuerier{})
		t.Cleanup(srv.Close)

		resp, err := http.Get(srv.URL + "/loki/api/v1/format_query")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status: got %d, want 400; body=%s", resp.StatusCode, body)
		}
		var env struct {
			Status    string `json:"status"`
			ErrorType string `json:"errorType"`
		}
		if err := json.Unmarshal([]byte(body), &env); err != nil {
			t.Fatalf("decode: %v body=%s", err, body)
		}
		if env.Status != "error" {
			t.Errorf("status: got %q, want error", env.Status)
		}
		if env.ErrorType != loki.ErrBadData {
			t.Errorf("errorType: got %q, want %q", env.ErrorType, loki.ErrBadData)
		}
	})
}

// TestConformance_LokiBuildInfoWire pins the wire shape of
// `/loki/api/v1/status/buildinfo`. Per the Loki HTTP API documentation
// (docs/sources/reference/loki-http-api.md), this endpoint returns a
// FLAT top-level JSON object — `{version, revision, branch, buildUser,
// buildDate, goVersion}` — NOT wrapped in the `{status, data}` envelope
// the rest of the /loki/api/v1/* surface uses. Grafana's Loki
// datasource per-page probe relies on the flat shape. Asserts:
//
//   - HTTP 200;
//   - Content-Type carries "application/json";
//   - the top-level body has NO `status` field (catching accidental
//     envelope wrapping);
//   - `goVersion` is non-empty (runtime.Version() is always populated).
func TestConformance_LokiBuildInfoWire(t *testing.T) {
	t.Parallel()

	srv := newServer(&stubQuerier{})
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/loki/api/v1/status/buildinfo")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type: got %q, want application/json", ct)
	}

	// Decode into the typed BuildInfo to pin the field names. The flat
	// (un-enveloped) shape means the top-level object IS the BuildInfo
	// — a {status, data} wrap would leave every field zero here.
	var info loki.BuildInfo
	if err := json.Unmarshal([]byte(body), &info); err != nil {
		t.Fatalf("decode: %v body=%s", err, body)
	}
	if info.GoVersion == "" {
		t.Errorf("goVersion should be populated by runtime.Version(); got empty; body=%s", body)
	}

	// Catch accidental envelope wrapping: the upstream Loki contract
	// for /status/buildinfo is flat. A `status` field at the top level
	// signals someone wrapped the body in the {status, data} envelope
	// the rest of the API uses; Grafana would then fail to find the
	// version fields it expects.
	var raw map[string]any
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatalf("decode raw: %v body=%s", err, body)
	}
	if _, ok := raw["status"]; ok {
		t.Errorf("buildinfo body must NOT carry a top-level `status` envelope; got body=%s", body)
	}
	if _, ok := raw["data"]; ok {
		t.Errorf("buildinfo body must NOT carry a top-level `data` envelope; got body=%s", body)
	}
}
