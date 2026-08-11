// Perf ratchet: the RENDERED SIZE of the combined metadata query.
//
// # What this pins, and why it is its own ratchet
//
// The other perf ratchets in this package measure what ClickHouse does with a
// query — the cardinality ratchet pins fan_factor, the scale-wall pin pins
// scan amplification and wall. This one pins something upstream of any of
// that: how big the query TEXT is before it is ever sent. That is a real
// failure axis for the metadata endpoints and it has bitten twice in
// production, both times as a hard 502 rather than a slowdown:
//
//   - PR #790: the first fan-in attempt UNION-ALL'd every matcher variant into
//     one statement, which crossed ClickHouse's 256KB `max_query_size` at byte
//     position 262124 (code 62, "Max query size exceeded").
//   - #799: the same ceiling, reached again on
//     `otelcol_process_runtime_total_sys_memory_bytes`, because the rendered-
//     size guard measured `len(placeholderSQL)` while the native driver
//     inlines every bound arg client-side.
//
// Both were fixed with CEILINGS — an arm-count cap (maxMetricCandidatesPerQuery)
// and a byte guard (maxRenderedQueryBytes) that split an oversize query. A
// ceiling keeps the endpoint correct but it is deliberately blind to growth
// underneath it: a change that quadruples the arm count is invisible to a
// 200KB guard right up until the request that crosses it. #1528 is exactly
// that shape — a metadata-side matcher-string fan-out over the dotted-storage
// candidates multiplied the arm count by the candidate count while the
// predicate-level fan-out in internal/promql.metricNamePredicate already
// resolved the same candidate set inside ONE arm, so the multiplication bought
// no reachable stored name and pushed every broad probe toward the ceiling.
//
// So this ratchet pins the measured size against a committed budget with
// modest headroom, not against the CH ceiling. Re-introducing a per-candidate
// arm expansion fails it immediately (arms per selector goes 4 -> ~104)
// instead of silently consuming the safety margin the two incidents bought.
//
// # Why it is pure Go
//
// The measurement is of the query the handler RENDERS, which needs no engine
// at all — a recording Querier returning empty results is enough, and it makes
// the ratchet deterministic and chDB-free so it rides the already-required
// `check` job (mirrors TestSolverDecisionRatchet).
package perf

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	"github.com/tsouza/cerberus/internal/api/format"
	"github.com/tsouza/cerberus/internal/api/prom"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/schema"
)

// metadataQuerySizeBaselinePath is the committed budget file, relative to this
// package.
const metadataQuerySizeBaselinePath = "metadata-query-size-baseline.json"

// metadataQuerySizeUpdateEnv, when "1", rewrites the budget from the current
// measurement instead of asserting against it. Driven by
// `just update-metadata-query-size-baseline`; mirrors UPDATE_SCALE_WALL_BASELINE.
const metadataQuerySizeUpdateEnv = "UPDATE_METADATA_QUERY_SIZE_BASELINE"

// metadataQuerySizeHeadroom is the multiple of the measured figure the
// regenerated budget carries. Small on purpose: the measurement is fully
// deterministic (no engine, no timing), so the only reason to leave room at
// all is an incidental emit-shape change — a schema column rename widening
// every arm by a few bytes, say — that is not the fan-out growth this pins.
// A 4x arm multiplication cannot hide under it.
const metadataQuerySizeHeadroom = 1.25

// The metrics-explorer broad probe. Grafana's Metrics Drilldown asks for the
// labels of every metric it just enumerated, which arrives as one /series
// request carrying many match[] selectors at once. These names are the shape
// that actually hurts: heavily underscored (so the dotted-candidate powerset
// is large) and bare classic-histogram bases (so the companion layer fans out
// too).
var metadataProbeMetricNames = []string{
	"http_server_request_body_size",
	"http_server_response_body_size",
	"http_client_request_body_size",
	"http_server_active_requests",
	"otelcol_process_runtime_total_sys_memory_bytes",
	"otelcol_process_runtime_heap_alloc_bytes",
	"otelcol_exporter_sent_metric_points",
	"otelcol_receiver_accepted_metric_points",
	"k8s_node_cpu_usage_seconds_total",
	"k8s_pod_memory_working_set_bytes",
	"container_cpu_usage_seconds",
	"container_network_receive_bytes",
	"system_disk_operation_time_seconds",
	"system_network_connection_count",
	"process_runtime_go_goroutines_count",
	"traces_service_graph_request_server_seconds",
}

