package prom

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/prometheus/common/model"
	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/api/format"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
)

// maxMetricCandidatesPerQuery bounds how many matcher variants the
// fan-in batched metadata endpoints (/api/v1/series, /api/v1/labels,
// /api/v1/label/<name>/values) fold into a single combined ClickHouse
// query (task #71).
//
// Why a cap: the fan-in batching collapses the V×H×matcher variant
// fan-out into ONE combined UNION-ALL query (N round-trips → 1). Each
// variant lowers to a full Sample-projecting / matched-row SELECT — a
// multi-line statement with its own PREWHERE / WHERE / window clauses.
// The metrics-explorer "every published metric" probe (and the
// home/drilldown bulk load) send hundreds-to-thousands of match[]
// selectors at once; each fans out to up to ~192 variants. UNION-ALL'ing
// that many full SELECTs blows the rendered SQL text past ClickHouse's
// `max_query_size` (256KB default) — the exact failure that 502'd the
// first un-bounded fan-in attempt (PR #790) at byte position 262124.
//
// The cap chunks the variant set into ⌈N/cap⌉ combined queries, each well
// under the 256KB ceiling, merged through the same Go dedup the single-
// query path uses. Typical requests (a handful of variants) stay one
// round-trip — the win is preserved; only pathologically broad requests
// fan to a few bounded queries, still ≪ the per-variant round-trip count.
//
// Sizing: a lowered matcher SELECT on the default OTel schema renders to
// ~1KB now that internal/promql/lower.go::metricNamePredicate emits the
// candidate set as a flat parameterized `MetricName IN (?,…)` (PR #795)
// rather than the old inline OR-chain (~3KB/arm — the per-arm blowup that
// actually killed #790). 128 arms × ~1KB ≈ 130KB of arm text plus
// UNION-ALL glue — comfortably under the 256KB ceiling even when arms run
// ~50% wider on a custom schema. The rendered-size guard below
// (maxRenderedQueryBytes) is the belt-and-suspenders: it re-checks each
// built chunk's actual byte length and splits further if a pathological
// schema still breaches the budget, so correctness never depends on the
// arm-count heuristic alone.
const maxMetricCandidatesPerQuery = 128

// maxRenderedQueryBytes is the byte budget the rendered-size guard
// enforces on every combined query the batched endpoints build. It sits
// below ClickHouse's default `max_query_size` (256KB / 262144 bytes) with
// headroom for wider custom schemas: a combined query whose BOUND size
// (placeholder SQL with every `?` replaced by its inlined arg literal —
// see boundQueryBytes) exceeds this budget is split further (down to a
// single arm) so no query ever approaches the CH ceiling for ANY request,
// however broad.
//
// This is the guard the first fan-in attempt (PR #790) lacked: it relied
// on the arm-count cap alone, and a wide-schema / heavily-underscored
// probe rendered arms fat enough to cross 256KB even under the cap. The
// guard makes the bound unconditional — correctness over the perf win:
// a pathologically broad request degrades to a few bounded queries (still
// ≪ N round-trips), never a 502.
//
// The budget is measured against the BOUND query size, not the compact
// placeholder SQL: clickhouse-go/v2 inlines positional args client-side
// (no native bound-param channel), so the bytes CH's max_query_size counts
// are the literal-substituted query. Measuring `len(placeholderSQL)` alone
// — as the original #71 guard did — undercounted the wire size by the
// entire arg-literal payload, which is exactly how the #799 502 on
// `otelcol_process_runtime_total_sys_memory_bytes` slipped past the guard
// in CI yet 502'd against real clickhouse-server in compose-smoke.
const maxRenderedQueryBytes = 200 * 1024

// chunkMatcherVariants splits the matcher-variant slice into chunks of at
// most maxMetricCandidatesPerQuery so each combined query the batched
// endpoints build starts well under ClickHouse's max_query_size. A slice
// at or below the cap returns a single chunk (the typical one-round-trip
// case); only an over-cap set fans into ⌈len/cap⌉ chunks. The
// rendered-size guard (splitOversizeChunk) may split any of these chunks
// further at build time. Returns nil for an empty input so callers
// short-circuit without issuing a query.
func chunkMatcherVariants(variants []string) [][]string {
	if len(variants) == 0 {
		return nil
	}
	if len(variants) <= maxMetricCandidatesPerQuery {
		return [][]string{variants}
	}
	chunks := make([][]string, 0, (len(variants)+maxMetricCandidatesPerQuery-1)/maxMetricCandidatesPerQuery)
	for i := 0; i < len(variants); i += maxMetricCandidatesPerQuery {
		end := i + maxMetricCandidatesPerQuery
		if end > len(variants) {
			end = len(variants)
		}
		chunks = append(chunks, variants[i:end])
	}
	return chunks
}

// renderedSQLBytes returns the byte length of the statement `combine`
// produces for the given matcher-subquery arms — measured as the size
// ClickHouse actually PARSES, not the size of the placeholder SQL.
//
// chDB-lenient-vs-prod-strict gap (the #799 502): clickhouse-go/v2 speaks
// the native protocol, which has no server-side bound-parameter channel —
// the driver substitutes every positional `?` with its rendered literal
// CLIENT-SIDE before the query reaches the server (lib/column bind path).
// So the bytes CH parses, and the bytes its `max_query_size` (256KB)
// ceiling counts, are the placeholder SQL with every `?` REPLACED by its
// argument literal — not the compact `?`-carrying string `combine`
// returns. A heavily-underscored gauge metric like
// `otelcol_process_runtime_total_sys_memory_bytes` fans out to a 2^6
// dotted-candidate × histogram-companion arm set, and each arm binds its
// candidate powerset as `MetricName IN (?,…)`; the placeholder SQL stays
// ~1KB/arm (the args ride the `[]any` channel, invisible to a `len(sql)`
// probe), but once the driver inlines ~thousands of candidate string
// literals the wire query crosses 256KB at parse position ~262142 and CH
// rejects it with code 62 "Max query size exceeded" — a 502 the old
// `len(sql)` guard could never see because it measured the wrong string.
// chDB masks this entirely (its bind path tolerates the oversize), which
// is why only the real-clickhouse-server compose-smoke reproduced it.
//
// We add the per-arg inlined-literal cost so the guard measures the bound
// query the driver transmits, and buildBoundedChunkSQL splits on the real
// figure.
func renderedSQLBytes(arms []chsql.Frag, combine func([]chsql.Frag) (string, []any)) int {
	sql, args := combine(arms)
	return boundQueryBytes(sql, args)
}

// boundQueryBytes estimates the byte length of the query ClickHouse parses
// after clickhouse-go/v2 inlines the positional args client-side: the
// placeholder SQL plus, for each `?`, the extra bytes its rendered literal
// occupies beyond the single `?` it replaces. The estimate is a safe
// over-approximation (it never undercounts the wire size), so the
// rendered-size guard errs toward smaller, safer chunks rather than
// shipping an oversize query CH would 502 on.
func boundQueryBytes(sql string, args []any) int {
	total := len(sql)
	for _, a := range args {
		// Each arg replaces one `?` (1 byte) with its rendered literal.
		total += argLiteralBytes(a) - 1
	}
	return total
}

// argLiteralBytes returns an upper bound on the byte length of the literal
// clickhouse-go/v2 renders for one bound arg. String args dominate the
// series/metadata fan-out (metric-name + label-key candidates); they render
// as `'<value>'` with each quote/backslash byte-doubled by escaping, so the
// worst case is `2*len + 2` (every byte escaped, plus the surrounding
// quotes). Non-string args (ints, the empty-string sentinel, time bounds)
// are bounded by a small constant — generous enough that no realistic
// numeric/temporal literal undercounts.
func argLiteralBytes(a any) int {
	if s, ok := a.(string); ok {
		return 2*len(s) + 2
	}
	const nonStringLiteralBudget = 40
	return nonStringLiteralBudget
}

// buildBoundedChunkSQL renders the matcher-variant arms of ONE arm-cap
// chunk into one or more (sql, args) combined statements, each guaranteed
// under maxRenderedQueryBytes — the rendered-size guard (task #71
// redesign). `combine` takes a slice of matcher-subquery arm Frags and
// returns the combined statement (the per-endpoint projection over the
// UNION-ALL of those arms).
//
// The common case — a chunk whose rendered SQL fits the budget — returns
// exactly one statement (one round-trip). Only a chunk that still renders
// oversize (a pathologically wide custom schema where even ≤cap arms blow
// the budget) is split: the arms are halved recursively until each
// sub-chunk fits, or down to a single arm (which is emitted as-is even if
// it alone exceeds the budget — a single matcher SELECT can't be split
// further, and CH will surface its own max_query_size error rather than
// cerberus silently dropping it). Correctness over the perf win: the
// caller's Go dedup folds the overlapping sub-chunk results safely.
func buildBoundedChunkSQL(arms []chsql.Frag, combine func([]chsql.Frag) (string, []any)) []renderedQuery {
	if len(arms) == 0 {
		return nil
	}
	if len(arms) == 1 || renderedSQLBytes(arms, combine) <= maxRenderedQueryBytes {
		sql, args := combine(arms)
		return []renderedQuery{{sql: sql, args: args}}
	}
	mid := len(arms) / 2
	left := buildBoundedChunkSQL(arms[:mid], combine)
	right := buildBoundedChunkSQL(arms[mid:], combine)
	return append(left, right...)
}

