//go:build chdb

// chDB-backed coverage for `/api/v1/label/<name>/values?match[]=` and
// `/api/v1/metadata?limit=N`. Mirrors the stub-only tests in
// handler_label_values_matched_test.go end-to-end so the lowering →
// emit → chDB execute round trip is asserted against ClickHouse
// semantics rather than a stubbed Querier.
//
// Backed by issue/PR-style ask in #375 (coverage audit) — these are
// the Layer 7 conformance fills for the
// `fetchLabelValuesMatched`/`labelValuesForMatcher`/`truncateMetadata`
// trio flagged at 0% coverage.

package prom_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/prom"
)

// metaShapedGaugeDDL is the gaugeDDL extended with MetricDescription /
// MetricUnit columns so the metadata handler's per-table SELECT
// projects against a non-empty column set. Mirrors the production
// OTel-CH gauge table's relevant subset.
const metaShapedGaugeDDL = `CREATE TABLE otel_metrics_gauge (
    MetricName String,
    MetricDescription String,
    MetricUnit String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree() ORDER BY (MetricName, TimeUnix);`

// metaShapedSumDDL mirrors metaShapedGaugeDDL for the sum (counter)
// table — needed so the metadata handler's per-table fetch doesn't 502
// on missing-table errors. Same columns as gauge — the metadata SQL
// only reads (MetricName, MetricDescription, MetricUnit).
const metaShapedSumDDL = `CREATE TABLE otel_metrics_sum (
    MetricName String,
    MetricDescription String,
    MetricUnit String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree() ORDER BY (MetricName, TimeUnix);`

// metaShapedHistogramDDL completes the trio. The metadata-handler SQL
// reads the same three columns regardless of table kind; the histogram
// kind only matters when query / range-query SQL targets the table.
const metaShapedHistogramDDL = `CREATE TABLE otel_metrics_histogram (
    MetricName String,
    MetricDescription String,
    MetricUnit String,
    Attributes Map(String, String),
    TimeUnix DateTime64(9),
    BucketCounts Array(UInt64),
    ExplicitBounds Array(Float64)
) ENGINE = MergeTree() ORDER BY (MetricName, TimeUnix);`

// TestLabelValues_MatchSelector_ChDB pins the
// `fetchLabelValuesMatched` → `labelValuesForMatcher` → chsql roundtrip
// against a real chDB session. Seeds two `up` rows with distinct
// `job` labels and asserts the handler returns both as the label/values
// result for the `up{instance="h1:8080"}` selector matching one of them.
func TestLabelValues_MatchSelector_ChDB(t *testing.T) {
	seedTime := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	ts := seedTime.Format("2006-01-02 15:04:05.000")
	// All three metric tables are created because fetchLabelValuesMatched
	// fans a bare classic-histogram base name out across the histogram
	// table via expandBareHistogramMatcher; the companion variants
	// (`up_bucket` / `up_count` / `up_sum`) lower to the histogram table
	// and chDB errors on missing-table reads. The histogram + sum tables
	// stay empty — only gauge carries rows.
	seed := metaShapedGaugeDDL + metaShapedSumDDL + metaShapedHistogramDDL + fmt.Sprintf(`
INSERT INTO otel_metrics_gauge (MetricName, MetricDescription, MetricUnit, Attributes, TimeUnix, Value) VALUES
    ('up', 'scrape ok', '', map('job', 'api', 'instance', 'h1:8080'), toDateTime64('%s', 9), 1.0),
    ('up', 'scrape ok', '', map('job', 'db',  'instance', 'h1:8080'), toDateTime64('%s', 9), 0.0),
    ('up', 'scrape ok', '', map('job', 'web', 'instance', 'h2:8080'), toDateTime64('%s', 9), 1.0);`,
		ts, ts, ts)

	srv, _ := newChDBServer(t, seed)
	// Anchor the matcher's LWR window so it includes seedTime. With the
	// default 5-minute instant lookback, end == seedTime keeps the seed
	// inside the non-strict upper / strict lower bound window.
	start := seedTime.Add(-5 * time.Minute).Unix()
	end := seedTime.Unix()
	url := srv.URL + "/api/v1/label/job/values?" +
		"match%5B%5D=up%7Binstance%3D%22h1%3A8080%22%7D" +
		fmt.Sprintf("&start=%d&end=%d", start, end)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}

	var parsed metadataResponse
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("unmarshal: %v\nbody=%s", err, body)
	}
	if parsed.Status != "success" {
		t.Fatalf("status=%q err=%s", parsed.Status, parsed.Error)
	}
	var values []string
	if err := json.Unmarshal(parsed.Data, &values); err != nil {
		t.Fatalf("decode data: %v\nbody=%s", err, body)
	}

	// instance="h1:8080" matches two rows (job=api, job=db). Sorted
	// alphabetic by fetchLabelValuesMatched. The job=web row is
	// excluded by the matcher.
	sort.Strings(values)
	want := []string{"api", "db"}
	if len(values) != len(want) {
		t.Fatalf("expected %v, got %v", want, values)
	}
	for i, w := range want {
		if values[i] != w {
			t.Errorf("values[%d]: got %q, want %q", i, values[i], w)
		}
	}
}

