package loki

import (
	"net/http"
	"sort"
	"time"

	"github.com/prometheus/prometheus/model/labels"

	"github.com/tsouza/cerberus/internal/api/format"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/schema"
)

// DetectedLabel is one entry in the /loki/api/v1/detected_labels response.
// Mirrors the upstream Loki shape Grafana 11.2+ expects when populating
// the datasource label-explorer pane.
type DetectedLabel struct {
	Label       string `json:"label"`
	Cardinality uint64 `json:"cardinality"`
}

// DetectedLabelsData is the body of a /loki/api/v1/detected_labels
// response — a single `detectedLabels` array.
type DetectedLabelsData struct {
	DetectedLabels []DetectedLabel `json:"detectedLabels"`
}

// handleDetectedLabels implements GET /loki/api/v1/detected_labels.
//
// Grafana 11.2+ probes this endpoint when opening the Loki datasource UI
// to populate label autocomplete and surface per-label cardinality. The
// upstream Loki contract is documented at
// https://grafana.com/docs/loki/latest/reference/loki-http-api/#detected-labels.
//
// Query parameters:
//   - query (optional): LogQL stream selector to constrain the rows. An
//     empty selector means "all streams in the time window".
//   - start / end (optional): time range, defaults to last hour / now.
//
// The handler walks the distinct ResourceAttributes label sets matched in
// the window (the same shape /series fetches) and counts the cardinality
// of each key client-side. This reuses QueryLabelSets so no new Querier
// method is needed.
func (h *Handler) handleDetectedLabels(w http.ResponseWriter, r *http.Request) {
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

	sqlStr, args, err := buildDetectedLabelsSQL(h.Schema, matchers, start, end)
	if err != nil {
		h.respondError(w, &apiError{Kind: ErrInternal, Err: err, Status: http.StatusInternalServerError})
		return
	}
	h.Logger.Debug("cerberus loki detected_labels", "sql", sqlStr, "args", args)

	rows, err := h.Client.QueryLabelSets(r.Context(), sqlStr, args...)
	if err != nil {
		h.Logger.Error("cerberus loki detected_labels CH query failed", "err", err, "sql", sqlStr)
		h.respondError(w, classifyMetadataErr(err))
		return
	}

	out := summariseDetectedLabels(rows)

	writeJSON(w, http.StatusOK, DetectedLabelsData{DetectedLabels: out})
}

// buildDetectedLabelsSQL renders:
//
//	SELECT `ResourceAttributes` AS labels
//	FROM `otel_logs`
//	WHERE <matchers> AND `Timestamp` >= ? AND `Timestamp` <= ?
//	GROUP BY labels
//
// The shape mirrors /series — one row per distinct stream label set in
// the window. Per-key cardinality is then derived in Go (see
// summariseDetectedLabels): a label key's cardinality is the number of
// distinct values it carries across the matched stream set.
//
// All identifiers + time bounds flow through QueryBuilder slots; the
// selector predicate composes typed Frags via logql.SelectorPredicate.
func buildDetectedLabelsSQL(s schema.Logs, matchers []*labels.Matcher, start, end time.Time) (string, []any, error) {
	sb := chsql.NewQuery().
		Select(chsql.As(chsql.Col(s.ResourceAttributesColumn), "labels")).
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
	sb.GroupBy(chsql.Col("labels"))

	sqlStr, args := sb.Build()
	return sqlStr, args, nil
}

// summariseDetectedLabels walks the distinct stream label sets returned
// by buildDetectedLabelsSQL and aggregates per-key cardinality: for each
// label key, the cardinality is the count of distinct values observed
// across the row set. Empty values are dropped — Loki's own implementation
// treats unset attributes as absent rather than as a distinct value.
//
// Results are sorted by label name for deterministic wire output;
// Grafana's autocomplete consumes the response sorted regardless, so
// keying the test assertions on order is safe.
func summariseDetectedLabels(rows []map[string]string) []DetectedLabel {
	values := map[string]map[string]struct{}{}
	for _, m := range rows {
		// Mirror /series: normalise OTel-dotted keys to the Prom/Loki
		// grammar so the result envelope matches the wire form Grafana
		// expects (and so collision policy collapses dotted+underscored
		// siblings to a single bucket).
		m = format.NormalizeLabelMap(m)
		for k, v := range m {
			if v == "" {
				continue
			}
			set, ok := values[k]
			if !ok {
				set = map[string]struct{}{}
				values[k] = set
			}
			set[v] = struct{}{}
		}
	}

	out := make([]DetectedLabel, 0, len(values))
	for k, set := range values {
		out = append(out, DetectedLabel{
			Label:       k,
			Cardinality: uint64(len(set)),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}
