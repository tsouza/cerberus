package tempo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tsouza/cerberus/internal/api/format"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/telemetry"
	traceql_lower "github.com/tsouza/cerberus/internal/traceql"
)

// MetricsQueryRangeResponse is the body of `GET /api/metrics/query_range`.
// Mirrors Tempo's native wire shape — one MetricsSeries per (group-by
// labels) tuple, each carrying per-anchor samples sorted ascending by
// timestamp. Grafana's Tempo datasource consumes this directly for the
// service-graph node + edge metrics.
type MetricsQueryRangeResponse struct {
	Series []MetricsSeries `json:"series"`
}

// MetricsSeries is one entry of MetricsQueryRangeResponse.Series.
type MetricsSeries struct {
	Labels  []MetricsLabel  `json:"labels"`
	Samples []MetricsSample `json:"samples"`
}

// MetricsLabel is one (key, value) pair in MetricsSeries.Labels.
//
// On the wire it serialises to Tempo's tempopb shape:
//
//	{"key":"<k>","value":{"stringValue":"<v>"}}
//
// matching `pkg/tempopb/common/v1` KeyValue + AnyValue rendered via
// `gogo/protobuf/jsonpb` (which honours the `json=stringValue` proto
// tag, not the Go-side `json:"string_value"` struct tag). Holding the
// value as a plain Go string in the in-process struct keeps handler /
// test code ergonomic; the custom MarshalJSON wraps the value in the
// AnyValue envelope so Grafana's Tempo datasource parses cerberus's
// metrics responses identically to a reference Tempo backend.
//
// See https://github.com/tsouza/cerberus/pull/398 (EF #398) for the
// shape-divergence finding that motivated this.
type MetricsLabel struct {
	Key   string
	Value string
}

// metricsLabelWire mirrors the on-wire tempopb KeyValue + AnyValue
// shape (string variant only — TraceQL group-by keys are always
// stringified via toString(...) on the SQL side, so other AnyValue
// variants don't apply here).
type metricsLabelWire struct {
	Key   string                `json:"key"`
	Value metricsLabelValueWire `json:"value"`
}

type metricsLabelValueWire struct {
	StringValue string `json:"stringValue"`
}

// MarshalJSON emits the tempopb KeyValue + AnyValue wire shape.
func (l MetricsLabel) MarshalJSON() ([]byte, error) {
	return json.Marshal(metricsLabelWire{
		Key:   l.Key,
		Value: metricsLabelValueWire{StringValue: l.Value},
	})
}