// renderedQuery is one combined (sql, args) statement the batched
// endpoints execute. buildBoundedChunkSQL returns a slice of these so the
// caller runs each as its own round-trip and merges the results.
type renderedQuery struct {
	sql  string
	args []any
}

// handleLabels implements GET /api/v1/labels — distinct label names across
// all metric tables, plus the synthetic `__name__` for the metric-name
// dimension. Optional `match[]` selectors narrow the result to labels of
// the matched series only.
// parseMetadataWindow extracts the optional `start` / `end` parameters
// shared by the metadata endpoints (/series, /labels, /label/<name>/values).
// r.ParseForm must already have run. Both bounds are optional; an absent
// one returns zero-time, which promql.LowerMetadataRange treats as "no
// bound on that side" (whole-table scan, matching reference Prometheus's
// min/max-retention default). A malformed value is a bad_data error. The
// returned [start,end] is the closed window a metadata enumeration scans —
// it must NOT be collapsed to an instant staleness window at `end`, which
// silently drops any series/label/value whose only sample sits earlier in
// the window.
func parseMetadataWindow(r *http.Request) (start, end time.Time, err error) {
	if raw := r.Form.Get("start"); raw != "" {
		t, perr := format.ParseTimeProm(raw, time.Time{})
		if perr != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid 'start' parameter: %w", perr)
		}
		start = t
	}
	if raw := r.Form.Get("end"); raw != "" {
		t, perr := format.ParseTimeProm(raw, time.Time{})
		if perr != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid 'end' parameter: %w", perr)
		}
		end = t
	}
	return start, end, nil
}

// defaultMetadataLookback bounds the absent-window metadata-discovery scan
// to the data-retention horizon rather than to a short recent window.
//
// Reference Prometheus answers a windowless /api/v1/label/<l>/values,
// /labels and /series over the ENTIRE queryable range (web/api/v1/api.go
// defaults start to MinTime and end to MaxTime) — every name / value /
// series still inside retention. Grafana's metric / label pickers send no
// start/end under the common "On dashboard load" variable refresh, so
// without a default the discovery arms emit a WHERE-less scan over every
// toDate(TimeUnix) partition (the prod full-column scan this fix targets).
//
// Defaulting the absent window to the retention horizon — the OTel-CH
// metric tables' TTL, two weeks — keeps the answer byte-identical to
// Prometheus's: anything older than the TTL has already been
// ttl_only_drop_parts-dropped and is absent from both backends, so no
// name/value/series that Prometheus would return is dropped. It still
// emits a closed TimeUnix bound, so ClickHouse partition-prunes whenever
// the table physically spans more than the horizon (a request-supplied
// narrower range, honored verbatim, is where the real pruning comes from).
//
// A SHORTER default (1h/6h/24h) prunes harder but silently drops metrics
// that went quiet within retention — the no-silent-drop divergence the
// windowless-completeness guards forbid.
const defaultMetadataLookback = 14 * 24 * time.Hour

// boundMetadataWindow applies the default retention-horizon lookback when a
// metadata request carries NO window (both bounds zero) — the Grafana
// variable-query case. A request that supplies either bound is honored
// verbatim: a one-sided window stays deliberately open-ended, matching
// reference Prometheus's MinTime/MaxTime default and the matched-path
// semantics in promql.wrapMetadataFullRange. Both the no-match arms
// (metadataWindowPred) and the matched path (promql.LowerMetadataRange)
// consume the returned [start,end], so wiring this once at the handler
// covers every discovery shape.
func boundMetadataWindow(start, end time.Time) (time.Time, time.Time) {
	if start.IsZero() && end.IsZero() {
		end = time.Now().UTC()
		start = end.Add(-defaultMetadataLookback)
	}
	return start, end
}

func (h *Handler) handleLabels(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}
	matchers := r.Form["match[]"]

	startT, endT, err := parseMetadataWindow(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}
	// nowAnchored mirrors handleLabelValues: with no explicit upper bound the
	// effective window ends "now", so the label-name fan-out routes onto the
	// proj_series projection via the aggregate-only HAVING bound. A
	// user-supplied finite [start,end] keeps the exact WHERE-bounded scan.
	nowAnchored := endT.IsZero()
	startT, endT = boundMetadataWindow(startT, endT)

	var names []string
	if len(matchers) == 0 {
		names, err = h.fetchLabelNames(r.Context(), startT, endT, nowAnchored)
	} else {
		names, err = h.fetchLabelNamesMatched(r.Context(), matchers, startT, endT)
	}
	if err != nil {
		h.respondError(w, err)
		return
	}

	// Wire-format contract: Prometheus emits `"data":[]` (not null) when
	// the result set is empty. JSON-marshalling a nil []string yields
	// `null`, which Grafana's discovery probes reject.
	if names == nil {
		names = []string{}
	}
	writeJSON(w, http.StatusOK, Response{
		Status: "success",
		Data:   names,
	})
}

// handleLabelValues implements GET /api/v1/label/<name>/values.
//
// For the synthetic `__name__` label we union the `MetricName` column
// across metric tables. For other labels we read `Attributes[<name>]` and
// drop the empty-string sentinel.
//
// Optional `start` / `end` parameters bound the closed [start,end] window
// the `match[]` enumeration scans (promql.LowerMetadataRange). A value is
// returned if it appears on any series with a sample ANYWHERE in that
// window — not just within a staleness lookback at `end`. Without the
// bounds the scan covers the whole table. (Reference Prometheus matches
// label values over the full [start,end] metadata range; an instant
// staleness window would drop a value whose only sample sits early in the
// range — the rc.9 /series window-drop bug, same class.)
func (h *Handler) handleLabelValues(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New("missing label name"))
		return
	}
	if !validLabelName(name) {
		writeError(w, http.StatusBadRequest, ErrBadData,
			fmt.Errorf("invalid label name %q", name))
		return
	}
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}
	matchers := r.Form["match[]"]

	// `start` / `end` are optional on label/values; when present they bound
	// the closed [start,end] window the matcher enumeration scans (see
	// parseMetadataWindow + promql.LowerMetadataRange). Missing values fall
	// through as zero-time (no bound on that side — whole-table scan).
	startT, endT, err := parseMetadataWindow(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}
	// nowAnchored is true when the request supplies no explicit upper bound,
	// so the effective window's end is "now or open" — the case where the
	// metric-name enumeration can answer exactly from the max(TimeUnix)
	// aggregating projection (max(TimeUnix) >= start ⇔ a sample exists in
	// [start, now], since samples are never future-dated). A user-supplied
	// finite end stays on the exact WHERE-bounded scan.
	nowAnchored := endT.IsZero()
	startT, endT = boundMetadataWindow(startT, endT)

	var values []string
	if len(matchers) == 0 {
		values, err = h.fetchLabelValues(r.Context(), name, startT, endT, nowAnchored)
	} else {
		values, err = h.fetchLabelValuesMatched(r.Context(), name, matchers, startT, endT)
	}
	if err != nil {
		h.respondError(w, err)
		return
	}

	// Wire-format contract: Prometheus emits `"data":[]` (not null) when
	// no values match. JSON-marshalling a nil []string yields `null`,
	// which Grafana's label/values probe rejects.
	if values == nil {
		values = []string{}
	}
	writeJSON(w, http.StatusOK, Response{
		Status: "success",
		Data:   values,
	})
}

// MetricMetaEntry is one entry in the per-metric metadata array Prometheus
// emits from /api/v1/metadata. Cerberus typically emits one entry per
// metric; a name that appears under multiple types (e.g. a sum-table
// metric written with both IsMonotonic values) yields one entry per
// type, matching the Prometheus wire format's per-metric entry slice.
type MetricMetaEntry struct {
	Type string `json:"type"`
	Help string `json:"help"`
	Unit string `json:"unit"`
}

// handleMetadata implements GET /api/v1/metadata — per-metric type / help /
// unit, sourced from the OTel `MetricDescription` and `MetricUnit` columns.
// Type is derived from the source table — gauge / counter / histogram —
// with the sum table further split on `IsMonotonic`: per the
// OTel→Prometheus compatibility spec a non-monotonic Sum (UpDownCounter)
// maps to Prom type "gauge", not "counter".
//
// The `metric` query parameter restricts the result to a single metric;
// `limit` caps the number of metrics returned (per Prom convention).
func (h *Handler) handleMetadata(w http.ResponseWriter, r *http.Request) {
	metricName := r.URL.Query().Get("metric")
	limitStr := r.URL.Query().Get("limit")

	// /api/v1/metadata takes no time params (upstream Prometheus accepts
	// only `metric` + `limit`), so it would otherwise GROUP BY MetricName
	// over every partition on every call. Bound it to the same default
	// retention-horizon window the discovery handlers use so the per-table
	// metadata fan-out partition-prunes too.
	startT, endT := boundMetadataWindow(time.Time{}, time.Time{})
	// /api/v1/metadata supplies no explicit end, so its window always ends
	// "now" — the now-anchored case that routes onto proj_metric_metadata via
	// the aggregate-only HAVING bound (see metricMetaSQL).
	rows, err := h.fetchMetricMeta(r.Context(), metricName, startT, endT, true)
	if err != nil {
		h.respondError(w, err)
		return
	}

	// Group by metric name; each metric gets a slice of entries. Names
	// pass through Prom's metric-name grammar (`OTelToPromMetric`) so
	// OTel-dotted metric names (`http.server.duration`) surface as the
	// underscored form expected by `/api/v1/label/__name__/values`.
	grouped := make(map[string][]MetricMetaEntry, len(rows))
	for _, row := range rows {
		name := format.OTelToPromMetric(row.Name)
		grouped[name] = append(grouped[name], MetricMetaEntry{
			Type: row.Type,
			Help: row.Description,
			Unit: row.Unit,
		})
	}

	if limitStr != "" {
		limit, err := parseLimit(limitStr)
		if err != nil {
			writeError(w, http.StatusBadRequest, ErrBadData, err)
			return
		}
		grouped = truncateMetadata(grouped, limit)
	}

	writeJSON(w, http.StatusOK, Response{
		Status: "success",
		Data:   grouped,
	})
}