// TestLabelValues_MatchSelector_Regex_ChDB exercises the regex matcher
// `{job=~".+"}` against the chDB-backed labelValuesForMatcher path. The
// matcher has no `__name__=` so it lowers against the default gauge
// table; the predicate becomes `match(Attributes['job'], '^(?:.+)$')`.
// Asserts the handler returns the union of `job` values across the
// matched gauge rows.
func TestLabelValues_MatchSelector_Regex_ChDB(t *testing.T) {
	seedTime := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	ts := seedTime.Format("2006-01-02 15:04:05.000")
	seed := metaShapedGaugeDDL + fmt.Sprintf(`
INSERT INTO otel_metrics_gauge (MetricName, MetricDescription, MetricUnit, Attributes, TimeUnix, Value) VALUES
    ('up', '', '', map('job', 'api'), toDateTime64('%s', 9), 1.0),
    ('up', '', '', map('job', 'db'),  toDateTime64('%s', 9), 1.0);`,
		ts, ts)

	srv, _ := newChDBServer(t, seed)
	start := seedTime.Add(-5 * time.Minute).Unix()
	end := seedTime.Unix()
	url := srv.URL + "/api/v1/label/job/values?" +
		"match%5B%5D=%7Bjob%3D~%22.%2B%22%7D" +
		fmt.Sprintf("&start=%d&end=%d", start, end)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var parsed metadataResponse
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var values []string
	if err := json.Unmarshal(parsed.Data, &values); err != nil {
		t.Fatalf("decode data: %v", err)
	}
	sort.Strings(values)
	want := []string{"api", "db"}
	if !equalStringSlice(values, want) {
		t.Errorf("expected %v, got %v", want, values)
	}
}

// TestLabelValues_MatchSelector_Multiple_ChDB pins the union-across-
// selectors path in fetchLabelValuesMatched: two `match[]=` selectors,
// each matching a disjoint slice of rows, and the handler emits the
// dedup'd union.
func TestLabelValues_MatchSelector_Multiple_ChDB(t *testing.T) {
	seedTime := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	ts := seedTime.Format("2006-01-02 15:04:05.000")
	// Histogram + sum tables are created (empty) so the bare-name
	// classic-histogram companion fan-out
	// (expandBareHistogramMatcher) finds the histogram table when it
	// probes `up_bucket` / `up_count` / `up_sum`.
	seed := metaShapedGaugeDDL + metaShapedSumDDL + metaShapedHistogramDDL + fmt.Sprintf(`
INSERT INTO otel_metrics_gauge (MetricName, MetricDescription, MetricUnit, Attributes, TimeUnix, Value) VALUES
    ('up',   '', '', map('job', 'api'), toDateTime64('%s', 9), 1.0),
    ('up',   '', '', map('job', 'db'),  toDateTime64('%s', 9), 1.0),
    ('down', '', '', map('job', 'web'), toDateTime64('%s', 9), 1.0);`,
		ts, ts, ts)

	srv, _ := newChDBServer(t, seed)
	start := seedTime.Add(-5 * time.Minute).Unix()
	end := seedTime.Unix()
	url := srv.URL + "/api/v1/label/job/values?" +
		"match%5B%5D=up&match%5B%5D=down" +
		fmt.Sprintf("&start=%d&end=%d", start, end)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var parsed metadataResponse
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var values []string
	if err := json.Unmarshal(parsed.Data, &values); err != nil {
		t.Fatalf("decode data: %v", err)
	}
	sort.Strings(values)
	want := []string{"api", "db", "web"}
	if !equalStringSlice(values, want) {
		t.Errorf("expected %v, got %v", want, values)
	}
}

