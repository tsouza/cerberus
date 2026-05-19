package loki

import (
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/prometheus/model/labels"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/schema"
)

// defaultVolumeLimit mirrors Loki's documented default for
// /loki/api/v1/index/volume — the top-N series by byte volume.
const defaultVolumeLimit = 100

// handleIndexVolume implements GET /loki/api/v1/index/volume. The body
// shape mirrors a Prometheus instant vector — Grafana's "logs volume"
// panel rebuilds its bar chart from this — with the byte volume of each
// matched series in the value slot.
//
// Query parameters honoured:
//   - query (required): LogQL stream selector
//   - start / end (optional): time range (defaults to last hour)
//   - limit (optional): top-N row cap (default 100)
//   - targetLabels (optional): comma-separated label whitelist; when set,
//     only those keys appear in the per-row metric map and rows that
//     share the projected keys collapse into one
//   - aggregateBy (optional): "series" (default, group by full label
//     set) or "labels" (group by `targetLabels`; equivalent to "series"
//     when targetLabels is unset)
func (h *Handler) handleIndexVolume(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("query")
	if q == "" {
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New("missing query parameter"))
		return
	}
	start, end, err := parseStartEnd(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}

	limit, err := parseVolumeLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}

	targetLabels := parseTargetLabels(r.URL.Query().Get("targetLabels"))
	aggregateBy := r.URL.Query().Get("aggregateBy")

	matchers, err := selectorMatchers(q)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}

	sqlStr, args, err := buildIndexVolumeSQL(h.Schema, matchers, start, end, limit, targetLabels, aggregateBy)
	if err != nil {
		h.respondError(w, &apiError{Kind: ErrInternal, Err: err, Status: http.StatusInternalServerError})
		return
	}
	h.Logger.Debug("cerberus loki index_volume", "logql", q, "sql", sqlStr, "args", args)

	rows, err := h.Client.QueryIndexVolume(r.Context(), sqlStr, args...)
	if err != nil {
		h.Logger.Error("cerberus loki index_volume CH query failed", "err", err, "sql", sqlStr)
		h.respondError(w, &apiError{Kind: ErrInternal, Err: err, Status: http.StatusBadGateway})
		return
	}

	stamp := float64(end.UnixMilli()) / 1e3
	result := make([]VectorSample, 0, len(rows))
	for _, row := range rows {
		result = append(result, VectorSample{
			Metric: dropOTelDottedLabels(row.Labels),
			Value:  [2]any{stamp, strconv.FormatUint(row.Bytes, 10)},
		})
	}

	writeJSON(w, http.StatusOK, Response{
		Status: "success",
		Data: &QueryData{
			ResultType: "vector",
			Result:     result,
		},
	})
}

// buildIndexVolumeSQL builds the GROUP BY-on-label-set SELECT used by
// /index/volume. The CH shape is:
//
//	SELECT
//	    <group-key-frag> AS labels,
//	    sum(length(`Body`)) AS bytes
//	FROM `otel_logs`
//	WHERE <matchers> AND <time bounds>
//	GROUP BY labels
//	ORDER BY bytes DESC
//	LIMIT <n>
//
// `<group-key-frag>` is one of:
//
//   - `ResourceAttributes` (default — full label set)
//   - `mapFilter((k, v) -> k IN (?, ?, …), ResourceAttributes)` when
//     `targetLabels` is set and aggregateBy is "labels" (or unset)
//
// All identifiers and bound keys flow through Builder helpers — no
// fmt.Sprintf-on-SQL (CLAUDE.md "no raw SQL strings" rule).
func buildIndexVolumeSQL(
	s schema.Logs,
	matchers []*labels.Matcher,
	start, end time.Time,
	limit int,
	targetLabels []string,
	aggregateBy string,
) (string, []any, error) {
	groupFrag := volumeGroupFrag(s, targetLabels, aggregateBy)

	sb := chsql.NewQuery().
		Select(
			chsql.As(groupFrag, "labels"),
			chsql.As(bytesAggFrag(s.BodyColumn), "bytes"),
		).
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

	sb.GroupBy(chsql.Col("labels")).
		OrderBy(chsql.Col("bytes"), true).
		Limit(int64(limit))

	sqlStr, args := sb.Build()
	return sqlStr, args, nil
}

// volumeGroupFrag picks the CH expression that produces the row's
// label-set group key. "series" (or empty + no targetLabels) groups by
// the full ResourceAttributes map; otherwise we project to the
// targetLabels subset via mapFilter.
//
// chplan.MapWithoutKeys (and Builder.MapFilterExcept) cover the
// NEGATED form ("everything except these keys"). The positive form
// here composes the mapFilter body inline: the outer Call("mapFilter",
// …) is typed, the lambda head is composed via Builder.Lambda, and
// the bare lambda-parameter reference `k` inside In's left slot uses
// chsql.BareIdent (the typed constructor for CH-safe bare identifiers
// — narrow trust contract, no backtick quoting). All composition lives
// inside the typed Frag surface.
func volumeGroupFrag(s schema.Logs, targetLabels []string, aggregateBy string) chsql.Frag {
	if len(targetLabels) == 0 || aggregateBy == "series" {
		return chsql.Col(s.ResourceAttributesColumn)
	}
	keys := append([]string(nil), targetLabels...)
	sort.Strings(keys)
	keyArgs := make([]chsql.Frag, len(keys))
	for i, k := range keys {
		keyArgs[i] = chsql.Lit(k)
	}
	inFrag := chsql.In(chsql.BareIdent("k"), keyArgs...)
	lambda := func(b *chsql.Builder) {
		b.Lambda([]string{"k", "v"}, func(b *chsql.Builder) { inFrag(b) })
	}
	return chsql.Call("mapFilter", lambda, chsql.Col(s.ResourceAttributesColumn))
}

// parseVolumeLimit decodes the optional `limit` parameter; missing /
// empty returns the documented default. Negative or non-numeric values
// are rejected with a 400.
func parseVolumeLimit(raw string) (int, error) {
	if raw == "" {
		return defaultVolumeLimit, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, errors.New("'limit' must be a positive integer")
	}
	return n, nil
}

// parseTargetLabels splits a comma-separated label-name list, trimming
// whitespace and dropping empties. Returns nil for the empty input.
func parseTargetLabels(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