// metricMetaSpec is one (table, reported type) arm of the metadata
// fan-out. A non-nil monotonic narrows the arm to rows whose
// IsMonotonic column matches — the sum table holds both OTel
// monotonic Sums (Prom counters) and non-monotonic Sums /
// UpDownCounters, which the OTel→Prometheus compatibility spec maps
// to Prom type "gauge".
type metricMetaSpec struct {
	table     string
	kind      string
	monotonic *bool
}

func (h *Handler) fetchMetricMeta(ctx context.Context, metricName string, start, end time.Time, nowAnchored bool) ([]chclient.MetricMetaRow, error) {
	monotonic, nonMonotonic := true, false
	specs := []metricMetaSpec{
		{table: h.Schema.GaugeTable, kind: "gauge"},
	}
	if h.Schema.IsMonotonicColumn != "" {
		// Split the sum table by monotonicity: monotonic Sums are Prom
		// counters; non-monotonic Sums (OTel UpDownCounters — queue
		// depths, in-flight gauges, pool sizes) are Prom gauges.
		// Reporting them as counters makes consumers like Grafana's
		// Metrics Drilldown wrap them in rate(), which renders a
		// meaningless flat-0 preview.
		specs = append(
			specs,
			metricMetaSpec{table: h.Schema.SumTable, kind: "counter", monotonic: &monotonic},
			metricMetaSpec{table: h.Schema.SumTable, kind: "gauge", monotonic: &nonMonotonic},
		)
	} else {
		// Fallback for schema overrides whose sum table has no
		// IsMonotonic column: without the discriminator every sum-table
		// metric reports as counter (the pre-split behaviour).
		specs = append(specs, metricMetaSpec{table: h.Schema.SumTable, kind: "counter"})
	}
	specs = append(specs, metricMetaSpec{table: h.Schema.HistogramTable, kind: "histogram"})

	var out []chclient.MetricMetaRow
	for _, spec := range specs {
		sql, args := h.metricMetaSQL(spec.table, metricName, spec.monotonic, start, end, nowAnchored)
		rows, err := h.Client.QueryMetricMeta(ctx, sql, spec.kind, args...)
		if err != nil {
			return nil, &apiError{Kind: ErrInternal, Err: err, Status: http.StatusBadGateway}
		}
		out = append(out, rows...)
	}
	return out, nil
}

// metricMetaSQL builds the per-table metadata SQL. The result columns are
// (MetricName, MetricDescription, MetricUnit). When metricName is empty
// we list all distinct metrics; otherwise we filter to the named one.
// A non-nil monotonic adds a `IsMonotonic` / `NOT IsMonotonic` predicate
// (combined via AND with the metric-name filter when both are present).
//
// The window is applied in one of two shapes, picked by nowAnchored (mirroring
// metricNamesSQL):
//
//   - nowAnchored (the /api/v1/metadata listing, which takes no time params, so
//     its window always ends "now"): the bound is an aggregate-only
//     `HAVING max(TimeUnix) >= <start>`. Because samples are never future-dated
//     and MetricDescription/MetricUnit are metric-level (constant across a
//     metric's rows), this returns the byte-identical name/description/unit set
//     the WHERE-bounded form did, but the pure `GROUP BY MetricName` + aggregate
//     HAVING routes to the proj_metric_metadata aggregating projection — turning
//     the whole-table group into a tiny projection read. A raw WHERE
//     (metric-name filter or IsMonotonic predicate) keeps that arm off the
//     projection, so only the unfiltered list-all shape — the hot Grafana
//     call — routes; the filtered arms fall back to the bounded scan, which is
//     already cheap.
//   - !nowAnchored (a user-supplied finite [start,end]): the closed
//     WHERE-bounded window is kept (a historical window cannot be answered
//     exactly from per-name max alone), and partition pruning bounds the scan.
func (h *Handler) metricMetaSQL(table, metricName string, monotonic *bool, start, end time.Time, nowAnchored bool) (string, []any) {
	nameCol := h.Schema.MetricNameColumn
	descCol := h.Schema.MetricDescriptionColumn
	unitCol := h.Schema.MetricUnitColumn
	tsCol := h.Schema.TimestampColumn

	anyCall := func(col string) chsql.Frag {
		return chsql.Call("any", chsql.Col(col))
	}

	sb := chsql.NewQuery().
		Select(chsql.Col(nameCol), anyCall(descCol), anyCall(unitCol)).
		From(chsql.Col(table)).
		GroupBy(chsql.Col(nameCol))

	if nowAnchored {
		if !start.IsZero() {
			sb.Having(chsql.Gte(chsql.Call("max", chsql.Col(tsCol)), dateTime64Frag(start)))
		}
	} else if pred := h.metadataWindowPred(start, end); pred != nil {
		sb.Where(pred)
	}

	if monotonic != nil {
		// Bare boolean-column predicate (`IsMonotonic` / `NOT
		// IsMonotonic`) — no bound args, so the metric-name filter
		// below keeps its positional slot.
		pred := chsql.Col(h.Schema.IsMonotonicColumn)
		if !*monotonic {
			pred = chsql.Not(pred)
		}
		sb.Where(pred)
	}

	if metricName == "" {
		sb.OrderBy(chsql.Col(nameCol), false)
		sql, args := sb.Build()
		return sql, args
	}
	sb.Where(chsql.Eq(chsql.Col(nameCol), chsql.Lit(metricName)))
	return sb.Build()
}

// parseLimit decodes the `limit` query parameter — a positive integer.
func parseLimit(raw string) (int, error) {
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, errors.New("'limit' must be a non-negative integer")
	}
	return n, nil
}

// truncateMetadata trims the metadata map to at most `limit` metric keys
// (in alphabetic order to make the truncation deterministic).
func truncateMetadata(in map[string][]MetricMetaEntry, limit int) map[string][]MetricMetaEntry {
	if limit <= 0 || len(in) <= limit {
		return in
	}
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string][]MetricMetaEntry, limit)
	for _, k := range keys[:limit] {
		out[k] = in[k]
	}
	return out
}

// handleSeries implements GET /api/v1/series. Each matcher in `match[]`
// is expected to be a single VectorSelector; the union of distinct label
// sets across all matchers is returned (Prom convention: each entry
// includes the synthetic `__name__` label).
func (h *Handler) handleSeries(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}
	matchers := append([]string(nil), r.Form["match[]"]...)
	if len(matchers) == 0 {
		writeError(w, http.StatusBadRequest, ErrBadData,
			errors.New("at least one 'match[]' selector is required"))
		return
	}

	// Grafana's `label_values(metric, label)` template vars and panel
	// series probes pass the dashboard time range as start/end. Honour it
	// as the closed enumeration window — evaluating /series at wall-clock
	// `now` with a 5m staleness window (the pre-fix behaviour) silently
	// dropped any series whose newest sample was older than 5m (late
	// delta-temporality ingestion), so the env dropdown flapped empty.
	startT, endT, err := parseMetadataWindow(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}
	startT, endT = boundMetadataWindow(startT, endT)

	// Two-layer matcher fan-out — `expandUnderscoredMetricNameMatcher`
	// (dotted-storage candidates) nested inside `expandBareHistogramMatcher`
	// (classic-histogram companion variants). See the per-helper docstrings
	// for the resolution semantics each layer covers.
	//
	// Fan-in batching (task #71): the V×H (and matcher) variant set is
	// collapsed into ONE combined CH round-trip instead of one round-trip
	// per variant. Each variant lowers to a Sample-projecting SELECT; the
	// arms are UNION-ALL'd into a single query whose row stream the Go
	// dedup below folds into distinct label sets — same series returned,
	// N round-trips → 1. Pathologically broad probes chunk into ⌈N/K⌉
	// bounded queries (still ≪ N); see fetchSeries.
	variants := expandSeriesMatchers(h.parser, matchers, h.Schema.HistogramTable)
	sets, err := h.fetchSeries(r.Context(), variants, startT, endT)
	if err != nil {
		h.respondError(w, err)
		return
	}

	seen := make(map[string]map[string]string)
	for _, lset := range sets {
		key := format.CanonicalKey(lset)
		if _, ok := seen[key]; !ok {
			seen[key] = lset
		}
	}

	out := make([]map[string]string, 0, len(seen))
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, seen[k])
	}

	writeJSON(w, http.StatusOK, Response{
		Status: "success",
		Data:   out,
	})
}

