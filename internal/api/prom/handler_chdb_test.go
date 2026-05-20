//go:build chdb

// chDB-backed mirror of the handler_test.go cases. The default
// (untagged) test lane uses stubQuerier so it stays CGO-free; this
// file exercises the same HTTP surface against a real chDB session
// so the full pipeline (parse → lower → optimize → emit → execute)
// is asserted end-to-end on every chdb-tagged run.
//
// Each test seeds an OTel-metrics-gauge-shaped table inside an
// ephemeral chDB session, executes the handler, and asserts the
// envelope returned to the client. The seed values are chosen to
// match the assertions in the parallel stub-based test verbatim —
// the SQL the handler emits is therefore exercised end-to-end
// against ClickHouse semantics rather than against a stubbed-out
// slice of [chclient.Sample].

package prom_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
)

// gaugeDDL is the OTel-metrics-gauge-shaped table the chDB-backed
// handler tests seed before exercising the handler. The full upstream
// OTel exporter DDL has many more columns than the handler reads —
// this minimal shape covers exactly the four columns the prom
// emitter's outer Project projects, plus the columns the gauge-table
// SQL filters on (MetricName). chDB is happy to project a subset of
// the columns it knows about; missing-column INSERTs would fail.
//
// Engine is MergeTree (not Memory) because the chsql emitter promotes
// Filter(Scan) predicates from WHERE → PREWHERE, and ClickHouse's
// Memory engine rejects PREWHERE with `ILLEGAL_PREWHERE`. The sort key
// `(MetricName, TimeUnix)` mirrors the production OTel-CH layout, so
// the same PREWHERE-promotion shapes the production deployment hits
// also run here.
const gaugeDDL = `CREATE TABLE otel_metrics_gauge (
    MetricName String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree() ORDER BY (MetricName, TimeUnix);`

func newChDBServer(t *testing.T, ddl string) (*httptest.Server, *chclienttest.Client) {
	t.Helper()
	c := chclienttest.NewChDB(t)
	if ddl != "" {
		c.Seed(t, ddl)
	}
	h := prom.New(c, schema.DefaultOTelMetrics(), nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, c
}

func TestQuery_Vector_ChDB(t *testing.T) {
	// Two gauge rows for `up`, distinct job labels; the handler will
	// emit SELECT … FROM otel_metrics_gauge WHERE MetricName=? and
	// receive both rows back.
	//
	// The query `time` is pinned to the seed timestamp so the bare-
	// selector LWR wrapper's `Timestamp <= eval_ts` and 5-minute
	// staleness predicates both match. A query `time` earlier than the
	// seed would LWR-filter every row out and yield an empty vector.
	seedTime := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	ts := seedTime.Format("2006-01-02 15:04:05.000")
	seed := gaugeDDL + fmt.Sprintf(`
INSERT INTO otel_metrics_gauge VALUES
    ('up', map('job', 'api'), toDateTime64('%s', 9), 1.0),
    ('up', map('job', 'db'),  toDateTime64('%s', 9), 0.0);`, ts, ts)

	srv, _ := newChDBServer(t, seed)
	resp, err := http.Get(fmt.Sprintf("%s/api/v1/query?query=up&time=%d",
		srv.URL, seedTime.Unix()))
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
		t.Fatalf("status=%q err=%s", parsed.Status, parsed.Error)
	}
	if parsed.Data.ResultType != "vector" {
		t.Fatalf("resultType=%q, want vector", parsed.Data.ResultType)
	}

	rawResult, _ := json.Marshal(parsed.Data.Result)
	var vec []prom.VectorSample
	if err := json.Unmarshal(rawResult, &vec); err != nil {
		t.Fatalf("decode vector: %v", err)
	}
	if len(vec) != 2 {
		t.Fatalf("expected 2 series, got %d: %+v", len(vec), vec)
	}
	jobs := map[string]bool{}
	for _, v := range vec {
		if v.Metric["__name__"] != "up" {
			t.Errorf("missing __name__ in %+v", v.Metric)
		}
		jobs[v.Metric["job"]] = true
	}
	for _, j := range []string{"api", "db"} {
		if !jobs[j] {
			t.Errorf("missing series with job=%s; got %+v", j, vec)
		}
	}
}

