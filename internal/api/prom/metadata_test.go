package prom_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
)

// metadataResponse decodes the Prom metadata-endpoint shape — `data` is a
// direct slice rather than a {resultType, result} wrapper.
type metadataResponse struct {
	Status string          `json:"status"`
	Data   json.RawMessage `json:"data"`
	Error  string          `json:"error"`
}

func TestLabels_Endpoint(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{
		strings: []string{"foo", "bar", "instance"},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/labels")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	var parsed metadataResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	defer resp.Body.Close()

	if parsed.Status != "success" {
		t.Fatalf("status: got %q, want success; err=%s", parsed.Status, parsed.Error)
	}

	var names []string
	if err := json.Unmarshal(parsed.Data, &names); err != nil {
		t.Fatalf("decode data: %v", err)
	}

	// `__name__` is always prepended; result is sorted.
	if len(names) < 1 || names[0] != "__name__" {
		t.Fatalf("expected first name to be __name__, got %v", names)
	}
	wantContains := []string{"bar", "foo", "instance"}
	for _, w := range wantContains {
		found := false
		for _, n := range names {
			if n == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing %q in %v", w, names)
		}
	}

	if !strings.Contains(q.lastSQL, "mapKeys") {
		t.Errorf("expected SQL to use mapKeys; got %q", q.lastSQL)
	}
}

func TestLabels_MatchSelector(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{
		strings: []string{"job", "instance"},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/labels?" +
		"match%5B%5D=up%7Bjob%3D%22api%22%7D")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", resp.StatusCode, readBody(t, resp))
	}
	var parsed metadataResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	defer resp.Body.Close()

	var names []string
	if err := json.Unmarshal(parsed.Data, &names); err != nil {
		t.Fatalf("decode data: %v", err)
	}
	if len(names) < 1 || names[0] != "__name__" {
		t.Fatalf("expected __name__ in names, got %v", names)
	}
	// SQL should wrap the matched scan in `SELECT DISTINCT arrayJoin(mapKeys(...))`.
	if !strings.Contains(q.lastSQL, "arrayJoin(mapKeys") {
		t.Errorf("expected SQL to project mapKeys; got %q", q.lastSQL)
	}
}

