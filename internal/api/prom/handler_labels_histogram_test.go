package prom_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/tsouza/cerberus/internal/chclient"
)

// labelKeysStub captures every QueryStrings invocation so a conformance
// test can assert that the labels / series endpoints fan a bare
// classic-histogram base name out across the three Prom-wire companion
// variants (`<base>_bucket` / `<base>_count` / `<base>_sum`). The other
// stubQuerier in handler_test.go only retains `lastSQL` + `lastArgs`,
// which loses the per-arm history we need to assert here.
type labelKeysStub struct {
	mu sync.Mutex
	// resultsBySQLContains keys are substring matchers; the first key
	// found inside the QueryStrings SQL returns the corresponding
	// values slice. Default (no key matches) returns an empty slice so
	// the union flows through without picking up phantom labels.
	resultsBySQLContains map[string][]string
	sqls                 []string
	// samples mirror stubQuerier's behaviour for the /series surface;
	// the executeInstant path runs the matcher through QueryCursor, so
	// we return one sample per matched series.
	samplesBySQLContains map[string][]chclient.Sample
}

func (s *labelKeysStub) recordSQL(sql string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sqls = append(s.sqls, sql)
}

func (s *labelKeysStub) Query(_ context.Context, sql string, _ ...any) ([]chclient.Sample, error) {
	s.recordSQL(sql)
	return s.matchSamples(sql), nil
}

func (s *labelKeysStub) QueryCursor(_ context.Context, sql string, _ ...any) (chclient.Cursor, error) {
	s.recordSQL(sql)
	return newSliceCursor(s.matchSamples(sql)), nil
}

func (s *labelKeysStub) QueryStrings(_ context.Context, sql string, _ ...any) ([]string, error) {
	s.recordSQL(sql)
	s.mu.Lock()
	defer s.mu.Unlock()
	for needle, vals := range s.resultsBySQLContains {
		if strings.Contains(sql, needle) {
			return vals, nil
		}
	}
	return nil, nil
}

func (s *labelKeysStub) QueryLabelSets(_ context.Context, sql string, _ ...any) ([]map[string]string, error) {
	s.recordSQL(sql)
	return nil, nil
}

func (s *labelKeysStub) QueryMetricMeta(_ context.Context, sql, _ string, _ ...any) ([]chclient.MetricMetaRow, error) {
	s.recordSQL(sql)
	return nil, nil
}

func (s *labelKeysStub) QueryExemplars(_ context.Context, sql string, _ ...any) ([]chclient.ExemplarRow, error) {
	s.recordSQL(sql)
	return nil, nil
}

func (s *labelKeysStub) matchSamples(sql string) []chclient.Sample {
	s.mu.Lock()
	defer s.mu.Unlock()
	for needle, samples := range s.samplesBySQLContains {
		if strings.Contains(sql, needle) {
			return samples
		}
	}
	return nil
}

func (s *labelKeysStub) sqlCount(substr string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, sql := range s.sqls {
		if strings.Contains(sql, substr) {
			n++
		}
	}
	return n
}

// TestLabels_HistogramBareName_FansOutCompanions pins the fix for the
// "Unable to fetch labels" regression Grafana's Metrics Explorer hits
// against a histogram tile. The Metrics Explorer surfaces the bare
// base name from cerberus's `__name__` listing and queries
// `match[]=<base>` for the labels chip — before the fix that matcher
// lowered to a gauge-table scan (TableFor's default), matched zero
// rows, and the chip rendered "Unable to fetch labels".
//
// Asserts (a) the labels handler emits at least one SQL whose FROM
// clause references the histogram table (the `_bucket` companion
// variant), and (b) the response body carries the `le` label that
// only lives on the bucket-fanned rows.
func TestLabels_HistogramBareName_FansOutCompanions(t *testing.T) {
	t.Parallel()

	// The companion fan-out emits four matcher arms (base, _bucket,
	// _count, _sum). We seed the _bucket arm's projection with `le`
	// + `cerberus_ql` to mimic the histogram's stored label keys;
	// the gauge-targeted base arm returns nothing.
	stub := &labelKeysStub{
		resultsBySQLContains: map[string][]string{
			"otel_metrics_histogram": {"le", "cerberus_ql"},
		},
	}
	srv := newServer(stub)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL +
		"/api/v1/labels?match%5B%5D=cerberus_clickhouse_bytes_read")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}

	// (a) the handler must have visited the histogram table.
	if got := stub.sqlCount("otel_metrics_histogram"); got == 0 {
		t.Fatalf("expected at least one SQL targeting otel_metrics_histogram (companion fan-out); got 0\nall SQLs: %v",
			stub.sqls)
	}

	// (b) the response body carries the histogram-specific `le`
	// label — the chip Grafana renders in the Metrics Explorer.
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
	gotLe := false
	for _, name := range env.Data {
		if name == "le" {
			gotLe = true
			break
		}
	}
	if !gotLe {
		t.Errorf("expected `le` label in response (from bucket companion fan-out); got %v", env.Data)
	}
}

// TestLabelValues_HistogramBareName_FansOutCompanions covers the
// label-VALUES counterpart: a bare-name matcher also has to reach
// the histogram table so `/api/v1/label/le/values?match[]=<base>`
// returns the bucket boundaries.
func TestLabelValues_HistogramBareName_FansOutCompanions(t *testing.T) {
	t.Parallel()

	stub := &labelKeysStub{
		resultsBySQLContains: map[string][]string{
			"otel_metrics_histogram": {"0.5", "1", "+Inf"},
		},
	}
	srv := newServer(stub)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL +
		"/api/v1/label/le/values?match%5B%5D=cerberus_clickhouse_bytes_read")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}

	if got := stub.sqlCount("otel_metrics_histogram"); got == 0 {
		t.Fatalf("expected at least one SQL targeting otel_metrics_histogram (companion fan-out); got 0\nall SQLs: %v",
			stub.sqls)
	}

	var env struct {
		Status string   `json:"status"`
		Data   []string `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode: %v body=%s", err, body)
	}
	wantAny := map[string]bool{"0.5": false, "1": false, "+Inf": false}
	for _, v := range env.Data {
		if _, ok := wantAny[v]; ok {
			wantAny[v] = true
		}
	}
	for v, seen := range wantAny {
		if !seen {
			t.Errorf("expected `%s` bucket-boundary value in response; got %v", v, env.Data)
		}
	}
}

// TestLabels_BucketCompanionDoesNotDoubleScan pins the other edge of
// the fan-out: when Grafana sends the `_bucket` companion form
// directly, the handler MUST NOT also fan out into a second `_bucket`
// arm (which would be a wasted scan). The helper's suffix check
// short-circuits the fan-out; this test pins the byte-stable
// single-arm SQL for the companion path.
func TestLabels_BucketCompanionDoesNotDoubleScan(t *testing.T) {
	t.Parallel()

	stub := &labelKeysStub{
		resultsBySQLContains: map[string][]string{
			"otel_metrics_histogram": {"le", "cerberus_ql"},
		},
	}
	srv := newServer(stub)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL +
		"/api/v1/labels?match%5B%5D=cerberus_clickhouse_bytes_read_bucket")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}

	// One QueryStrings call total (the bucket-suffix matcher path
	// lowers to a histogram-table scan in a single arm).
	if got := stub.sqlCount("otel_metrics_histogram"); got != 1 {
		t.Fatalf("expected exactly 1 SQL targeting otel_metrics_histogram; got %d\nall SQLs: %v",
			got, stub.sqls)
	}
}
