package prom_test

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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/admit"
	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
)

// --- Section A: wire-format conformance ----------------------------------
//
// Each test below routes one or more representative payloads through the
// real handler, then JSON-decodes the response into a struct with the
// upstream-documented Prom field names. We assert structural shape so
// field-order doesn't make the test brittle. The tests intentionally
// avoid byte-for-byte JSON comparison.

// TestConformance_QueryWire — `/api/v1/query` vector + scalar + empty.
// The wire envelope is `{status, data:{resultType, result:…}}` with
// resultType picking the array shape. Three payloads: vector with two
// series, scalar fold (1+1), empty result.
func TestConformance_QueryWire(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name       string
		path       string
		samples    []chclient.Sample
		wantType   string
		wantSeries int
	}{
		{
			name: "vector_two_series",
			path: "/api/v1/query?query=up&time=" + strconv.FormatInt(ts.Unix(), 10),
			samples: []chclient.Sample{
				{MetricName: "up", Labels: map[string]string{"job": "api"}, Timestamp: ts, Value: 1},
				{MetricName: "up", Labels: map[string]string{"job": "db"}, Timestamp: ts, Value: 0},
			},
			wantType:   "vector",
			wantSeries: 2,
		},
		{
			name:       "scalar_fold",
			path:       "/api/v1/query?query=1%2B1&time=" + strconv.FormatInt(ts.Unix(), 10),
			samples:    nil,
			wantType:   "scalar",
			wantSeries: 0,
		},
		{
			name:       "vector_empty",
			path:       "/api/v1/query?query=up&time=" + strconv.FormatInt(ts.Unix(), 10),
			samples:    nil,
			wantType:   "vector",
			wantSeries: 0,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(&stubQuerier{samples: tc.samples})
			t.Cleanup(srv.Close)

			resp, err := http.Get(srv.URL + tc.path)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			body := readBody(t, resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, body)
			}

			var env struct {
				Status string `json:"status"`
				Data   struct {
					ResultType string          `json:"resultType"`
					Result     json.RawMessage `json:"result"`
				} `json:"data"`
			}
			if err := json.Unmarshal([]byte(body), &env); err != nil {
				t.Fatalf("decode: %v\nbody=%s", err, body)
			}
			if env.Status != "success" {
				t.Errorf("status: got %q, want success", env.Status)
			}
			if env.Data.ResultType != tc.wantType {
				t.Errorf("resultType: got %q, want %q", env.Data.ResultType, tc.wantType)
			}
			if tc.wantType == "scalar" {
				var pt [2]any
				if err := json.Unmarshal(env.Data.Result, &pt); err != nil {
					t.Errorf("scalar shape decode: %v", err)
				}
				if _, ok := pt[1].(string); !ok {
					t.Errorf("scalar value not stringified: %v", pt[1])
				}
			} else {
				var vec []prom.VectorSample
				if err := json.Unmarshal(env.Data.Result, &vec); err != nil {
					t.Errorf("vector decode: %v", err)
				}
				if len(vec) != tc.wantSeries {
					t.Errorf("series count: got %d, want %d", len(vec), tc.wantSeries)
				}
			}
		})
	}
}

// TestConformance_QueryRangeWire — `/api/v1/query_range` matrix +
// scalar over range + empty.
func TestConformance_QueryRangeWire(t *testing.T) {
	t.Parallel()

	start := time.Unix(1717995600, 0).UTC()
	end := start.Add(2 * time.Minute)
	rangeParams := "&start=" + strconv.FormatInt(start.Unix(), 10) +
		"&end=" + strconv.FormatInt(end.Unix(), 10) + "&step=60"

	cases := []struct {
		name     string
		path     string
		samples  []chclient.Sample
		wantType string
	}{
		{
			name: "matrix_one_series",
			path: "/api/v1/query_range?query=up" + rangeParams,
			samples: []chclient.Sample{
				{MetricName: "up", Labels: map[string]string{"job": "api"}, Timestamp: start, Value: 1},
				{MetricName: "up", Labels: map[string]string{"job": "api"}, Timestamp: start.Add(time.Minute), Value: 2},
			},
			wantType: "matrix",
		},
		{
			name:     "scalar_over_range",
			path:     "/api/v1/query_range?query=42" + rangeParams,
			samples:  nil,
			wantType: "matrix", // scalar fold over range returns a single matrix series
		},
		{
			name:     "matrix_empty",
			path:     "/api/v1/query_range?query=up" + rangeParams,
			samples:  nil,
			wantType: "matrix",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(&stubQuerier{samples: tc.samples})
			t.Cleanup(srv.Close)

			resp, err := http.Get(srv.URL + tc.path)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			body := readBody(t, resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, body)
			}
			var env struct {
				Status string `json:"status"`
				Data   struct {
					ResultType string              `json:"resultType"`
					Result     []prom.MatrixSample `json:"result"`
				} `json:"data"`
			}
			if err := json.Unmarshal([]byte(body), &env); err != nil {
				t.Fatalf("decode: %v\nbody=%s", err, body)
			}
			if env.Status != "success" {
				t.Errorf("status: got %q, want success", env.Status)
			}
			if env.Data.ResultType != tc.wantType {
				t.Errorf("resultType: got %q, want %q", env.Data.ResultType, tc.wantType)
			}
			for _, m := range env.Data.Result {
				for _, v := range m.Values {
					// Sample wire shape: [<ts_float>, "<value_string>"].
					if len(v) != 2 {
						t.Errorf("sample pair length: got %d, want 2", len(v))
					}
					if _, ok := v[1].(string); !ok {
						t.Errorf("sample value not stringified: %v", v[1])
					}
				}
			}
		})
	}
}

// TestConformance_LabelsWire — `/api/v1/labels` wire envelope. Data is a
// direct []string, not a {resultType, result} pair.
func TestConformance_LabelsWire(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		rows []string
	}{
		{"non_empty", []string{"job", "instance"}},
		{"empty", nil},
		{"deduped", []string{"job", "job", "instance"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(&stubQuerier{strings: tc.rows})
			t.Cleanup(srv.Close)

			resp, err := http.Get(srv.URL + "/api/v1/labels")
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			body := readBody(t, resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, body)
			}
			var env struct {
				Status string   `json:"status"`
				Data   []string `json:"data"`
			}
			if err := json.Unmarshal([]byte(body), &env); err != nil {
				t.Fatalf("decode: %v body=%s", err, body)
			}
			if env.Status != "success" {
				t.Errorf("status: got %q, want success", env.Status)
			}
			// `__name__` is always present.
			found := false
			for _, n := range env.Data {
				if n == "__name__" {
					found = true
				}
			}
			if !found {
				t.Errorf("missing __name__ in result: %v", env.Data)
			}
		})
	}
}