// metadataQuerySizeBaseline is the committed budget. Kept tiny + JSON so a
// deliberate move is a one-line review diff.
type metadataQuerySizeBaseline struct {
	// BoundBytesPerSelectorBound is the max total rendered SQL a single
	// input match[] selector may cost, summed across every combined query
	// the request fans into. This is the ratchet's primary signal, and it is
	// deliberately a TOTAL rather than a per-query figure: the rendered-size
	// guard splits an oversize query, so fan-out growth shows up as more
	// queries of the same size, which a per-query budget alone would absorb
	// silently. Summing across the split makes the growth visible wherever
	// the guard puts it.
	BoundBytesPerSelectorBound int `json:"bound_bytes_per_selector_bound"`

	// MaxBoundQueryBytesBound is the max BOUND size (placeholder SQL with
	// every `?` accounted for by its inlined arg literal — the bytes CH's
	// parser actually counts) of any single combined query the probe
	// produces. This is the figure both incidents were measured in, and the
	// one that must stay far below ClickHouse's 262144-byte ceiling.
	MaxBoundQueryBytesBound int `json:"max_bound_query_bytes_bound"`

	// CombinedQueriesBound is the max number of round-trips the probe may
	// fan into. A broad probe splits into ⌈arms/128⌉ queries (and further on
	// the byte guard), so this rises in lock-step with the fan-out and pins
	// the round-trip cost the fan-in batching (task #71) bought.
	CombinedQueriesBound int `json:"combined_queries_bound"`
}

// recordingQuerier captures the SQL and bound args of every query the handler
// issues and returns empty results — the handler's rendering is what is being
// measured, not any engine's answer.
type recordingQuerier struct {
	queries []recordedQuery
}

type recordedQuery struct {
	sql  string
	args []any
}

func (q *recordingQuerier) record(sql string, args []any) {
	q.queries = append(q.queries, recordedQuery{sql: sql, args: args})
}

func (q *recordingQuerier) Query(_ context.Context, sql string, args ...any) ([]chclient.Sample, error) {
	q.record(sql, args)
	return nil, nil
}

func (q *recordingQuerier) QueryCursor(_ context.Context, sql string, args ...any) (chclient.Cursor, error) {
	q.record(sql, args)
	return emptyCursor{}, nil
}

func (q *recordingQuerier) QueryStrings(_ context.Context, sql string, args ...any) ([]string, error) {
	q.record(sql, args)
	return nil, nil
}

func (q *recordingQuerier) QueryLabelSets(_ context.Context, sql string, args ...any) ([]map[string]string, error) {
	q.record(sql, args)
	return nil, nil
}

func (q *recordingQuerier) QueryMetricMeta(_ context.Context, _, _ string, _ ...any) ([]chclient.MetricMetaRow, error) {
	return nil, nil
}

func (q *recordingQuerier) QueryExemplars(_ context.Context, _ string, _ ...any) ([]chclient.ExemplarRow, error) {
	return nil, nil
}

var _ prom.Querier = (*recordingQuerier)(nil)

type emptyCursor struct{}

func (emptyCursor) Next() bool              { return false }
func (emptyCursor) Sample() chclient.Sample { return chclient.Sample{} }
func (emptyCursor) Err() error              { return nil }
func (emptyCursor) Close() error            { return nil }
func (emptyCursor) Inspected() int64        { return 0 }

// metadataProbeMeasurement is one run's figures.
type metadataProbeMeasurement struct {
	selectors       int
	combinedQueries int
	totalBoundBytes int
	maxBoundBytes   int
}

func (m metadataProbeMeasurement) boundBytesPerSelector() int {
	return m.totalBoundBytes / m.selectors
}