// fetchLabelNames unions the Attributes-map keys across the metric tables
// and prepends `__name__`. The returned slice is sorted + normalised to
// Prom's `[a-zA-Z_][a-zA-Z0-9_]*` label-name grammar — OTel telemetry
// stores dotted keys (`service.name`, `http.request.method`) that PromQL
// grammar forbids in identifier position; without the rewrite, panels
// doing `sum by (service_name)` silently produce empty matrices.
func (h *Handler) fetchLabelNames(ctx context.Context, start, end time.Time, nowAnchored bool) ([]string, error) {
	sql := h.unionLabelNamesSQL(start, end, nowAnchored)
	names, err := timeCH(ctx, func() ([]string, error) {
		return h.Client.QueryStrings(ctx, sql)
	})
	if err != nil {
		return nil, &apiError{Kind: ErrInternal, Err: err, Status: http.StatusBadGateway}
	}
	collected := append([]string{model.MetricNameLabel}, names...)
	if h.resourceArmActive() {
		resNames, err := h.fetchResourceLabelNames(ctx, start, end)
		if err != nil {
			return nil, err
		}
		collected = append(collected, resNames...)
	}
	out := format.NormalizeLabelNames(collected)
	sort.Strings(out)
	return out, nil
}

// fetchResourceLabelNames returns the sanitized Prom label names for the
// allowlisted ResourceAttributes keys present across the metric tables.
// Each raw (dotted) resource key is intersected with the allowlist (nil
// allowlist = every key) and emitted in its dot->underscore sanitized form
// (the wire spelling operators see in Grafana). The caller folds these into
// the /labels listing alongside the Attributes keys + __name__.
func (h *Handler) fetchResourceLabelNames(ctx context.Context, start, end time.Time) ([]string, error) {
	resNames, err := timeCH(ctx, func() ([]string, error) {
		return h.Client.QueryStrings(ctx, h.unionResourceLabelNamesSQL(start, end))
	})
	if err != nil {
		return nil, &apiError{Kind: ErrInternal, Err: err, Status: http.StatusBadGateway}
	}
	allow := h.resourceAllowSet()
	out := make([]string, 0, len(resNames))
	for _, k := range resNames {
		if allow != nil {
			if _, ok := allow[k]; !ok {
				continue
			}
		}
		// Skip keys already backed by a dedicated top-level column
		// (service.name → ServiceName): the dedicated path surfaces them,
		// so promoting them via the resource arm too would double-list the
		// label and diverge from reference Prometheus.
		if promql.DedicatedResourceLabelExcluded(h.Schema, k) {
			continue
		}
		out = append(out, format.OTelToPromLabel(k))
	}
	return out, nil
}

func (h *Handler) fetchLabelValues(ctx context.Context, name string, start, end time.Time, nowAnchored bool) ([]string, error) {
	if name == model.MetricNameLabel {
		return h.fetchMetricNameValues(ctx, start, end, nowAnchored)
	}
	sql, args := h.unionLabelValuesSQL(name, start, end, nowAnchored)
	values, err := timeCH(ctx, func() ([]string, error) {
		return h.Client.QueryStrings(ctx, sql, args...)
	})
	if err != nil {
		return nil, &apiError{Kind: ErrInternal, Err: err, Status: http.StatusBadGateway}
	}
	sort.Strings(values)
	return values, nil
}

// fetchMetricNameValues assembles `/api/v1/label/__name__/values` so
// that every advertised name is actually queryable — the catalog's
// invariant against the query surface (`/api/v1/query` with
// `{__name__="<advertised>"}` must return the metric's series).
//
// Two table groups feed the catalog with different name shapes:
//
//   - Gauge + sum tables store one wire-visible series per row; their
//     MetricNames surface as-is (Prom-grammar-normalised through
//     [normalizeMetricValues] so dotted OTel names like
//     `k8s.node.cpu.usage` advertise as `k8s_node_cpu_usage` — the
//     selector lowering's MetricName candidate fan-out in
//     [internal/promql] resolves the underscored alias back to the
//     dotted storage rows).
//
//   - The classic-histogram table stores ONE row per histogram sample
//     under the BARE base name, but the PromQL surface exposes that
//     row only as the three companion series `<base>_bucket` /
//     `<base>_count` / `<base>_sum` — a bare `{__name__="<base>"}`
//     selector routes to the gauge/sum tables (see
//     [schema.Metrics.TablesFor]) and returns empty. Reference
//     Prometheus behaves the same way: a classic histogram's
//     `__name__` values contain ONLY the suffixed forms, never the
//     bare family name. So each histogram-table base name expands to
//     exactly the three companion names and the bare name is dropped.
//
// The exponential-histogram + summary tables stay out of the catalog
// deliberately: the bare-selector query surface doesn't read either
// table (exp-histograms are reachable only through the
// `histogram_quantile` + [schema.Metrics.ExpHistogramSuffix] routing;
// the summary table has no lowering at all), and advertising names the
// query surface can't serve is exactly the bug this function's split
// shape exists to prevent.
func (h *Handler) fetchMetricNameValues(ctx context.Context, start, end time.Time, nowAnchored bool) ([]string, error) {
	bareTables, histogramTable := h.catalogNameTables()
	var values []string
	if len(bareTables) > 0 {
		sql := h.metricNamesSQL(bareTables, start, end, nowAnchored)
		bare, err := timeCH(ctx, func() ([]string, error) {
			return h.Client.QueryStrings(ctx, sql)
		})
		if err != nil {
			return nil, &apiError{Kind: ErrInternal, Err: err, Status: http.StatusBadGateway}
		}
		// Metric-name values pass through Prom's metric-name grammar
		// (`[a-zA-Z_:][a-zA-Z0-9_:]*`); OTel may store dotted forms
		// (`http.server.duration`) that the PromQL selector position
		// can't reference directly.
		values = normalizeMetricValues(bare)
	}
	if histogramTable != "" {
		sql := h.metricNamesSQL([]string{histogramTable}, start, end, nowAnchored)
		hist, err := timeCH(ctx, func() ([]string, error) {
			return h.Client.QueryStrings(ctx, sql)
		})
		if err != nil {
			return nil, &apiError{Kind: ErrInternal, Err: err, Status: http.StatusBadGateway}
		}
		for _, base := range normalizeMetricValues(hist) {
			for _, suf := range []string{"_bucket", "_count", "_sum"} {
				values = append(values, base+suf)
			}
		}
	}
	// Cross-group dedupe: a sum-table counter that already carries a
	// companion-shaped name (`http_server_request_duration_count` from
	// the OTel-hostmetrics emitters) collides with the histogram
	// expansion of its base name; both spellings denote the same wire
	// series set, so one entry survives.
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Strings(out)
	return out, nil
}

// catalogNameTables partitions the configured metric tables into the
// bare-name group (gauge + sum — names advertise verbatim) and the
// histogram table (names advertise as the three companion suffixes).
// Empty table names are skipped; duplicate configurations collapse —
// in particular, a deployment that points SumTable at the histogram
// table keeps that table in the bare group (matching the lowering's
// single-arm fallback for that degenerate config) and disables the
// suffix expansion rather than advertising the same physical rows
// twice.
func (h *Handler) catalogNameTables() (bareTables []string, histogramTable string) {
	inBare := map[string]struct{}{}
	for _, t := range []string{h.Schema.GaugeTable, h.Schema.SumTable} {
		if t == "" {
			continue
		}
		if _, dup := inBare[t]; dup {
			continue
		}
		inBare[t] = struct{}{}
		bareTables = append(bareTables, t)
	}
	if t := h.Schema.HistogramTable; t != "" {
		if _, dup := inBare[t]; !dup {
			histogramTable = t
		}
	}
	return bareTables, histogramTable
}

// fetchLabelNamesMatched returns the distinct label names of series
// matching any of the given match[] selectors. The synthetic `__name__`
// is always included if at least one selector matches anything. Names
// pass through Prom-grammar normalisation before dedupe; see
// `format.NormalizeLabelNames` for the collision policy.
//
// Each input matcher fans out through expandBareHistogramMatcher: a
// bare classic-histogram base name (no `_bucket` / `_count` / `_sum`
// / `_total` suffix) also visits its three Prom-companion variants
// against the histogram table. Without the fan-out, Grafana's Metrics
// Explorer — which surfaces the bare base name from cerberus's
// `__name__` listing and queries `match[]=<base>` for the labels chip
// — would see only `__name__` and render "Unable to fetch labels".
// Non-histogram inputs short-circuit through the no-op (single-element)
// return of the expander.
func (h *Handler) fetchLabelNamesMatched(ctx context.Context, matchers []string, start, end time.Time) ([]string, error) {
	// Fan-in batching (task #71): the variant fan-out across all matchers
	// collapses into ONE combined query (chunked under CH's max_query_size
	// when broad). Each variant lowers to its inner matcher SELECT; the
	// arms UNION-ALL into the FROM source of a single
	// `SELECT DISTINCT arrayJoin(mapKeys(Attributes))` — N round-trips → 1
	// (⌈N/K⌉ for a pathologically broad probe), same distinct key set the
	// per-arm loop collected. start/end bound the closed metadata window
	// each variant scans (zero = whole table).
	variants := expandSeriesMatchers(h.parser, matchers, h.Schema.HistogramTable)
	keys, err := h.labelKeysForMatchers(ctx, variants, start, end)
	if err != nil {
		return nil, err
	}
	collected := append([]string{model.MetricNameLabel}, keys...)
	out := format.NormalizeLabelNames(collected)
	sort.Strings(out)
	return out, nil
}

