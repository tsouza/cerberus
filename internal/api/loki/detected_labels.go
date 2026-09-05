package loki

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/prometheus/prometheus/model/labels"

	"github.com/tsouza/cerberus/internal/api/format"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/telemetry"
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
// A selector-less request eligible for the loki_catalog_mv refreshable-view
// catalog (cerberus issue #2770 — see labelCatalogEligible) is served from
// there when h.LabelCatalogEnabled; every other request — and any catalog
// miss — walks the distinct ResourceAttributes label sets matched in the
// window (the same shape /series fetches) and counts the cardinality of
// each key client-side, reusing QueryLabelSets.
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

	// Catalog-eligible fast path (cerberus issue #2770): a selector-less
	// (or trivially empty, e.g. `{}`) request is exactly the
	// datasource-open-probe shape the refreshable-MV catalog answers — see
	// labelCatalogEligible's doc comment for why this is the whole
	// eligibility rule. detectedLabelsFromCatalog only returns ok=true on a
	// genuine catalog hit; any miss (feature off, table not yet
	// provisioned, catalog query error, or an empty/not-yet-refreshed
	// snapshot) falls straight through to the SAME per-request path every
	// other request already takes — that path is untouched below.
	if h.LabelCatalogEnabled && labelCatalogEligible(matchers) {
		if out, ok := h.detectedLabelsFromCatalog(r.Context()); ok {
			writeJSON(w, http.StatusOK, DetectedLabelsData{DetectedLabels: out})
			return
		}
	}

	sqlStr, args, err := buildDetectedLabelsSQL(h.Schema, h.AttrStrategies, matchers, start, end)
	if err != nil {
		h.respondError(w, &apiError{Kind: ErrInternal, Err: err, Status: http.StatusInternalServerError})
		return
	}
	h.Logger.Debug("cerberus loki detected_labels", "sql", sqlStr, "args", telemetry.SanitizeArgsForLog(args))

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
//	SELECT mapSort(`ResourceAttributes`) AS labels
//	FROM `otel_logs`
//	WHERE <matchers> AND `Timestamp` >= ? AND `Timestamp` <= ?
//	GROUP BY labels
//
// The shape mirrors /series — one row per distinct stream label set in
// the window. Per-key cardinality is then derived in Go (see
// summariseDetectedLabels): a label key's cardinality is the number of
// distinct values it carries across the matched stream set.
//
// The GROUP BY key is the whole label-set Map, so it carries the same
// canonical key-order wrap /series does, for the same reason: one logical
// stream delivered under two OTLP key orders otherwise groups as two
// rows. summariseDetectedLabels folds values into per-key sets so the
// cardinalities are correct either way, but the duplicated rows are
// charged against the client's row drain budget — see buildSeriesSQL.
//
// All identifiers + time bounds flow through QueryBuilder slots; the
// selector predicate and the request window are placed by
// applySelectorAndWindow, which composes typed Frags throughout.
func buildDetectedLabelsSQL(s schema.Logs, strategies chsql.AttrStrategies, matchers []*labels.Matcher, start, end time.Time) (string, []any, error) {
	sb := chsql.NewQuery().
		Select(chsql.As(canonicalLabelsFrag(attrMapFrag(strategies, s.ResourceAttributesColumn)), "labels")).
		From(chsql.Col(s.LogsTable)).
		WithAttrStrategies(strategies)

	if err := applySelectorAndWindow(sb, s, matchers, start, end); err != nil {
		return "", nil, err
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

// --- Catalog-eligible fast path (cerberus issue #2770) ---
//
// labelCatalogEligible reports whether a /detected_labels request may be
// served from the refreshable-MV label catalog instead of the per-request
// GROUP BY above. The rule is deliberately the SIMPLEST, most conservative
// one the issue names: eligible only when the request carries NO stream
// matchers at all — an empty `query` param, or one that parses to zero
// matchers (e.g. `{}`) — i.e. a datasource-open probe asking "what labels
// exist across every stream". The catalog itself is unkeyed by stream (see
// FeatureLokiCatalogMV's doc comment), so it has no way to answer a
// SELECTOR-scoped request (Grafana Logs Drilldown's per-service view)
// correctly; those stay on the fallback path unconditionally, forever —
// not a gap this rule tries to paper over.
func labelCatalogEligible(matchers []*labels.Matcher) bool {
	return len(matchers) == 0
}

// detectedLabelsFromCatalog attempts the catalog read: it queries
// schema.LabelCatalogTable and returns ok=true only on a genuine hit — a
// successful query that returned at least one row. Both failure shapes
// (a query error — most commonly the table not yet existing, on a
// deployment where LabelCatalogEnabled predates a successful DDL apply, or
// UNKNOWN_TABLE on a server that never got the DDL at all — and a
// zero-row result — the table exists but no refresh has EVER succeeded
// since creation, so it holds no snapshot yet) degrade to ok=false rather
// than erroring the request: the caller's contract is "fall through to the
// existing path on anything short of a real answer", exactly the same
// posture isColumnStatisticsUnsupported and QueryViewRefreshState's
// UNKNOWN_TABLE handling take elsewhere in this codebase. A logged 24h
// window that genuinely has zero labelled streams is the one legitimate
// case this conflates with "never refreshed" — an operationally
// negligible edge (an otherwise-idle deployment) traded for not having to
// distinguish the two via a second query (system.view_refreshes) on every
// request.
func (h *Handler) detectedLabelsFromCatalog(ctx context.Context) ([]DetectedLabel, bool) {
	sqlStr, args := buildLabelCatalogSQL()
	rows, err := h.Client.QueryLabelCardinalities(ctx, sqlStr, args...)
	if err != nil {
		h.Logger.Debug("cerberus loki detected_labels catalog query failed; falling back to per-request path", "err", err, "sql", sqlStr)
		return nil, false
	}
	if len(rows) == 0 {
		return nil, false
	}
	out := make([]DetectedLabel, 0, len(rows))
	for _, r := range rows {
		out = append(out, DetectedLabel{Label: r.LabelKey, Cardinality: r.Cardinality})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out, true
}

// buildLabelCatalogSQL renders:
//
//	SELECT LabelKey, uniqMerge(CardinalityState) AS cardinality
//	FROM loki_label_catalog
//	GROUP BY LabelKey
//
// The table name is schema.LabelCatalogTable — a fixed, cerberus-invented
// constant rather than a schema.Logs field (see that constant's doc
// comment for why), so this takes no schema.Logs parameter, unlike every
// other SQL builder in this file.
//
// uniqMerge finalises the per-key uniqState sketch
// internal/schema/ddl.renderLokiLabelCatalogView's refresh maintains — the
// GROUP BY is required even though LabelKey is the table's whole ORDER BY,
// because AggregatingMergeTree only guarantees per-key states are
// EVENTUALLY merged by background merges, not that every part has already
// been merged into one row per key at read time.
func buildLabelCatalogSQL() (string, []any) {
	sb := chsql.NewQuery().
		Select(
			chsql.Col(schema.LabelCatalogKeyColumn),
			chsql.As(chsql.Call("uniqMerge", chsql.Col(schema.LabelCatalogCardinalityStateColumn)), "cardinality"),
		).
		From(chsql.Col(schema.LabelCatalogTable)).
		GroupBy(chsql.Col(schema.LabelCatalogKeyColumn))
	return sb.Build()
}