// runMetadataProbe issues the broad metrics-explorer /series request against a
// recording handler and returns the rendered-size figures.
func runMetadataProbe(t *testing.T) metadataProbeMeasurement {
	t.Helper()
	q := &recordingQuerier{}
	h := prom.New(q, schema.DefaultOTelMetrics(), nil)
	mux := http.NewServeMux()
	h.Mount(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	form := url.Values{}
	for _, name := range metadataProbeMetricNames {
		form.Add("match[]", `{__name__="`+name+`"}`)
	}
	resp, err := http.PostForm(srv.URL+"/api/v1/series", form)
	if err != nil {
		t.Fatalf("POST /series: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /series: status=%d", resp.StatusCode)
	}

	m := metadataProbeMeasurement{selectors: len(metadataProbeMetricNames)}
	for _, rq := range q.queries {
		m.combinedQueries++
		b := boundQuerySize(rq.sql, rq.args)
		m.totalBoundBytes += b
		if b > m.maxBoundBytes {
			m.maxBoundBytes = b
		}
	}
	if m.combinedQueries == 0 {
		t.Fatal("the probe issued no query — the measurement is vacuous and every bound " +
			"below would pass trivially")
	}
	return m
}

// boundQuerySize is the byte length of the query ClickHouse parses after
// clickhouse-go/v2 inlines the positional args client-side: the placeholder
// SQL plus, per `?`, the extra bytes its rendered literal occupies beyond the
// single `?` it replaces. Mirrors boundQueryBytes in
// internal/api/prom/metadata.go — the accounting whose absence let #799 past
// the guard, since the metric-name candidate payload rides the `[]any` channel
// and is invisible to a `len(sql)` probe.
func boundQuerySize(sql string, args []any) int {
	total := len(sql)
	for _, a := range args {
		if s, ok := a.(string); ok {
			total += (2*len(s) + 2) - 1
			continue
		}
		const nonStringLiteralBudget = 40
		total += nonStringLiteralBudget - 1
	}
	return total
}

// TestMetadataQuerySizeRatchet asserts the broad metrics-explorer probe's
// rendered query size stays within the committed budget.
func TestMetadataQuerySizeRatchet(t *testing.T) {
	m := runMetadataProbe(t)

	t.Logf("metrics-explorer broad probe: %d match[] selectors -> %d combined queries, "+
		"%d total bound bytes (%d per selector), largest bound query %d bytes",
		m.selectors, m.combinedQueries, m.totalBoundBytes, m.boundBytesPerSelector(), m.maxBoundBytes)

	if os.Getenv(metadataQuerySizeUpdateEnv) == "1" {
		writeMetadataQuerySizeBaseline(t, m)
		return
	}

	base := readMetadataQuerySizeBaseline(t)

	if got := m.boundBytesPerSelector(); got > base.BoundBytesPerSelectorBound {
		t.Errorf("rendered SQL per match[] selector regressed: %d > %d byte budget. Every "+
			"matcher variant is a full Sample-projecting SELECT UNION-ALL'd into the combined "+
			"metadata query, so the variant fan-out is the multiplier on the rendered SQL that "+
			"twice crossed ClickHouse's 256KB max_query_size. The surviving fan-out is the "+
			"classic-histogram companion layer alone (bare name + _bucket/_count/_sum); a figure "+
			"several times this one means a per-candidate fan-out came back at the "+
			"matcher-string level, where internal/promql.metricNamePredicate already resolves "+
			"the same candidate set inside ONE arm as a flat `MetricName IN (?,…)`. Fix the "+
			"fan-out; only run `just update-metadata-query-size-baseline` if the extra text "+
			"reaches stored rows the predicate cannot.", got, base.BoundBytesPerSelectorBound)
	}

	if m.maxBoundBytes > base.MaxBoundQueryBytesBound {
		t.Errorf("largest bound combined query regressed: %d > %d byte budget. This is the "+
			"figure ClickHouse's max_query_size (262144) counts — the driver inlines every bound "+
			"arg client-side — and the budget sits far below the ceiling on purpose, so this "+
			"fails while there is still margin rather than as a 502 in production.",
			m.maxBoundBytes, base.MaxBoundQueryBytesBound)
	}

	if m.combinedQueries > base.CombinedQueriesBound {
		t.Errorf("combined-query round-trips regressed: %d > %d budget. The arm cap and the "+
			"rendered-size guard split a broad probe into more queries, so a rising round-trip "+
			"count is a rising fan-out that a per-query byte budget absorbs by splitting.",
			m.combinedQueries, base.CombinedQueriesBound)
	}
}

// TestMetadataQuerySizeRatchetIsSensitive guards the guard. A ratchet whose
// budget sits far above what the code can produce is a rubber stamp: it would
// pass through the exact regression it exists to catch. The retired #1528
// fan-out multiplied the variant count — and so the rendered SQL — by the
// dotted-candidate powerset of each name; this test reconstructs that
// multiplication from the same candidate generator the retired layer used and
// asserts the committed budget REJECTS it.
func TestMetadataQuerySizeRatchetIsSensitive(t *testing.T) {
	base := readMetadataQuerySizeBaseline(t)
	m := runMetadataProbe(t)

	// The retired layer emitted one matcher string per dotted-storage
	// candidate, and the surviving companion layer then ran per candidate —
	// so the rendered SQL was multiplied by the candidate count of each name.
	// The smallest candidate count across the probe corpus is the most
	// conservative reconstruction of that multiplier.
	minCandidates := 0
	for _, name := range metadataProbeMetricNames {
		// The same generator the retired matcher-string layer and the
		// surviving predicate-level fan-out both call.
		n := len(format.PromLabelToOTelCandidates(name))
		if minCandidates == 0 || n < minCandidates {
			minCandidates = n
		}
	}
	if minCandidates <= 1 {
		t.Fatalf("the probe corpus has a name with %d dotted candidates — it cannot "+
			"reconstruct the retired multiplication and this sensitivity check is vacuous",
			minCandidates)
	}

	regressed := m.boundBytesPerSelector() * minCandidates
	if regressed <= base.BoundBytesPerSelectorBound {
		t.Fatalf("the committed per-selector byte budget (%d) would ACCEPT the #1528 "+
			"regression: re-introducing the matcher-string candidate fan-out takes the probe to "+
			"at least %d bytes/selector (current %d x the smallest candidate powerset in the "+
			"corpus, %d). A budget that tolerates the regression it exists to catch is a rubber "+
			"stamp — tighten it.",
			base.BoundBytesPerSelectorBound, regressed, m.boundBytesPerSelector(), minCandidates)
	}
}

func readMetadataQuerySizeBaseline(t *testing.T) metadataQuerySizeBaseline {
	t.Helper()
	raw, err := os.ReadFile(metadataQuerySizeBaselinePath)
	if err != nil {
		t.Fatalf("read baseline %s: %v — run `just update-metadata-query-size-baseline` to "+
			"create it", metadataQuerySizeBaselinePath, err)
	}
	var b metadataQuerySizeBaseline
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatalf("parse baseline %s: %v", metadataQuerySizeBaselinePath, err)
	}
	if b.BoundBytesPerSelectorBound <= 0 || b.MaxBoundQueryBytesBound <= 0 || b.CombinedQueriesBound <= 0 {
		t.Fatalf("baseline %s has a non-positive bound (%+v) — every assertion would pass or "+
			"fail for the wrong reason", metadataQuerySizeBaselinePath, b)
	}
	return b
}

