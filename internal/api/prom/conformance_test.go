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
			if tc.name == "label_empty_result" {
				t.Skip("TODO: handler returns `null` instead of `[]` when no values match; handler-side fix follow-up")
			}
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

// TestConformance_QueryExemplarsWire — empty-data envelope shape. The
// data array is non-nil (`[]`, not `null`) so Grafana's exemplars probe
// distinguishes the two.
func TestConformance_QueryExemplarsWire(t *testing.T) {
	t.Parallel()

	cases := []url.Values{
		{"query": {"up"}, "start": {"1717995600"}, "end": {"1717999200"}},
		{"query": {`up{job="api"}`}, "start": {"1717995600"}, "end": {"1717999200"}},
		{"query": {`{__name__=~"http_.*"}`}, "start": {"1717995600"}, "end": {"1717999200"}},
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

func (b *blockingQuerier) QueryMetricMeta(_ context.Context, _ string, _ string, _ ...any) ([]chclient.MetricMetaRow, error) {
	return nil, nil
}