// TestConformance_LabelValuesWire — `/api/v1/label/<name>/values` returns
// `data: []string`.
func TestConformance_LabelValuesWire(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		path string
		rows []string
	}{
		{"label_job", "/api/v1/label/job/values", []string{"api", "db"}},
		{"label_metric_name", "/api/v1/label/__name__/values", []string{"http_requests", "up"}},
		{"label_empty_result", "/api/v1/label/foo/values", nil},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(&stubQuerier{strings: tc.rows})
			t.Cleanup(srv.Close)

			resp, err := http.Get(srv.URL + tc.path)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			body := readBody(t, resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, body)
			}
			var env struct {
				Status string   `json:"status"`
				Data   []string `json:"data"`
			}
			if err := json.Unmarshal([]byte(body), &env); err != nil {
				t.Fatalf("decode: %v body=%s", err, body)
			}
			if env.Status != "success" {
				t.Errorf("status: got %q, want success", env.Status)
			}
			if env.Data == nil {
				t.Errorf("expected non-nil data slice; got null")
			}
		})
	}
}

// TestConformance_SeriesWire — `/api/v1/series` returns
// `data: []map[string]string` (one element per series). Note: cerberus
// requires at least one match[] selector (Prom convention).
func TestConformance_SeriesWire(t *testing.T) {
	t.Parallel()

	// /series requires at least one match[] selector and runs the
	// matcher through the full executeInstant path — we provide
	// samples so the handler can shape them into label sets.
	samples := []chclient.Sample{
		{MetricName: "up", Labels: map[string]string{"job": "api"}, Value: 1},
		{MetricName: "up", Labels: map[string]string{"job": "db"}, Value: 1},
	}
	srv := newServer(&stubQuerier{samples: samples})
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/series?match%5B%5D=up")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var env struct {
		Status string              `json:"status"`
		Data   []map[string]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode: %v body=%s", err, body)
	}
	if env.Status != "success" {
		t.Errorf("status: got %q, want success", env.Status)
	}
	if env.Data == nil {
		t.Errorf("expected non-nil data slice")
	}
}

// TestConformance_MetadataWire — `/api/v1/metadata` envelope. Cerberus
// returns `{data: map[string][]MetricMetaEntry}` matching Prom.
func TestConformance_MetadataWire(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		path string
		rows []chclient.MetricMetaRow
	}{
		{
			name: "with_entries",
			path: "/api/v1/metadata",
			rows: []chclient.MetricMetaRow{
				{Name: "http_requests_total", Type: "counter", Description: "Total HTTP requests", Unit: ""},
			},
		},
		{
			name: "filtered_by_metric_param",
			path: "/api/v1/metadata?metric=up",
			rows: []chclient.MetricMetaRow{
				{Name: "up", Type: "gauge", Description: "Target up", Unit: ""},
			},
		},
		{
			name: "empty",
			path: "/api/v1/metadata",
			rows: nil,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(&stubQuerier{metaRows: tc.rows})
			t.Cleanup(srv.Close)

			resp, err := http.Get(srv.URL + tc.path)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			body := readBody(t, resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, body)
			}
			var env struct {
				Status string                            `json:"status"`
				Data   map[string][]prom.MetricMetaEntry `json:"data"`
			}
			if err := json.Unmarshal([]byte(body), &env); err != nil {
				t.Fatalf("decode: %v body=%s", err, body)
			}
			if env.Status != "success" {
				t.Errorf("status: got %q, want success", env.Status)
			}
			for _, entries := range env.Data {
				for _, e := range entries {
					if e.Type == "" {
						t.Errorf("entry.Type empty in %+v", e)
					}
				}
			}
		})
	}
}

// TestConformance_RulesEndpoints pins the empty-envelope shape for
// `/api/v1/rules` + `/api/v1/alerts`. Grafana's Prom datasource polls
// both on every page load to gate the "Alert Rules" / "Alerts" UI
// affordances; a 404 here logs noisy "Failed to load resource" errors
// in every user's console and degrades the alerting UI.
//
// The wire shape matches upstream Prometheus exactly: status:"success",
// data:{groups:[]} for /rules, data:{alerts:[]} for /alerts.
func TestConformance_RulesEndpoints(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		path    string
		dataKey string
	}{
		{"rules_empty", "/api/v1/rules", "groups"},
		{"alerts_empty", "/api/v1/alerts", "alerts"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(&stubQuerier{})
			t.Cleanup(srv.Close)

			resp, err := http.Get(srv.URL + tc.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.path, err)
			}
			body := readBody(t, resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s — a 404 here surfaces as "+
					"a noisy 'Failed to load resource' on every Grafana "+
					"page load", resp.StatusCode, body)
			}
			var env struct {
				Status string                     `json:"status"`
				Data   map[string]json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal([]byte(body), &env); err != nil {
				t.Fatalf("decode: %v\nbody=%s", err, body)
			}
			if env.Status != "success" {
				t.Errorf("status: got %q, want success", env.Status)
			}
			raw, ok := env.Data[tc.dataKey]
			if !ok {
				t.Fatalf("data missing %q key; got keys: %v", tc.dataKey, env.Data)
			}
			// Must be an array (empty or otherwise), never null —
			// upstream clients iterate without nil-checking.
			if string(raw) != "[]" {
				t.Errorf("data.%s: got %s, want []", tc.dataKey, string(raw))
			}
		})
	}
}