func TestLabelValues_Endpoint(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{
		strings: []string{"api", "db", "cache"},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/label/job/values")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	var parsed metadataResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	defer resp.Body.Close()

	var values []string
	if err := json.Unmarshal(parsed.Data, &values); err != nil {
		t.Fatalf("decode data: %v", err)
	}
	if len(values) != 3 {
		t.Fatalf("expected 3 values, got %d: %v", len(values), values)
	}
	// Sorted ascending.
	if values[0] != "api" || values[1] != "cache" || values[2] != "db" {
		t.Errorf("expected sorted values, got %v", values)
	}

	// The job label should bind through Attributes['job'].
	if !strings.Contains(q.lastSQL, "Attributes`[?]") {
		t.Errorf("expected SQL to reference Attributes map access; got %q", q.lastSQL)
	}
	// Per UNION arm the bind order is:
	//   <name (SELECT DISTINCT Attributes[?])>,
	//   <name (WHERE Attributes[?] != ?)>,
	//   <"" (WHERE empty-sentinel Lit)>.
	// All `name` slots should be "job"; the empty-sentinel slots should be "".
	for i, a := range q.lastArgs {
		var want any = "job"
		if i%3 == 2 {
			want = ""
		}
		if a != want {
			t.Errorf("arg[%d] = %v, want %v", i, a, want)
		}
	}
}

func TestLabelValues_MetricNameLabel(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{strings: []string{"up", "http_requests_total"}}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/label/__name__/values")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}

	// __name__ uses MetricName column, no Attributes mapAccess.
	if strings.Contains(q.lastSQL, "Attributes`[") {
		t.Errorf("__name__ should NOT use Attributes mapAccess; got %q", q.lastSQL)
	}
	if !strings.Contains(q.lastSQL, "MetricName") {
		t.Errorf("__name__ should query the MetricName column; got %q", q.lastSQL)
	}
}

// TestMetadataDiscovery_WindowPrune pins the index/partition-bounding fix:
// the no-`match[]` discovery endpoints (`/label/__name__/values`,
// `/labels`, `/label/<name>/values`) must push the request `start`/`end`
// window onto each per-table arm so ClickHouse prunes by the
// `toDate(TimeUnix)` partition instead of streaming the full leading-key
// column (the unbounded `SELECT DISTINCT MetricName` that scanned ~2.6B
// rows in prod). When no window is sent the SQL stays unbounded — that is
// the inherent exact answer for Prometheus's no-bound metadata semantics.
func TestMetadataDiscovery_WindowPrune(t *testing.T) {
	t.Parallel()

	const window = "start=1700000000&end=1700003600"
	cases := []struct {
		name string
		path string
	}{
		{"metric_names", "/api/v1/label/__name__/values"},
		{"label_names", "/api/v1/labels"},
		{"label_values", "/api/v1/label/job/values"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// With a window: every arm must carry the TimeUnix bound.
			qw := &stubQuerier{strings: []string{"up"}}
			srvW := newServer(qw)
			t.Cleanup(srvW.Close)
			resp, err := http.Get(srvW.URL + tc.path + "?" + window)
			if err != nil {
				t.Fatalf("GET windowed: %v", err)
			}
			resp.Body.Close()
			if !strings.Contains(qw.lastSQL, "toDateTime64(") ||
				!strings.Contains(qw.lastSQL, "TimeUnix") {
				t.Errorf("windowed %s must push a TimeUnix partition bound; got %q",
					tc.path, qw.lastSQL)
			}

			// Without a window: the scan must STILL be bounded by a default
			// lookback so a Grafana variable query that sends no start/end
			// (the common "On dashboard load" refresh) does not stream every
			// toDate(TimeUnix) partition — the ~30B-row prod scan. REPRO:
			// metadataWindowPred returns nil for the zero/zero case, so on
			// current main this arm emits a WHERE-less full scan and the
			// assertion below is RED. The companion result-identity guard
			// (the windowless scan must still return the full catalog) lives
			// in metadata_scan_bound_guard_chdb_test.go.
			qn := &stubQuerier{strings: []string{"up"}}
			srvN := newServer(qn)
			t.Cleanup(srvN.Close)
			resp, err = http.Get(srvN.URL + tc.path)
			if err != nil {
				t.Fatalf("GET unbounded: %v", err)
			}
			resp.Body.Close()
			if !strings.Contains(qn.lastSQL, "toDateTime64(") ||
				!strings.Contains(qn.lastSQL, "TimeUnix") {
				t.Errorf("windowless %s must push a default TimeUnix partition bound; got %q",
					tc.path, qn.lastSQL)
			}
		})
	}
}

func TestLabelValues_InvalidName(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	// Invalid char (slash would be eaten by routing, so use a leading digit).
	resp, err := http.Get(srv.URL + "/api/v1/label/1invalid/values")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid label name, got %d", resp.StatusCode)
	}
}

