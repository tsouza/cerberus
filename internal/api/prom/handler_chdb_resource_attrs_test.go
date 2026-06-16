//go:build chdb

// chDB-backed COMPLETENESS ORACLE for rc.5 (task #123): projecting OTel
// ResourceAttributes as Prometheus labels. Seeds rows whose resource
// attributes (k8s.namespace.name, k8s.pod.name) live ONLY in the
// ResourceAttributes Map column — never in the per-datapoint Attributes
// map — then asserts every read surface exposes AND filters on them:
//
//   - /api/v1/labels             — k8s_namespace_name / k8s_pod_name listed
//   - /api/v1/label/<n>/values   — resource values returned
//   - /api/v1/series             — series carry the sanitized resource label
//   - /api/v1/query (selector)   — bare selector surfaces the label, and the
//                                  {k8s_namespace_name="prod"} matcher filters
//   - precedence                 — a key in BOTH maps resolves to the
//                                  Attributes value (metric-level wins)
//
// This is the missing oracle the TXTAR fixtures alone don't cover: the
// fixtures pin the SQL shape + a single-arm roundtrip, but only this test
// drives the FULL handler stack (metadata catalog builders + the selector
// seam) over one shared seed and asserts the four surfaces agree.

package prom_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclienttest"
	"github.com/tsouza/cerberus/internal/schema"
)