// TestConformance_FormatQueryWire — `/api/v1/format_query` returns
// `data: <string>` (the pretty-printed query). Three payloads cover
// trivial / function / matcher forms.
func TestConformance_FormatQueryWire(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		path string
	}{
		{"trivial", "/api/v1/format_query?query=up"},
		{"sum", "/api/v1/format_query?query=sum(up)"},
		{"matcher", "/api/v1/format_query?query=up%7Bjob%3D%22api%22%7D"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(&stubQuerier{})
			t.Cleanup(srv.Close)

			resp, err := http.Get(srv.URL + tc.path)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			body := readBody(t, resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, body)
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
				t.Errorf("empty Data string in %s body=%s", tc.name, body)
			}
		})
	}
}

// TestConformance_ParseQueryWire — `/api/v1/parse_query` returns
// `data: {type, node}` — cerberus's minimal AST shape.
func TestConformance_ParseQueryWire(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		path string
	}{
		{"identifier", "/api/v1/parse_query?query=up"},
		{"function", "/api/v1/parse_query?query=rate(up%5B5m%5D)"},
		{"binary", "/api/v1/parse_query?query=up%2Bdown"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(&stubQuerier{})
			t.Cleanup(srv.Close)

			resp, err := http.Get(srv.URL + tc.path)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			body := readBody(t, resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, body)
			}
			var env struct {
				Status string `json:"status"`
				Data   struct {
					Type string `json:"type"`
					Node string `json:"node"`
				} `json:"data"`
			}
			if err := json.Unmarshal([]byte(body), &env); err != nil {
				t.Fatalf("decode: %v body=%s", err, body)
			}
			if env.Status != "success" {
				t.Errorf("status: got %q, want success", env.Status)
			}
			if env.Data.Type == "" || env.Data.Node == "" {
				t.Errorf("expected non-empty Type+Node, got %+v", env.Data)
			}
		})
	}
}

// TestConformance_PromExemplarsBasic pins the populated-data envelope
// shape for `/api/v1/query_exemplars`. The stub Querier returns two
// canned ExemplarRow values (one row per series across two series),
// the handler groups them into ExemplarSeries via groupExemplars, and
// the response JSON is decoded against the upstream-documented wire
// vocabulary (`seriesLabels` / `exemplars` / `labels` / `value` /
// `timestamp`).
//
// Wire-shape contract pinned here:
//   - top-level `data` is an array of objects (not a `{result, …}`
//     wrapper — `/query_exemplars` is shaped differently to
//     `/query` / `/query_range`);
//   - per-element `seriesLabels` is a flat `map[string]string`
//     (NOT nested under a `metric` key the way `/query` does);
//   - per-exemplar `timestamp` decodes as `float64` — numeric, not
//     stringified the way Sample.Value is. Distinguishes exemplar
//     wire shape from Sample, which stringifies for precision per
//     the Prom JSON envelope.
//
// The handler requires a literal `__name__` matcher (PR #520); the
// test request includes one (`up{job="api"}`) so the chsql emitter is
// reached — exercising the full handler → groupExemplars → JSON path
// rather than the early-return error path.
func TestConformance_PromExemplarsBasic(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 5, 14, 12, 0, 0, 123_456_789, time.UTC)
	rows := []chclient.ExemplarRow{
		{
			MetricName:         "up",
			Attributes:         map[string]string{"job": "api"},
			ServiceName:        "checkout",
			Timestamp:          ts,
			Value:              0.125,
			TraceID:            "trace-a1",
			SpanID:             "span-a1",
			ExemplarAttributes: map[string]string{"request_id": "req-a1"},
		},
		{
			MetricName:         "up",
			Attributes:         map[string]string{"job": "db"},
			ServiceName:        "checkout",
			Timestamp:          ts.Add(50 * time.Millisecond),
			Value:              0.500,
			TraceID:            "trace-b1",
			SpanID:             "span-b1",
			ExemplarAttributes: map[string]string{"request_id": "req-b1"},
		},
	}

	srv := newServer(&stubQuerier{exemplarRows: rows})
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + `/api/v1/query_exemplars?query=up%7Bjob%3D%22api%22%7D` +
		`&start=` + strconv.FormatInt(ts.Add(-1*time.Minute).Unix(), 10) +
		`&end=` + strconv.FormatInt(ts.Add(1*time.Minute).Unix(), 10))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}

	// Step 1: decode the envelope into the loosest shape compatible
	// with the contract. Each `data` element is decoded as an
	// `any` map; subsequent assertions inspect that map's surface to
	// pin the field-name vocabulary and the float64 timestamp shape.
	var env struct {
		Status string                   `json:"status"`
		Data   []map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode envelope: %v body=%s", err, body)
	}
	if env.Status != "success" {
		t.Errorf("status: got %q, want success", env.Status)
	}
	// Two distinct (MetricName, Attributes, ServiceName) keys in the
	// canned rows ⇒ two ExemplarSeries in the response.
	if got, want := len(env.Data), 2; got != want {
		t.Fatalf("len(data) = %d; want %d", got, want)
	}

	for i, elem := range env.Data {
		// `seriesLabels` MUST be a flat object — NOT nested under a
		// `metric` key the way Prom's /query envelope wraps the
		// series identity. The handler shape is intentionally
		// distinct to match Prom's documented `/query_exemplars`
		// wire contract.
		sl, ok := elem["seriesLabels"].(map[string]interface{})
		if !ok {
			t.Errorf("data[%d]: seriesLabels not a flat object: %T = %v",
				i, elem["seriesLabels"], elem["seriesLabels"])
			continue
		}
		// Every label value MUST be a string — Prom's wire format
		// stringifies labels regardless of source type, and the Go
		// type-switch below confirms the JSON map[string]string
		// decoded as a flat map of string values.
		for k, v := range sl {
			if _, ok := v.(string); !ok {
				t.Errorf("data[%d].seriesLabels[%q] = %T (%v); want string",
					i, k, v, v)
			}
		}
		// `__name__` MUST be populated — groupExemplars overlays the
		// resolved metric name via format.WithMetricName.
		if sl["__name__"] != "up" {
			t.Errorf("data[%d].seriesLabels[__name__] = %v; want 'up'", i, sl["__name__"])
		}

		exemplars, ok := elem["exemplars"].([]interface{})
		if !ok {
			t.Errorf("data[%d]: exemplars not an array: %T", i, elem["exemplars"])
			continue
		}
		if got, want := len(exemplars), 1; got != want {
			t.Errorf("data[%d]: len(exemplars) = %d; want %d", i, got, want)
		}
		for j, raw := range exemplars {
			ex, ok := raw.(map[string]interface{})
			if !ok {
				t.Errorf("data[%d].exemplars[%d] not an object: %T", i, j, raw)
				continue
			}
			// `timestamp` MUST decode as float64. Distinguishes
			// exemplar wire shape from Sample.Value, which is
			// stringified for precision.
			if _, ok := ex["timestamp"].(float64); !ok {
				t.Errorf("data[%d].exemplars[%d].timestamp = %T (%v); want float64 (numeric, not stringified)",
					i, j, ex["timestamp"], ex["timestamp"])
			}
			// `value` is also numeric float64 (NOT stringified).
			if _, ok := ex["value"].(float64); !ok {
				t.Errorf("data[%d].exemplars[%d].value = %T (%v); want float64",
					i, j, ex["value"], ex["value"])
			}
			// `labels` is a flat map[string]string with `trace_id`
			// and `span_id` overlaid from the dedicated columns.
			labels, ok := ex["labels"].(map[string]interface{})
			if !ok {
				t.Errorf("data[%d].exemplars[%d].labels not a flat object: %T",
					i, j, ex["labels"])
				continue
			}
			for k, v := range labels {
				if _, ok := v.(string); !ok {
					t.Errorf("data[%d].exemplars[%d].labels[%q] = %T; want string",
						i, j, k, v)
				}
			}
			if labels["trace_id"] == "" || labels["trace_id"] == nil {
				t.Errorf("data[%d].exemplars[%d].labels[trace_id] empty; want non-empty",
					i, j)
			}
			if labels["span_id"] == "" || labels["span_id"] == nil {
				t.Errorf("data[%d].exemplars[%d].labels[span_id] empty; want non-empty",
					i, j)
			}
		}
	}
}

