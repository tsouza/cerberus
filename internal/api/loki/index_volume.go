package loki

import (
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/prometheus/model/labels"

	"github.com/tsouza/cerberus/internal/api/format"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/telemetry"
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
	q := r.FormValue("query")
	if q == "" {
		writeError(w, http.StatusBadRequest, ErrBadData, errors.New("missing query parameter"))
		return
	}
	start, end, err := parseStartEnd(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}

	limit, err := parseVolumeLimit(r.FormValue("limit"))
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}

	targetLabels := parseTargetLabels(r.FormValue("targetLabels"))
	aggregateBy := r.FormValue("aggregateBy")

	matchers, err := selectorMatchers(q)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}

	sqlStr, args, err := buildIndexVolumeSQL(h.Schema, h.AttrStrategies, matchers, start, end, limit, targetLabels, aggregateBy)
	if err != nil {
		h.respondError(r.Context(), w, &apiError{Kind: ErrInternal, Err: err, Status: http.StatusInternalServerError})
		return
	}
	h.Logger.Debug("cerberus loki index_volume", "logql", telemetry.SanitizeForLog(q), "sql", sqlStr, "args", telemetry.SanitizeArgsForLog(args))

	rows, err := h.Client.QueryIndexVolume(r.Context(), sqlStr, args...)
	if err != nil {
		h.Logger.Error("cerberus loki index_volume CH query failed", "err", err, "sql", sqlStr)
		h.respondError(r.Context(), w, classifyMetadataErr(err))
		return
	}

	stamp := float64(end.UnixMilli()) / 1e3
	result := make([]VectorSample, 0, len(rows))
	for _, row := range rows {
		result = append(result, VectorSample{
			Metric: format.NormalizeLabelMap(row.Labels),
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
//	    mapSort(<group-key-frag>) AS labels,
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
//   - `mapFilter((k, v) -> v != ”, map(?, <value expr>, …))` when
//     `targetLabels` is set and aggregateBy is "labels" (or unset)
//
// The group key is the WHOLE label-set Map, so it carries the canonical
// key-order wrap (canonicalLabelsFrag). Without it one logical stream
// delivered under two OTLP key orders groups as two rows, each holding
// half the byte volume — and, because the wrap sits inside the aliased
// projection that GROUP BY / ORDER BY / LIMIT all read, the split also
// corrupts the top-N ranking: a genuinely-top stream can be halved out of
// the returned set entirely. mapFilter preserves the source map's key
// order, so the projected form needs the wrap just as much as the bare
// column does; wrapping the outer frag covers both branches at once.
// There is no Go-side re-aggregation here — handleIndexVolume loops rows
// straight into VectorSample — so this SQL is the only place the split
// can be closed.
//
// All identifiers and bound keys flow through Builder helpers — no
// fmt.Sprintf-on-SQL (CLAUDE.md "no raw SQL strings" rule).
func buildIndexVolumeSQL(
	s schema.Logs,
	strategies chsql.AttrStrategies,
	matchers []*labels.Matcher,
	start, end time.Time,
	limit int,
	targetLabels []string,
	aggregateBy string,
) (string, []any, error) {
	groupFrag, err := volumeGroupFrag(s, strategies, targetLabels, aggregateBy)
	if err != nil {
		return "", nil, err
	}

	sb := chsql.NewQuery().
		Select(
			chsql.As(canonicalLabelsFrag(groupFrag), "labels"),
			chsql.As(bytesAggFrag(s.BodyColumn), "bytes"),
		).
		From(chsql.Col(s.LogsTable)).
		WithAttrStrategies(strategies)

	if err := applySelectorAndWindow(sb, s, matchers, start, end); err != nil {
		return "", nil, err
	}

	sb.GroupBy(chsql.Col("labels")).
		OrderBy(chsql.Col("bytes"), true).
		Limit(int64(limit))

	sqlStr, args := sb.Build()
	return sqlStr, args, nil
}

// volumeGroupFrag picks the CH expression that produces the row's
// label-set group key. "series" (or empty + no targetLabels) groups by
// the full attribute map; otherwise we project to the targetLabels
// subset.
//
// The projection resolves each requested label through
// [logql.LabelValueExpr] — the SAME storage-shape precedence
// [logql.SelectorPredicate] scopes the request with. Projecting by the
// literal map key instead is the /index/volume wrong-answer bug:
// `targetLabels=service_name` is selected through the dedicated
// `ServiceName` column (the OTel-CH exporter hoists `service.name` out
// of the map, leaving `ResourceAttributes['service_name']` empty on
// every such row), so a `k IN ('service_name')` mapFilter returned the
// EMPTY map for every row and the whole tenant's volume collapsed into
// one unlabelled `metric: {}` sample. Reference Loki reads the value off
// the stream's own labels (pkg/ingester/instance.go:889-903), so the
// projected key must carry the value cerberus matched on.
//
// The outer `mapFilter((k, v) -> v != ”, …)` reproduces the one thing
// the old shape got right: a stream that does not carry a requested
// label contributes no entry for it, because upstream ranges over the
// labels the stream HAS rather than the labels that were asked for
// (`s.labels.Range` at instance.go:889). Building the map from an
// explicit, sorted key list also makes its key order deterministic —
// the canonical wrap outside still applies, and is now belt-and-braces
// rather than load-bearing on this branch.
//
// All composition lives inside the typed Frag surface: the map literal
// and the filter are Call constructors, the lambda head is
// Builder.Lambda, and the bare lambda-parameter reference `v` uses
// chsql.BareIdent (the typed constructor for CH-safe bare identifiers).
func volumeGroupFrag(
	s schema.Logs,
	strategies chsql.AttrStrategies,
	targetLabels []string,
	aggregateBy string,
) (chsql.Frag, error) {
	if len(targetLabels) == 0 || aggregateBy == "series" {
		return attrMapFrag(strategies, s.ResourceAttributesColumn), nil
	}
	keys := append([]string(nil), targetLabels...)
	sort.Strings(keys)
	// map(key, value, key, value, …) — CH's map-literal arity.
	entries := make([]chsql.Frag, 0, 2*len(keys))
	for _, k := range keys {
		valueFrag, err := exprFrag(logql.LabelValueExpr(k, s))
		if err != nil {
			return nil, err
		}
		entries = append(entries, chsql.Lit(k), valueFrag)
	}
	dropAbsent := func(b *chsql.Builder) {
		b.Lambda([]string{"k", "v"}, func(b *chsql.Builder) {
			chsql.Neq(chsql.BareIdent("v"), chsql.Lit(""))(b)
		})
	}
	return chsql.Call("mapFilter", dropAbsent, chsql.Call("map", entries...)), nil
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