func TestQueryRange_Matrix_ChDB(t *testing.T) {
	// One seeded sample at `start`; query window is [start, start+4m]
	// with step=60s (5 anchor points). Pre-LWR the bare `up` selector
	// returned every raw sample and the matrix pivot's "latest at step"
	// rule fanned them across the step grid; the LWR wrapper added in
	// `2a67f3e` now collapses to a single per-series row at the latest
	// sample's TimeUnix, so each subsequent step bucket simply
	// re-uses that LWR-collapsed sample (its TS sits at-or-before every
	// step). Anchoring the seed at `start` therefore keeps the
	// 5-step matrix shape this test pins, only with a single repeated
	// value instead of the pre-LWR ramp.
	start := time.Unix(1717995600, 0).UTC()
	end := start.Add(4 * time.Minute)
	ts := start.Format("2006-01-02 15:04:05.000")
	seed := gaugeDDL + fmt.Sprintf(`
INSERT INTO otel_metrics_gauge VALUES
    ('up', map('job', 'api'), toDateTime64('%s', 9), 7.0);`, ts)

	srv, _ := newChDBServer(t, seed)

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
		t.Fatalf("resultType=%q, want matrix", parsed.Data.ResultType)
	}

	rawResult, _ := json.Marshal(parsed.Data.Result)
	var matrix []prom.MatrixSample
	if err := json.Unmarshal(rawResult, &matrix); err != nil {
		t.Fatalf("decode matrix: %v", err)
	}
	if len(matrix) != 1 {
		t.Fatalf("expected 1 series, got %d: %+v", len(matrix), matrix)
	}
	// 5 step points expected for [start..start+4m] step=60s.
	if got := len(matrix[0].Values); got != 5 {
		t.Fatalf("expected 5 sample points in matrix, got %d: %+v",
			got, matrix[0].Values)
	}
	// LWR-collapsed seed (the only sample, at TimeUnix=start) reappears
	// at every step within the 5-minute staleness window.
	wantValues := []string{"7", "7", "7", "7", "7"}
	for i, want := range wantValues {
		if got := matrix[0].Values[i][1]; got != want {
			t.Errorf("step %d: got value %q, want %q",
				i, got, want)
		}
	}
}

func TestResponseHeaders_ChDB(t *testing.T) {
	ts := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC).Format("2006-01-02 15:04:05.000")
	seed := gaugeDDL + fmt.Sprintf(`
INSERT INTO otel_metrics_gauge VALUES
    ('up', map('job', 'api'), toDateTime64('%s', 9), 1.0);`, ts)

	srv, _ := newChDBServer(t, seed)
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
}

func TestQuery_ScalarFold_ChDB(t *testing.T) {
	// Scalar fold short-circuits the CH path. The chDB session is
	// initialised but never queried — same contract as the stub test.
	srv, _ := newChDBServer(t, gaugeDDL)
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
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed.Data.ResultType != "scalar" {
		t.Fatalf("resultType=%q, want scalar", parsed.Data.ResultType)
	}
	rawResult, _ := json.Marshal(parsed.Data.Result)
	var point [2]any
	if err := json.Unmarshal(rawResult, &point); err != nil {
		t.Fatalf("decode scalar: %v", err)
	}
	if got := point[1]; got != "2" {
		t.Errorf("folded value: got %v, want \"2\"", got)
	}
}