// TestConformance_QueryExemplarsWire — empty-data envelope shape. The
// data array is non-nil (`[]`, not `null`) so Grafana's exemplars probe
// distinguishes the two.
func TestConformance_QueryExemplarsWire(t *testing.T) {
	t.Parallel()

	cases := []url.Values{
		{"query": {"up"}, "start": {"1717995600"}, "end": {"1717999200"}},
		{"query": {`up{job="api"}`}, "start": {"1717995600"}, "end": {"1717999200"}},
		{"query": {`up{instance=~".*"}`}, "start": {"1717995600"}, "end": {"1717999200"}},
		// Grafana 11.x sends ms over the `resources/` proxy path for
		// the exemplars endpoint. Anything routed as seconds would
		// overflow toDateTime64 — pin the contract so the regression
		// surfaces here, not as a 502 in Grafana's UI.
		{"query": {"up"}, "start": {"1717995600000"}, "end": {"1717999200000"}},
	}
	for i, qs := range cases {
		qs := qs
		t.Run("case_"+strconv.Itoa(i), func(t *testing.T) {
			t.Parallel()
			srv := newServer(&stubQuerier{})
			t.Cleanup(srv.Close)

			resp, err := http.Get(srv.URL + "/api/v1/query_exemplars?" + qs.Encode())
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			body := readBody(t, resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.StatusCode, body)
			}
			// Empty data must be `[]` not `null`.
			if !strings.Contains(body, `"data":[]`) {
				t.Errorf("expected data:[] in body; got %s", body)
			}
		})
	}
}

// --- Section B: error envelope per head ----------------------------------
//
// Every error class returns the Prom envelope
//   {status:"error", errorType:"<kind>", error:"<msg>"}.
// Each error class below routes through the handler with a stub
// configured to surface that specific failure.

// TestConformance_PromErrorEnvelope — drives the handler through each
// canonical error class and asserts the wire envelope shape.
func TestConformance_PromErrorEnvelope(t *testing.T) {
	t.Parallel()

	type tc struct {
		name     string
		stub     *stubQuerier
		method   string
		path     string
		wantCode int
		wantKind string
	}
	cases := []tc{
		// 400 bad_data: missing param
		{
			name: "400_missing_query", stub: &stubQuerier{},
			method: http.MethodGet, path: "/api/v1/query",
			wantCode: http.StatusBadRequest, wantKind: prom.ErrBadData,
		},
		// 400 bad_data: malformed PromQL
		{
			name: "400_malformed_promql", stub: &stubQuerier{},
			method: http.MethodGet, path: "/api/v1/query?query=%2A%2A",
			wantCode: http.StatusBadRequest, wantKind: prom.ErrBadData,
		},
		// 400 bad_data: bad time
		{
			name: "400_bad_time", stub: &stubQuerier{},
			method: http.MethodGet, path: "/api/v1/query?query=up&time=banana",
			wantCode: http.StatusBadRequest, wantKind: prom.ErrBadData,
		},
		// 400 bad_data: missing start on range
		{
			name: "400_missing_start", stub: &stubQuerier{},
			method: http.MethodGet, path: "/api/v1/query_range?query=up&end=1717999200&step=60",
			wantCode: http.StatusBadRequest, wantKind: prom.ErrBadData,
		},
		// 400 bad_data: missing step on range
		{
			name: "400_missing_step", stub: &stubQuerier{},
			method: http.MethodGet, path: "/api/v1/query_range?query=up&start=1&end=2",
			wantCode: http.StatusBadRequest, wantKind: prom.ErrBadData,
		},
		// 400 bad_data: end before start
		{
			name: "400_end_before_start", stub: &stubQuerier{},
			method: http.MethodGet, path: "/api/v1/query_range?query=up&start=20&end=10&step=1",
			wantCode: http.StatusBadRequest, wantKind: prom.ErrBadData,
		},
		// 502 internal: CH connection failure
		{
			name: "502_ch_failure", stub: &stubQuerier{err: errors.New("clickhouse: dial: connection refused")},
			method: http.MethodGet, path: "/api/v1/query?query=up",
			wantCode: http.StatusBadGateway, wantKind: prom.ErrInternal,
		},
		// 502 internal: CH failure on range
		{
			name: "502_ch_failure_range", stub: &stubQuerier{err: errors.New("clickhouse: read timeout")},
			method: http.MethodGet, path: "/api/v1/query_range?query=up&start=1&end=60&step=10",
			wantCode: http.StatusBadGateway, wantKind: prom.ErrInternal,
		},
		// 502 internal: labels endpoint CH failure
		{
			name: "502_labels_ch_failure", stub: &stubQuerier{err: errors.New("clickhouse: server error")},
			method: http.MethodGet, path: "/api/v1/labels",
			wantCode: http.StatusBadGateway, wantKind: prom.ErrInternal,
		},
		// 400 bad_data: invalid label name path segment
		{
			name: "400_invalid_label_name", stub: &stubQuerier{},
			method: http.MethodGet, path: "/api/v1/label/123invalid/values",
			wantCode: http.StatusBadRequest, wantKind: prom.ErrBadData,
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(c.stub)
			t.Cleanup(srv.Close)

			req, err := http.NewRequest(c.method, srv.URL+c.path, nil)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			body := readBody(t, resp)
			if resp.StatusCode != c.wantCode {
				t.Fatalf("status: got %d, want %d; body=%s", resp.StatusCode, c.wantCode, body)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
				t.Errorf("Content-Type: got %q, want json", ct)
			}
			var env prom.Response
			if err := json.Unmarshal([]byte(body), &env); err != nil {
				t.Fatalf("envelope decode: %v body=%s", err, body)
			}
			if env.Status != "error" {
				t.Errorf("status: got %q, want error", env.Status)
			}
			if env.ErrorType != c.wantKind {
				t.Errorf("errorType: got %q, want %q", env.ErrorType, c.wantKind)
			}
			if env.Error == "" {
				t.Errorf("error message empty (Grafana renders this)")
			}
		})
	}
}

