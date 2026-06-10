//go:build chdb

// chDB-backed pins for regex `__name__` matchers on the catalog
// surfaces. Grafana's Metrics Drilldown breakdown tab sends
// `match[]={__name__=~".*<metric>.*"}` for every metric it lists, so
// these selectors must resolve on /api/v1/labels and /api/v1/series
// for metrics that are (a) stored in the SUM table — a regex carries
// no literal name for the suffix heuristic, so the scan must fan
// across (Gauge, Sum) instead of defaulting to gauge-only — and
// (b) stored under OTel-dotted names — the catalog advertises the
// Prom-normalised (underscored) spelling, so the regex must also be
// evaluated against `replaceRegexpAll(MetricName, '[^a-zA-Z0-9_:]',
// '_')`, not just the raw stored name. Before the fix both shapes
// returned empty everywhere, blanking the breakdown tab.

package prom_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"testing"
	"time"
)

// regexNameSeed builds gauge + sum tables and seeds a sum-stored
// UpDownCounter (`cerberus_query_inflight`, one row per query language)
// plus a gauge decoy row (`up`) that the regex must filter out. The
// seed timestamp is "now" because the labels / series matcher paths
// anchor their LWR window at request time (no start/end is forwarded
// for /api/v1/labels; /api/v1/series always anchors at now).
func regexNameSeed(t *testing.T) string {
	t.Helper()
	ts := time.Now().UTC().Format("2006-01-02 15:04:05.000")
	return metaShapedGaugeDDL + metaShapedSumDDL + fmt.Sprintf(`
INSERT INTO otel_metrics_gauge (MetricName, MetricDescription, MetricUnit, Attributes, TimeUnix, Value) VALUES
    ('up', '', '', map('job', 'api'), toDateTime64('%[1]s', 9), 1.0);
INSERT INTO otel_metrics_sum (MetricName, MetricDescription, MetricUnit, Attributes, TimeUnix, Value) VALUES
    ('cerberus_query_inflight', '', '', map('cerberus_ql', 'promql'),  toDateTime64('%[1]s', 9), 2.0),
    ('cerberus_query_inflight', '', '', map('cerberus_ql', 'logql'),   toDateTime64('%[1]s', 9), 1.0),
    ('cerberus_query_inflight', '', '', map('cerberus_ql', 'traceql'), toDateTime64('%[1]s', 9), 0.0);`,
		ts)
}

// dottedNameSeed seeds a gauge-stored OTel-dotted metric
// (`container.cpu.usage`) plus a dotted decoy that the underscored
// regex must not match.
func dottedNameSeed(t *testing.T) string {
	t.Helper()
	ts := time.Now().UTC().Format("2006-01-02 15:04:05.000")
	return metaShapedGaugeDDL + metaShapedSumDDL + fmt.Sprintf(`
INSERT INTO otel_metrics_gauge (MetricName, MetricDescription, MetricUnit, Attributes, TimeUnix, Value) VALUES
    ('container.cpu.usage',    '', '', map('core', '0'), toDateTime64('%[1]s', 9), 0.42),
    ('container.memory.usage', '', '', map('core', '0'), toDateTime64('%[1]s', 9), 1024.0);`,
		ts)
}

// decodeStringData unmarshals the `data` array of a labels-shaped
// response and fails the test with the full body on error.
func decodeStringData(t *testing.T, body string) []string {
	t.Helper()
	var parsed metadataResponse
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("unmarshal: %v\nbody=%s", err, body)
	}
	if parsed.Status != "success" {
		t.Fatalf("status=%q err=%s body=%s", parsed.Status, parsed.Error, body)
	}
	var values []string
	if err := json.Unmarshal(parsed.Data, &values); err != nil {
		t.Fatalf("decode data: %v\nbody=%s", err, body)
	}
	return values
}

// decodeSeriesData unmarshals the `data` array of a /api/v1/series
// response into label maps.
func decodeSeriesData(t *testing.T, body string) []map[string]string {
	t.Helper()
	var parsed metadataResponse
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("unmarshal: %v\nbody=%s", err, body)
	}
	if parsed.Status != "success" {
		t.Fatalf("status=%q err=%s body=%s", parsed.Status, parsed.Error, body)
	}
	var sets []map[string]string
	if err := json.Unmarshal(parsed.Data, &sets); err != nil {
		t.Fatalf("decode data: %v\nbody=%s", err, body)
	}
	return sets
}