// TestQuery_VectorVectorSynthBinop_ChDB exercises the Grafana PromQL
// CheckHealth probe shape `vector(N)+vector(N)` end-to-end against
// chDB. Pre-fix this returned a 502 with `converting UInt16 to *float64
// is unsupported` — the clickhouse-go/v2 driver renders Go's
// `float64(1.0)` as the SQL literal `1` (no decimal, fmt.Sprint fallback
// in its bind.go::format), CH narrows that to `UInt8`, the synthetic-
// fold Value projection's `(? OP ?)` expression promotes to `UInt16`,
// and the chclient cursor refuses to scan a UInt16 column into
// `chclient.Sample.Value` (`*float64`). The fix wraps the synthetic
// LitFloat in `toFloat64(...)` at [syntheticScalarVector], which keeps
// the Value column Float64 across the V-V binop fold.
//
// Mirrors [internal/api/loki/conformance_test.go::
// TestConformance_LokiQueryConstantArithmetic_HealthProbe] for the
// PromQL side — but executes against a real chDB session rather than a
// stub so the UInt8 → UInt16 → *float64 scan path is exercised.
//
// Unlike `1+1` (which TryFoldScalar short-circuits to a scalar without
// touching CH), `vector(N)+vector(N)` is rejected by the fold (LHS /
// RHS are Calls, not NumberLiterals) and flows through lowering / emit
// / chDB execution / chclient scan — the exact path where the
// narrowing surfaces.
func TestQuery_VectorVectorSynthBinop_ChDB(t *testing.T) {
	cases := []struct {
		name      string
		query     string
		wantValue string
	}{
		{"add", "vector(1)+vector(1)", "2"},
		{"sub", "vector(3)-vector(1)", "2"},
		{"mul", "vector(2)*vector(3)", "6"},
		{"div", "vector(8)/vector(2)", "4"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newChDBServer(t, gaugeDDL)
			// url.QueryEscape so the binop's `+` isn't decoded to a
			// space by net/http — Grafana sends pre-encoded queries,
			// mirror that here so the parser sees `vector(N)+vector(N)`
			// rather than `vector(N) vector(N)` (the latter is a parse
			// error: `unexpected identifier "vector"`).
			resp, err := http.Get(srv.URL + "/api/v1/query?query=" + url.QueryEscape(tc.query))
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			body := readBody(t, resp)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d body=%s — Grafana PromQL CheckHealth "+
					"probe lands here; a non-200 surfaces as 'Unable to "+
					"connect with Prometheus' on every Grafana page load",
					resp.StatusCode, body)
			}
			var parsed queryResponse
			if err := json.Unmarshal([]byte(body), &parsed); err != nil {
				t.Fatalf("unmarshal: %v\nbody=%s", err, body)
			}
			if parsed.Status != "success" {
				t.Fatalf("status=%q err=%s body=%s", parsed.Status, parsed.Error, body)
			}
			if parsed.Data.ResultType != "vector" {
				t.Fatalf("resultType=%q, want vector; body=%s",
					parsed.Data.ResultType, body)
			}
			rawResult, _ := json.Marshal(parsed.Data.Result)
			var vec []prom.VectorSample
			if err := json.Unmarshal(rawResult, &vec); err != nil {
				t.Fatalf("decode vector: %v", err)
			}
			if len(vec) != 1 {
				t.Fatalf("expected 1 synthetic sample, got %d: %+v", len(vec), vec)
			}
			if got := vec[0].Value[1]; got != tc.wantValue {
				t.Errorf("Value: got %q, want %q (folded V-V scalar)", got, tc.wantValue)
			}
		})
	}
}

func TestQuery_UpstreamError_ChDB(t *testing.T) {
	// NewChDBWithError synthesises a Querier that errors every call —
	// the proxy for an unreachable ClickHouse. The handler should
	// translate that into a 502.
	c := chclienttest.NewChDBWithError(t, errors.New("clickhouse: connection refused"))
	h := prom.New(c, schema.DefaultOTelMetrics(), nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/query?query=up")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d body=%s",
			resp.StatusCode, readBody(t, resp))
	}
}

func TestQueryRange_BadInput_ChDB(t *testing.T) {
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
			srv, _ := newChDBServer(t, gaugeDDL)
			resp, err := http.Get(srv.URL + tc.url)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d body=%s",
					resp.StatusCode, readBody(t, resp))
			}
		})
	}
}

func TestQuery_BadInput_ChDB(t *testing.T) {
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
			srv, _ := newChDBServer(t, gaugeDDL)
			resp, err := http.Get(srv.URL + tc.url)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			body := readBody(t, resp)
			if resp.StatusCode != tc.status {
				t.Fatalf("status: got %d, want %d; body=%s",
					resp.StatusCode, tc.status, body)
			}
			var parsed prom.Response
			if err := json.Unmarshal([]byte(body), &parsed); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if parsed.Status != "error" || parsed.ErrorType != tc.errKey {
				t.Fatalf("got status=%q errorType=%q, want error/%s",
					parsed.Status, parsed.ErrorType, tc.errKey)
			}
		})
	}
}

func TestFormatQuery_ChDB(t *testing.T) {
	srv, _ := newChDBServer(t, gaugeDDL)
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
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed.Status != "success" {
		t.Fatalf("status=%q", parsed.Status)
	}
	if !strings.Contains(parsed.Data, "up") {
		t.Errorf("expected formatted query to contain up; got %q", parsed.Data)
	}
}

func TestParseQuery_ChDB(t *testing.T) {
	srv, _ := newChDBServer(t, gaugeDDL)
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
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed.Status != "success" {
		t.Fatalf("status=%q", parsed.Status)
	}
	if parsed.Data.Type == "" || parsed.Data.Node != "up" {
		t.Errorf("unexpected parse data: %+v", parsed.Data)
	}
}

