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
//   - aggregateBy (optional): "series" (the default — one row per
//     distinct label SET) or "labels" (one row per bare label NAME, its
//     value summed across every value that label takes). The two are
//     genuinely different response SHAPES, not two spellings of one; see
//     [buildIndexVolumeSQL].
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

	sqlStr, args, err := buildIndexVolumeSQL(h.Schema, matchers, start, end, limit, targetLabels, aggregateBy)
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

// Loki's two `aggregateBy` options
// (pkg/storage/stores/index/seriesvolume/volume.go's Series / Labels).
// The default when the parameter is absent is Series
// (`DefaultAggregateBy`).
const (
	aggregateBySeries = "series"
	aggregateByLabels = "labels"
)

// volumeLabelNameAlias is the ARRAY JOIN alias the "labels" aggregation
// explodes one label NAME per row into. It has to be a name no column of
// `otel_logs` carries, because GROUP BY / ORDER BY resolve identifiers
// against SELECT and ARRAY JOIN aliases before FROM columns.
const volumeLabelNameAlias = "label_name"

// buildIndexVolumeSQL builds the SELECT used by /index/volume. Upstream's
// `aggregateBy` picks between two genuinely different response shapes
// (pkg/ingester/instance.go:886-903), so it picks between two SQL shapes
// here too.
//
// # aggregateBy=series (and the default)
//
// Keyed by the label SET — one row per distinct series:
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
//   - `mapFilter((k, v) -> k IN (?, ?, …), ResourceAttributes)` when
//     `targetLabels` is set and aggregateBy is not "series"
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
// # aggregateBy=labels
//
// Keyed by the bare label NAME, summing across every value that label
// takes — `{service_name="a"}` at 10 B and `{service_name="b"}` at 5 B
// are ONE `service_name` row of 15 B, not two rows:
//
//	SELECT
//	    map(`label_name`, '') AS labels,
//	    sum(length(`Body`)) AS bytes
//	FROM `otel_logs`
//	ARRAY JOIN mapKeys(<group-key-frag>) AS `label_name`
//	WHERE <matchers> AND <time bounds>
//	GROUP BY labels
//	ORDER BY bytes DESC
//	LIMIT <n>
//
// ARRAY JOIN is upstream's `s.labels.Range` (instance.go:889) expressed
// in ClickHouse: it replicates each matched row once per label the row's
// stream carries, so `sum(length(Body))` charges the row's full byte
// count to every one of its labels — exactly `labelVolumes[l.Name] +=
// size`. A row whose projected map is empty explodes to nothing and
// contributes nothing, which is the same thing ranging over a stream's
// own labels does. `<group-key-frag>` is shared with the series shape,
// so `targetLabels` restricts the exploded key set identically.
//
// The one-entry `map(label_name, ”)` reproduces upstream's decode:
// `toPrometheusData` builds this mode's metric with
// `labels.FromStrings(name, "")` (queryrange/volume.go:174), a single
// label whose NAME is the volume's name and whose VALUE is empty. Keeping
// the wire shape a Map here — rather than returning a bare String and
// re-wrapping in Go — is what lets both modes share one
// chclient.QueryIndexVolume decode and one GROUP BY / ORDER BY / LIMIT
// tail. No canonical-key-order wrap is needed on a map literal built from
// a single key.
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

	sb := chsql.NewQuery().From(chsql.Col(s.LogsTable))
	if aggregateBy == aggregateByLabels {
		sb.Select(
			chsql.As(volumeLabelNameMapFrag(), "labels"),
			chsql.As(bytesAggFrag(s.BodyColumn), "bytes"),
		).ArrayJoin(chsql.As(chsql.Call("mapKeys", groupFrag), volumeLabelNameAlias))
	} else {
		sb.Select(
			chsql.As(canonicalLabelsFrag(groupFrag), "labels"),
			chsql.As(bytesAggFrag(s.BodyColumn), "bytes"),
		)
	}

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
// label-set Map. "series" (or empty + no targetLabels) uses the full
// ResourceAttributes map; otherwise we project to the targetLabels
// subset via mapFilter.
//
// Both /index/volume shapes read it: the series shape groups by this Map
// directly, the labels shape ARRAY JOINs over its KEYS. That is why the
// `targetLabels` projection lives here rather than in either branch —
// upstream restricts to `labelsToMatch` in both of its branches too
// (pkg/ingester/instance.go:886-903).
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
	if len(targetLabels) == 0 || aggregateBy == aggregateBySeries {
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

// volumeLabelNameMapFrag renders the one-entry `map(<label_name>, ”)`
// the "labels" aggregation reports each row's metric as — upstream's
// `labels.FromStrings(name, "")` (queryrange/volume.go:174), where the
// label NAME is the payload and the value slot is deliberately empty.
//
// The empty value is a literal, not a placeholder-bound arg: it is part
// of the query SHAPE (every row's value slot is empty by construction),
// never client data.
func volumeLabelNameMapFrag() chsql.Frag {
	return chsql.Call("map", chsql.Col(volumeLabelNameAlias), chsql.InlineLit(""))
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