// newChDBServerWithResourceAllowlist builds a handler whose schema pins
// the CERBERUS_PROM_RESOURCE_LABELS allowlist to allow, then seeds it.
func newChDBServerWithResourceAllowlist(t *testing.T, seed string, allow []string) *httptest.Server {
	t.Helper()
	c := chclienttest.NewChDB(t)
	c.Seed(t, seed)
	s := schema.DefaultOTelMetrics()
	s.PromResourceLabels = allow
	h := prom.New(c, s, nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// resourceAttrGaugeDDL / resourceAttrSumDDL / resourceAttrHistogramDDL
// carry the ResourceAttributes Map column the production OTel-CH tables
// declare (internal/schema/otel.go::DefaultOTelMetrics). All three exist
// because the metadata fan-out reads the histogram + sum tables even when
// only the sum table is seeded.
const resourceAttrGaugeDDL = `CREATE TABLE otel_metrics_gauge (
    MetricName String,
    Attributes Map(String, String),
    ResourceAttributes Map(String, String),
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree() ORDER BY (MetricName, TimeUnix);`

const resourceAttrSumDDL = `CREATE TABLE otel_metrics_sum (
    MetricName String,
    Attributes Map(String, String),
    ResourceAttributes Map(String, String),
    TimeUnix DateTime64(9),
    Value Float64
) ENGINE = MergeTree() ORDER BY (MetricName, TimeUnix);`

const resourceAttrHistogramDDL = `CREATE TABLE otel_metrics_histogram (
    MetricName String,
    Attributes Map(String, String),
    ResourceAttributes Map(String, String),
    TimeUnix DateTime64(9),
    Count UInt64,
    Sum Float64,
    BucketCounts Array(UInt64),
    ExplicitBounds Array(Float64)
) ENGINE = MergeTree() ORDER BY (MetricName, TimeUnix);`

// resourceAttrSeed seeds three sum rows for `http_requests_total`:
//   - prod/pod-a: namespace+pod ONLY in ResourceAttributes, route in Attributes
//   - prod/pod-b: same namespace, different pod (proves /series fans pods)
//   - staging/pod-c: a second namespace the matcher must exclude
//
// `region` is present in BOTH maps (us-east in Attributes, us-west in
// ResourceAttributes) on pod-a to exercise the Attributes-win precedence
// contract end-to-end. Seeded at "now" because /labels + /series + the
// selector LWR all anchor their window at request time.
func resourceAttrSeed(t *testing.T) string {
	t.Helper()
	// Seed at "now" so the /labels + /series catalog surfaces (which
	// anchor their LWR window at request time, no start/end forwarded)
	// and the selector instant query (whose `time` we pin to a moment
	// just after now) both fall inside the 5-minute staleness window.
	ts := time.Now().UTC().Format("2006-01-02 15:04:05.000")
	return resourceAttrGaugeDDL + resourceAttrSumDDL + resourceAttrHistogramDDL + fmt.Sprintf(`
INSERT INTO otel_metrics_sum (MetricName, Attributes, ResourceAttributes, TimeUnix, Value) VALUES
    ('http_requests_total', map('route', '/api', 'region', 'us-east'), map('k8s.namespace.name', 'prod', 'k8s.pod.name', 'pod-a', 'region', 'us-west'), toDateTime64('%[1]s', 9), 5.0),
    ('http_requests_total', map('route', '/api'),                       map('k8s.namespace.name', 'prod', 'k8s.pod.name', 'pod-b'),                       toDateTime64('%[1]s', 9), 7.0),
    ('http_requests_total', map('route', '/admin'),                     map('k8s.namespace.name', 'staging', 'k8s.pod.name', 'pod-c'),                    toDateTime64('%[1]s', 9), 3.0);`,
		ts)
}

// TestResourceAttrs_Labels_ChDB pins /api/v1/labels: the sanitized
// resource-attribute names (k8s_namespace_name, k8s_pod_name) must appear
// alongside the metric Attributes keys (region, route) + __name__.
func TestResourceAttrs_Labels_ChDB(t *testing.T) {
	srv, _ := newChDBServer(t, resourceAttrSeed(t))

	resp, err := http.Get(srv.URL + "/api/v1/labels")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	got := decodeStringData(t, body)
	gotSet := make(map[string]struct{}, len(got))
	for _, n := range got {
		gotSet[n] = struct{}{}
	}
	for _, want := range []string{"__name__", "region", "route", "k8s_namespace_name", "k8s_pod_name"} {
		if _, ok := gotSet[want]; !ok {
			t.Errorf("/labels missing %q; got %v", want, got)
		}
	}
}

// TestResourceAttrs_LabelValues_ChDB pins /api/v1/label/<n>/values for a
// resource-only label: the namespace values stored ONLY in
// ResourceAttributes must surface.
func TestResourceAttrs_LabelValues_ChDB(t *testing.T) {
	srv, _ := newChDBServer(t, resourceAttrSeed(t))

	resp, err := http.Get(srv.URL + "/api/v1/label/k8s_namespace_name/values")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	got := decodeStringData(t, body)
	sort.Strings(got)
	want := []string{"prod", "staging"}
	if !equalStringSlice(got, want) {
		t.Errorf("/label/k8s_namespace_name/values: expected %v, got %v", want, got)
	}
}

// TestResourceAttrs_Series_ChDB pins /api/v1/series: each series must
// carry the sanitized resource labels (k8s_namespace_name, k8s_pod_name)
// projected from ResourceAttributes, with the Attributes-win precedence
// on `region` (the pod-a series reports us-east, the Attributes value, not
// the us-west ResourceAttributes value).
func TestResourceAttrs_Series_ChDB(t *testing.T) {
	srv, _ := newChDBServer(t, resourceAttrSeed(t))

	resp, err := http.Get(srv.URL + "/api/v1/series?match%5B%5D=" +
		url.QueryEscape(`http_requests_total`))
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
	var sawPodA bool
	for _, lset := range sets {
		if lset["k8s_namespace_name"] == "" {
			t.Errorf("series missing k8s_namespace_name: %v", lset)
		}
		if lset["k8s_pod_name"] == "" {
			t.Errorf("series missing k8s_pod_name: %v", lset)
		}
		if lset["k8s_pod_name"] == "pod-a" {
			sawPodA = true
			if lset["region"] != "us-east" {
				t.Errorf("precedence: pod-a region should be Attributes value us-east, got %q", lset["region"])
			}
		}
	}
	if !sawPodA {
		t.Errorf("pod-a series not present: %v", sets)
	}
}

// TestResourceAttrs_SelectorQuery_ChDB pins the instant /api/v1/query
// path: a bare selector surfaces the resource labels, and the
// {k8s_namespace_name="prod"} matcher filters to ONLY the prod rows
// (excluding staging) — the matcher and the projection agree on the same
// merged label map (the precedence contract).
func TestResourceAttrs_SelectorQuery_ChDB(t *testing.T) {
	srv, _ := newChDBServer(t, resourceAttrSeed(t))
	// Anchor the instant query's eval a minute after the seed so the seed
	// rows sit strictly inside the LWR window (TimeUnix <= eval AND
	// TimeUnix > eval-5m) regardless of sub-second seed/query skew.
	now := time.Now().Add(time.Minute).Unix()

	// Bare selector: both prod pods + staging pod surface, each carrying
	// k8s_namespace_name from ResourceAttributes.
	bare := decodeVectorQuery(t, srv.URL, `http_requests_total`, now)
	if len(bare) != 3 {
		t.Fatalf("bare selector: expected 3 series, got %d: %v", len(bare), bare)
	}
	for _, s := range bare {
		if s.Metric["k8s_namespace_name"] == "" {
			t.Errorf("bare selector series missing k8s_namespace_name: %v", s.Metric)
		}
	}

	// Matcher: {k8s_namespace_name="prod"} filters on ResourceAttributes.
	// Only the two prod pods survive; staging is excluded.
	matched := decodeVectorQuery(t, srv.URL, `http_requests_total{k8s_namespace_name="prod"}`, now)
	if len(matched) != 2 {
		t.Fatalf("matched selector: expected 2 prod series, got %d: %v", len(matched), matched)
	}
	for _, s := range matched {
		if s.Metric["k8s_namespace_name"] != "prod" {
			t.Errorf("matched selector leaked non-prod series: %v", s.Metric)
		}
	}
}

// TestResourceAttrs_Allowlist_NarrowsLabels_ChDB pins the
// CERBERUS_PROM_RESOURCE_LABELS allowlist end-to-end on /api/v1/labels:
// with the allowlist pinned to ONLY k8s.namespace.name, the namespace
// surfaces but k8s_pod_name (a resource key outside the allowlist) does
// NOT, while the metric Attributes keys (route, region) stay listed.
func TestResourceAttrs_Allowlist_NarrowsLabels_ChDB(t *testing.T) {
	srv := newChDBServerWithResourceAllowlist(t, resourceAttrSeed(t), []string{"k8s.namespace.name"})

	resp, err := http.Get(srv.URL + "/api/v1/labels")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	got := decodeStringData(t, body)
	gotSet := make(map[string]struct{}, len(got))
	for _, n := range got {
		gotSet[n] = struct{}{}
	}
	if _, ok := gotSet["k8s_namespace_name"]; !ok {
		t.Errorf("allowlisted k8s_namespace_name missing from /labels: %v", got)
	}
	if _, ok := gotSet["k8s_pod_name"]; ok {
		t.Errorf("non-allowlisted k8s_pod_name leaked into /labels: %v", got)
	}
	// Metric Attributes keys are always listed regardless of the
	// resource allowlist.
	for _, want := range []string{"route", "region", "__name__"} {
		if _, ok := gotSet[want]; !ok {
			t.Errorf("/labels missing always-on key %q: %v", want, got)
		}
	}
}

// vectorSample is the decoded shape of one /api/v1/query vector element.
type vectorSample struct {
	Metric map[string]string `json:"metric"`
	Value  [2]any            `json:"value"`
}

// decodeVectorQuery issues an instant query and decodes the vector result.
func decodeVectorQuery(t *testing.T, base, query string, ts int64) []vectorSample {
	t.Helper()
	u := fmt.Sprintf("%s/api/v1/query?query=%s&time=%d", base, url.QueryEscape(query), ts)
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET %s: %v", query, err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("query %q status=%d body=%s", query, resp.StatusCode, body)
	}
	var parsed struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ResultType string         `json:"resultType"`
			Result     []vectorSample `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("unmarshal %q: %v\nbody=%s", query, err, body)
	}
	if parsed.Status != "success" {
		t.Fatalf("query %q status=%q err=%s", query, parsed.Status, parsed.Error)
	}
	if parsed.Data.ResultType != "vector" {
		t.Fatalf("query %q resultType=%q, want vector", query, parsed.Data.ResultType)
	}
	return parsed.Data.Result
}
