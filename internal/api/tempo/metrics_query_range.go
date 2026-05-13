package tempo

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/grafana/tempo/pkg/traceql"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	traceql_lower "github.com/tsouza/cerberus/internal/traceql"
)

// MetricsQueryRangeResponse is the body of `GET /api/metrics/query_range`.
// Mirrors Tempo's native shape: one MetricsSeries per (group-by labels)
// tuple, each with the per-anchor samples sorted ascending by timestamp.
type MetricsQueryRangeResponse struct {
	Series []MetricsSeries `json:"series"`
}

// MetricsSeries is one entry of MetricsQueryRangeResponse.Series.
type MetricsSeries struct {
	Labels  []MetricsLabel  `json:"labels"`
	Samples []MetricsSample `json:"samples"`
}

// MetricsLabel is one (key, value) pair in MetricsSeries.Labels.
type MetricsLabel struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// MetricsSample is one point in MetricsSeries.Samples — timestamp in
// milliseconds (Tempo's wire unit) plus the per-bucket float value.
type MetricsSample struct {
	TimestampMs int64   `json:"timestampMs"`
	Value       float64 `json:"value"`
}

// handleMetricsQueryRange implements `GET /api/metrics/query_range`.
//
// Parses the TraceQL metrics-pipeline query, lowers it to a chplan
// MetricsAggregate (optionally wrapped in a single Filter), wraps that
// with a RangeWindow carrying the request's start / end / step, runs
// the resulting SQL, and reshapes the matrix rows into Tempo's
// series-of-samples envelope.
//
// Contract: `q` = TraceQL metrics query, `start`/`end` = unix nanos
// (parseTempoStartEnd accepts seconds / nanos / RFC3339), `step` =
// Prom-style duration (`30s`, `1m`, ...).
func (h *Handler) handleMetricsQueryRange(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		writeError(w, http.StatusBadRequest, "", "", errors.New("missing 'q' parameter"))
		return
	}
	start, end, err := parseTempoStartEnd(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "", "", err)
		return
	}
	if start.IsZero() || end.IsZero() {
		writeError(w, http.StatusBadRequest, "", "", errors.New("'start' and 'end' parameters are required"))
		return
	}
	stepStr := r.URL.Query().Get("step")
	if stepStr == "" {
		writeError(w, http.StatusBadRequest, "", "", errors.New("missing 'step' parameter"))
		return
	}
	step, err := parseMetricsStep(stepStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "", "", err)
		return
	}

	expr, err := traceql.Parse(q)
	if err != nil {
		writeError(w, http.StatusBadRequest, "", "", err)
		return
	}
	plan, err := traceql_lower.Lower(expr, h.Schema)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "", "", err)
		return
	}

	metrics, ok := unwrapMetricsAggregate(plan)
	if !ok {
		writeError(w, http.StatusBadRequest, "", "",
			fmt.Errorf("query %q is not a TraceQL metrics-pipeline expression — /api/metrics/query_range requires `| rate()`, `| count_over_time()`, `| *_over_time(...)` or `| quantile_over_time(...)`", q))
		return
	}

	// Wrap the lowered plan with a RangeWindow carrying the request's
	// eval grid. Range = Step so each bucket covers exactly one step
	// width (matches Tempo's reference TraceQL metrics semantics, where
	// `count_over_time` over a step-sized bucket is the per-step count).
	rw := &chplan.RangeWindow{
		Input:           plan,
		Range:           step,
		Step:            step,
		Start:           start,
		End:             end,
		TimestampColumn: h.Schema.TimestampColumn,
	}

	// Wrap with the Sample-shape projection so chclient.Sample decoding
	// works. anchor_ts becomes TimeUnix; the group-by columns become
	// keys of the Attributes map (one map entry per by(...) label).
	wrapped := wrapMetricsForSample(rw, metrics)
	wrapped = h.Optimizer.Run(wrapped)

	sqlStr, args, err := chsql.Emit(wrapped)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "", "", err)
		return
	}
	h.Logger.Debug("cerberus tempo metrics_query_range",
		"traceql", q, "start", start, "end", end, "step", step,
		"sql", sqlStr, "args", args)

	samples, err := h.Client.Query(r.Context(), sqlStr, args...)
	if err != nil {
		h.Logger.Warn("cerberus tempo metrics_query_range CH query failed",
			"err", err.Error(), "sql", sqlStr)
		writeError(w, http.StatusBadGateway, "", "", err)
		return
	}

	writeJSON(w, http.StatusOK, MetricsQueryRangeResponse{
		Series: toMetricsSeries(samples, metrics),
	})
}

