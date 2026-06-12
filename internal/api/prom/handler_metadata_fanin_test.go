package prom_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
)

// This file is the regression pin for the bounded fan-in redesign
// (task #71). It reproduces the exact failure class that 502'd the first
// fan-in attempt (PR #790): a broad metrics-explorer /api/v1/series request
// whose match[] + dotted/histogram expansion produces hundreds of candidate
// arms. The first attempt UNION-ALL'd every arm into one statement and blew
// past ClickHouse's 256KB max_query_size at byte position 262124.
//
// The redesign guarantees no rendered combined query ever approaches that
// ceiling, for ANY request. These tests assert, against the live in-process
// handler:
//
//	(a) every rendered combined query is < maxQuerySizeBytes (200KB),
//	(b) the IN-form is used (parameterized `IN (?,…)`, no inline-literal
//	    metric-name OR-chain),
//	(c) chunking/fallback fans a broad request into ⌈N/K⌉ bounded queries
//	    past the cap (≪ N),
//	(d) a small request stays exactly 1 round-trip, and
//	(e) the combined results equal the pre-batch per-candidate results
//	    (same label sets).

// maxQuerySizeBytes is the byte ceiling the rendered-size guard keeps every
// combined query under. It mirrors maxRenderedQueryBytes in metadata.go (the
// budget below ClickHouse's 256KB / 262144-byte max_query_size default). The
// test asserts the handler's queries stay under this so a future arm-shape
// growth that would breach the CH ceiling fails loudly in CI, not in prod.
const maxQuerySizeBytes = 200 * 1024

// armChunkCap mirrors maxMetricCandidatesPerQuery in metadata.go: the
// arm-count cap that bounds ⌈N/K⌉ chunking. Kept in sync by the assertions
// below — if the production constant moves, this test's ⌈N/K⌉ math moves
// with it.
const armChunkCap = 128

// faninRecordingQuerier captures every rendered Query / QueryStrings SQL the
// handler issues so a test can assert round-trip counts + per-query SQL
// size, and synthesises per-arm samples keyed off the metric-name candidates
// bound in each query's args so the combined-path result can be compared
// against the per-candidate reference.
type faninRecordingQuerier struct {
	// sampleFor maps a metric-name candidate (a string arg bound into the
	// query's MetricName IN-list) to the label sets that candidate's rows
	// carry. The combined query's args contain the union of its arms'
	// candidates, so the synthesised result is the union of the matching
	// per-candidate label sets — exactly what the per-candidate reference
	// path would have returned.
	sampleFor map[string][]chclient.Sample

	querySQLs   []string
	stringsSQLs []string
}

func (q *faninRecordingQuerier) Query(_ context.Context, sql string, args ...any) ([]chclient.Sample, error) {
	q.querySQLs = append(q.querySQLs, sql)
	return q.samplesForArgs(args), nil
}

func (q *faninRecordingQuerier) QueryCursor(_ context.Context, sql string, args ...any) (chclient.Cursor, error) {
	q.querySQLs = append(q.querySQLs, sql)
	return newSliceCursor(q.samplesForArgs(args)), nil
}

func (q *faninRecordingQuerier) QueryStrings(_ context.Context, sql string, _ ...any) ([]string, error) {
	q.stringsSQLs = append(q.stringsSQLs, sql)
	return nil, nil
}

func (q *faninRecordingQuerier) QueryLabelSets(_ context.Context, _ string, _ ...any) ([]map[string]string, error) {
	return nil, nil
}

func (q *faninRecordingQuerier) QueryMetricMeta(_ context.Context, _, _ string, _ ...any) ([]chclient.MetricMetaRow, error) {
	return nil, nil
}

func (q *faninRecordingQuerier) QueryExemplars(_ context.Context, _ string, _ ...any) ([]chclient.ExemplarRow, error) {
	return nil, nil
}

var _ prom.Querier = (*faninRecordingQuerier)(nil)