func writeMetadataQuerySizeBaseline(t *testing.T, m metadataProbeMeasurement) {
	t.Helper()
	b := metadataQuerySizeBaseline{
		BoundBytesPerSelectorBound: int(float64(m.boundBytesPerSelector()) * metadataQuerySizeHeadroom),
		MaxBoundQueryBytesBound:    int(float64(m.maxBoundBytes) * metadataQuerySizeHeadroom),
		CombinedQueriesBound:       int(float64(m.combinedQueries)*metadataQuerySizeHeadroom) + 1,
	}
	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		t.Fatalf("marshal baseline: %v", err)
	}
	if err := os.WriteFile(metadataQuerySizeBaselinePath, append(raw, '\n'), 0o644); err != nil {
		t.Fatalf("write baseline %s: %v", metadataQuerySizeBaselinePath, err)
	}
	t.Logf("wrote %s: %s", metadataQuerySizeBaselinePath, raw)
}

func init() {
	// Fail loudly at package init if the probe corpus is ever emptied: a
	// zero-selector probe divides by zero in boundBytesPerSelector and every
	// bound would be compared against garbage.
	if len(metadataProbeMetricNames) == 0 {
		panic(fmt.Sprintf("%s: the metrics-explorer probe corpus is empty",
			metadataQuerySizeBaselinePath))
	}
}
