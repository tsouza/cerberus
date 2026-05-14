package loki

import (
	"net/http"
	"sort"
	"time"

	"github.com/prometheus/prometheus/model/labels"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/schema"
)

// handleLabels implements GET /loki/api/v1/labels. Returns the set of
// label keys present in the rows matched by the optional stream
// selector + time range. Mirrors the upstream Loki schema documented at
// https://grafana.com/docs/loki/latest/reference/loki-http-api/#list-labels-within-a-range-of-time.
//
// Query parameters:
//   - query (optional): LogQL stream selector. When absent the full
//     ResourceAttributes key space across the time range is returned.
//   - start / end (optional): time range, defaults to last hour / now.
//
// The SQL groups distinct map keys via arrayJoin(mapKeys(...)) on the
// resource-attributes column.
func (h *Handler) handleLabels(w http.ResponseWriter, r *http.Request) {
	start, end, err := parseStartEnd(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}

	var matchers []*labels.Matcher
	if q := r.URL.Query().Get("query"); q != "" {
		matchers, err = selectorMatchers(q)
		if err != nil {
			writeError(w, http.StatusBadRequest, ErrBadData, err)
			return
		}
	}

	sqlStr, args, err := buildLabelsSQL(h.Schema, matchers, start, end)
	if err != nil {
		h.respondError(w, &apiError{kind: ErrInternal, err: err, status: http.StatusInternalServerError})
		return
	}
	h.Logger.Debug("cerberus loki labels", "sql", sqlStr, "args", args)

	vals, err := h.Client.QueryStrings(r.Context(), sqlStr, args...)
	if err != nil {
		h.Logger.Error("cerberus loki labels CH query failed", "err", err, "sql", sqlStr)
		h.respondError(w, &apiError{kind: ErrInternal, err: err, status: http.StatusBadGateway})
		return
	}

	// Defensive: drop empties + ensure stable ordering for Grafana's
	// dropdowns. Loki itself returns the list sorted.
	out := dedupeAndSort(vals)

	writeJSON(w, http.StatusOK, Response{
		Status: "success",
		Data:   out,
	})
}

// buildLabelsSQL renders:
//
//	SELECT DISTINCT arrayJoin(mapKeys(`ResourceAttributes`)) AS k
//	FROM `otel_logs`
//	WHERE <matchers> AND `Timestamp` >= ? AND `Timestamp` <= ?
//	ORDER BY k
//
// All identifiers + time-range bounds flow through QueryBuilder slots —
// no fmt.Sprintf-on-SQL.
func buildLabelsSQL(s schema.Logs, matchers []*labels.Matcher, start, end time.Time) (string, []any, error) {
	sb := chsql.NewQuery().
		Select(chsql.As(distinctMapKeysFrag(s.ResourceAttributesColumn), "k")).
		From(chsql.Col(s.LogsTable))

	pred := logql.SelectorPredicate(matchers, s)
	if pred != nil {
		whereFrag, err := exprFrag(pred)
		if err != nil {
			return "", nil, err
		}
		sb.Where(whereFrag)
	}
	if !start.IsZero() {
		sb.Where(timeBoundFrag(s.TimestampColumn, ">=", start))
	}
	if !end.IsZero() {
		sb.Where(timeBoundFrag(s.TimestampColumn, "<=", end))
	}
	sb.OrderBy(chsql.Col("k"), false)

	sqlStr, args := sb.Build()
	return sqlStr, args, nil
}

// distinctMapKeysFrag emits
//
//	DISTINCT arrayJoin(mapKeys(`<col>`))
//
// — the CH idiom for flattening a Map column's key array into the row
// stream and de-duping. Used by /labels (the per-row key set) and is the
// shape Grafana's label autocomplete expects. The arrayJoin / mapKeys
// function calls compose through the typed Call constructor wrapping
// Builder.MapKeys for the inner mapKeys(col) call.
func distinctMapKeysFrag(col string) chsql.Frag {
	mapKeys := func(b *chsql.Builder) { b.MapKeys(col) }
	return chsql.Distinct(chsql.Call("arrayJoin", mapKeys))
}

// dedupeAndSort drops empty strings, removes duplicates, and sorts the
// result. CH already de-dupes via DISTINCT but the function is a tiny
// belt-and-braces in case the driver returns the raw (un-deduped) rows
// — and it gives us deterministic output for the table-tests.
func dedupeAndSort(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