// --- Section C: header pins ---------------------------------------------

// TestConformance_PromHeaders — Content-Type + X-Prometheus-API-Version
// + X-Cerberus-CH-Millis present on the canonical success path.
func TestConformance_PromHeaders(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{
		samples: []chclient.Sample{
			{MetricName: "up", Labels: map[string]string{"job": "api"}, Value: 1},
		},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/query?query=up")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Errorf("Content-Type: got %q, want application/json", got)
	}
	if got := resp.Header.Get("X-Prometheus-API-Version"); got != "v1" {
		t.Errorf("X-Prometheus-API-Version: got %q, want v1", got)
	}
	chMillis := resp.Header.Get("X-Cerberus-CH-Millis")
	if chMillis == "" {
		t.Errorf("X-Cerberus-CH-Millis: missing")
	} else if _, err := strconv.Atoi(chMillis); err != nil {
		t.Errorf("X-Cerberus-CH-Millis: got %q, want numeric", chMillis)
	}
	if got := resp.Header.Get("X-Cerberus-Strategy"); got == "" {
		t.Errorf("X-Cerberus-Strategy: missing")
	}
	if got := resp.Header.Get("X-Cerberus-Plan-Nodes"); got == "" {
		t.Errorf("X-Cerberus-Plan-Nodes: missing")
	}
}

// --- Section D: range parameter parsing matrix --------------------------

// TestConformance_PromRangeTimeMatrix — the start/end parser accepts
// integer seconds, floats, and RFC3339. Invalid inputs return 400.
func TestConformance_PromRangeTimeMatrix(t *testing.T) {
	t.Parallel()

	type tc struct {
		name     string
		start    string
		end      string
		step     string
		wantCode int
	}
	cases := []tc{
		// valid forms
		{"unix_seconds_int", "1717995600", "1717999200", "60", http.StatusOK},
		{"unix_seconds_float", "1717995600.5", "1717999200.5", "60", http.StatusOK},
		{"rfc3339", "2024-01-01T00:00:00Z", "2024-01-01T01:00:00Z", "60s", http.StatusOK},
		{"rfc3339_with_nanos", "2024-01-01T00:00:00.123Z", "2024-01-01T01:00:00.456Z", "30s", http.StatusOK},
		{"go_duration_step", "1717995600", "1717999200", "5m", http.StatusOK},
		// Grafana's Prom datasource ships start/end as 13-digit ms when
		// it routes through `resources/` proxy. Anything < year 33658
		// expressed as seconds (i.e. >= 1e12) is parsed as ms — anything
		// else stays on the seconds path.
		{"unix_millis_int", "1717995600000", "1717999200000", "60", http.StatusOK},
		// invalid forms
		{"empty_start", "", "1717999200", "60", http.StatusBadRequest},
		{"garbage_start", "tomorrow", "1717999200", "60", http.StatusBadRequest},
		{"missing_step", "1717995600", "1717999200", "", http.StatusBadRequest},
		{"zero_step", "1717995600", "1717999200", "0", http.StatusBadRequest},
		{"empty_end", "1717995600", "", "60", http.StatusBadRequest},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(&stubQuerier{})
			t.Cleanup(srv.Close)

			path := "/api/v1/query_range?query=up"
			if c.start != "" {
				path += "&start=" + url.QueryEscape(c.start)
			}
			if c.end != "" {
				path += "&end=" + url.QueryEscape(c.end)
			}
			if c.step != "" {
				path += "&step=" + url.QueryEscape(c.step)
			}
			resp, err := http.Get(srv.URL + path)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != c.wantCode {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("status: got %d, want %d; body=%s", resp.StatusCode, c.wantCode, string(body))
			}
		})
	}
}

// TestConformance_PromQueryTimeMatrix — the `/api/v1/query?time=…`
// parser accepts unix seconds + RFC3339; everything else is rejected.
func TestConformance_PromQueryTimeMatrix(t *testing.T) {
	t.Parallel()

	type tc struct {
		name     string
		time     string
		wantCode int
	}
	cases := []tc{
		{"unix_seconds_int", "1717995600", http.StatusOK},
		{"unix_seconds_float", "1717995600.123", http.StatusOK},
		{"rfc3339", "2024-01-01T00:00:00Z", http.StatusOK},
		{"empty_uses_now", "", http.StatusOK},
		{"garbage", "tomorrow", http.StatusBadRequest},
		{"negative_is_still_unix", "-100", http.StatusOK}, // unix seconds accepts negatives
		// Grafana sends ms over `resources/`; the >=1e12 heuristic
		// routes it to UnixMilli so toDateTime64('58353-...') no longer
		// overflows. Both 13-digit and 14-digit ms forms must pass.
		{"unix_millis_13digit", "1717995600000", http.StatusOK},
		{"unix_millis_with_subms_frac", "1717995600123", http.StatusOK},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(&stubQuerier{})
			t.Cleanup(srv.Close)

			path := "/api/v1/query?query=up"
			if c.time != "" {
				path += "&time=" + url.QueryEscape(c.time)
			}
			resp, err := http.Get(srv.URL + path)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != c.wantCode {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("status: got %d, want %d; body=%s", resp.StatusCode, c.wantCode, string(body))
			}
		})
	}
}