// TestLabelValues_MatchSelector_Empty_ChDB pins the empty-result path:
// a matcher targeting a metric that doesn't exist in the seed yields
// `data: []` (not null).
func TestLabelValues_MatchSelector_Empty_ChDB(t *testing.T) {
	seedTime := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	ts := seedTime.Format("2006-01-02 15:04:05.000")
	// Histogram + sum tables are created (empty) so the bare-name
	// fan-out for `does_not_exist` finds the histogram table when it
	// probes `does_not_exist_bucket` / `_count` / `_sum`.
	seed := metaShapedGaugeDDL + metaShapedSumDDL + metaShapedHistogramDDL + fmt.Sprintf(`
INSERT INTO otel_metrics_gauge (MetricName, MetricDescription, MetricUnit, Attributes, TimeUnix, Value) VALUES
    ('up', '', '', map('job', 'api'), toDateTime64('%s', 9), 1.0);`, ts)

	srv, _ := newChDBServer(t, seed)
	url := srv.URL + "/api/v1/label/job/values?" +
		"match%5B%5D=does_not_exist"
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, `"data":[]`) {
		t.Errorf("expected `data:[]` for empty match; got %s", body)
	}
}

// TestLabelValues_MatchSelector_MetricName_ChDB exercises the
// `__name__` branch of labelValuesForMatcher under chDB: the SELECT
// projects DISTINCT MetricName from the matcher subquery (not
// Attributes['__name__']).
func TestLabelValues_MatchSelector_MetricName_ChDB(t *testing.T) {
	seedTime := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	ts := seedTime.Format("2006-01-02 15:04:05.000")
	seed := metaShapedGaugeDDL + fmt.Sprintf(`
INSERT INTO otel_metrics_gauge (MetricName, MetricDescription, MetricUnit, Attributes, TimeUnix, Value) VALUES
    ('up',                  '', '', map('job', 'api'), toDateTime64('%s', 9), 1.0),
    ('http_requests_total', '', '', map('job', 'api'), toDateTime64('%s', 9), 1.0),
    ('http_requests_total', '', '', map('job', 'db'),  toDateTime64('%s', 9), 1.0);`,
		ts, ts, ts)

	srv, _ := newChDBServer(t, seed)
	start := seedTime.Add(-5 * time.Minute).Unix()
	end := seedTime.Unix()
	url := srv.URL + "/api/v1/label/__name__/values?" +
		"match%5B%5D=%7Bjob%3D%22api%22%7D" +
		fmt.Sprintf("&start=%d&end=%d", start, end)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var parsed metadataResponse
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var values []string
	if err := json.Unmarshal(parsed.Data, &values); err != nil {
		t.Fatalf("decode data: %v", err)
	}
	sort.Strings(values)
	want := []string{"http_requests_total", "up"}
	if !equalStringSlice(values, want) {
		t.Errorf("expected %v, got %v", want, values)
	}
}