// parseMetricsStep parses the `step` query parameter — Prom-style
// duration ("30s", "1m", "1h"), plain integer seconds, or a float
// number of seconds. Returns an error if the parsed duration is
// non-positive (zero would lock the matrix at a single anchor).
func parseMetricsStep(raw string) (time.Duration, error) {
	if f, err := strconv.ParseFloat(raw, 64); err == nil {
		d := time.Duration(f * float64(time.Second))
		if d <= 0 {
			return 0, errors.New("'step' must be > 0")
		}
		return d, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("'step' is not a valid duration: %w", err)
	}
	if d <= 0 {
		return 0, errors.New("'step' must be > 0")
	}
	return d, nil
}

// unwrapMetricsAggregate accepts a lowered TraceQL plan and returns the
// MetricsAggregate at its root (or directly under a single Filter
// wrapper). Returns ok=false for any other shape — the trigger for the
// handler's "not a metrics query" 400.
//
// Filter(MetricsAggregate) is currently unreachable from the lowering
// (the spanset predicate becomes Filter inside MetricsAggregate.Inner),
// but kept here for forward-compat with a future scalar-filter step
// that wraps the aggregate.
func unwrapMetricsAggregate(plan chplan.Node) (*chplan.MetricsAggregate, bool) {
	switch v := plan.(type) {
	case *chplan.MetricsAggregate:
		return v, true
	case *chplan.Filter:
		if inner, ok := v.Input.(*chplan.MetricsAggregate); ok {
			return inner, true
		}
	}
	return nil, false
}

// wrapMetricsForSample is the projection that maps the matrix-shape
// RangeWindow's outer SELECT (g0/<alias>..., anchor_ts, Value) into
// chclient.Sample's positional shape (MetricName, Attributes,
// TimeUnix, Value).
//
// Attributes is a Map(String,String) constructed inline:
//
//	map('<label1>', toString(<alias1>), '<label2>', toString(<alias2>), ...)
//
// When MetricsAggregate has no GroupBy, the map is empty (CAST to
// Map(String,String) so CH accepts the Attributes column type).
//
// MetricName is the empty string (TraceQL metrics aggregations don't
// carry a name like PromQL's `__name__`). anchor_ts arrives as
// DateTime64(9); the CH driver maps it to time.Time on the wire.
func wrapMetricsForSample(rw *chplan.RangeWindow, m *chplan.MetricsAggregate) chplan.Node {
	attrAliases := metricsOuterGroupAliases(m.GroupBy, m.GroupByAliases)
	labelNames := metricsLabelNames(m.GroupByAliases, len(m.GroupBy))

	var attrs chplan.Expr
	if len(m.GroupBy) == 0 {
		attrs = emptyAttrsMap()
	} else {
		args := make([]chplan.Expr, 0, len(m.GroupBy)*2)
		for i := range m.GroupBy {
			args = append(args,
				&chplan.LitString{V: labelNames[i]},
				&chplan.FuncCall{
					Name: "toString",
					Args: []chplan.Expr{&chplan.ColumnRef{Name: attrAliases[i]}},
				},
			)
		}
		attrs = &chplan.FuncCall{Name: "map", Args: args}
	}

	return &chplan.Project{
		Input: rw,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: "MetricName"},
			{Expr: attrs, Alias: "Attributes"},
			{Expr: &chplan.ColumnRef{Name: "anchor_ts"}, Alias: "TimeUnix"},
			{Expr: &chplan.ColumnRef{Name: m.ValueAlias}, Alias: "Value"},
		},
	}
}

