package loki

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/prometheus/model/labels"

	"github.com/tsouza/cerberus/internal/api/format"
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
	if q := r.FormValue("query"); q != "" {
		matchers, err = selectorMatchers(q)
		if err != nil {
			writeError(w, http.StatusBadRequest, ErrBadData, err)
			return
		}
	}

	sqlStr, args, err := buildLabelValuesSQL(h.Schema, name, matchers, start, end)
	if err != nil {
		h.respondError(w, &apiError{Kind: ErrInternal, Err: err, Status: http.StatusInternalServerError})
		return
	}
	h.Logger.Debug("cerberus loki label values", "name", name, "sql", sqlStr, "args", args)

	vals, err := h.Client.QueryStrings(r.Context(), sqlStr, args...)
	if err != nil {
		h.Logger.Error("cerberus loki label values CH query failed", "err", err, "sql", sqlStr)
		h.respondError(w, &apiError{Kind: ErrInternal, Err: err, Status: http.StatusBadGateway})
		return
	}

	writeJSON(w, http.StatusOK, Response{
		Status: "success",
		Data:   dedupeAndSort(vals),
	})
}

// buildLabelValuesSQL renders the DISTINCT-values query for one label.
//
// The default shape is:
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
//
// For labels with an OTel resource-attribute fallback column (e.g.
// `service_name` → `ServiceName`) the function emits a UNION ALL across
// every storage shape the OTel-CH exporter might have used:
//
//   - the dedicated top-level column (`ServiceName`),
//   - the underscored map key (`ResourceAttributes['service_name']`),
//   - the dotted OTel-semantic-convention map key
//     (`ResourceAttributes['service.name']`).
//
// Without the union, cerberus's own logs — which the OTel collector → CH
// exporter pipeline routes through the dedicated `ServiceName` column —
// are invisible to `/loki/api/v1/label/service_name/values`, while
// seed-fixture rows that wrote the value to the map are visible only
// through that map key. The matcher lowering in
// `internal/logql/lower.go::matcherToExpr` mirrors the same fallback so
// the two endpoints agree on what counts as a row carrying the label.
func buildLabelValuesSQL(s schema.Logs, name string, matchers []*labels.Matcher, start, end time.Time) (string, []any, error) {
	keys := labelValueLookupKeys(name)
	topCol := labelValueTopLevelColumn(s, name)
	if topCol == "" && len(keys) == 1 {
		// Fast path: single map-key lookup, no top-level fallback.
		sb := chsql.NewQuery().
			Select(chsql.As(distinctMapAtFrag(s.ResourceAttributesColumn, keys[0]), "v")).
			From(chsql.Col(s.LogsTable))
		if err := applyLabelValuesPredicates(sb, s, matchers, start, end); err != nil {
			return "", nil, err
		}
		sb.Where(nonEmptyMapAtFrag(s.ResourceAttributesColumn, keys[0]))
		sb.OrderBy(chsql.Col("v"), false)
		sqlStr, args := sb.Build()
		return sqlStr, args, nil
	}

	// Fallback path: UNION ALL one arm per storage shape, wrap in an
	// outer SELECT DISTINCT for de-dup + ORDER BY.
	arms := make([]chsql.Frag, 0, len(keys)+1)
	if topCol != "" {
		arm := chsql.NewQuery().
			Select(chsql.As(chsql.Col(topCol), "v")).
			From(chsql.Col(s.LogsTable))
		if err := applyLabelValuesPredicates(arm, s, matchers, start, end); err != nil {
			return "", nil, err
		}
		arm.Where(chsql.Neq(chsql.Col(topCol), chsql.Lit("")))
		arms = append(arms, arm.Frag())
	}
	for _, k := range keys {
		arm := chsql.NewQuery().
			Select(chsql.As(mapAtFrag(s.ResourceAttributesColumn, k), "v")).
			From(chsql.Col(s.LogsTable))
		if err := applyLabelValuesPredicates(arm, s, matchers, start, end); err != nil {
			return "", nil, err
		}
		arm.Where(nonEmptyMapAtFrag(s.ResourceAttributesColumn, k))
		arms = append(arms, arm.Frag())
	}

	outer := chsql.NewQuery().
		Select(chsql.Distinct(chsql.Col("v"))).
		From(chsql.Paren(unionAllQuery(arms))).
		OrderBy(chsql.Col("v"), false)
	sqlStr, args := outer.Build()
	return sqlStr, args, nil
}

// applyLabelValuesPredicates threads the matcher + time-range filters
// onto sb. Extracted so each UNION arm wires up the same WHERE shape
// without duplicating the matcher-lowering call.
func applyLabelValuesPredicates(sb *chsql.QueryBuilder, s schema.Logs, matchers []*labels.Matcher, start, end time.Time) error {
	pred := logql.SelectorPredicate(matchers, s)
	if pred != nil {
		whereFrag, err := exprFrag(pred)
		if err != nil {
			return err
		}
		sb.Where(whereFrag)
	}
	if !start.IsZero() {
		sb.Where(timeBoundFrag(s.TimestampColumn, ">=", start))
	}
	if !end.IsZero() {
		sb.Where(timeBoundFrag(s.TimestampColumn, "<=", end))
	}
	return nil
}

// labelValueTopLevelColumn returns the dedicated top-level CH column
// that mirrors a Prom/Loki label name, or "" when no such column
// exists. Currently covers only `service_name` → `ServiceName` — the
// one fallback the OTel-CH default schema promotes out of the
// ResourceAttributes map. Callers that get "" stay on the map-only
// path. The dotted form (`service.name`) is treated symmetrically so
// the `/label/service.name/values` endpoint surfaces the same union of
// storage shapes as `/label/service_name/values`.
func labelValueTopLevelColumn(s schema.Logs, name string) string {
	switch name {
	case "service_name", "service.name":
		return s.ServiceNameColumn
	}
	return ""
}

// labelValueLookupKeys returns the ResourceAttributes map keys the
// /label/<name>/values endpoint should check. The Prom-grammar
// underscored form is always first; if the label has dotted
// candidates (e.g. `service_name` → `service.name`) they follow. A
// dotted input name returns just itself — the lookup goes straight to
// the OTel-shaped key.
func labelValueLookupKeys(name string) []string {
	if strings.Contains(name, ".") {
		return []string{name}
	}
	return format.PromLabelToOTelCandidates(name)
}

// unionAllQuery returns a single Frag that wraps one arm verbatim or
// UNION ALLs the full list. Defends UnionAll's "at least one part"
// contract for callers that may pass a single-arm slice.
func unionAllQuery(arms []chsql.Frag) chsql.Frag {
	if len(arms) == 1 {
		return arms[0]
	}
	return chsql.UnionAll(arms...)
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