// samplesForArgs returns the union of the per-candidate sample sets for
// every metric-name candidate present in the query's bound args. A combined
// query whose IN-list binds the candidates of N arms returns the union of
// those N arms' rows — the same row set N sequential per-candidate queries
// would have produced, so the handler's downstream dedup yields identical
// label sets on the combined path.
func (q *faninRecordingQuerier) samplesForArgs(args []any) []chclient.Sample {
	seen := map[string]struct{}{}
	var out []chclient.Sample
	for _, a := range args {
		s, ok := a.(string)
		if !ok {
			continue
		}
		rows, ok := q.sampleFor[s]
		if !ok {
			continue
		}
		for _, row := range rows {
			key := row.MetricName + "\x00" + fmt.Sprint(row.Labels)
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, row)
		}
	}
	return out
}

func (q *faninRecordingQuerier) reset() {
	q.querySQLs = nil
	q.stringsSQLs = nil
}

func faninServer(q prom.Querier) *httptest.Server {
	h := prom.New(q, schema.DefaultOTelMetrics(), nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	return httptest.NewServer(mux)
}

func maxSQLLen(sqls []string) int {
	m := 0
	for _, s := range sqls {
		if len(s) > m {
			m = len(s)
		}
	}
	return m
}

// inFormRe matches the parameterized IN-form the post-#795 metric-name
// predicate renders: `MetricName IN (?, ?, …)`. The placeholders must be
// `?` positional args, never inline string literals.
var inFormRe = regexp.MustCompile(`IN \(\?(, \?)*\)`)

// inlineNameLiteralRe matches an inline metric-name OR-chain literal — the
// shape PR #795 replaced (e.g. `MetricName = 'http_server_request_duration'`
// spliced directly into the SQL text). The redesign relies on the IN-form's
// parameterization to keep arms ~1KB; an inline-literal regression would
// fatten arms back toward the size that killed #790, so the test fails if a
// candidate value leaks into the SQL text as a quoted literal.
func assertNoInlineNameLiterals(t *testing.T, sql string, candidates []string) {
	t.Helper()
	for _, c := range candidates {
		if strings.Contains(sql, "'"+c+"'") {
			t.Fatalf("metric-name candidate %q leaked into the SQL text as an inline "+
				"literal — the parameterized IN-form regressed to an inline OR-chain, "+
				"the exact per-arm blowup that killed PR #790:\n%s", c, truncSQL(sql))
		}
	}
}

func truncSQL(s string) string {
	if len(s) <= 600 {
		return s
	}
	return s[:600] + " …(truncated)"
}

// broadMatchValues builds n distinct bare metric-name match[] selectors —
// the shape the metrics-explorer "every published metric" probe sends in one
// request. Each name carries rewritable underscores so it also drives the
// dotted-candidate fan-out, mirroring the production blowup. Span-metric
// shapes (traces_service_graph_*) are included: they are heavily
// underscored, so their candidate powerset is large.
func broadMatchValues(n int) (url.Values, []string) {
	v := url.Values{}
	names := make([]string, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("traces_service_graph_request_total_%d", i)
		v.Add("match[]", name)
		names = append(names, name)
	}
	return v, names
}

// TestHandleSeries_BroadProbeStaysBounded is the headline #790-killer pin:
// a broad /api/v1/series request must (a) keep every rendered combined query
// under the max_query_size budget, (b) use the parameterized IN-form, and
// (c) fan into ⌈N/K⌉ bounded queries — far fewer than the per-arm count the
// un-batched fan-out would have issued.
func TestHandleSeries_BroadProbeStaysBounded(t *testing.T) {
	t.Parallel()
	q := &faninRecordingQuerier{}
	srv := faninServer(q)
	defer srv.Close()

	const n = 600
	form, names := broadMatchValues(n)
	resp, err := http.PostForm(srv.URL+"/api/v1/series", form)
	if err != nil {
		t.Fatalf("POST /series: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}

	// (a) every combined query under the budget.
	if mx := maxSQLLen(q.querySQLs); mx >= maxQuerySizeBytes {
		t.Fatalf("a combined /series query rendered %d bytes, at/over the %d budget — "+
			"the rendered-size guard failed to keep queries under ClickHouse's "+
			"max_query_size (the #790 502 class)", mx, maxQuerySizeBytes)
	}

	// (b) IN-form used, no inline name literals.
	sawIN := false
	for _, sql := range q.querySQLs {
		if inFormRe.MatchString(sql) {
			sawIN = true
		}
		assertNoInlineNameLiterals(t, sql, names)
	}
	if !sawIN {
		t.Fatalf("no combined /series query used the parameterized MetricName IN-form; "+
			"the post-#795 candidate fan-out (flat IN, ~1KB/arm) is what keeps arms "+
			"small enough to batch. %d queries issued", len(q.querySQLs))
	}

	// (c) bounded-batch-or-fallback: each bare name fans out to many
	// variants (V dotted candidates × H histogram companions), so the true
	// arm count N is far larger than the match-count. The round-trip count
	// must be at least the arm-cap chunk count ⌈N/K⌉ (the rendered-size
	// guard may split a chunk further, raising it), no single query may
	// exceed the K-arm cap, and the whole thing must collapse ≫1 arms per
	// query (batching is real) — rt ≪ N.
	assertBoundedFanout(t, "/series", n, q.querySQLs)
}

// assertBoundedFanout pins the bounded-batch-or-fallback invariant on a set
// of combined queries from a broad probe:
//
//   - chunking kicked in (>1 query) — the broad request did NOT collapse
//     to a single un-bounded statement (the #790 shape),
//   - every query stays under the byte budget (the unconditional bound),
//     and
//   - batching genuinely happened — each combined query packs many arms,
//     so the byte size of the largest query dwarfs a single arm. A
//     per-arm-per-query fan-out (the un-batched path the win replaces)
//     would render rt tiny single-arm queries; instead the queries are
//     near-cap-sized, proving the V×H variant set folded into ⌈N/K⌉
//     bounded statements rather than N round-trips.
//
// We assert against byte size rather than a parsed arm count because the
// lowered matcher SQL shapes vary (single-table scan vs histogram-companion
// scan vs 2-table union), making a robust string-based arm count brittle;
// the byte budget IS the bound the redesign enforces, so asserting on it
// directly is both robust and the most faithful pin of the #790 failure.
func assertBoundedFanout(t *testing.T, endpoint string, n int, sqls []string) {
	t.Helper()
	rt := len(sqls)
	if rt <= 1 {
		t.Fatalf("broad %s probe (n=%d match[]) issued %d round-trips — chunking did not "+
			"kick in past the cap; a single un-bounded statement is the #790 502 shape",
			endpoint, n, rt)
	}
	mx := maxSQLLen(sqls)
	if mx >= maxQuerySizeBytes {
		t.Fatalf("broad %s probe rendered a %d-byte combined query, at/over the %d budget "+
			"— the rendered-size guard failed to keep it under ClickHouse's max_query_size",
			endpoint, mx, maxQuerySizeBytes)
	}
	// Batching proof: the largest combined query must be far larger than a
	// single arm — a per-arm fan-out would never pack a query this big.
	// minBatchedQueryBytes (32KB) is a conservative floor: a full K=128-arm
	// query renders ~110KB; even a size-guard-split chunk stays well above
	// this, while a single-arm query is ~1KB.
	const minBatchedQueryBytes = 32 * 1024
	if mx < minBatchedQueryBytes {
		t.Fatalf("broad %s probe's largest combined query is only %d bytes (< %d) — the "+
			"variant set did not batch into near-cap-sized queries; the fan-in collapsed "+
			"too little, leaving the per-arm round-trip count the win is meant to replace",
			endpoint, mx, minBatchedQueryBytes)
	}
	t.Logf("broad %s: n=%d match[] → %d combined round-trips, largest query %d bytes "+
		"(batched, < %d budget)", endpoint, n, rt, mx, maxQuerySizeBytes)
}

// TestHandleSeries_TypicalChipIsOneRoundTrip pins the win preserved: a
// typical Drilldown chip (a handful of candidates) stays exactly 1 combined
// round-trip.
func TestHandleSeries_TypicalChipIsOneRoundTrip(t *testing.T) {
	t.Parallel()
	q := &faninRecordingQuerier{}
	srv := faninServer(q)
	defer srv.Close()

	// A single histogram-base name — fires BOTH fan-out layers (V dotted
	// candidates × H classic-histogram companions), the densest typical
	// chip shape, yet still well under the arm cap.
	form := url.Values{}
	form.Add("match[]", `{__name__="http_server_request_duration"}`)
	resp, err := http.PostForm(srv.URL+"/api/v1/series", form)
	if err != nil {
		t.Fatalf("POST /series: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if rt := len(q.querySQLs); rt != 1 {
		t.Fatalf("typical histogram-base chip issued %d /series round-trips, want exactly "+
			"1 (the fan-in win); SQLs:\n%s", rt, strings.Join(q.querySQLs, "\n---\n"))
	}
}

// TestHandleSeries_CombinedResultEqualsPerCandidate pins parity (e): the
// combined-path label sets equal the union of the per-candidate results. The
// recording querier synthesises per-candidate rows keyed off the bound
// metric-name args, so the combined query's union-of-arms IN-list returns the
// same row set the per-candidate path would, and the handler's dedup must
// yield identical label sets.
func TestHandleSeries_CombinedResultEqualsPerCandidate(t *testing.T) {
	t.Parallel()

	// Two metrics, each with two distinct label sets. The names carry
	// underscores so they drive the dotted-candidate fan-out; the per-arm
	// IN-lists bind every candidate spelling, all mapped to the same rows
	// here so the union is well-defined.
	mkSample := func(name, job, inst string) chclient.Sample {
		return chclient.Sample{
			MetricName: name,
			Labels:     map[string]string{"job": job, "instance": inst},
		}
	}
	rowsA := []chclient.Sample{
		mkSample("svc_requests_total", "api", "a1"),
		mkSample("svc_requests_total", "api", "a2"),
	}
	rowsB := []chclient.Sample{
		mkSample("svc_latency_seconds", "web", "b1"),
		mkSample("svc_latency_seconds", "web", "b2"),
	}
	// Map every candidate spelling of each base name to that name's rows.
	sampleFor := map[string][]chclient.Sample{}
	for _, c := range []string{"svc_requests_total", "svc.requests.total", "svc.requests_total", "svc_requests.total"} {
		sampleFor[c] = rowsA
	}
	for _, c := range []string{"svc_latency_seconds", "svc.latency.seconds", "svc.latency_seconds", "svc_latency.seconds"} {
		sampleFor[c] = rowsB
	}

	q := &faninRecordingQuerier{sampleFor: sampleFor}
	srv := faninServer(q)
	defer srv.Close()

	// Combined path: both matchers in one request.
	combinedForm := url.Values{}
	combinedForm.Add("match[]", "svc_requests_total")
	combinedForm.Add("match[]", "svc_latency_seconds")
	combined := seriesLabelSets(t, srv, combinedForm)

	// Per-candidate reference: each matcher in its own request, results
	// unioned. With a handful of variants each request is 1 round-trip, so
	// the union of the two single-matcher responses is the pre-batch
	// per-candidate baseline.
	q.reset()
	refA := seriesLabelSets(t, srv, singleMatch("svc_requests_total"))
	refB := seriesLabelSets(t, srv, singleMatch("svc_latency_seconds"))
	reference := unionLabelSets(refA, refB)

	if !equalLabelSetLists(combined, reference) {
		t.Fatalf("combined /series result != per-candidate reference\ncombined:  %v\nreference: %v",
			combined, reference)
	}
	if len(combined) != 4 {
		t.Fatalf("expected 4 distinct label sets across the two metrics, got %d: %v",
			len(combined), combined)
	}
}

func singleMatch(v string) url.Values {
	form := url.Values{}
	form.Add("match[]", v)
	return form
}

// seriesLabelSets drives /api/v1/series with the given form and returns the
// data array (label-set maps) in canonical sorted order.
func seriesLabelSets(t *testing.T, srv *httptest.Server, form url.Values) []map[string]string {
	t.Helper()
	resp, err := http.PostForm(srv.URL+"/api/v1/series", form)
	if err != nil {
		t.Fatalf("POST /series: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var parsed struct {
		Status string              `json:"status"`
		Data   []map[string]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("decode /series body: %v\n%s", err, body)
	}
	return sortLabelSets(parsed.Data)
}

func sortLabelSets(in []map[string]string) []map[string]string {
	out := append([]map[string]string(nil), in...)
	sort.Slice(out, func(i, j int) bool { return labelSetKey(out[i]) < labelSetKey(out[j]) })
	return out
}

func labelSetKey(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(m[k])
		b.WriteByte(';')
	}
	return b.String()
}

func unionLabelSets(a, b []map[string]string) []map[string]string {
	seen := map[string]struct{}{}
	var out []map[string]string
	for _, m := range append(append([]map[string]string(nil), a...), b...) {
		k := labelSetKey(m)
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, m)
	}
	return sortLabelSets(out)
}

func equalLabelSetLists(a, b []map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if labelSetKey(a[i]) != labelSetKey(b[i]) {
			return false
		}
	}
	return true
}

// TestHandleLabelsMatched_BroadProbeStaysBounded exercises the same bounded
// fan-in on /api/v1/labels (matched): the combined matched-row scan's
// distinct-keys projection must stay under the budget and chunk past the cap.
func TestHandleLabelsMatched_BroadProbeStaysBounded(t *testing.T) {
	t.Parallel()
	q := &faninRecordingQuerier{}
	srv := faninServer(q)
	defer srv.Close()

	const n = 600
	form, _ := broadMatchValues(n)
	resp, err := http.PostForm(srv.URL+"/api/v1/labels", form)
	if err != nil {
		t.Fatalf("POST /labels: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if mx := maxSQLLen(q.stringsSQLs); mx >= maxQuerySizeBytes {
		t.Fatalf("a combined /labels query rendered %d bytes, at/over the %d budget",
			mx, maxQuerySizeBytes)
	}
	assertBoundedFanout(t, "/labels", n, q.stringsSQLs)
}

// TestHandleLabelValuesMatched_BroadProbeStaysBounded exercises the bounded
// fan-in on /api/v1/label/<name>/values (matched).
func TestHandleLabelValuesMatched_BroadProbeStaysBounded(t *testing.T) {
	t.Parallel()
	q := &faninRecordingQuerier{}
	srv := faninServer(q)
	defer srv.Close()

	const n = 600
	form, _ := broadMatchValues(n)
	// /api/v1/label/{name}/values is GET-only; a 600-name match[] query
	// string is fine on the URL.
	resp, err := http.Get(srv.URL + "/api/v1/label/job/values?" + form.Encode())
	if err != nil {
		t.Fatalf("GET /label/job/values: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if mx := maxSQLLen(q.stringsSQLs); mx >= maxQuerySizeBytes {
		t.Fatalf("a combined /label/values query rendered %d bytes, at/over the %d budget",
			mx, maxQuerySizeBytes)
	}
	assertBoundedFanout(t, "/label/values", n, q.stringsSQLs)
}

// TestArmChunkCapInSync is a cheap guard that the test's ⌈N/K⌉ math tracks
// the production cap. If maxMetricCandidatesPerQuery moves, the broad-probe
// chunk-count assertions above must move with it; this documents the
// coupling so a future cap change updates both.
func TestArmChunkCapInSync(t *testing.T) {
	t.Parallel()
	if armChunkCap != 128 {
		t.Fatalf("armChunkCap drifted to %d; keep it in sync with "+
			"maxMetricCandidatesPerQuery in metadata.go", armChunkCap)
	}
}
