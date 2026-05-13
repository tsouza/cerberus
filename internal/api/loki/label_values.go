package loki

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/prometheus/model/labels"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/schema"
)

// handleLabelValues implements GET /loki/api/v1/label/<name>/values.
// Returns the distinct values for one label key inside the matched rows.
// Mirrors https://grafana.com/docs/loki/latest/reference/loki-http-api/#list-label-values-within-a-range-of-time.
//
// Query parameters:
//   - query (optional): LogQL stream selector to constrain rows.
//   - start / end (optional): time range, defaults to last hour / now.
//
// The label name is taken from the URL path segment between
// "/label/" and "/values".
func (h *Handler) handleLabelValues(w http.ResponseWriter, r *http.Request) {
	name, err := labelNameFromPath(r.URL.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}

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

	sqlStr, args, err := buildLabelValuesSQL(h.Schema, name, matchers, start, end)
	if err != nil {
		h.respondError(w, &apiError{kind: ErrInternal, err: err, status: http.StatusInternalServerError})
		return
	}
	h.Logger.Debug("cerberus loki label values", "name", name, "sql", sqlStr, "args", args)

	vals, err := h.Client.QueryStrings(r.Context(), sqlStr, args...)
	if err != nil {
		h.Logger.Warn("cerberus loki label values CH query failed", "err", err.Error(), "sql", sqlStr)
		h.respondError(w, &apiError{kind: ErrInternal, err: err, status: http.StatusBadGateway})
		return
	}

	writeJSON(w, http.StatusOK, Response{
		Status: "success",
		Data:   dedupeAndSort(vals),
	})
}

// buildLabelValuesSQL renders:
//
//	SELECT DISTINCT `ResourceAttributes`[?] AS v
//	FROM `otel_logs`
//	WHERE <matchers>
//	  AND `Timestamp` >= ?
//	  AND `Timestamp` <= ?
//	  AND `ResourceAttributes`[?] != ''
//	ORDER BY v
//
// The label name is bound twice as a positional argument — once for the
// SELECT projection and once for the non-empty filter — so the label
// name never reaches the SQL string itself. Empty values are filtered
// out CH-side to avoid one round-trip-then-prune in Go.
func buildLabelValuesSQL(s schema.Logs, name string, matchers []*labels.Matcher, start, end time.Time) (string, []any, error) {
	sb := chsql.NewQuery().
		Select(chsql.As(distinctMapAtFrag(s.ResourceAttributesColumn, name), "v")).
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
	sb.Where(nonEmptyMapAtFrag(s.ResourceAttributesColumn, name))
	sb.OrderBy(chsql.Col("v"), false)

	sqlStr, args := sb.Build()
	return sqlStr, args, nil
}

// distinctMapAtFrag emits "DISTINCT `<col>`[?]" with name bound as a `?`
// positional argument. Composed via the typed Distinct constructor
// wrapping a typed map-access Frag.
func distinctMapAtFrag(col, name string) chsql.Frag {
	return chsql.Distinct(mapAtFrag(col, name))
}

// nonEmptyMapAtFrag emits "`<col>`[?] != ?" binding both the map key and
// the empty-string sentinel as positional arguments. Composed via the
// typed Neq operator so neither operand reaches a raw SQL literal.
func nonEmptyMapAtFrag(col, name string) chsql.Frag {
	return chsql.Neq(mapAtFrag(col, name), chsql.Lit(""))
}

// mapAtFrag adapts Builder.MapAt into a Frag — emits "`<col>`[?]" with
// the key bound as a positional argument.
func mapAtFrag(col, name string) chsql.Frag {
	return func(b *chsql.Builder) { b.MapAt(col, name) }
}

// labelNameFromPath extracts <name> from /loki/api/v1/label/<name>/values.
// Returns an error when the segment is empty or the path has the wrong
// shape — defends the handler against a stray `mux` mismount.
func labelNameFromPath(path string) (string, error) {
	const prefix = "/loki/api/v1/label/"
	const suffix = "/values"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return "", errors.New("malformed label-values path")
	}
	name := strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix)
	if name == "" {
		return "", errors.New("missing label name in path")
	}
	if strings.Contains(name, "/") {
		return "", errors.New("invalid label name in path")
	}
	return name, nil
}
