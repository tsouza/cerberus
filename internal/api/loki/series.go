package loki

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/prometheus/model/labels"

	"github.com/tsouza/cerberus/internal/api/format"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/schema"
)

// handleSeries implements GET /loki/api/v1/series. Returns the distinct
// stream label sets present in the time range matching every `match[]`
// selector. Mirrors
// https://grafana.com/docs/loki/latest/reference/loki-http-api/#list-series.
//
// Query parameters:
//   - match[] (zero or more): LogQL stream selector. When no selector is
//     provided every stream in the time range is returned.
//   - start / end (optional): time range, defaults to last hour / now.
//
// The SQL groups by the full ResourceAttributes map so each unique label
// set returns as one row; Grafana's logs-builder reads the result for
// its label-set chooser.
func (h *Handler) handleSeries(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}
	start, end, err := parseStartEnd(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}

	// match[] may appear repeated or absent — collect every one,
	// each contributing its own AND-group of matchers.
	rawSelectors := r.Form["match[]"]
	selectorGroups := make([][]*labels.Matcher, 0, len(rawSelectors))
	for _, raw := range rawSelectors {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		matchers, err := selectorMatchers(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, ErrBadData, err)
			return
		}
		selectorGroups = append(selectorGroups, matchers)
	}

	sqlStr, args, err := buildSeriesSQL(h.Schema, selectorGroups, start, end)
	if err != nil {
		h.respondError(w, &apiError{Kind: ErrInternal, Err: err, Status: http.StatusInternalServerError})
		return
	}
	h.Logger.Debug("cerberus loki series", "selectors", len(selectorGroups), "sql", sqlStr, "args", args)

	rows, err := h.Client.QueryLabelSets(r.Context(), sqlStr, args...)
	if err != nil {
		h.Logger.Error("cerberus loki series CH query failed", "err", err, "sql", sqlStr)
		h.respondError(w, &apiError{Kind: ErrInternal, Err: err, Status: http.StatusBadGateway})
		return
	}

	out := dedupeLabelSets(rows)

	writeJSON(w, http.StatusOK, Response{
		Status: "success",
		Data:   out,
	})
}

// buildSeriesSQL renders:
//
//	SELECT `ResourceAttributes` AS labels
//	FROM `otel_logs`
//	WHERE (<group1>) OR (<group2>) ...
//	  AND `Timestamp` >= ? AND `Timestamp` <= ?
//	GROUP BY labels
//
// Multiple match[] selectors are OR'd (Loki's documented semantics —
// each match[] independently contributes streams). All identifiers and
// time bounds flow through QueryBuilder slots.
func buildSeriesSQL(s schema.Logs, selectorGroups [][]*labels.Matcher, start, end time.Time) (string, []any, error) {
	sb := chsql.NewQuery().
		Select(chsql.As(chsql.Col(s.ResourceAttributesColumn), "labels")).
		From(chsql.Col(s.LogsTable))

	if len(selectorGroups) > 0 {
		fragments := make([]chsql.Frag, 0, len(selectorGroups))
		for _, matchers := range selectorGroups {
			pred := logql.SelectorPredicate(matchers, s)
			if pred == nil {
				continue
			}
			frag, err := exprFrag(pred)
			if err != nil {
				return "", nil, err
			}
			fragments = append(fragments, frag)
		}
		if len(fragments) > 0 {
			sb.Where(orJoinedFrag(fragments))
		}
	}
	if !start.IsZero() {
		sb.Where(timeBoundFrag(s.TimestampColumn, ">=", start))
	}
	if !end.IsZero() {
		sb.Where(timeBoundFrag(s.TimestampColumn, "<=", end))
	}
	sb.GroupBy(chsql.Col("labels"))

	sqlStr, args := sb.Build()
	return sqlStr, args, nil
}

// orJoinedFrag emits "(<f1>) OR (<f2>) OR ..." for the disjunction of
// match[] selector predicates. A single fragment is emitted bare (no
// outer parens) to keep the WHERE-AND chain readable.
func orJoinedFrag(fragments []chsql.Frag) chsql.Frag {
	if len(fragments) == 1 {
		return fragments[0]
	}
	parts := make([]chsql.Frag, len(fragments))
	for i, f := range fragments {
		parts[i] = chsql.Paren(f)
	}
	return chsql.Or(parts...)
}

// dedupeLabelSets normalises the rows: drops empty maps, dedupes the
// stream and returns them in a deterministic canonical order.
// QueryLabelSets returns the rows as they arrive from CH — GROUP BY
// guarantees uniqueness at the row level but the Map iteration order on
// the Go side is non-deterministic, so we re-sort here.
func dedupeLabelSets(in []map[string]string) []map[string]string {
	seen := make(map[string]struct{}, len(in))
	out := make([]map[string]string, 0, len(in))
	for _, m := range in {
		if len(m) == 0 {
			continue
		}
		key := format.CanonicalKey(m)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		return format.CanonicalKey(out[i]) < format.CanonicalKey(out[j])
	})
	return out
}