// fetchLabelValuesMatched returns the distinct values of <name> across
// series matching any of the given match[] selectors. The start/end pair
// is forwarded to matcherSQL via labelValuesForMatchers so the lowering's
// LWR anchor reflects the request window when present.
//
// As with fetchLabelNamesMatched, each matcher fans out through
// expandBareHistogramMatcher so the bare-base-name shape (which
// otherwise lowers to a gauge-table scan and returns empty for any
// histogram metric) also visits the three classic-histogram companion
// variants. See expandBareHistogramMatcher for the rationale.
func (h *Handler) fetchLabelValuesMatched(ctx context.Context, name string, matchers []string, start, end time.Time) ([]string, error) {
	// Fan-in batching (task #71): the variant fan-out across all matchers
	// collapses into ONE combined query (chunked under CH's max_query_size
	// when broad). Each variant's matched-row subquery is a UNION-ALL arm
	// of the shared scan; the per-name value projection (the `__name__` /
	// single-candidate / multi-candidate shapes) runs once over that union
	// — N round-trips → 1 (⌈N/K⌉ for a pathologically broad probe).
	variants := expandSeriesMatchers(h.parser, matchers, h.Schema.HistogramTable)
	vals, err := h.labelValuesForMatchers(ctx, name, variants, start, end)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, v := range vals {
		if v != "" {
			seen[v] = true
		}
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	if name == model.MetricNameLabel {
		// The matcher subquery projects the STORED MetricName, which may
		// be OTel-dotted (`k8s.node.cpu.usage`) — the selector lowering's
		// candidate fan-out matches those rows from an underscored-alias
		// matcher. Normalise so the matched values surface in the same
		// Prom grammar the unmatched catalog emits; without this the
		// dotted storage spelling leaks to the wire and the client gets a
		// name the selector position can't reference.
		out = normalizeMetricValues(out)
	}
	sort.Strings(out)
	return out, nil
}

// matcherArms lowers each match[] selector variant to its inner matcher
// SELECT and wraps it as a parenthesised subquery Frag — the per-variant
// UNION-ALL arm shared by the batched label-keys / label-values builders.
// start/end anchor the matcher lowering's LWR window (zero-time falls back
// to the lowering default).
func (h *Handler) matcherArms(ctx context.Context, matchers []string, start, end time.Time) ([]chsql.Frag, error) {
	arms := make([]chsql.Frag, 0, len(matchers))
	for _, m := range matchers {
		innerSQL, args, err := h.matcherSQL(ctx, m, start, end)
		if err != nil {
			return nil, err
		}
		arms = append(arms, matcherSubqueryFrag(innerSQL, args))
	}
	return arms, nil
}

// labelKeysForMatchers lowers each match[] selector variant, UNION-ALLs
// their matched-row subqueries into one scan, and wraps the union in a
// `SELECT DISTINCT arrayJoin(mapKeys(Attributes))` to extract the attribute
// keys across all variants in a single CH round-trip (task #71).
//
// Bounded-batch-or-fallback: the variants are arm-capped into ⌈N/K⌉
// chunks (chunkMatcherVariants); the rendered-size guard
// (buildBoundedChunkSQL) then splits any chunk whose combined SQL still
// breaches maxRenderedQueryBytes. The caller (fetchLabelNamesMatched)
// re-dedupes via format.NormalizeLabelNames, so the per-query key sets can
// overlap safely. An empty variant list yields no keys (and no query).
func (h *Handler) labelKeysForMatchers(ctx context.Context, matchers []string, start, end time.Time) ([]string, error) {
	if len(matchers) == 0 {
		return nil, nil
	}
	attrsCol := h.Schema.AttributesColumn
	combine := func(arms []chsql.Frag) (string, []any) {
		return chsql.NewQuery().
			Select(chsql.As(arrayJoinMapKeysFrag(attrsCol), "name")).
			From(chsql.Paren(chsql.UnionAll(arms...))).
			OrderBy(chsql.Col("name"), false).
			Build()
	}
	var all []string
	for _, chunk := range chunkMatcherVariants(matchers) {
		arms, err := h.matcherArms(ctx, chunk, start, end)
		if err != nil {
			return nil, err
		}
		for _, q := range buildBoundedChunkSQL(arms, combine) {
			keys, err := timeCH(ctx, func() ([]string, error) {
				return h.Client.QueryStrings(ctx, q.sql, q.args...)
			})
			if err != nil {
				return nil, err
			}
			all = append(all, keys...)
		}
	}
	return all, nil
}

// labelValuesForMatchers lowers each match[] selector variant, UNION-ALLs
// their matched-row subqueries into one shared scan, and projects the
// named label's distinct values over that union in a single CH round-trip
// (task #71). `__name__` resolves to MetricName; other labels to
// `Attributes[<name>]`. start/end anchor the matcher lowering's LWR window
// (zero-time falls back to the lowering default).
//
// Bounded-batch-or-fallback: as with labelKeysForMatchers the variants are
// arm-capped into ⌈N/K⌉ chunks and the rendered-size guard splits any
// chunk that still breaches maxRenderedQueryBytes. The caller re-dedupes
// via its `seen` map, so per-query value sets can overlap safely. An empty
// variant list yields no values (and no query).
func (h *Handler) labelValuesForMatchers(ctx context.Context, name string, matchers []string, start, end time.Time) ([]string, error) {
	if len(matchers) == 0 {
		return nil, nil
	}
	combine := h.labelValueCombine(name)
	var all []string
	for _, chunk := range chunkMatcherVariants(matchers) {
		arms, err := h.matcherArms(ctx, chunk, start, end)
		if err != nil {
			return nil, err
		}
		for _, q := range buildBoundedChunkSQL(arms, combine) {
			vals, err := timeCH(ctx, func() ([]string, error) {
				return h.Client.QueryStrings(ctx, q.sql, q.args...)
			})
			if err != nil {
				return nil, err
			}
			all = append(all, vals...)
		}
	}
	return all, nil
}

// labelValueCombine returns the per-endpoint combine closure that projects
// the distinct values of label <name> over a UNION-ALL of matcher-variant
// arms. The closure shape is the three label-value projections the
// pre-batch single-matcher path used: `__name__` → MetricName, a
// single-candidate label → one `Attributes[k]` projection, a
// multi-candidate label → an inner per-candidate UNION over the shared
// matched-row scan (so a user-supplied `cerberus_ql` reaches both the
// underscored and dotted storage forms).
func (h *Handler) labelValueCombine(name string) func([]chsql.Frag) (string, []any) {
	if name == model.MetricNameLabel {
		return func(arms []chsql.Frag) (string, []any) {
			return chsql.NewQuery().
				Select(chsql.As(distinctIdent(h.Schema.MetricNameColumn), "value")).
				From(chsql.Paren(chsql.UnionAll(arms...))).
				OrderBy(chsql.Col("value"), false).
				Build()
		}
	}
	attrsCol := h.Schema.AttributesColumn
	candidates := labelValueCandidates(name)
	if len(candidates) == 1 {
		// Single candidate (the typical `job` / `instance` shape): the
		// value projection runs directly over the combined matched-row
		// scan.
		return func(arms []chsql.Frag) (string, []any) {
			return chsql.NewQuery().
				Select(chsql.As(distinctMapAtFrag(attrsCol, candidates[0]), "value")).
				From(chsql.Paren(chsql.UnionAll(arms...))).
				Where(mapAtNotEmptyFrag(attrsCol, candidates[0])).
				OrderBy(chsql.Col("value"), false).
				Build()
		}
	}
	// Multi-candidate fan-out: emit one inner UNION arm per candidate over
	// the SAME combined matched-row scan so a user-supplied `cerberus_ql`
	// reaches both the underscored and dotted storage forms. The matched
	// scan is itself a UNION-ALL of the matcher variants; both fan-outs
	// stay inside the single combined query.
	return func(arms []chsql.Frag) (string, []any) {
		matchedFrom := chsql.Paren(chsql.UnionAll(arms...))
		parts := make([]chsql.Frag, 0, len(candidates))
		for _, k := range candidates {
			arm := chsql.NewQuery().
				Select(chsql.As(distinctMapAtFrag(attrsCol, k), "value")).
				From(matchedFrom).
				Where(mapAtNotEmptyFrag(attrsCol, k))
			parts = append(parts, arm.Frag())
		}
		return chsql.NewQuery().
			Select(chsql.As(distinctIdent("value"), "")).
			From(chsql.Paren(chsql.UnionAll(parts...))).
			OrderBy(chsql.Col("value"), false).
			Build()
	}
}

// matcherSQL lowers a single /labels or /label/<name>/values matcher to
// its inner SQL + args. The caller wraps this in whatever projection it
// needs (DISTINCT mapKeys, DISTINCT Attributes[name], etc.). start/end
// bound the closed [start,end] metadata window (promql.LowerMetadataRange);
// a zero bound is omitted. The full-range window — not an instant
// staleness window at `end` — is what lets these endpoints surface a
// label/value whose only sample sits early in the requested range.
func (h *Handler) matcherSQL(ctx context.Context, matcher string, start, end time.Time) (string, []any, error) {
	expr, err := h.parseExpr(ctx, matcher)
	if err != nil {
		return "", nil, &apiError{Kind: ErrBadData, Err: err, Status: http.StatusBadRequest}
	}
	plan, err := promql.LowerMetadataRange(ctx, expr, h.Schema, start, end)
	if err != nil {
		return "", nil, &apiError{Kind: ErrBadData, Err: err, Status: http.StatusBadRequest}
	}
	plan = h.Optimizer.Run(ctx, plan)
	sql, args, err := chsql.Emit(ctx, plan)
	if err != nil {
		return "", nil, &apiError{Kind: ErrInternal, Err: err, Status: http.StatusInternalServerError}
	}
	return sql, args, nil
}

// expandSeriesMatchers fans every input match[] selector out through the
// two-layer variant expansion (`expandUnderscoredMetricNameMatcher` ⊃
// `expandBareHistogramMatcher`) and flattens the result into one
// deduplicated list of matcher strings. The flattened list is the
// candidate set the combined /api/v1/series query UNION-ALLs into a single
// scan — collapsing the former V×H×matcher round-trip fan-out (task #71).
func expandSeriesMatchers(parser promparser.Parser, matchers []string, histogramTable string) []string {
	seen := make(map[string]struct{})
	out := make([]string, 0, len(matchers))
	for _, m := range matchers {
		for _, nameVariant := range expandUnderscoredMetricNameMatcher(parser, m) {
			for _, variant := range expandBareHistogramMatcher(parser, nameVariant, histogramTable) {
				if _, dup := seen[variant]; dup {
					continue
				}
				seen[variant] = struct{}{}
				out = append(out, variant)
			}
		}
	}
	return out
}

// fetchSeries lowers every matcher variant to a Sample-projecting
// Scan+Filter, UNION-ALLs the arms into ONE combined query, runs it as a
// single CH round-trip, and dedupes the resulting label sets.
//
// Fan-in batching (task #71): the pre-#71 shape issued one `Client.Query`
// per variant (V×H fan-out — up to 32 sequential round-trips for a
// histogram-base request, ~330ms on the demo dataset). Each variant's
// lowered Sample-shape SELECT is now a UNION-ALL arm of a single query;
// the Go dedup below folds the combined row stream into distinct label
// sets exactly as the per-arm loop did, so the returned series are
// identical — only the round-trip count drops to 1.
//
// Bounded-batch-or-fallback: the variant set is arm-capped into ⌈N/K⌉
// chunks (chunkMatcherVariants) and the rendered-size guard
// (buildBoundedChunkSQL) splits any chunk whose combined SQL still
// breaches maxRenderedQueryBytes — typical requests stay one round-trip;
// only a pathologically broad probe fans into a few bounded queries
// (still ≪ N), merged through the dedup below.
func (h *Handler) fetchSeries(ctx context.Context, matchers []string, start, end time.Time) ([]map[string]string, error) {
	if len(matchers) == 0 {
		return nil, nil
	}

	seen := make(map[string]map[string]string)
	// No labelMemo here: this loop folds samples from SEVERAL independent
	// queries (chunk × matcher-variant), each its own cursor with its OWN
	// SeriesID namespace restarting at 1. A SeriesID-keyed memo would alias
	// rows from different cursors that happen to share a per-cursor ordinal.
	// The memo also buys nothing on this path — /series emits ONE label set
	// per series, not K samples per series, and the `seen` map already dedups
	// by canonical key. So normalise directly, per row.
	for _, chunk := range chunkMatcherVariants(matchers) {
		samples, err := h.fetchSeriesChunk(ctx, chunk, start, end)
		if err != nil {
			return nil, err
		}
		for _, s := range samples {
			labels := format.NormalizeLabelMap(format.WithMetricName(s.Labels, s.MetricName))
			key := format.CanonicalKey(labels)
			if _, ok := seen[key]; !ok {
				seen[key] = labels
			}
		}
	}
	out := make([]map[string]string, 0, len(seen))
	for _, l := range seen {
		out = append(out, l)
	}
	return out, nil
}

// fetchSeriesChunk runs the combined Sample-projecting query (or queries,
// when the rendered-size guard splits) over a single arm-cap chunk of
// matcher variants and returns the raw samples. The caller folds the
// per-chunk samples into the cross-chunk dedup.
func (h *Handler) fetchSeriesChunk(ctx context.Context, matchers []string, start, end time.Time) ([]chclient.Sample, error) {
	// Single-matcher fast path: run the lowered Sample-shape SELECT
	// directly as the top-level statement — byte-identical to the
	// pre-#71 per-arm query (the engine ran this same SQL). Avoids
	// wrapping the Map-typed Attributes column in an extra `SELECT * FROM
	// (…)` boundary, which some CH drivers (chdb) refuse to cast back to
	// MAP.
	if len(matchers) == 1 {
		sql, args, err := h.seriesMatcherSQL(ctx, matchers[0], start, end)
		if err != nil {
			return nil, err
		}
		return h.querySamples(ctx, sql, args)
	}
	// Multi-matcher: UNION-ALL the per-variant Sample-shape SELECTs into
	// ONE statement. `chsql.UnionAll` emits `(arm1) UNION ALL (arm2) …` —
	// itself a valid top-level SELECT, so no outer `SELECT *` wrapper is
	// needed (and the Map column stays castable). The rendered-size guard
	// splits the arm set further if the combined SQL would breach the
	// byte budget.
	arms := make([]chsql.Frag, 0, len(matchers))
	for _, m := range matchers {
		s, a, err := h.seriesMatcherSQL(ctx, m, start, end)
		if err != nil {
			return nil, err
		}
		arms = append(arms, matcherSubqueryFrag(s, a))
	}
	combine := func(arms []chsql.Frag) (string, []any) {
		return chsql.Render(chsql.UnionAll(arms...))
	}
	var out []chclient.Sample
	for _, q := range buildBoundedChunkSQL(arms, combine) {
		samples, err := h.querySamples(ctx, q.sql, q.args)
		if err != nil {
			return nil, err
		}
		out = append(out, samples...)
	}
	return out, nil
}

// querySamples runs a Sample-projecting SELECT and maps the CH error to a
// 502 apiError. Shared by the single-matcher fast path and the
// multi-matcher UNION-ALL path in fetchSeriesChunk.
func (h *Handler) querySamples(ctx context.Context, sql string, args []any) ([]chclient.Sample, error) {
	samples, err := timeCH(ctx, func() ([]chclient.Sample, error) {
		return h.Client.Query(ctx, sql, args...)
	})
	if err != nil {
		return nil, &apiError{Kind: ErrInternal, Err: err, Status: http.StatusBadGateway}
	}
	return samples, nil
}

// seriesMatcherSQL lowers a single /api/v1/series matcher to a SELECT that
// projects the Sample-shape columns (MetricName, Attributes, TimeUnix,
// Value) — the projection `chclient.Client.Query` binds positionally. It
// mirrors the engine instant-query path (`executeInstant` →
// `lang.ProjectSamples`): lower, apply `wrapWithSampleProjection`,
// optimize, emit. The Sample shape is what lets every variant become a
// UNION-ALL arm of the one combined query fetchSeries runs.
//
// start/end bound the closed [start,end] metadata window the matcher
// enumeration scans (promql.LowerMetadataRange) — NOT an instant
// staleness window at `end`. A zero start/end omits that bound. This is
// what makes /series return a series with any in-window sample instead of
// only series with a sample in the last 5m at wall-clock `now`.
func (h *Handler) seriesMatcherSQL(ctx context.Context, matcher string, start, end time.Time) (string, []any, error) {
	expr, err := h.parseExpr(ctx, matcher)
	if err != nil {
		return "", nil, &apiError{Kind: ErrBadData, Err: err, Status: http.StatusBadRequest}
	}
	plan, err := promql.LowerMetadataRange(ctx, expr, h.Schema, start, end)
	if err != nil {
		return "", nil, &apiError{Kind: ErrBadData, Err: err, Status: http.StatusBadRequest}
	}
	plan = wrapWithSampleProjection(plan, h.Schema)
	plan = h.Optimizer.Run(ctx, plan)
	sql, args, err := chsql.Emit(ctx, plan)
	if err != nil {
		return "", nil, &apiError{Kind: ErrInternal, Err: err, Status: http.StatusInternalServerError}
	}
	return sql, args, nil
}

// resourceArmActive reports whether the unmatched catalog endpoints
// (/labels, /label/<name>/values) should surface OTel ResourceAttributes
// keys as Prometheus labels. The schema must name a ResourceAttributes
// column; a custom schema that clears it opts out entirely.
//
// The allowlist (Schema.PromResourceLabels) narrows WHICH keys surface but
// does NOT gate the feature on/off — an empty allowlist promotes every
// resource key (the locked promote-all default).
func (h *Handler) resourceArmActive() bool {
	return h.Schema.ResourceAttributesColumn != ""
}

// resourceAllowSet returns the allowlist as a set of ORIGINAL dotted OTel
// keys, or nil when no allowlist is configured (promote-all). Callers
// treat nil as "every key allowed".
func (h *Handler) resourceAllowSet() map[string]struct{} {
	if len(h.Schema.PromResourceLabels) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(h.Schema.PromResourceLabels))
	for _, k := range h.Schema.PromResourceLabels {
		set[k] = struct{}{}
	}
	return set
}

