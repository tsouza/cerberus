package prom_test

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// tableMetricNamesQuerier is a stubQuerier whose QueryStrings result
// depends on which metric table the SQL reads. The `__name__` catalog
// endpoint (`/api/v1/label/__name__/values`) issues one UNION query
// per name-shape group — gauge+sum (bare names) and histogram
// (companion-suffix expansion) — so a single flat `strings` stub can't
// distinguish the groups. Keys are bare table names; a table's rows
// contribute whenever the SQL references the backtick-quoted table.
type tableMetricNamesQuerier struct {
	stubQuerier
	byTable map[string][]string
}

func (q *tableMetricNamesQuerier) QueryStrings(_ context.Context, sql string, _ ...any) ([]string, error) {
	var out []string
	for table, rows := range q.byTable {
		if strings.Contains(sql, "`"+table+"`") {
			out = append(out, rows...)
		}
	}
	sort.Strings(out)
	return out, nil
}

// TestConformance_MetricNameCatalogMatchesQuerySurface pins the
// catalog ↔ query-surface invariant for `/api/v1/label/__name__/values`
// (the endpoint Grafana's Drilldown-Metrics enumerates to build its
// preview-panel grid):
//
//   - Gauge/sum-table names advertise verbatim, Prom-grammar-normalised
//     (dotted OTel names like `k8s.node.cpu.usage` surface as
//     `k8s_node_cpu_usage` — the selector lowering's MetricName
//     candidate fan-out makes the underscored alias queryable).
//   - Classic-histogram-table names advertise ONLY as their three
//     companion series (`_bucket` / `_count` / `_sum`) — never the
//     bare base name, because a bare-name selector routes to the
//     gauge/sum tables and returns empty. Reference Prometheus is the
//     model: a classic histogram's `__name__` values contain only the
//     suffixed forms.
//   - A sum-table counter whose name collides with a histogram
//     expansion (`http_server_request_duration_count` stored as a
//     cumulative sum while `http.server.request.duration` lives in the
//     histogram table) dedupes to a single entry.
//
// Round-3 sweep regression (live stack): 83 advertised names, only 10
// queryable — every dotted-stored gauge/sum metric and every bare
// histogram base name rendered an empty Drilldown-Metrics preview
// panel. This test pins the catalog half; the chdb-tagged
// TestCatalog_AdvertisedNamesAreQueryable_ChDB pins the end-to-end
// invariant against real ClickHouse semantics.
func TestConformance_MetricNameCatalogMatchesQuerySurface(t *testing.T) {
	t.Parallel()

	q := &tableMetricNamesQuerier{
		byTable: map[string][]string{
			"otel_metrics_gauge": {
				"k8s.node.cpu.usage", // dotted OTel kubeletstats shape
				"up",                 // already Prom-grammar
			},
			"otel_metrics_sum": {
				"k8s.pod.network.io",                 // dotted counter
				"cerberus_queries_total",             // underscored counter
				"http_server_request_duration_count", // collides with histogram expansion
			},
			"otel_metrics_histogram": {
				"cerberus_queries_duration_seconds", // underscored histogram base
				"http.server.request.duration",      // dotted histogram base
			},
		},
	}
	srv := newServer(q)
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
	if env.Status != "success" {
		t.Fatalf("status=%q body=%s", env.Status, body)
	}

	want := []string{
		"cerberus_queries_duration_seconds_bucket",
		"cerberus_queries_duration_seconds_count",
		"cerberus_queries_duration_seconds_sum",
		"cerberus_queries_total",
		"http_server_request_duration_bucket",
		"http_server_request_duration_count",
		"http_server_request_duration_sum",
		"k8s_node_cpu_usage",
		"k8s_pod_network_io",
		"up",
	}
	if !reflect.DeepEqual(env.Data, want) {
		t.Fatalf("advertised __name__ values mismatch:\n got: %v\nwant: %v", env.Data, want)
	}

	// Belt-and-braces negative pins so a future regression's failure
	// message names the broken invariant directly rather than only the
	// slice diff above.
	got := make(map[string]struct{}, len(env.Data))
	for _, n := range env.Data {
		got[n] = struct{}{}
	}
	for _, bare := range []string{"cerberus_queries_duration_seconds", "http_server_request_duration"} {
		if _, ok := got[bare]; ok {
			t.Errorf("bare classic-histogram base name %q advertised — only the _bucket/_count/_sum companions are queryable", bare)
		}
	}
	for _, dotted := range []string{"k8s.node.cpu.usage", "k8s.pod.network.io", "http.server.request.duration"} {
		if _, ok := got[dotted]; ok {
			t.Errorf("dotted storage spelling %q leaked to the wire — catalog must emit the Prom-grammar form", dotted)
		}
	}
}