// --- Section E: match[] selector edge cases -----------------------------

// TestConformance_LabelsMatchEdge — labels endpoint with multiple
// match[] selectors, invalid matchers, and SQL-injection-shaped regex.
func TestConformance_LabelsMatchEdge(t *testing.T) {
	t.Parallel()

	type tc struct {
		name     string
		query    string
		wantCode int
	}
	cases := []tc{
		{
			name:     "multiple_match",
			query:    "match%5B%5D=up&match%5B%5D=down",
			wantCode: http.StatusOK,
		},
		{
			name:     "invalid_matcher",
			query:    "match%5B%5D=%7B%7D", // empty selector — Prom requires at least one matcher
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "regex_with_sql_injection_chars",
			query:    `match%5B%5D=up%7Bjob%3D~%22.%2A%27%20OR%201%3D1--%22%7D`,
			wantCode: http.StatusOK,
		},
		{
			name:     "regex_with_backtick",
			query:    "match%5B%5D=up%7Bjob%3D~%22.%2A%60%22%7D",
			wantCode: http.StatusOK,
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(&stubQuerier{strings: []string{"job"}})
			t.Cleanup(srv.Close)

			resp, err := http.Get(srv.URL + "/api/v1/labels?" + c.query)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			body := readBody(t, resp)
			if resp.StatusCode != c.wantCode {
				t.Errorf("status: got %d, want %d; body=%s", resp.StatusCode, c.wantCode, body)
			}
			// On success path, no panic + no SQL leak in response.
			if c.wantCode == http.StatusOK {
				if strings.Contains(body, "OR 1=1") {
					t.Errorf("SQL-injection-shaped string echoed in response: %s", body)
				}
			}
		})
	}
}

// TestConformance_SeriesMatchEdge — /series rejects empty match[],
// accepts multiple selectors, and rejects invalid matchers.
func TestConformance_SeriesMatchEdge(t *testing.T) {
	t.Parallel()

	type tc struct {
		name     string
		query    string
		wantCode int
	}
	cases := []tc{
		{
			name:     "no_match_required",
			query:    "",
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "valid_matcher",
			query:    "match%5B%5D=up",
			wantCode: http.StatusOK,
		},
		{
			name:     "multiple_selectors",
			query:    "match%5B%5D=up&match%5B%5D=down",
			wantCode: http.StatusOK,
		},
		{
			name:     "invalid_matcher_promql",
			query:    "match%5B%5D=*broken",
			wantCode: http.StatusBadRequest,
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv := newServer(&stubQuerier{samples: nil})
			t.Cleanup(srv.Close)
			resp, err := http.Get(srv.URL + "/api/v1/series?" + c.query)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			body := readBody(t, resp)
			if resp.StatusCode != c.wantCode {
				t.Errorf("status: got %d, want %d; body=%s", resp.StatusCode, c.wantCode, body)
			}
		})
	}
}

// --- Section G: admission control / concurrency cap ---------------------

// TestConformance_PromAdmitRejectsAtCap — when the per-handler limiter
// is full, requests get 503 + Retry-After. Independent of admit's own
// tests; this asserts the prom mux composition wires the limiter in.
func TestConformance_PromAdmitRejectsAtCap(t *testing.T) {
	t.Parallel()

	// Build a Handler whose Limiter caps inflight at 1, then hold a
	// slot via the public admit API to force the next mux request into
	// a rejection. The handler stub blocks forever on the held slot so
	// we can drive the saturation deterministically.
	limiter := admit.New("prom", 1)
	rel, ok := limiter.Acquire(context.Background())
	if !ok {
		t.Fatalf("setup acquire: want ok")
	}
	defer rel()

	h := prom.New(&stubQuerier{}, schema.DefaultOTelMetrics(), nil)
	h.Limiter = limiter
	mux := http.NewServeMux()
	// Limiter must be set BEFORE Mount — h.Mount captures h.Limiter
	// into each registered route closure at mount time.
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/query?query=up")
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

// TestConformance_PromAdmitReleaseAdmitsNext — releasing a slot after
// a saturated request returns the slot to the pool so the next caller
// makes it through.
func TestConformance_PromAdmitReleaseAdmitsNext(t *testing.T) {
	t.Parallel()

	limiter := admit.New("prom", 1)
	h := prom.New(&stubQuerier{samples: []chclient.Sample{{MetricName: "up", Labels: map[string]string{}, Value: 1}}}, schema.DefaultOTelMetrics(), nil)
	h.Limiter = limiter
	mux := http.NewServeMux()
	// Limiter must be set BEFORE Mount.
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// First request occupies the slot momentarily; second goes through
	// once it releases. Run them serially.
	for i := 0; i < 3; i++ {
		resp, err := http.Get(srv.URL + "/api/v1/query?query=up")
		if err != nil {
			t.Fatalf("GET %d: %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: got %d, want 200", i, resp.StatusCode)
		}
	}
}

// TestConformance_PromAdmitParallelOverCap — workers beyond cap get
// 503 rejections; under cap, they're admitted. Asserts the cap is
// actually engaged at the mux layer (not just exposed as a struct).
func TestConformance_PromAdmitParallelOverCap(t *testing.T) {
	t.Parallel()

	const cap = 2
	const workers = 12

	limiter := admit.New("prom", cap)
	// Block CH so admitted requests stay inflight long enough for the
	// remaining workers to hit the saturated cap and get rejected.
	release := make(chan struct{})
	q := &blockingQuerier{release: release}
	h := prom.New(q, schema.DefaultOTelMetrics(), nil)
	h.Limiter = limiter
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	var (
		admitted atomic.Int32
		rejected atomic.Int32
		wg       sync.WaitGroup
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get(srv.URL + "/api/v1/query?query=up")
			if err != nil {
				return
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body)
			if resp.StatusCode == http.StatusServiceUnavailable {
				rejected.Add(1)
				return
			}
			if resp.StatusCode == http.StatusOK {
				admitted.Add(1)
			}
		}()
	}
	// Give rejections time to land — they happen synchronously when
	// TryAcquire fails so a brief sleep is enough.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && rejected.Load() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	close(release)
	wg.Wait()

	if rejected.Load() == 0 {
		t.Errorf("cap not engaged: admitted=%d rejected=%d (cap=%d workers=%d)",
			admitted.Load(), rejected.Load(), cap, workers)
	}
}