// TestQuery_CountAgg_ReturnsFloat_ChDB pins the chsql/emit_node.go
// toFloat64(count(...)) wrap for the plain Aggregate path. CH's
// `count()` aggregate returns UInt64; chclient.Sample.Value scans as
// `*float64`; clickhouse-go/v2 refuses the coercion at scan time and
// the handler surfaces it as a 502. The matrix path (range_window.go)
// has long wrapped every reducer in toFloat64; this regression test
// guards the equivalent wrap on the plain Aggregate path so the
// compat-lane `count(metric)` / `count by (...) (metric)` queries
// stop 502'ing.
//
// The seed produces a vector with two label-different samples; the
// outer query collapses them via `count(...)` into a single scalar of
// 2.0. Without the toFloat64 wrap the handler returns 502.
func TestQuery_CountAgg_ReturnsFloat_ChDB(t *testing.T) {
	seedTime := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	ts := seedTime.Format("2006-01-02 15:04:05.000")
	seed := gaugeDDL + fmt.Sprintf(`
INSERT INTO otel_metrics_gauge VALUES
    ('demo_memory_usage_bytes', map('job', 'api'), toDateTime64('%s', 9), 1024.0),
    ('demo_memory_usage_bytes', map('job', 'db'),  toDateTime64('%s', 9), 2048.0);`, ts, ts)

	srv, _ := newChDBServer(t, seed)
	resp, err := http.Get(fmt.Sprintf(
		"%s/api/v1/query?query=count(demo_memory_usage_bytes)&time=%d",
		srv.URL, seedTime.Unix()))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("count(metric): status=%d body=%s", resp.StatusCode, body)
	}
	var parsed queryResponse
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("unmarshal: %v\nbody=%s", err, body)
	}
	if parsed.Status != "success" {
		t.Fatalf("status=%q err=%s", parsed.Status, parsed.Error)
	}
	if parsed.Data.ResultType != "vector" {
		t.Fatalf("resultType=%q, want vector", parsed.Data.ResultType)
	}

	rawResult, _ := json.Marshal(parsed.Data.Result)
	var vec []prom.VectorSample
	if err := json.Unmarshal(rawResult, &vec); err != nil {
		t.Fatalf("decode vector: %v", err)
	}
	if len(vec) != 1 {
		t.Fatalf("expected 1 series (count collapses all labels), got %d: %+v",
			len(vec), vec)
	}
	if got := vec[0].Value[1]; got != "2" {
		t.Errorf("count(metric) value: got %v, want \"2\"", got)
	}
}

// TestQuery_CountAggBy_ReturnsFloat_ChDB is the `count by (...) (metric)`
// variant — same wrap requirement, but the Aggregate carries a
// GroupBy slot so the no-group fast path is bypassed and the regular
// emitAggregate branch produces the SELECT.
func TestQuery_CountAggBy_ReturnsFloat_ChDB(t *testing.T) {
	seedTime := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	ts := seedTime.Format("2006-01-02 15:04:05.000")
	seed := gaugeDDL + fmt.Sprintf(`
INSERT INTO otel_metrics_gauge VALUES
    ('demo_memory_usage_bytes', map('job', 'api', 'instance', 'a'), toDateTime64('%s', 9), 1024.0),
    ('demo_memory_usage_bytes', map('job', 'api', 'instance', 'b'), toDateTime64('%s', 9), 2048.0),
    ('demo_memory_usage_bytes', map('job', 'db', 'instance', 'a'),  toDateTime64('%s', 9), 4096.0);`,
		ts, ts, ts)

	srv, _ := newChDBServer(t, seed)
	resp, err := http.Get(fmt.Sprintf(
		"%s/api/v1/query?query=count%%20by%%20(job)(demo_memory_usage_bytes)&time=%d",
		srv.URL, seedTime.Unix()))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("count by (job): status=%d body=%s", resp.StatusCode, body)
	}
	var parsed queryResponse
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("unmarshal: %v\nbody=%s", err, body)
	}
	if parsed.Status != "success" {
		t.Fatalf("status=%q err=%s", parsed.Status, parsed.Error)
	}

	rawResult, _ := json.Marshal(parsed.Data.Result)
	var vec []prom.VectorSample
	if err := json.Unmarshal(rawResult, &vec); err != nil {
		t.Fatalf("decode vector: %v", err)
	}
	if len(vec) != 2 {
		t.Fatalf("expected 2 grouped series, got %d: %+v", len(vec), vec)
	}
	got := map[string]string{}
	for _, v := range vec {
		got[v.Metric["job"]] = fmt.Sprintf("%v", v.Value[1])
	}
	want := map[string]string{"api": "2", "db": "1"}
	for job, w := range want {
		if got[job] != w {
			t.Errorf("count by (job) for job=%q: got %q, want %q",
				job, got[job], w)
		}
	}
}