// TestLabels_RegexNameMatcher_SumStored_ChDB pins the Drilldown
// breakdown-tab shape end-to-end: `/api/v1/labels` with
// `match[]={__name__=~".*cerberus_query_inflight.*"}` must return the
// full label-name set of the sum-stored metric — `__name__` plus
// `cerberus_ql` — not just the unconditional `__name__` entry the
// handler emits even when the matcher matches nothing (which is
// exactly what the gauge-only table fallback produced).
func TestLabels_RegexNameMatcher_SumStored_ChDB(t *testing.T) {
	srv, _ := newChDBServer(t, regexNameSeed(t))

	u := srv.URL + "/api/v1/labels?match%5B%5D=" +
		url.QueryEscape(`{__name__=~".*cerberus_query_inflight.*"}`)
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	values := decodeStringData(t, body)
	want := []string{"__name__", "cerberus_ql"}
	if !equalStringSlice(values, want) {
		t.Errorf("label names: expected %v, got %v", want, values)
	}
}

// TestSeries_RegexNameMatcher_SumStored_ChDB asserts /api/v1/series
// with the same regex matcher returns all three per-language series of
// the sum-stored metric, each carrying the `__name__` +
// `cerberus_ql` labels Grafana renders as chips.
func TestSeries_RegexNameMatcher_SumStored_ChDB(t *testing.T) {
	srv, _ := newChDBServer(t, regexNameSeed(t))

	u := srv.URL + "/api/v1/series?match%5B%5D=" +
		url.QueryEscape(`{__name__=~".*cerberus_query_inflight.*"}`)
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	sets := decodeSeriesData(t, body)
	if len(sets) != 3 {
		t.Fatalf("expected 3 series, got %d: %v\nbody=%s", len(sets), sets, body)
	}
	gotQL := make([]string, 0, len(sets))
	for _, lset := range sets {
		if lset["__name__"] != "cerberus_query_inflight" {
			t.Errorf("series __name__: expected cerberus_query_inflight, got %q (%v)",
				lset["__name__"], lset)
		}
		gotQL = append(gotQL, lset["cerberus_ql"])
	}
	sort.Strings(gotQL)
	wantQL := []string{"logql", "promql", "traceql"}
	if !equalStringSlice(gotQL, wantQL) {
		t.Errorf("cerberus_ql values: expected %v, got %v", wantQL, gotQL)
	}
}

// TestSeries_RegexNameMatcher_UnderscoredMatchesDotted_ChDB pins the
// Prom-normalisation bridge for regex values: the catalog advertises
// `container_cpu_usage` for the dotted-stored `container.cpu.usage`,
// and Drilldown echoes the underscored spelling back inside
// `match[]={__name__=~".*container_cpu_usage.*"}`. The series must
// resolve (via the normalised-MetricName regex arm) and surface under
// the same Prom-grammar name the catalog advertised. The dotted decoy
// (`container.memory.usage`) must stay excluded.
func TestSeries_RegexNameMatcher_UnderscoredMatchesDotted_ChDB(t *testing.T) {
	srv, _ := newChDBServer(t, dottedNameSeed(t))

	u := srv.URL + "/api/v1/series?match%5B%5D=" +
		url.QueryEscape(`{__name__=~".*container_cpu_usage.*"}`)
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	sets := decodeSeriesData(t, body)
	if len(sets) != 1 {
		t.Fatalf("expected exactly 1 series, got %d: %v\nbody=%s", len(sets), sets, body)
	}
	want := map[string]string{"__name__": "container_cpu_usage", "core": "0"}
	got := sets[0]
	if len(got) != len(want) {
		t.Fatalf("series labels: expected %v, got %v", want, got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("series label %q: expected %q, got %q", k, v, got[k])
		}
	}
}

// TestLabels_RegexNameMatcher_UnderscoredMatchesDotted_ChDB is the
// /api/v1/labels sibling of the series pin above: the underscored
// regex must surface the dotted-stored metric's label-name set
// (`__name__` + `core`), proving labelKeysForMatcher's lowering picks
// up the normalised-MetricName regex arm.
func TestLabels_RegexNameMatcher_UnderscoredMatchesDotted_ChDB(t *testing.T) {
	srv, _ := newChDBServer(t, dottedNameSeed(t))

	u := srv.URL + "/api/v1/labels?match%5B%5D=" +
		url.QueryEscape(`{__name__=~".*container_cpu_usage.*"}`)
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	values := decodeStringData(t, body)
	want := []string{"__name__", "core"}
	if !equalStringSlice(values, want) {
		t.Errorf("label names: expected %v, got %v", want, values)
	}
}