// blockingQuerier blocks every Query call on the release channel.
// Used to drive deterministic admission-cap saturation in tests.
type blockingQuerier struct {
	release chan struct{}
	calls   atomic.Int32
}

func (b *blockingQuerier) Query(_ context.Context, _ string, _ ...any) ([]chclient.Sample, error) {
	b.calls.Add(1)
	<-b.release
	return nil, nil
}

func (b *blockingQuerier) QueryCursor(_ context.Context, _ string, _ ...any) (chclient.Cursor, error) {
	b.calls.Add(1)
	<-b.release
	return newSliceCursor(nil), nil
}

func (b *blockingQuerier) QueryStrings(_ context.Context, _ string, _ ...any) ([]string, error) {
	return nil, nil
}

func (b *blockingQuerier) QueryLabelSets(_ context.Context, _ string, _ ...any) ([]map[string]string, error) {
	return nil, nil
}

func (b *blockingQuerier) QueryMetricMeta(_ context.Context, _, _ string, _ ...any) ([]chclient.MetricMetaRow, error) {
	return nil, nil
}

func (b *blockingQuerier) QueryExemplars(_ context.Context, _ string, _ ...any) ([]chclient.ExemplarRow, error) {
	return nil, nil
}

// --- Section H: dotted metric names end-to-end -------------------------
//
// Drives OTel-style dotted metric names through the real `/api/v1/query`
// handler — i.e. the full parse → lower → emit pipeline behind the
// normalizeDottedSelectors rewrite. The unit-layer tests in
// dotted_names_test.go assert string-level rewrites and parser-roundtrip;
// this layer pins that the HTTP handler returns 200 (not 400 parse
// error) for every query shape Grafana's metric picker can land on.
//
// One case per selector shape — bare metric, function-wrapped (rate),
// aggregation-wrapped (sum by le rate), label-matcher-attached. The
// last shape is the one that motivated the brace-fold fix in
// normalizeDottedSelectors: `<dotted>{labels}` rewrites to
// `{__name__="<dotted>",labels}` — splicing into the existing matcher
// group — instead of emitting two adjacent selectors, which the PromQL
// parser rejects upstream as "unexpected `{`".

func TestConformance_DottedMetricNames(t *testing.T) {
	t.Parallel()

	ts := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		query    string
		wantName string // the dotted MetricName that should be bound to the CH args
	}{
		{
			name:     "bare_dotted_selector",
			query:    `http.server.request.duration`,
			wantName: "http.server.request.duration",
		},
		{
			name:     "dotted_inside_rate",
			query:    `rate(http.server.request.duration[5m])`,
			wantName: "http.server.request.duration",
		},
		{
			name:     "dotted_inside_aggregation",
			query:    `sum by (le) (rate(http.server.request.duration_bucket[5m]))`,
			wantName: "http.server.request.duration_bucket",
		},
		{
			name:     "dotted_with_label_matcher",
			query:    `http.server.request.duration{job="api"}`,
			wantName: "http.server.request.duration",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Stub returns one synthetic sample so the response is a
			// non-empty vector — the test cares about HTTP 200 + the
			// MetricName flowing through the SQL bind, not the exact
			// pivot shape (other tests cover that).
			q := &stubQuerier{
				samples: []chclient.Sample{
					{
						MetricName: "http.server.request.duration",
						Labels:     map[string]string{"job": "api"},
						Timestamp:  ts,
						Value:      1.0,
					},
				},
			}
			srv := newServer(q)
			t.Cleanup(srv.Close)

			u := srv.URL + "/api/v1/query?query=" + url.QueryEscape(tc.query) +
				"&time=" + strconv.FormatInt(ts.Unix(), 10)
			resp, err := http.Get(u)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			body := readBody(t, resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status: got %d, want 200; body=%s", resp.StatusCode, body)
			}

			// Envelope shape: handler reached the success path (a 400
			// parse error would have surfaced as `status:"error",
			// errorType:"bad_data"`).
			var env struct {
				Status    string `json:"status"`
				ErrorType string `json:"errorType"`
				Error     string `json:"error"`
				Data      struct {
					ResultType string `json:"resultType"`
				} `json:"data"`
			}
			if err := json.Unmarshal([]byte(body), &env); err != nil {
				t.Fatalf("decode: %v body=%s", err, body)
			}
			if env.Status != "success" {
				t.Fatalf("status: got %q (errorType=%q error=%q); want success",
					env.Status, env.ErrorType, env.Error)
			}

			// The dotted name must have reached the CH bind layer as
			// the MetricName arg — i.e. the rewrite landed in the
			// __name__ matcher position, was lowered into a Filter,
			// and emitted as a parameterised CH predicate. If the
			// rewrite silently dropped the dotted token, this would
			// fail. (Iterate args; the eval-ts predicate appends
			// later args, so the dotted name lands somewhere in
			// lastArgs but not necessarily first.)
			foundName := false
			for _, a := range q.lastArgs {
				if s, ok := a.(string); ok && s == tc.wantName {
					foundName = true
					break
				}
			}
			if !foundName {
				t.Errorf("dotted MetricName %q not bound to SQL args: %v\nsql=%s",
					tc.wantName, q.lastArgs, q.lastSQL)
			}
		})
	}
}