// resourceLabelValueArmActive reports whether a /label/<name>/values
// lookup for promLabel should emit a ResourceAttributes value arm. True
// when the resource arm is active AND either no allowlist is configured or
// at least one dot<->underscore candidate of promLabel names an
// allowlisted dotted key — mirroring the matcher-side
// promql.resourceLabelAllowed gate so the listing surface and the query
// surface agree on which labels are resource-backed.
func (h *Handler) resourceLabelValueArmActive(promLabel string) bool {
	if !h.resourceArmActive() {
		return false
	}
	// A dedicated-column-backed label (service.name → ServiceName) is
	// surfaced by the dedicated path, never the resource arm — promoting it
	// here too would double-promote and diverge from reference Prometheus.
	if promql.DedicatedResourceLabelExcluded(h.Schema, promLabel) {
		return false
	}
	allow := h.resourceAllowSet()
	if allow == nil {
		return true
	}
	for _, cand := range format.PromLabelToOTelCandidates(promLabel) {
		if _, ok := allow[cand]; ok {
			return true
		}
	}
	return false
}

// unionLabelNamesSQL builds a UNION of all metric tables' label keys. In the
// now-anchored case each arm emits the grouped form
// `arrayJoin(mapKeys(Attributes)) ... GROUP BY MetricName, Attributes HAVING
// max(TimeUnix) >= start`, so the key fan-out routes onto the proj_series
// aggregating projection (Attributes is a grouping key, the time bound an
// aggregate predicate). A user-supplied finite window keeps the exact
// WHERE-bounded scan.
func (h *Handler) unionLabelNamesSQL(start, end time.Time, nowAnchored bool) string {
	tables := h.metricTables()
	attrsCol := h.Schema.AttributesColumn
	metricCol := h.Schema.MetricNameColumn
	tsCol := h.Schema.TimestampColumn
	pred := h.metadataWindowPred(start, end)
	parts := make([]chsql.Frag, 0, len(tables))
	for _, t := range tables {
		arm := chsql.NewQuery().
			Select(chsql.As(arrayJoinMapKeysFrag(attrsCol), "name")).
			From(chsql.Col(t))
		if nowAnchored {
			arm = arm.GroupBy(chsql.Col(metricCol), chsql.Col(attrsCol))
			if !start.IsZero() {
				arm = arm.Having(chsql.Gte(chsql.Call("max", chsql.Col(tsCol)), dateTime64Frag(start)))
			}
		} else if pred != nil {
			arm = arm.Where(pred)
		}
		parts = append(parts, arm.Frag())
	}
	outer := chsql.NewQuery().
		Select(chsql.As(distinctIdent("name"), "")).
		From(chsql.Paren(chsql.UnionAll(parts...))).
		OrderBy(chsql.Col("name"), false)
	sql, _ := outer.Build()
	return sql
}

