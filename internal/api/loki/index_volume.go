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
	"github.com/tsouza/cerberus/internal/chclient"
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
//     only those keys appear in the per-row metric map, rows that share
//     the projected keys collapse into one, and a row must carry EVERY
//     requested label to be counted at all (see
//     [targetLabelPresenceMatchers])
//   - aggregateBy (optional): "series" (the default — one row per
//     distinct label SET) or "labels" (one row per bare label NAME, its
//     value summed across every value that label takes). The two are
//     genuinely different response SHAPES, not two spellings of one; see
//     [buildIndexVolumeSQL]. `targetLabels` restricts the group key in
//     BOTH of them — upstream's aggregateBySeries branch builds its
//     series key from `labelsToMatch` exactly as its labels branch does
//     (the `aggregateBySeries` split inside `getVolume`,
//     pkg/ingester/instance.go)
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
	if err := validateAggregateBy(aggregateBy); err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}

	matchers, err := selectorMatchers(q)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrBadData, err)
		return
	}
	matchers = append(matchers, targetLabelPresenceMatchers(targetLabels, matchers)...)

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
	ranked := rankIndexVolumeRows(rows, limit)
	result := make([]VectorSample, 0, len(ranked))
	for _, row := range ranked {
		result = append(result, VectorSample{
			Metric: row.metric,
			Value:  [2]any{stamp, strconv.FormatUint(row.bytes, 10)},
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

// rankedVolumeRow is one /index/volume row in the shape the wire order is
// decided on: the label map the response actually carries (post
// [format.NormalizeLabelMap], which is where an OTel attribute key becomes
// a Prometheus label name) and the label-set string upstream ranks that row
// by.
type rankedVolumeRow struct {
	metric map[string]string
	name   string
	bytes  uint64
}

// rankIndexVolumeRows puts the endpoint's rows into the order upstream
// serves them in and cuts them to `limit`. It is the WHOLE of the
// endpoint's ranking — both the order the response carries and which
// rows are in it.
//
// Loki's volume response is ordered twice by the same rule.
// `MapToVolumeResponse` (pkg/storage/stores/index/seriesvolume/volume.go)
// applies it before truncating to `limit`; `toPrometheusData`
// (pkg/querier/queryrange/volume.go) applies it again to the vector it
// serves. Both compare the volume descending and fall back to the entry's
// own Name ascending — for an `aggregateBy=series` response that Name is
// the stream's label-set string (`seriesNames[hash] = seriesLabels.String()`
// in `getVolume`, pkg/ingester/instance.go).
//
// That Name is a property of the SERVED label set, not of the stored one.
// The SQL groups on raw OTel attribute keys and only
// [format.NormalizeLabelMap] turns them into Prometheus label names, and
// the rewrite is not order-preserving: `a.b` sorts before `aZ` stored
// (`.` is 0x2E, `Z` is 0x5A) and `a_b` sorts after it served (`_` is
// 0x5F). So the comparison cannot be performed before normalization, and
// normalization does not happen until here.
//
// Which is why the SQL does not perform the cut either — it hands back a
// superset. [buildIndexVolumeSQL] emits `ORDER BY bytes DESC LIMIT n WITH
// TIES`, which returns every row whose volume beats the n-th plus EVERY
// row that ties with it. The correct top-n is a subset of that set for
// any tie-break rule at all, because a tie-break only ever reorders rows
// of equal volume and every row of the boundary volume is present. This
// function then applies upstream's comparator to those rows and takes the
// first `limit` — the same two-step upstream performs, with the sort in
// the same place upstream has it and only the pre-filter pushed down.
//
// The name is built with upstream's own renderer ([labels.Labels.String],
// via [labels.FromMap]) rather than a hand-rolled equivalent, so the two
// cannot drift. Reproducing that renderer in ClickHouse instead — and
// cutting in SQL after all — is not available at any reasonable price: it
// would have to reproduce Go's byte-wise (not rune-wise) grammar rewrite,
// [format.NormalizeLabelMap]'s entry-DROPPING collision policy, and
// `strconv.Quote`'s escaping of arbitrary attribute VALUES, which
// `Labels.String()` compares once two rows agree on their names.
func rankIndexVolumeRows(rows []chclient.IndexVolumeRow, limit int) []rankedVolumeRow {
	ranked := make([]rankedVolumeRow, 0, len(rows))
	for _, row := range rows {
		metric := format.NormalizeLabelMap(row.Labels)
		ranked = append(ranked, rankedVolumeRow{
			metric: metric,
			name:   labels.FromMap(metric).String(),
			bytes:  row.Bytes,
		})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].bytes != ranked[j].bytes {
			return ranked[i].bytes > ranked[j].bytes
		}
		return ranked[i].name < ranked[j].name
	})
	if limit > 0 && len(ranked) > limit {
		ranked = ranked[:limit]
	}
	return ranked
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
// `aggregateBy` picks between two genuinely different response shapes —
// the two branches of upstream's `getVolume` (`pkg/ingester/instance.go`) —
// so it picks between two SQL shapes here too.
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
//	LIMIT <n> WITH TIES
//
// `<group-key-frag>` is one of:
//
//   - `ResourceAttributes` (default — full label set)
//   - `mapFilter((k, v) -> v != ”, map(?, <value expr>, …))` when
//     `targetLabels` is set
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
//	LIMIT <n> WITH TIES
//
// ARRAY JOIN is upstream's own `s.labels.Range` over each stream, in
// `getVolume`, expressed in ClickHouse: it replicates each matched row
// once per label the row's
// stream carries, so `sum(length(Body))` charges the row's full byte
// count to every one of its labels — exactly `labelVolumes[l.Name] +=
// size`. A row whose projected map is empty explodes to nothing and
// contributes nothing, which is the same thing ranging over a stream's
// own labels does. `<group-key-frag>` is shared with the series shape,
// so `targetLabels` restricts the exploded key set identically.
//
// The one-entry `map(label_name, ”)` reproduces upstream's decode:
// `toPrometheusData` (`pkg/querier/queryrange/volume.go`) builds this
// mode's metric with `labels.FromStrings(name, "")`, a single label whose
// NAME is the volume's name and whose VALUE is empty. Keeping
// the wire shape a Map here — rather than returning a bare String and
// re-wrapping in Go — is what lets both modes share one
// chclient.QueryIndexVolume decode and one GROUP BY / ORDER BY / LIMIT
// tail. No canonical-key-order wrap is needed on a map literal built from
// a single key.
//
// Sharing that tail is also what gives this shape the WITH TIES cut
// below for free, and it needs one just as much: label NAMES tie on byte
// volume at least as readily as label sets do, and this shape's key goes
// through the same OTel-to-Prometheus rewrite on the way out — the
// exploded key is a raw attribute name, and `toPrometheusData`'s
// single-label metric carries its normalized spelling.
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
	groupFrag, err := volumeGroupFrag(s, strategies, targetLabels)
	if err != nil {
		return "", nil, err
	}

	sb := chsql.NewQuery().
		From(chsql.Col(s.LogsTable)).
		WithAttrStrategies(strategies)
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
		// `bytes` is deliberately the ONLY sort key, and the cut is
		// deliberately WITH TIES.
		//
		// `bytes` alone is not a total order, so a plain `LIMIT n` over it
		// returns whichever equal-volume groups the aggregation happened
		// to emit first — an order ClickHouse does not promise to repeat
		// (the parallel merge order is not fixed), so the returned SET
		// varied run to run. Adding a second sort key would fix that, but
		// only by settling the tie HERE, over the group key as STORED. The
		// tie upstream settles is over the label-set string it SERVES, and
		// `format.NormalizeLabelMap` reverses the collation of some key
		// pairs on the way out (`a.b` before `aZ` stored, `a_b` after it
		// served) — so a SQL tie-break is not merely a different arbitrary
		// choice, it is a WRONG one on exactly the rows a cap discards.
		//
		// WITH TIES declines to make the choice at all: every row that
		// ties with the n-th on `bytes` comes back, so the returned set is
		// `{bytes > B} ∪ {bytes = B}` for B the n-th largest volume. That
		// set is a pure function of the data — no merge order can perturb
		// it, which is the determinism the second sort key was there for —
		// and it CONTAINS the correct top-n under any tie-break, because a
		// tie-break only ever reorders rows of one volume and every row of
		// the boundary volume is in it. rankIndexVolumeRows then applies
		// upstream's own comparator over the served names and takes the
		// first `limit`, which is where the choice belongs.
		//
		// The cost is the size of the boundary tie group: the rows shipped
		// beyond `limit` are exactly the equal-volume groups the cap fell
		// inside of, and never any group of a lower volume. That is
		// data-dependent rather than capped by `limit`, so the row count is
		// left to the drain budget every other metadata endpoint is already
		// bounded by (chclient's drainBudgetExceeded, which aborts the
		// request with the Loki "maximum … reached" 400) rather than to a
		// second cap here — a cap of our own would have to choose which
		// tied groups to drop, which is the very choice this clause exists
		// to avoid making before normalization.
		LimitWithTies(int64(limit))

	sqlStr, args := sb.Build()
	return sqlStr, args, nil
}

// volumeGroupFrag picks the CH expression that produces the row's
// label-set group key. "series" (or empty + no targetLabels) groups by
// the full attribute map; otherwise we project to the targetLabels
// subset.
//
// Both /index/volume shapes read it: the series shape groups by this Map
// directly, the labels shape ARRAY JOINs over its KEYS. That is why the
// `targetLabels` projection lives here rather than in either branch —
// upstream's `getVolume` restricts to `labelsToMatch` in both of its
// branches too.
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
// the stream's own labels (the `s.labels.Range` walks inside
// `getVolume`, pkg/ingester/instance.go), so the projected key must
// carry the value cerberus matched on.
//
// The outer `mapFilter((k, v) -> v != ”, …)` reproduces the one thing
// the old shape got right: a stream that does not carry a requested
// label contributes no entry for it, because upstream ranges over the
// labels the stream HAS rather than the labels that were asked for
// (`s.labels.Range` inside `getVolume`). Building the map from an
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
) (chsql.Frag, error) {
	if len(targetLabels) == 0 {
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

// validateAggregateBy mirrors upstream's `volumeAggregateBy`
// (pkg/loghttp/query.go): absent means the default, one of the
// two names is accepted, and anything else is a 400. Cerberus used to
// accept any string silently, so `aggregateBy=banana` answered over a
// grouping the client never asked for.
//
// The VALUE does not select a group KEY here, because upstream's does
// not either: both of its branches restrict the key to `labelsToMatch`
// (`getVolume`, pkg/ingester/instance.go). What it does select is the
// aggregation SHAPE — upstream's labels branch sums per label NAME
// across that label's values, a different wire shape from a
// per-label-set row — which [buildIndexVolumeSQL] now answers with a
// SQL shape of its own, so the validated value is threaded down to it
// rather than discarded here.
func validateAggregateBy(raw string) error {
	switch raw {
	case "", aggregateBySeries, aggregateByLabels:
		return nil
	default:
		return errors.New("invalid aggregation option")
	}
}

// targetLabelPresenceMatchers returns the matchers upstream ADDS for a
// `targetLabels` request: one `<target>=~".+"` per requested label the
// selector does not already constrain
// (the "Make sure all target labels are included in the matchers" loop
// in `prepareLabelsAndMatchersWithTargets`,
// pkg/util/series_volume.go).
//
// It is not a projection detail — it decides which rows are COUNTED. A
// stream that does not carry a requested label contributes nothing to an
// upstream `targetLabels` volume, where cerberus counted it and merely
// left the key out of its metric map, inflating the volume of the
// remaining group.
//
// Upstream also drops its match-all matcher once a target has added one.
// Cerberus has no match-all matcher to drop: [selectorMatchers] parses a
// real stream selector, and an empty-compatible one is already accepted
// by the permissive parse rather than represented as a nameless matcher.
func targetLabelPresenceMatchers(targetLabels []string, matchers []*labels.Matcher) []*labels.Matcher {
	if len(targetLabels) == 0 {
		return nil
	}
	constrained := make(map[string]bool, len(matchers))
	for _, m := range matchers {
		constrained[m.Name] = true
	}
	// Sorted so the emitted predicate order is deterministic; upstream
	// iterates a map here and does not care, but a golden does.
	targets := append([]string(nil), targetLabels...)
	sort.Strings(targets)
	var out []*labels.Matcher
	for _, t := range targets {
		if constrained[t] {
			continue
		}
		out = append(out, labels.MustNewMatcher(labels.MatchRegexp, t, ".+"))
	}
	return out
}

// volumeLabelNameMapFrag renders the one-entry `map(<label_name>, ”)`
// the "labels" aggregation reports each row's metric as — the
// `labels.FromStrings(name, "")` upstream's `toPrometheusData` builds,
// where the label NAME is the payload and the value slot is deliberately
// empty.
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