// TestMetadata_TruncateAtLimit_ChDB pins truncateMetadata's
// alphabetic-truncation behaviour end-to-end. Seeds five gauge metrics
// with distinct names; with limit=2 the handler returns just the first
// two in sorted order.
//
// Note: the metadata handler hits gauge / sum / histogram tables in
// sequence, so all three need to exist (or chDB fails the table-not-
// found check). The seed creates all three; only the gauge table
// carries rows.
func TestMetadata_TruncateAtLimit_ChDB(t *testing.T) {
	seedTime := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	ts := seedTime.Format("2006-01-02 15:04:05.000")
	seed := metaShapedGaugeDDL + metaShapedSumDDL + metaShapedHistogramDDL + fmt.Sprintf(`
INSERT INTO otel_metrics_gauge (MetricName, MetricDescription, MetricUnit, Attributes, TimeUnix, Value) VALUES
    ('alpha',   'a desc', 'a_unit', map(), toDateTime64('%s', 9), 1.0),
    ('beta',    'b desc', 'b_unit', map(), toDateTime64('%s', 9), 1.0),
    ('gamma',   'g desc', 'g_unit', map(), toDateTime64('%s', 9), 1.0),
    ('delta',   'd desc', 'd_unit', map(), toDateTime64('%s', 9), 1.0),
    ('epsilon', 'e desc', 'e_unit', map(), toDateTime64('%s', 9), 1.0);`,
		ts, ts, ts, ts, ts)

	srv, _ := newChDBServer(t, seed)
	resp, err := http.Get(srv.URL + "/api/v1/metadata?limit=2")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}

	var parsed metadataResponse
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var grouped map[string][]prom.MetricMetaEntry
	if err := json.Unmarshal(parsed.Data, &grouped); err != nil {
		t.Fatalf("decode data: %v\nbody=%s", err, body)
	}

	if len(grouped) != 2 {
		t.Fatalf("expected 2 entries after truncate, got %d: keys=%v",
			len(grouped), keysOf(grouped))
	}
	for _, want := range []string{"alpha", "beta"} {
		entries, ok := grouped[want]
		if !ok {
			t.Errorf("expected %q to survive truncate; keys=%v",
				want, keysOf(grouped))
			continue
		}
		if len(entries) == 0 {
			t.Errorf("expected at least one entry for %q", want)
		}
	}
	for _, drop := range []string{"delta", "epsilon", "gamma"} {
		if _, ok := grouped[drop]; ok {
			t.Errorf("expected %q to be truncated; keys=%v",
				drop, keysOf(grouped))
		}
	}
}

// TestMetadata_LimitAboveCount_ChDB pins the second early-return guard
// in truncateMetadata against chDB: limit exceeds the seeded count, so
// the handler must return every row untouched.
func TestMetadata_LimitAboveCount_ChDB(t *testing.T) {
	seedTime := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	ts := seedTime.Format("2006-01-02 15:04:05.000")
	seed := metaShapedGaugeDDL + metaShapedSumDDL + metaShapedHistogramDDL + fmt.Sprintf(`
INSERT INTO otel_metrics_gauge (MetricName, MetricDescription, MetricUnit, Attributes, TimeUnix, Value) VALUES
    ('alpha', 'a', '', map(), toDateTime64('%s', 9), 1.0),
    ('beta',  'b', '', map(), toDateTime64('%s', 9), 1.0);`, ts, ts)

	srv, _ := newChDBServer(t, seed)
	resp, err := http.Get(srv.URL + "/api/v1/metadata?limit=100")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var parsed metadataResponse
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var grouped map[string][]prom.MetricMetaEntry
	if err := json.Unmarshal(parsed.Data, &grouped); err != nil {
		t.Fatalf("decode data: %v", err)
	}
	if len(grouped) != 2 {
		t.Fatalf("limit > count should leave the map untouched; "+
			"got %d entries: keys=%v", len(grouped), keysOf(grouped))
	}
}