// unionResourceLabelNamesSQL builds a UNION of all metric tables'
// ResourceAttributes keys — the resource-side mirror of
// [unionLabelNamesSQL]. The raw (dotted) keys are returned; the caller
// sanitizes + allowlist-filters in Go (cheaper than an N-key SQL IN over
// every row's map, and it keeps the Attributes union byte-identical so the
// promote-all default adds no churn to existing fixtures).
func (h *Handler) unionResourceLabelNamesSQL(start, end time.Time) string {
	tables := h.metricTables()
	resCol := h.Schema.ResourceAttributesColumn
	pred := h.metadataWindowPred(start, end)
	parts := make([]chsql.Frag, 0, len(tables))
	for _, t := range tables {
		arm := chsql.NewQuery().
			Select(chsql.As(arrayJoinMapKeysFrag(resCol), "name")).
			From(chsql.Col(t))
		if pred != nil {
			arm = arm.Where(pred)
		}
		parts = append(parts, arm.Frag())
	}
	outer := chsql.NewQuery().
		Select(chsql.As(distinctIdent("name"), "")).
		From(chsql.Paren(chsql.UnionAll(parts...))).
		OrderBy(chsql.Col("name"), false)
	sql, _ := outer.Build()
	return sql
}

// dateTime64Frag renders the `toDateTime64('<ts>', 9)` literal used to
// bound the metadata-discovery window. It mirrors the matched-path bound
// emitted by promql.metadataBoundExpr so the unmatched (no-`match[]`)
// discovery arms carry byte-identical time semantics.
func dateTime64Frag(t time.Time) chsql.Frag {
	return chsql.Frag(func(b *chsql.Builder) { b.DateTime64Lit(t) })
}

// metadataWindowPred builds the closed `[start,end]` TimeUnix bound the
// no-`match[]` discovery arms push so a request that carries a window
// (Grafana's metric/label picker forwards the dashboard range) prunes by
// the `toDate(TimeUnix)` partition instead of streaming the whole table —
// turning the leading-key DISTINCT from an O(rows) full-column scan into
// an O(window) partition-bounded scan. Each bound is omitted when zero, so
// a no-bound side scans the whole table, matching reference Prometheus's
// min/max-retention default and the matched-path semantics in
// promql.wrapMetadataFullRange. Returns nil when both bounds are zero
// (no WHERE emitted — byte-identical to the prior unbounded form, which is
// the inherent exact answer when the caller supplies no window).
func (h *Handler) metadataWindowPred(start, end time.Time) chsql.Frag {
	tsCol := h.Schema.TimestampColumn
	var bounds []chsql.Frag
	if !start.IsZero() {
		bounds = append(bounds, chsql.Gte(chsql.Col(tsCol), dateTime64Frag(start)))
	}
	if !end.IsZero() {
		bounds = append(bounds, chsql.Lte(chsql.Col(tsCol), dateTime64Frag(end)))
	}
	switch len(bounds) {
	case 0:
		return nil
	case 1:
		return bounds[0]
	default:
		return chsql.And(bounds...)
	}
}

// metricNamesSQL returns the distinct MetricName values across the
// given tables. Callers group tables by how their names surface in the
// catalog (see fetchMetricNameValues) — the SQL shape itself is the
// same UNION-of-per-table-arms for any group size.
//
// Two arm shapes, picked by nowAnchored:
//
//   - nowAnchored (the Grafana metric-picker case: no explicit end, so the
//     window ends "now" or is open-ended): each arm is
//     `SELECT MetricName AS value FROM <t> GROUP BY MetricName
//     HAVING max(TimeUnix) >= <start>`. Because samples are never
//     future-dated, max(TimeUnix) >= start ⇔ the name has a sample in
//     [start, now] — byte-for-byte the same name set as the WHERE-bounded
//     DISTINCT, but the aggregate-only predicate is served from the
//     `proj_series` aggregating projection (GROUP BY MetricName, Attributes
//     carrying max(TimeUnix)) instead of full-scanning the fact table.
//     ClickHouse re-aggregates the finer (MetricName, Attributes) projection
//     to the coarser GROUP BY MetricName via max-of-maxes, so one projection
//     serves both this enumeration and the generic label_values / label-name
//     shapes. On prod this turns the ~4.2B-row / ~139 GiB windowless
//     enumeration into a sub-megabyte projection read.
//
//   - !nowAnchored (a user-supplied finite [start,end]): the exact
//     WHERE-bounded DISTINCT is kept — a closed historical window cannot be
//     answered exactly from per-name min/max alone (a name whose samples
//     straddle but skip the window would be a false positive), and the
//     request's own range is where MergeTree partition pruning already bounds
//     the scan.
func (h *Handler) metricNamesSQL(tables []string, start, end time.Time, nowAnchored bool) string {
	metricCol := h.Schema.MetricNameColumn
	parts := make([]chsql.Frag, 0, len(tables))
	if nowAnchored {
		tsCol := h.Schema.TimestampColumn
		for _, t := range tables {
			arm := chsql.NewQuery().
				Select(chsql.As(chsql.Col(metricCol), "value")).
				From(chsql.Col(t)).
				GroupBy(chsql.Col(metricCol))
			if !start.IsZero() {
				arm = arm.Having(chsql.Gte(chsql.Call("max", chsql.Col(tsCol)), dateTime64Frag(start)))
			}
			parts = append(parts, arm.Frag())
		}
	} else {
		pred := h.metadataWindowPred(start, end)
		for _, t := range tables {
			arm := chsql.NewQuery().
				Select(chsql.As(distinctIdent(metricCol), "value")).
				From(chsql.Col(t))
			if pred != nil {
				arm = arm.Where(pred)
			}
			parts = append(parts, arm.Frag())
		}
	}
	outer := chsql.NewQuery().
		Select(chsql.As(distinctIdent("value"), "")).
		From(chsql.Paren(chsql.UnionAll(parts...))).
		OrderBy(chsql.Col("value"), false)
	sql, _ := outer.Build()
	return sql
}

