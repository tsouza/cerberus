package prom

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/prometheus/common/model"
)

// handleLabels implements GET /api/v1/labels — distinct label names across
// all metric tables, plus the synthetic `__name__` for the metric-name
// dimension.
//
// `match[]` selector filtering is not yet supported; it lands with M2.7.
func (h *Handler) handleLabels(w http.ResponseWriter, r *http.Request) {
	if len(r.URL.Query()["match[]"]) > 0 {
		writeError(w, http.StatusBadRequest, ErrBadData,
			errors.New("'match[]' selectors are not yet supported on /api/v1/labels"))
		return
	}

	names, err := h.fetchLabelNames(r.Context())
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
	if len(r.URL.Query()["match[]"]) > 0 {
		writeError(w, http.StatusBadRequest, ErrBadData,
			errors.New("'match[]' selectors are not yet supported on /api/v1/label/<name>/values"))
		return
	}

	values, err := h.fetchLabelValues(r.Context(), name)
	if err != nil {
		h.respondError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, Response{
		Status: "success",
		Data:   values,
	})
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
	names, err := h.Client.QueryStrings(ctx, sql)
	if err != nil {
		return nil, &apiError{kind: ErrInternal, err: err, status: http.StatusBadGateway}
	}
	out := append([]string{model.MetricNameLabel}, names...)
	sort.Strings(out)
	return out, nil
}

func (h *Handler) fetchLabelValues(ctx context.Context, name string) ([]string, error) {
	if name == model.MetricNameLabel {
		values, err := h.Client.QueryStrings(ctx, h.unionMetricNamesSQL())
		if err != nil {
			return nil, &apiError{kind: ErrInternal, err: err, status: http.StatusBadGateway}
		}
		sort.Strings(values)
		return values, nil
	}
	sql, args := h.unionLabelValuesSQL(name)
	values, err := h.Client.QueryStrings(ctx, sql, args...)
	if err != nil {
		return nil, &apiError{kind: ErrInternal, err: err, status: http.StatusBadGateway}
	}
	sort.Strings(values)
	return values, nil
}

// fetchSeries lowers the matcher to a Scan+Filter, runs as a sample query,
// and dedupes the resulting label sets.
func (h *Handler) fetchSeries(ctx context.Context, matcher string) ([]map[string]string, error) {
	// Reuse the existing instant-query pipeline; rows come back as Samples
	// and we dedupe to label sets in canonicalKey order.
	samples, err := h.executeInstant(ctx, matcher)
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