func TestSeries_Endpoint(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{
		samples: []chclient.Sample{
			{MetricName: "up", Labels: map[string]string{"job": "api", "instance": "h1:8080"}},
			{MetricName: "up", Labels: map[string]string{"job": "api", "instance": "h2:8080"}},
			// Duplicate of the first row → should dedupe.
			{MetricName: "up", Labels: map[string]string{"job": "api", "instance": "h1:8080"}},
		},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/series?match%5B%5D=up")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	var parsed metadataResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	defer resp.Body.Close()

	var series []map[string]string
	if err := json.Unmarshal(parsed.Data, &series); err != nil {
		t.Fatalf("decode data: %v", err)
	}
	if len(series) != 2 {
		t.Fatalf("expected 2 deduped series, got %d: %+v", len(series), series)
	}
	for _, lset := range series {
		if lset["__name__"] != "up" {
			t.Errorf("expected __name__=up, got %+v", lset)
		}
	}
}

func TestSeries_RequiresMatch(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/series")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestMetadata_Endpoint(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{
		metaRows: []chclient.MetricMetaRow{
			{Name: "up", Description: "scrape ok", Unit: "", Type: "gauge"},
			{Name: "temperature", Description: "ambient temp", Unit: "celsius", Type: "gauge"},
		},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/metadata")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	var parsed metadataResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	defer resp.Body.Close()

	if parsed.Status != "success" {
		t.Fatalf("status: got %q", parsed.Status)
	}

	var grouped map[string][]prom.MetricMetaEntry
	if err := json.Unmarshal(parsed.Data, &grouped); err != nil {
		t.Fatalf("decode data: %v", err)
	}
	if _, ok := grouped["up"]; !ok {
		t.Errorf("expected 'up' metadata; got %+v", grouped)
	}
	if _, ok := grouped["temperature"]; !ok {
		t.Errorf("expected 'temperature' metadata; got %+v", grouped)
	}
}

func TestMetadata_FilterByName(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{
		metaRows: []chclient.MetricMetaRow{
			{Name: "up", Description: "scrape ok", Type: "gauge"},
		},
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/metadata?metric=up")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	defer resp.Body.Close()

	if len(q.lastArgs) == 0 || q.lastArgs[0] != "up" {
		t.Errorf("expected last query arg = 'up', got %v", q.lastArgs)
	}
}

// TestMetadata_NonMonotonicSumIsGauge pins the OTel→Prometheus type
// mapping for the sum table: monotonic Sums report as "counter",
// non-monotonic Sums (OTel UpDownCounters — e.g. the in-flight-query
// gauge cerberus itself emits) report as "gauge". Before the split the
// handler typed every sum-table metric "counter", and Grafana's
// Metrics Drilldown wrapped UpDownCounters in rate() — a flat-0 chart.
func TestMetadata_NonMonotonicSumIsGauge(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{}
	q.metaRowsFn = func(call metaCall) []chclient.MetricMetaRow {
		if !strings.Contains(call.sql, "otel_metrics_sum") {
			return nil
		}
		switch {
		case strings.Contains(call.sql, "NOT any(`IsMonotonic`)"):
			return []chclient.MetricMetaRow{{
				Name:        "cerberus_query_inflight",
				Description: "Currently-executing engine queries.",
				Unit:        "{query}",
				Type:        call.kind,
			}}
		case strings.Contains(call.sql, "any(`IsMonotonic`)"):
			return []chclient.MetricMetaRow{{
				Name:        "cerberus_queries_total",
				Description: "Total engine queries.",
				Unit:        "{query}",
				Type:        call.kind,
			}}
		default:
			t.Errorf("sum-table metadata SQL missing IsMonotonic predicate: %q", call.sql)
			return nil
		}
	}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/metadata")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	var parsed metadataResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	defer resp.Body.Close()

	// (a) Fan-out shape: the sum table is queried exactly twice — once
	// per monotonicity arm with the matching reported type — and the
	// gauge / histogram arms carry no IsMonotonic predicate.
	var sumCalls []metaCall
	for _, call := range q.metaCalls {
		if strings.Contains(call.sql, "otel_metrics_sum") {
			sumCalls = append(sumCalls, call)
			continue
		}
		if strings.Contains(call.sql, "IsMonotonic") {
			t.Errorf("non-sum metadata SQL (kind=%s) must not filter IsMonotonic: %q",
				call.kind, call.sql)
		}
	}
	if len(sumCalls) != 2 {
		t.Fatalf("expected 2 sum-table metadata queries, got %d: %+v",
			len(sumCalls), sumCalls)
	}
	for _, call := range sumCalls {
		nonMonotonic := strings.Contains(call.sql, "NOT any(`IsMonotonic`)")
		switch {
		case nonMonotonic && call.kind != "gauge":
			t.Errorf("NOT IsMonotonic arm reported type %q, want gauge; sql=%q",
				call.kind, call.sql)
		case !nonMonotonic && call.kind != "counter":
			t.Errorf("IsMonotonic arm reported type %q, want counter; sql=%q",
				call.kind, call.sql)
		}
	}

	// (b) Wire result: the non-monotonic metric surfaces as gauge, the
	// monotonic one as counter.
	var grouped map[string][]prom.MetricMetaEntry
	if err := json.Unmarshal(parsed.Data, &grouped); err != nil {
		t.Fatalf("decode data: %v", err)
	}
	wantTypes := map[string]string{
		"cerberus_query_inflight": "gauge",
		"cerberus_queries_total":  "counter",
	}
	for name, wantType := range wantTypes {
		entries, ok := grouped[name]
		if !ok || len(entries) != 1 {
			t.Errorf("expected exactly one entry for %q, got %+v", name, grouped[name])
			continue
		}
		if entries[0].Type != wantType {
			t.Errorf("%s: type=%q, want %q", name, entries[0].Type, wantType)
		}
	}
}

// TestMetadata_MonotonicFilterCombinesWithMetricName asserts the
// `?metric=` filter ANDs with the per-arm IsMonotonic predicate rather
// than replacing it — both clauses must land in the sum-table SQL and
// the metric name stays the (only) bound arg.
func TestMetadata_MonotonicFilterCombinesWithMetricName(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{}
	srv := newServer(q)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/metadata?metric=cerberus_query_inflight")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}

	checked := 0
	for _, call := range q.metaCalls {
		if !strings.Contains(call.sql, "otel_metrics_sum") {
			continue
		}
		checked++
		if !strings.Contains(call.sql, "IsMonotonic") {
			t.Errorf("sum arm missing IsMonotonic predicate: %q", call.sql)
		}
		if !strings.Contains(call.sql, "`MetricName` = ?") {
			t.Errorf("sum arm missing metric-name filter: %q", call.sql)
		}
		if len(call.args) != 1 || call.args[0] != "cerberus_query_inflight" {
			t.Errorf("sum arm args = %v, want [cerberus_query_inflight]", call.args)
		}
	}
	if checked != 2 {
		t.Fatalf("expected 2 sum-table metadata queries, got %d", checked)
	}
}

// TestMetadata_NoMonotonicColumnFallsBackToCounter pins the documented
// fallback for schema overrides whose sum table carries no IsMonotonic
// column: a single counter-typed sum-table query (the pre-split
// behaviour) with no IsMonotonic predicate.
func TestMetadata_NoMonotonicColumnFallsBackToCounter(t *testing.T) {
	t.Parallel()

	q := &stubQuerier{}
	s := schema.DefaultOTelMetrics()
	s.IsMonotonicColumn = ""
	h := prom.New(q, s, nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/api/v1/metadata")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}

	var sumCalls []metaCall
	for _, call := range q.metaCalls {
		if strings.Contains(call.sql, "IsMonotonic") {
			t.Errorf("no-IsMonotonic schema must not emit the predicate: %q", call.sql)
		}
		if strings.Contains(call.sql, "otel_metrics_sum") {
			sumCalls = append(sumCalls, call)
		}
	}
	if len(sumCalls) != 1 {
		t.Fatalf("expected 1 sum-table metadata query in fallback mode, got %d", len(sumCalls))
	}
	if sumCalls[0].kind != "counter" {
		t.Errorf("fallback sum arm reported type %q, want counter", sumCalls[0].kind)
	}
}

func TestMetadata_LimitBadValue(t *testing.T) {
	t.Parallel()

	cases := []string{"-1", "abc"}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			srv := newServer(&stubQuerier{})
			t.Cleanup(srv.Close)
			resp, err := http.Get(srv.URL + "/api/v1/metadata?limit=" + raw)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("limit=%q: expected 400, got %d", raw, resp.StatusCode)
			}
		})
	}
}