// unionLabelValuesSQL returns the distinct Attributes[?] values across
// tables, skipping the empty-string sentinel that mapAccess yields when
// a key is absent. Returns (sql, args). Each (table × candidate) pair
// binds three args: the candidate key (SELECT MapAt), the same key
// (WHERE MapAt), and the empty-string sentinel (WHERE Lit("")). The
// candidate set comes from [format.PromLabelToOTelCandidates] so a
// user-supplied `cerberus_ql` also reaches rows that store the OTel
// dotted form `cerberus.ql`. Without the fan-out
// `/api/v1/label/cerberus_ql/values` returned `[]` because PR #657
// normalised the LISTING side but kept the per-name lookup hitting
// `Attributes['cerberus_ql']` verbatim — the storage rows wrote the
// OTel dotted sibling and the underscored Map key was absent.
//
// Mirrors the matcher-side `attributeLookup` chain in
// `internal/promql/lower.go`: both query and listing surfaces now
// resolve the same Prom-grammar → OTel-key candidates the same way.
func (h *Handler) unionLabelValuesSQL(name string, start, end time.Time, nowAnchored bool) (string, []any) {
	tables := h.metricTables()
	attrsCol := h.Schema.AttributesColumn
	metricCol := h.Schema.MetricNameColumn
	tsCol := h.Schema.TimestampColumn
	candidates := labelValueCandidates(name)
	resCol := h.Schema.ResourceAttributesColumn
	resourceArm := h.resourceLabelValueArmActive(name)
	pred := h.metadataWindowPred(start, end)
	// withWindow ANDs the closed metadata window onto an arm's not-empty
	// predicate so the per-table scan prunes by partition when a window is
	// present; with no window it returns the not-empty predicate unchanged
	// (byte-identical to the prior emit).
	withWindow := func(notEmpty chsql.Frag) chsql.Frag {
		if pred == nil {
			return notEmpty
		}
		return chsql.And(notEmpty, pred)
	}
	// attrsArm builds the Attributes-map arm. In the now-anchored case it
	// emits the grouped form `GROUP BY MetricName, Attributes HAVING
	// max(TimeUnix) >= start` so the leading-key DISTINCT routes onto the
	// proj_series aggregating projection (Attributes is a grouping key, the
	// time bound an aggregate predicate — both materialized on the
	// projection). The not-empty sentinel filter is also a per-group
	// predicate (Attributes[k] is constant within a (MetricName, Attributes)
	// group), so it rides in HAVING and the projection still serves the read.
	// A user-supplied finite window keeps the exact WHERE-bounded DISTINCT.
	attrsArm := func(t, k string) chsql.Frag {
		if nowAnchored {
			arm := chsql.NewQuery().
				Select(chsql.As(distinctMapAtFrag(attrsCol, k), "value")).
				From(chsql.Col(t)).
				GroupBy(chsql.Col(metricCol), chsql.Col(attrsCol)).
				Having(mapAtNotEmptyFrag(attrsCol, k))
			if !start.IsZero() {
				arm.Having(chsql.Gte(chsql.Call("max", chsql.Col(tsCol)), dateTime64Frag(start)))
			}
			return arm.Frag()
		}
		return chsql.NewQuery().
			Select(chsql.As(distinctMapAtFrag(attrsCol, k), "value")).
			From(chsql.Col(t)).
			Where(withWindow(mapAtNotEmptyFrag(attrsCol, k))).
			Frag()
	}
	parts := make([]chsql.Frag, 0, len(tables)*len(candidates)*2)
	for _, t := range tables {
		for _, k := range candidates {
			parts = append(parts, attrsArm(t, k))
			// Resource arm: read the same candidate key out of the
			// ResourceAttributes map so a value stored only under a
			// resource attribute (k8s.namespace.name, …) surfaces on
			// /label/<name>/values. ResourceAttributes is absent from
			// proj_series, so this arm cannot route onto the projection; it
			// keeps the WHERE-bounded DISTINCT (partition-pruned by the
			// now-anchored window). The arm is allowlist-gated and rare, so
			// staying off the projection is by design, not a regression.
			if resourceArm {
				resArm := chsql.NewQuery().
					Select(chsql.As(distinctMapAtFrag(resCol, k), "value")).
					From(chsql.Col(t)).
					Where(withWindow(mapAtNotEmptyFrag(resCol, k)))
				parts = append(parts, resArm.Frag())
			}
		}
	}
	outer := chsql.NewQuery().
		Select(chsql.As(distinctIdent("value"), "")).
		From(chsql.Paren(chsql.UnionAll(parts...))).
		OrderBy(chsql.Col("value"), false)
	return outer.Build()
}

// labelValueCandidates returns the candidate Attributes-map keys for a
// /api/v1/label/<name>/values lookup. Names that don't carry any
// rewritable underscore (`job`, `__name__`, ...) short-circuit to the
// single-element list — preserves the pre-#663 byte-stable SQL for
// keys that never needed dot↔underscore aliasing. Names with at least
// one rewritable underscore (`cerberus_ql`, `http_request_method`)
// expand via [format.PromLabelToOTelCandidates] so the lookup hits
// both the underscored and dotted storage forms.
func labelValueCandidates(name string) []string {
	if !format.PromLabelNeedsDottedFallback(name) {
		return []string{name}
	}
	out := format.PromLabelToOTelCandidates(name)
	if len(out) == 0 {
		return []string{name}
	}
	return out
}

// metricTables returns the configured metric-table names in a stable
// order, used for UNION construction.
func (h *Handler) metricTables() []string {
	return []string{
		h.Schema.GaugeTable,
		h.Schema.SumTable,
		h.Schema.HistogramTable,
	}
}

// arrayJoinMapKeysFrag emits `arrayJoin(mapKeys(<col>))` — the CH idiom
// for fanning out a Map column's key set as one row per key.
func arrayJoinMapKeysFrag(col string) chsql.Frag {
	return chsql.Call("arrayJoin", chsql.Call("mapKeys", chsql.Col(col)))
}

// distinctIdent emits `DISTINCT <col>` as a SELECT-list expression. The
// DISTINCT keyword is a SELECT modifier in standard SQL but ClickHouse
// (like every modern engine) accepts it inline at the head of the
// SELECT list and renders identical query plans either way. Putting it
// in the projection slot keeps it inside the typed Frag surface.
func distinctIdent(col string) chsql.Frag {
	return chsql.Distinct(chsql.Col(col))
}

// distinctMapAtFrag emits `DISTINCT <col>[?]` and binds key as a
// positional argument — the projection shape for "distinct values of
// label <key> stored in the Attributes map".
func distinctMapAtFrag(col, key string) chsql.Frag {
	mapAt := chsql.Frag(func(b *chsql.Builder) { b.MapAt(col, key) })
	return chsql.Distinct(mapAt)
}

// mapAtNotEmptyFrag emits `<col>[?] != ?` and binds both the map key
// and the empty-string sentinel as positional args — the WHERE
// predicate that drops the empty-string CH returns when a Map key is
// absent. The empty-string RHS is parameterised through chsql.Lit so
// the whole expression stays inside the typed Frag surface (the public
// Raw / Concat escape hatches were retired).
func mapAtNotEmptyFrag(col, key string) chsql.Frag {
	mapAt := chsql.Frag(func(b *chsql.Builder) { b.MapAt(col, key) })
	return chsql.Neq(mapAt, chsql.Lit(""))
}

// matcherSubqueryFrag wraps the legacy chsql.Emit output (sql + args)
// in a parenthesised subquery Frag for use as a QueryBuilder FROM
// source. Threads through chsql.Subquery + chsql.PreRenderedSQL so the
// wrapped SQL's `?` placeholders and their bound args land in the
// outer Builder's args slice at the position the Frag emits — no
// manual splicing required on the caller side.
//
// This is the documented interop between the legacy string-typed
// chsql.Emit and the typed QueryBuilder surface. A future R6.x port
// of chsql.Emit to return a *QueryBuilder will retire this helper
// (and chsql.PreRenderedSQL).
func matcherSubqueryFrag(sql string, args []any) chsql.Frag {
	return chsql.Subquery(chsql.PreRenderedSQL{SQL: sql, Args: args})
}

// normalizeMetricValues runs each candidate through OTelToPromMetric
// (Prom's metric-name grammar) and de-dupes the result. Collision
// policy: a naturally-shaped entry wins over a rewrite that would
// land on the same target. Used on `/api/v1/label/__name__/values`.
func normalizeMetricValues(in []string) []string {
	if len(in) == 0 {
		return in
	}
	natural := make(map[string]struct{}, len(in))
	for _, s := range in {
		if s != "" && s == format.OTelToPromMetric(s) {
			natural[s] = struct{}{}
		}
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		n := format.OTelToPromMetric(s)
		if n == "" {
			continue
		}
		if n != s {
			if _, ok := natural[n]; ok {
				continue
			}
		}
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	return out
}

// validLabelName mirrors the Prometheus label-name grammar: [a-zA-Z_][a-zA-Z0-9_]*.
// The synthetic `__name__` matches this pattern naturally.
func validLabelName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c == '_':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}