// equalStringSlice is a small helper used by the chDB label-values
// tests; both slices are expected pre-sorted.
func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// keysOf collects the keys of the metadata-grouped map for diagnostic
// output when an assertion fails.
func keysOf(m map[string][]prom.MetricMetaEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestLabelValues_DottedSource_ChDB pins task #215 N4: a request to
// `/api/v1/label/cerberus_ql/values` against rows that store the OTel-
// canonical dotted sibling `cerberus.ql` (no underscored sibling)
// must surface the dotted-storage values. Without the multi-candidate
// fan-out in unionLabelValuesSQL the endpoint returns `[]` and
// Grafana's label picker shows no entries for the language partition.
//
// Seeds three rows under `cerberus.ql` (one per language) and asserts
// the handler returns the three values plus the `route` label values
// for the unrelated sanity check (`route` carries no internal
// underscore so it hits the byte-stable single-arm path).
func TestLabelValues_DottedSource_ChDB(t *testing.T) {
	seedTime := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	ts := seedTime.Format("2006-01-02 15:04:05.000")
	// Seed into otel_metrics_sum: `cerberus_queries_total` carries the
	// `_total` suffix so Metrics.TableFor routes the matched-listing
	// path's `match[]=cerberus_queries_total` selector to the sum table.
	// The unmatched-listing path scans every metric table via
	// metricTables(), so the values still surface for Pin 1 — and the
	// gauge / histogram tables are still created so each UNION arm
	// targets a real table (chDB errors on missing-table reads).
	seed := metaShapedGaugeDDL + metaShapedSumDDL + metaShapedHistogramDDL + fmt.Sprintf(`
INSERT INTO otel_metrics_sum (MetricName, MetricDescription, MetricUnit, Attributes, TimeUnix, Value) VALUES
    ('cerberus_queries_total', '', '', map('cerberus.ql', 'promql',  'route', '/api/v1/query'),  toDateTime64('%s', 9), 1.0),
    ('cerberus_queries_total', '', '', map('cerberus.ql', 'logql',   'route', '/loki/api/query'), toDateTime64('%s', 9), 1.0),
    ('cerberus_queries_total', '', '', map('cerberus.ql', 'traceql', 'route', '/api/traces'),     toDateTime64('%s', 9), 1.0);`,
		ts, ts, ts)

	srv, _ := newChDBServer(t, seed)

	// Pin 1: unmatched listing — `/api/v1/label/cerberus_ql/values`
	// surfaces every dotted-source value.
	resp, err := http.Get(srv.URL + "/api/v1/label/cerberus_ql/values")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var parsed metadataResponse
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("unmarshal: %v\nbody=%s", err, body)
	}
	var values []string
	if err := json.Unmarshal(parsed.Data, &values); err != nil {
		t.Fatalf("decode data: %v\nbody=%s", err, body)
	}
	sort.Strings(values)
	want := []string{"logql", "promql", "traceql"}
	if !equalStringSlice(values, want) {
		t.Errorf("unmatched listing: expected %v, got %v", want, values)
	}

	// Pin 2: matched listing — a `match[]` selector that touches the
	// dotted-key rows still fans out across candidates so the SELECT
	// projection picks up the dotted form. Use a matcher on `route`
	// (no internal underscore → bare lookup) to scope the rows, then
	// project `cerberus_ql` values across them.
	start := seedTime.Add(-5 * time.Minute).Unix()
	end := seedTime.Unix()
	url := srv.URL + "/api/v1/label/cerberus_ql/values?" +
		"match%5B%5D=cerberus_queries_total" +
		fmt.Sprintf("&start=%d&end=%d", start, end)
	resp, err = http.Get(url)
	if err != nil {
		t.Fatalf("GET match: %v", err)
	}
	body = readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("unmarshal match: %v\nbody=%s", err, body)
	}
	values = nil
	if err := json.Unmarshal(parsed.Data, &values); err != nil {
		t.Fatalf("decode data match: %v\nbody=%s", err, body)
	}
	sort.Strings(values)
	if !equalStringSlice(values, want) {
		t.Errorf("matched listing: expected %v, got %v", want, values)
	}
}
