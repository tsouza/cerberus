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

	"github.com/tsouza/cerberus/internal/api/format"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
)

// handleLabels implements GET /api/v1/labels — distinct label names across
// all metric tables, plus the synthetic `__name__` for the metric-name
// dimension. Optional `match[]` selectors narrow the result to labels of
// the matched series only.
func (h *Handler) handleLabels(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}
	matchers := r.Form["match[]"]

	var names []string
	var err error
	if len(matchers) == 0 {
		names, err = h.fetchLabelNames(r.Context())
	} else {
		names, err = h.fetchLabelNamesMatched(r.Context(), matchers)
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
// Optional `start` / `end` parameters anchor the LWR (latest-with-respect-
// to-T) window used when lowering each `match[]` selector. Without them
// the lowering defaults to `now64(9)` and the staleness window may
// exclude any sample older than the default lookback — the request would
// then return an empty value list even when matching rows exist in the
// table.
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

	// `start` / `end` are optional on label/values; when present they
	// anchor the matcher lowering's eval timestamp so the LWR window
	// can include samples within the requested range. Parse errors are
	// reported as bad_data; missing values fall through as zero-time
	// (handler retains the legacy `now64(9)` default in that case).
	startRaw := r.Form.Get("start")
	endRaw := r.Form.Get("end")
	var startT, endT time.Time
	if startRaw != "" {
		t, err := format.ParseTimeProm(startRaw, time.Time{})
		if err != nil {
			writeError(w, http.StatusBadRequest, ErrBadData,
				fmt.Errorf("invalid 'start' parameter: %w", err))
			return
		}
		startT = t
	}
	if endRaw != "" {
		t, err := format.ParseTimeProm(endRaw, time.Time{})
		if err != nil {
			writeError(w, http.StatusBadRequest, ErrBadData,
				fmt.Errorf("invalid 'end' parameter: %w", err))
			return
		}
		endT = t
	}

	var values []string
	var err error
	if len(matchers) == 0 {
		values, err = h.fetchLabelValues(r.Context(), name)
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
// emits from /api/v1/metadata. Cerberus emits exactly one entry per
// metric for now (multiple entries would be required only if the same
// metric appears in multiple types — uncommon).
type MetricMetaEntry struct {
	Type string `json:"type"`
	Help string `json:"help"`
	Unit string `json:"unit"`
}

// handleMetadata implements GET /api/v1/metadata — per-metric type / help /
// unit, sourced from the OTel `MetricDescription` and `MetricUnit` columns.
// Type is derived from the source table: gauge / counter / histogram.
//
// The `metric` query parameter restricts the result to a single metric;
// `limit` caps the number of metrics returned (per Prom convention).
func (h *Handler) handleMetadata(w http.ResponseWriter, r *http.Request) {
	metricName := r.URL.Query().Get("metric")
	limitStr := r.URL.Query().Get("limit")

	rows, err := h.fetchMetricMeta(r.Context(), metricName)
	if err != nil {
		h.respondError(w, err)
		return
	}

	// Group by metric name; each metric gets a slice of entries.
	grouped := make(map[string][]MetricMetaEntry, len(rows))
	for _, row := range rows {
		grouped[row.Name] = append(grouped[row.Name], MetricMetaEntry{
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

func (h *Handler) fetchMetricMeta(ctx context.Context, metricName string) ([]chclient.MetricMetaRow, error) {
	specs := []struct {
		table string
		kind  string
	}{
		{h.Schema.GaugeTable, "gauge"},
		{h.Schema.SumTable, "counter"},
		{h.Schema.HistogramTable, "histogram"},
	}

	var out []chclient.MetricMetaRow
	for _, spec := range specs {
		sql, args := h.metricMetaSQL(spec.table, metricName)
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
func (h *Handler) metricMetaSQL(table, metricName string) (string, []any) {
	nameCol := h.Schema.MetricNameColumn
	descCol := h.Schema.MetricDescriptionColumn
	unitCol := h.Schema.MetricUnitColumn

	anyCall := func(col string) chsql.Frag {
		return chsql.Call("any", chsql.Col(col))
	}

	sb := chsql.NewQuery().
		Select(chsql.Col(nameCol), anyCall(descCol), anyCall(unitCol)).
		From(chsql.Col(table)).
		GroupBy(chsql.Col(nameCol))

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
//
// Time-range filtering (start/end) is parsed but not yet pushed into the
// SQL — lands with M2.7.
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

	seen := make(map[string]map[string]string)
	for _, m := range matchers {
		sets, err := h.fetchSeries(r.Context(), m)
		if err != nil {
			h.respondError(w, err)
			return
		}
		for _, lset := range sets {
			key := format.CanonicalKey(lset)
			if _, ok := seen[key]; !ok {
				seen[key] = lset
			}
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
// and prepends `__name__`. The returned slice is sorted.
func (h *Handler) fetchLabelNames(ctx context.Context) ([]string, error) {
	sql := h.unionLabelNamesSQL()
	names, err := timeCH(ctx, func() ([]string, error) {
		return h.Client.QueryStrings(ctx, sql)
	})
	if err != nil {
		return nil, &apiError{Kind: ErrInternal, Err: err, Status: http.StatusBadGateway}
	}
	out := append([]string{model.MetricNameLabel}, names...)
	sort.Strings(out)
	return out, nil
}

func (h *Handler) fetchLabelValues(ctx context.Context, name string) ([]string, error) {
	if name == model.MetricNameLabel {
		sql := h.unionMetricNamesSQL()
		values, err := timeCH(ctx, func() ([]string, error) {
			return h.Client.QueryStrings(ctx, sql)
		})
		if err != nil {
			return nil, &apiError{Kind: ErrInternal, Err: err, Status: http.StatusBadGateway}
		}
		sort.Strings(values)
		return values, nil
	}
	sql, args := h.unionLabelValuesSQL(name)
	values, err := timeCH(ctx, func() ([]string, error) {
		return h.Client.QueryStrings(ctx, sql, args...)
	})
	if err != nil {
		return nil, &apiError{Kind: ErrInternal, Err: err, Status: http.StatusBadGateway}
	}
	sort.Strings(values)
	return values, nil
}

// fetchLabelNamesMatched returns the distinct label names of series
// matching any of the given match[] selectors. The synthetic `__name__`
// is always included if at least one selector matches anything.
func (h *Handler) fetchLabelNamesMatched(ctx context.Context, matchers []string) ([]string, error) {
	seen := map[string]bool{model.MetricNameLabel: true}
	for _, m := range matchers {
		keys, err := h.labelKeysForMatcher(ctx, m)
		if err != nil {
			return nil, err
		}
		for _, k := range keys {
			seen[k] = true
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

// fetchLabelValuesMatched returns the distinct values of <name> across
// series matching any of the given match[] selectors. The start/end pair
// is forwarded to matcherSQL via labelValuesForMatcher so the lowering's
// LWR anchor reflects the request window when present.
func (h *Handler) fetchLabelValuesMatched(ctx context.Context, name string, matchers []string, start, end time.Time) ([]string, error) {
	seen := map[string]bool{}
	for _, m := range matchers {
		vals, err := h.labelValuesForMatcher(ctx, name, m, start, end)
		if err != nil {
			return nil, err
		}
		for _, v := range vals {
			if v != "" {
				seen[v] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Strings(out)
	return out, nil
}

// labelKeysForMatcher lowers a single match[] selector, then wraps the
// matched rows in a `SELECT DISTINCT arrayJoin(mapKeys(Attributes))`
// to extract its attribute keys.
func (h *Handler) labelKeysForMatcher(ctx context.Context, matcher string) ([]string, error) {
	innerSQL, args, err := h.matcherSQL(ctx, matcher, time.Time{}, time.Time{})
	if err != nil {
		return nil, err
	}
	attrsCol := h.Schema.AttributesColumn

	sb := chsql.NewQuery().
		Select(chsql.As(arrayJoinMapKeysFrag(attrsCol), "name")).
		From(matcherSubqueryFrag(innerSQL, args)).
		OrderBy(chsql.Col("name"), false)
	sql, combined := sb.Build()
	return timeCH(ctx, func() ([]string, error) {
		return h.Client.QueryStrings(ctx, sql, combined...)
	})
}

// labelValuesForMatcher lowers a single match[] selector, then projects
// the named label's distinct values. `__name__` resolves to MetricName;
// other labels to `Attributes[<name>]`. start/end anchor the matcher
// lowering's LWR window (zero-time falls back to the lowering default).
func (h *Handler) labelValuesForMatcher(ctx context.Context, name, matcher string, start, end time.Time) ([]string, error) {
	innerSQL, args, err := h.matcherSQL(ctx, matcher, start, end)
	if err != nil {
		return nil, err
	}
	if name == model.MetricNameLabel {
		sb := chsql.NewQuery().
			Select(chsql.As(distinctIdent(h.Schema.MetricNameColumn), "value")).
			From(matcherSubqueryFrag(innerSQL, args)).
			OrderBy(chsql.Col("value"), false)
		sql, combined := sb.Build()
		return timeCH(ctx, func() ([]string, error) {
			return h.Client.QueryStrings(ctx, sql, combined...)
		})
	}
	attrsCol := h.Schema.AttributesColumn
	// chsql.Subquery splices the matcher's args inline at the FROM
	// position; QueryBuilder renders SELECT → FROM → WHERE so the final
	// args slice naturally interleaves as [SELECT-MapAt-key, matcher args,
	// WHERE-MapAt-key, WHERE-empty-sentinel] — no manual splicing.
	sb := chsql.NewQuery().
		Select(chsql.As(distinctMapAtFrag(attrsCol, name), "value")).
		From(matcherSubqueryFrag(innerSQL, args)).
		Where(mapAtNotEmptyFrag(attrsCol, name)).
		OrderBy(chsql.Col("value"), false)
	sql, combined := sb.Build()
	return timeCH(ctx, func() ([]string, error) {
		return h.Client.QueryStrings(ctx, sql, combined...)
	})
}

// matcherSQL lowers a single matcher to its inner SQL + args. The caller
// wraps this in whatever projection it needs (DISTINCT mapKeys, etc.).
// When end is non-zero the lowering threads start/end through to
// promql.LowerAt so the matcher's LWR window anchors at the request's
// `end` rather than the lowering default (`now64(9)`).
func (h *Handler) matcherSQL(ctx context.Context, matcher string, start, end time.Time) (string, []any, error) {
	expr, err := h.parseExpr(ctx, matcher)
	if err != nil {
		return "", nil, &apiError{Kind: ErrBadData, Err: err, Status: http.StatusBadRequest}
	}
	var plan chplan.Node
	if !end.IsZero() {
		plan, err = promql.LowerAt(ctx, expr, h.Schema, start, end)
	} else {
		plan, err = promql.Lower(ctx, expr, h.Schema)
	}
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

// fetchSeries lowers the matcher to a Scan+Filter, runs as a sample query,
// and dedupes the resulting label sets.
func (h *Handler) fetchSeries(ctx context.Context, matcher string) ([]map[string]string, error) {
	// Reuse the existing instant-query pipeline; rows come back as Samples
	// and we dedupe to label sets in canonicalKey order. Series matchers
	// don't carry @ start()/end(); pass `now` for both anchors so any
	// literal @<ts> still resolves but the start()/end() variants surface
	// as errors at lowering time.
	now := time.Now()
	samples, _, err := h.executeInstant(ctx, matcher, now, now)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]map[string]string)
	for _, s := range samples {
		labels := format.WithMetricName(s.Labels, s.MetricName)
		key := format.CanonicalKey(labels)
		if _, ok := seen[key]; !ok {
			seen[key] = labels
		}
	}
	out := make([]map[string]string, 0, len(seen))
	for _, l := range seen {
		out = append(out, l)
	}
	return out, nil
}

// unionLabelNamesSQL builds a UNION of all metric tables' label keys.
func (h *Handler) unionLabelNamesSQL() string {
	tables := h.metricTables()
	attrsCol := h.Schema.AttributesColumn
	parts := make([]chsql.Frag, 0, len(tables))
	for _, t := range tables {
		arm := chsql.NewQuery().
			Select(chsql.As(arrayJoinMapKeysFrag(attrsCol), "name")).
			From(chsql.Col(t))
		parts = append(parts, arm.Frag())
	}
	outer := chsql.NewQuery().
		Select(chsql.As(distinctIdent("name"), "")).
		From(chsql.Paren(chsql.UnionAll(parts...))).
		OrderBy(chsql.Col("name"), false)
	sql, _ := outer.Build()
	return sql
}

// unionMetricNamesSQL returns the distinct MetricName values across tables.
func (h *Handler) unionMetricNamesSQL() string {
	tables := h.metricTables()
	metricCol := h.Schema.MetricNameColumn
	parts := make([]chsql.Frag, 0, len(tables))
	for _, t := range tables {
		arm := chsql.NewQuery().
			Select(chsql.As(distinctIdent(metricCol), "value")).
			From(chsql.Col(t))
		parts = append(parts, arm.Frag())
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
// a key is absent. Returns (sql, args). Each table arm binds three
// args: the label name (SELECT MapAt), the label name (WHERE MapAt),
// and the empty-string sentinel (WHERE Lit("")) — args lists 3*N
// entries in [name, name, ""] groups per arm.
func (h *Handler) unionLabelValuesSQL(name string) (string, []any) {
	tables := h.metricTables()
	attrsCol := h.Schema.AttributesColumn
	parts := make([]chsql.Frag, 0, len(tables))
	for _, t := range tables {
		arm := chsql.NewQuery().
			Select(chsql.As(distinctMapAtFrag(attrsCol, name), "value")).
			From(chsql.Col(t)).
			Where(mapAtNotEmptyFrag(attrsCol, name))
		parts = append(parts, arm.Frag())
	}
	outer := chsql.NewQuery().
		Select(chsql.As(distinctIdent("value"), "")).
		From(chsql.Paren(chsql.UnionAll(parts...))).
		OrderBy(chsql.Col("value"), false)
	return outer.Build()
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