// metricsOuterGroupAliases mirrors the unexported chsql.outerGroupAliases:
// for each MetricsAggregate.GroupBy entry, return the SELECT-list alias
// used in the matrix-shape outer SELECT. Falls back to "g0", "g1", ...
// for missing / empty aliases — the chsql emitter applies the same rule.
func metricsOuterGroupAliases(groupBy []chplan.Expr, aliases []string) []string {
	if len(groupBy) == 0 {
		return nil
	}
	out := make([]string, 0, len(groupBy))
	for i := range groupBy {
		if i < len(aliases) && aliases[i] != "" {
			out = append(out, aliases[i])
			continue
		}
		out = append(out, "g"+strconv.Itoa(i))
	}
	return out
}

// metricsLabelNames returns the user-facing label names the response's
// {key,value} pairs surface. Today identical to the lowering's
// GroupByAliases (each by(...) attribute's Name); the helper exists so
// a fallback name ("group_0", ...) is used when an alias is empty —
// keeps the response self-describing for any future call-site that
// constructs a MetricsAggregate without aliases.
func metricsLabelNames(aliases []string, n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		if i < len(aliases) && aliases[i] != "" {
			out = append(out, aliases[i])
			continue
		}
		out = append(out, "group_"+strconv.Itoa(i))
	}
	return out
}

// toMetricsSeries pivots a flat sample stream into the Tempo
// series-of-samples envelope. Samples sharing an identical Labels map
// are coalesced into one series; each series' samples are sorted
// ascending by timestamp.
//
// The Labels slice's order within a series mirrors m.GroupByAliases
// (the TraceQL by(...) order). The series slice itself is sorted by
// the canonical (sorted-keys) label-set string so callers see a
// deterministic ordering.
func toMetricsSeries(samples []chclient.Sample, m *chplan.MetricsAggregate) []MetricsSeries {
	labelNames := metricsLabelNames(m.GroupByAliases, len(m.GroupBy))

	type bucket struct {
		labels  []MetricsLabel
		samples []MetricsSample
	}
	byKey := map[string]*bucket{}

	for _, s := range samples {
		key := canonicalKey(s.Labels)
		b, ok := byKey[key]
		if !ok {
			b = &bucket{labels: labelsFromSample(s.Labels, labelNames)}
			byKey[key] = b
		}
		b.samples = append(b.samples, MetricsSample{
			TimestampMs: s.Timestamp.UnixMilli(),
			Value:       s.Value,
		})
	}

	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]MetricsSeries, 0, len(byKey))
	for _, k := range keys {
		b := byKey[k]
		sort.Slice(b.samples, func(i, j int) bool {
			return b.samples[i].TimestampMs < b.samples[j].TimestampMs
		})
		out = append(out, MetricsSeries{
			Labels:  b.labels,
			Samples: b.samples,
		})
	}
	return out
}

// labelsFromSample materialises the {key,value} pair slice for one
// row's Attributes map. labelNames is the preferred ordering (the
// by(...) order); any keys present in the Sample's Labels but not in
// labelNames are appended afterwards in ASCII order — defensive against
// the SQL projection returning a label the handler didn't expect.
func labelsFromSample(attrs map[string]string, labelNames []string) []MetricsLabel {
	out := make([]MetricsLabel, 0, len(attrs))
	seen := make(map[string]struct{}, len(labelNames))
	for _, name := range labelNames {
		v, ok := attrs[name]
		if !ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, MetricsLabel{Key: name, Value: v})
	}
	extras := make([]string, 0)
	for k := range attrs {
		if _, ok := seen[k]; ok {
			continue
		}
		extras = append(extras, k)
	}
	sort.Strings(extras)
	for _, k := range extras {
		out = append(out, MetricsLabel{Key: k, Value: attrs[k]})
	}
	return out
}