// UnmarshalJSON parses the tempopb KeyValue + AnyValue wire shape.
// Also tolerates the legacy flat `{"key":"k","value":"v"}` shape that
// cerberus used to emit (pre-EF #398), so handler tests + any consumer
// that bound to the old shape keep round-tripping.
func (l *MetricsLabel) UnmarshalJSON(data []byte) error {
	// Probe the raw `value` field first — its JSON shape decides which
	// path to take. A flat-string value can't be decoded into the typed
	// metricsLabelWire (struct vs. string), so we'd lose the value in a
	// single-pass decode.
	var probe struct {
		Key   string          `json:"key"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return fmt.Errorf("metrics label: decode: %w", err)
	}
	l.Key = probe.Key
	if len(probe.Value) == 0 || string(probe.Value) == "null" {
		l.Value = ""
		return nil
	}
	// Tempopb shape — object with stringValue child.
	if probe.Value[0] == '{' {
		var v metricsLabelValueWire
		if err := json.Unmarshal(probe.Value, &v); err != nil {
			return fmt.Errorf("metrics label %q value: %w", probe.Key, err)
		}
		l.Value = v.StringValue
		return nil
	}
	// Legacy flat-string shape.
	if probe.Value[0] == '"' {
		var s string
		if err := json.Unmarshal(probe.Value, &s); err != nil {
			return fmt.Errorf("metrics label %q value: %w", probe.Key, err)
		}
		l.Value = s
		return nil
	}
	return fmt.Errorf("metrics label %q value: unrecognised shape: %s", probe.Key, string(probe.Value))
}

// MetricsSample is one point in MetricsSeries.Samples — timestamp in
// milliseconds (Tempo's wire unit) plus the per-bucket float value.
type MetricsSample struct {
	TimestampMs int64   `json:"timestampMs"`
	Value       float64 `json:"value"`
}

// metricsLang is a tiny Engine.Lang adapter used by
// /api/metrics/query_range. The handler hand-rolls the plan (parse +
// lower + wrap) before calling engine.QueryPlan, so Parse is unused
// and ProjectSamples is a passthrough (the matrix-shape Project is
// already on top of the plan).
type metricsLang struct{}

func (metricsLang) Name() string { return "traceql" }

func (metricsLang) Parse(_ context.Context, _ string) (chplan.Node, engine.Meta, error) {
	// Engine.QueryPlan never calls Parse; the error keeps the adapter
	// honest if Engine.Query is ever invoked against it by mistake.
	return nil, engine.Meta{}, errors.New("metricsLang: Parse not supported; use Engine.QueryPlan with a pre-wrapped plan")
}

func (metricsLang) ProjectSamples(plan chplan.Node, _ engine.Meta) chplan.Node {
	return plan
}

// handleMetricsQueryRange implements `GET /api/metrics/query_range`.
//
// Pipeline: parse the TraceQL metrics-pipeline query, lower it to a
// chplan MetricsAggregate (optionally inside a single Filter wrapper),
// wrap with chplan.RangeWindow carrying start / end / step, wrap with a
// Sample-shape Project, then route through engine.QueryPlan for
// optimize → emit → execute. Finally pivot the flat row stream into
// Tempo's series-of-samples envelope.
//
// Contract: `q` = TraceQL metrics query; `start` / `end` = unix seconds
// or nanoseconds (parseTempoStartEnd also accepts RFC3339); `step` =
// Prom-style duration ("30s", "1m") or plain seconds.
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

	ctx := r.Context()
	// Parse + lower inline so we can wrap the lowered plan with the
	// matrix-shape RangeWindow before engine.QueryPlan runs.
	parseT := telemetry.ObserveStage(telemetry.StageParse)
	expr, perr := parseExpr(ctx, q)
	parseT.Done(ctx)
	if perr != nil {
		writeError(w, http.StatusBadRequest, "", "", perr)
		return
	}
	lowerT := telemetry.ObserveStage(telemetry.StageLower)
	plan, lerr := traceql_lower.Lower(ctx, expr, h.Schema)
	lowerT.Done(ctx)
	if lerr != nil {
		writeError(w, http.StatusUnprocessableEntity, "", "", lerr)
		return
	}

	metrics, ok := unwrapMetricsAggregate(plan)
	if !ok {
		writeError(w, http.StatusBadRequest, "", "",
			fmt.Errorf("query %q is not a TraceQL metrics-pipeline expression — /api/metrics/query_range requires `| rate()`, `| count_over_time()`, `| *_over_time(...)` or `| quantile_over_time(...)`", q))
		return
	}

	// Range = Step → each bucket spans exactly one step width, matching
	// Tempo's reference metrics semantics where `count_over_time` over
	// a step-sized bucket is the per-step count.
	rw := &chplan.RangeWindow{
		Input:           plan,
		Range:           step,
		Step:            step,
		Start:           start,
		End:             end,
		TimestampColumn: h.Schema.TimestampColumn,
	}
	wrapped := wrapMetricsForSample(rw, metrics)

	// metricsLang.ProjectSamples is a passthrough (we already wrapped
	// with the Sample projection above) so engine.QueryPlan runs
	// optimize → emit → execute without re-wrapping the matrix shape.
	res, qerr := h.Engine.QueryPlan(ctx, metricsLang{}, wrapped, engine.Meta{
		IsMetric:      true,
		ResponseShape: "tempo-metrics-matrix",
	})
	if qerr != nil {
		writeError(w, classifyMetricsQueryRangeErr(qerr), "", "", qerr)
		return
	}
	h.Logger.Debug("cerberus tempo metrics_query_range",
		"traceql", q, "start", start, "end", end, "step", step,
		"sql", res.SQL, "args", res.Args)

	writeEngineHeaders(w, res.Headers)
	writeJSON(w, http.StatusOK, MetricsQueryRangeResponse{
		Series: toMetricsSeries(res.Samples, metrics),
	})
}

// classifyMetricsQueryRangeErr maps engine.QueryPlan failures to HTTP
// status: emit → 500, execute → 502. Parse / lower never bubble through
// QueryPlan here because the handler runs them inline before wrapping.
func classifyMetricsQueryRangeErr(err error) int {
	if err == nil {
		return http.StatusInternalServerError
	}
	if strings.Contains(err.Error(), "engine: execute:") {
		return http.StatusBadGateway
	}
	return http.StatusInternalServerError
}

// parseMetricsStep parses the `step` query parameter. Accepts a Go
// duration string ("30s", "1m"), plain integer seconds, or a float
// number of seconds — same tolerance as the PromQL handler so Grafana's
// Tempo datasource (which can send either shape) interoperates. Returns
// an error on non-positive values (zero would lock the matrix at one
// anchor).
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

// unwrapMetricsAggregate returns the MetricsAggregate at the plan root
// (or directly under a single Filter wrapper, kept for forward-compat
// with a future scalar-filter stage). Returns ok=false for any other
// shape — the trigger for the handler's "not a metrics query" 400.
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

// wrapMetricsForSample maps the matrix-shape RangeWindow's outer SELECT
// (g0/<alias>..., anchor_ts, Value) into chclient.Sample's positional
// shape (MetricName, Attributes, TimeUnix, Value). Attributes becomes
// `map('<label>', toString(<alias>), ...)` (or an empty Map(String,String)
// when there's no GroupBy); MetricName is empty (TraceQL has no
// __name__); anchor_ts arrives as DateTime64(9) → time.Time on the wire.
func wrapMetricsForSample(rw *chplan.RangeWindow, m *chplan.MetricsAggregate) chplan.Node {
	attrAliases := metricsOuterGroupAliases(m.GroupBy, m.GroupByAliases)
	labelNames := metricsLabelNames(m.GroupByAliases, len(m.GroupBy))

	var attrs chplan.Expr
	if len(m.GroupBy) == 0 {
		attrs = emptyAttrsMap()
	} else {
		args := make([]chplan.Expr, 0, len(m.GroupBy)*2)
		for i := range m.GroupBy {
			args = append(
				args,
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
// the SELECT-list alias used by emitRangeWindowMetrics for each
// MetricsAggregate.GroupBy entry, falling back to "g0", "g1", ... for
// missing aliases (same rule the chsql emitter applies — the two must
// stay in lockstep).
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
// {key,value} pairs surface — the lowering's GroupByAliases with a
// fallback ("group_0", ...) for any empty alias slot.
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
// series-of-samples envelope. Rows sharing a Labels map are coalesced
// into one series, samples sorted ascending by timestamp. Series order
// is deterministic (sorted by canonical label-set key).
func toMetricsSeries(samples []chclient.Sample, m *chplan.MetricsAggregate) []MetricsSeries {
	labelNames := metricsLabelNames(m.GroupByAliases, len(m.GroupBy))

	type bucket struct {
		labels  []MetricsLabel
		samples []MetricsSample
	}
	byKey := map[string]*bucket{}

	for _, s := range samples {
		key := format.CanonicalKey(s.Labels)
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
		out = append(out, MetricsSeries{Labels: b.labels, Samples: b.samples})
	}
	return out
}

// labelsFromSample materialises the {key,value} pair slice for one
// row's Attributes map, preferring labelNames' ordering (by(...) order)
// and appending any unexpected keys in ASCII order — defensive against
// the SQL projection surfacing a label the handler didn't expect.
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