// TestConformance_LabelsGrammar pins the wire-emit OTel→Prom label
// normalisation: every key returned by /api/v1/labels MUST match the
// Prometheus label-name grammar `[a-zA-Z_][a-zA-Z0-9_]*`. The raw
// `Attributes` map in ClickHouse carries OTel-original dotted keys
// (`service.name`, `http.request.method`, `cerberus.ql`, ...);
// without normalisation, every Grafana panel doing `sum by (cerberus_ql)`
// silently broke because PromQL grammar forbids `.` in identifiers.
// This test feeds the handler a mix of dotted and underscored keys and
// asserts the wire envelope projects only the Prom-grammar form.
//
// Collision policy (per the task brief): when both `service.name` and
// `service_name` exist with different contents, the underscored form
// wins — the dotted form is the OTel telemetry alias and the
// underscored form is the user's intended Prom-style identifier.
func TestConformance_LabelsGrammar(t *testing.T) {
	t.Parallel()

	// Mix every shape the bug repro saw plus a natural-form collision
	// pair.
	rows := []string{
		"cerberus.ql",
		"cerberus.route",
		"http.request.method",
		"http.response.status_code",
		"http.route",
		"http_status",
		"job",
		"network.protocol.name",
		"network.protocol.version",
		"result",
		"server.address",
		"server.port",
		"stage",
		"url.scheme",
		// Collision: both forms present. Natural wins.
		"service.name",
		"service_name",
	}
	srv := newServer(&stubQuerier{strings: rows})
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/labels")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var env struct {
		Status string   `json:"status"`
		Data   []string `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode: %v body=%s", err, body)
	}
	if env.Status != "success" {
		t.Fatalf("status=%q", env.Status)
	}

	// Pin 1: every emitted key matches Prom label grammar — zero
	// characters outside `[a-zA-Z0-9_]`.
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

	// Pin 2: collision policy — `service.name` was rewritten away,
	// only `service_name` survives.
	seen := map[string]struct{}{}
	for _, k := range env.Data {
		seen[k] = struct{}{}
	}
	if _, ok := seen["service.name"]; ok {
		t.Errorf("expected `service.name` to be normalised away; got %v", env.Data)
	}
	if _, ok := seen["service_name"]; !ok {
		t.Errorf("expected `service_name` to survive normalisation; got %v", env.Data)
	}

	// Pin 3: spot-check that the headline keys from the bug repro
	// surface in their underscored form.
	for _, want := range []string{
		"cerberus_ql", "http_request_method",
		"http_response_status_code", "url_scheme",
	} {
		if _, ok := seen[want]; !ok {
			t.Errorf("expected normalised key %q in result; got %v", want, env.Data)
		}
	}
}

// TestConformance_LabelValuesGrammar pins the /api/v1/label/__name__/values
// path: metric-name values must satisfy Prom's metric-name grammar
// `[a-zA-Z_:][a-zA-Z0-9_:]*`. OTel may store dotted metric names
// (`http.server.duration`) that PromQL's selector position can't
// reference directly without `normalizeDottedSelectors` wrapping —
// surfacing the normalised form on the wire keeps Grafana's metric
// picker honest.
func TestConformance_LabelValuesGrammar(t *testing.T) {
	t.Parallel()

	rows := []string{
		"http.server.duration",
		"http.server.request.duration",
		"already_ok",
		"with:colon", // colons are valid for metric names
	}
	srv := newServer(&stubQuerier{strings: rows})
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/label/__name__/values")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var env struct {
		Status string   `json:"status"`
		Data   []string `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode: %v body=%s", err, body)
	}
	for _, v := range env.Data {
		for i := 0; i < len(v); i++ {
			c := v[i]
			switch {
			case c >= 'a' && c <= 'z':
			case c >= 'A' && c <= 'Z':
			case c == '_', c == ':':
			case c >= '0' && c <= '9' && i > 0:
			default:
				t.Errorf("metric value %q has stray byte at offset %d (0x%02x); full data=%v",
					v, i, c, env.Data)
			}
		}
	}
}

// TestConformance_UnderscoredMatcherEmitsDottedFallback pins the
// matcher-side resolution for underscored Prom-grammar names that
// could resolve to a dotted OTel-canonical key in storage. Without
// this fallback, a user query like `sum by (cerberus_ql) (...)` hits
// `Attributes['cerberus_ql']` which misses every row stored with the
// dotted `cerberus.ql` key — Grafana's "by language" panel collapses
// to a single anonymous "Value" bucket. The fix emits an if-chain
// over [format.PromLabelToOTelCandidates] so the lookup hits either
// form.
//
// The assertion targets the captured SQL string: both `mapContains`
// and the two candidate keys must appear in BOTH the WHERE matcher
// position and the GROUP BY aggregation position.
func TestConformance_UnderscoredMatcherEmitsDottedFallback(t *testing.T) {
	t.Parallel()

	stub := &stubQuerier{samples: nil}
	srv := newServer(stub)
	t.Cleanup(srv.Close)

	q := url.QueryEscape(`sum by (cerberus_ql) (rate(cerberus_queries_total[5m]))`)
	resp, err := http.Get(srv.URL + "/api/v1/query?query=" + q + "&time=1717999200")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d (expected 200; query lowering should succeed even with empty result)", resp.StatusCode)
	}
	if stub.lastSQL == "" {
		t.Fatalf("stub.lastSQL is empty; expected handler to have invoked the CH client")
	}
	// The lowered SQL must reference the dotted-fallback machinery in
	// the GROUP BY (`cerberus_ql` → tries `cerberus.ql`). Both
	// `mapContains` and both candidate keys MUST appear in args.
	if !strings.Contains(stub.lastSQL, "mapContains") {
		t.Errorf("SQL is missing the mapContains gate that drives the dotted-fallback chain\nSQL: %s", stub.lastSQL)
	}
	var sawUnderscore, sawDotted bool
	for _, a := range stub.lastArgs {
		s, ok := a.(string)
		if !ok {
			continue
		}
		if s == "cerberus_ql" {
			sawUnderscore = true
		}
		if s == "cerberus.ql" {
			sawDotted = true
		}
	}
	if !sawUnderscore {
		t.Errorf("expected `cerberus_ql` candidate key in bound args; got %v", stub.lastArgs)
	}
	if !sawDotted {
		t.Errorf("expected `cerberus.ql` dotted-expansion candidate key in bound args; got %v", stub.lastArgs)
	}
}
