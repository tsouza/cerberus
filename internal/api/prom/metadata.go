package prom

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/common/model"

	"github.com/tsouza/cerberus/internal/chclient"
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

	var values []string
	var err error
	if len(matchers) == 0 {
		values, err = h.fetchLabelValues(r.Context(), name)
	} else {
		values, err = h.fetchLabelValuesMatched(r.Context(), name, matchers)
	}
	if err != nil {
		h.respondError(w, err)
		return
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
			return nil, &apiError{kind: ErrInternal, err: err, status: http.StatusBadGateway}
		}
		out = append(out, rows...)
	}
	return out, nil
}

// metricMetaSQL builds the per-table metadata SQL. The result columns are
// (MetricName, MetricDescription, MetricUnit). When metricName is empty
// we list all distinct metrics; otherwise we filter to the named one.
func (h *Handler) metricMetaSQL(table, metricName string) (string, []any) {
	nameCol := quoteIdentCH(h.Schema.MetricNameColumn)
	descCol := quoteIdentCH(h.Schema.MetricDescriptionColumn)
	unitCol := quoteIdentCH(h.Schema.MetricUnitColumn)
	tbl := quoteIdentCH(table)

	if metricName == "" {
		return fmt.Sprintf(
			"SELECT %s, any(%s), any(%s) FROM %s GROUP BY %s ORDER BY %s",
			nameCol, descCol, unitCol, tbl, nameCol, nameCol,
		), nil
	}
	return fmt.Sprintf(
		"SELECT %s, any(%s), any(%s) FROM %s WHERE %s = ? GROUP BY %s",
		nameCol, descCol, unitCol, tbl, nameCol, nameCol,
	), []any{metricName}
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
			key := canonicalKey(lset)
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
		return nil, &apiError{kind: ErrInternal, err: err, status: http.StatusBadGateway}
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
			return nil, &apiError{kind: ErrInternal, err: err, status: http.StatusBadGateway}
		}
		sort.Strings(values)
		return values, nil
	}
	sql, args := h.unionLabelValuesSQL(name)
	values, err := timeCH(ctx, func() ([]string, error) {
		return h.Client.QueryStrings(ctx, sql, args...)
	})
	if err != nil {
		return nil, &apiError{kind: ErrInternal, err: err, status: http.StatusBadGateway}
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
// series matching any of the given match[] selectors.
func (h *Handler) fetchLabelValuesMatched(ctx context.Context, name string, matchers []string) ([]string, error) {
	seen := map[string]bool{}
	for _, m := range matchers {
		vals, err := h.labelValuesForMatcher(ctx, name, m)
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
	innerSQL, args, err := h.matcherSQL(matcher)
	if err != nil {
		return nil, err
	}
	attrs := quoteIdentCH(h.Schema.AttributesColumn)
	sql := fmt.Sprintf("SELECT DISTINCT arrayJoin(mapKeys(%s)) AS name FROM (%s) ORDER BY name",
		attrs, innerSQL)
	return timeCH(ctx, func() ([]string, error) {
		return h.Client.QueryStrings(ctx, sql, args...)
	})
}

// labelValuesForMatcher lowers a single match[] selector, then projects
// the named label's distinct values. `__name__` resolves to MetricName;
// other labels to `Attributes[<name>]`.
func (h *Handler) labelValuesForMatcher(ctx context.Context, name, matcher string) ([]string, error) {
	innerSQL, args, err := h.matcherSQL(matcher)
	if err != nil {
		return nil, err
	}
	var sql string
	if name == model.MetricNameLabel {
		sql = fmt.Sprintf("SELECT DISTINCT %s AS value FROM (%s) ORDER BY value",
			quoteIdentCH(h.Schema.MetricNameColumn), innerSQL)
	} else {
		attrs := quoteIdentCH(h.Schema.AttributesColumn)
		sql = fmt.Sprintf("SELECT DISTINCT %s[?] AS value FROM (%s) WHERE %s[?] != '' ORDER BY value",
			attrs, innerSQL, attrs)
		args = append([]any{name}, args...)
		args = append(args, name)
	}
	return timeCH(ctx, func() ([]string, error) {
		return h.Client.QueryStrings(ctx, sql, args...)
	})
}

// matcherSQL lowers a single matcher to its inner SQL + args. The caller
// wraps this in whatever projection it needs (DISTINCT mapKeys, etc.).
func (h *Handler) matcherSQL(matcher string) (string, []any, error) {
	expr, err := h.parser.ParseExpr(matcher)
	if err != nil {
		return "", nil, &apiError{kind: ErrBadData, err: err, status: http.StatusBadRequest}
	}
	plan, err := promql.Lower(expr, h.Schema)
	if err != nil {
		return "", nil, &apiError{kind: ErrBadData, err: err, status: http.StatusBadRequest}
	}
	plan = h.Optimizer.Run(plan)
	sql, args, err := chsql.Emit(plan)
	if err != nil {
		return "", nil, &apiError{kind: ErrInternal, err: err, status: http.StatusInternalServerError}
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
	samples, err := h.executeInstant(ctx, matcher, now, now)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]map[string]string)
	for _, s := range samples {
		labels := withMetricName(s.Labels, s.MetricName)
		key := canonicalKey(labels)
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
	parts := make([]string, 0, len(tables))
	attrsCol := quoteIdentCH(h.Schema.AttributesColumn)
	for _, t := range tables {
		parts = append(parts,
			fmt.Sprintf("SELECT DISTINCT arrayJoin(mapKeys(%s)) AS name FROM %s",
				attrsCol, quoteIdentCH(t)))
	}
	return "SELECT DISTINCT name FROM (" + strings.Join(parts, " UNION ALL ") + ") ORDER BY name"
}

// unionMetricNamesSQL returns the distinct MetricName values across tables.
func (h *Handler) unionMetricNamesSQL() string {
	tables := h.metricTables()
	metricCol := quoteIdentCH(h.Schema.MetricNameColumn)
	parts := make([]string, 0, len(tables))
	for _, t := range tables {
		parts = append(parts, fmt.Sprintf("SELECT DISTINCT %s AS value FROM %s",
			metricCol, quoteIdentCH(t)))
	}
	return "SELECT DISTINCT value FROM (" + strings.Join(parts, " UNION ALL ") + ") ORDER BY value"
}

// unionLabelValuesSQL returns the distinct Attributes[?] values across
// tables, skipping the empty-string sentinel that mapAccess yields when a
// key is absent. Returns (sql, args). The name is bound once per table
// (twice: in SELECT and WHERE) — args lists it 2*N times.
func (h *Handler) unionLabelValuesSQL(name string) (string, []any) {
	tables := h.metricTables()
	attrsCol := quoteIdentCH(h.Schema.AttributesColumn)
	parts := make([]string, 0, len(tables))
	args := make([]any, 0, len(tables)*2)
	for _, t := range tables {
		parts = append(parts,
			fmt.Sprintf("SELECT DISTINCT %s[?] AS value FROM %s WHERE %s[?] != ''",
				attrsCol, quoteIdentCH(t), attrsCol))
		args = append(args, name, name)
	}
	return "SELECT DISTINCT value FROM (" + strings.Join(parts, " UNION ALL ") + ") ORDER BY value", args
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

// quoteIdentCH backtick-quotes a CH identifier; thin local helper since
// chsql.writeIdent is unexported.
func quoteIdentCH(name string) string {
	var b strings.Builder
	b.WriteByte('`')
	b.WriteString(strings.ReplaceAll(name, "`", "``"))
	b.WriteByte('`')
	return b.String()
}
