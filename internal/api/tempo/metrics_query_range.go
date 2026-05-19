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
	"github.com/tsouza/cerberus/internal/chsql"
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
//
// Exemplars is always emitted as a JSON array (never omitted), even
// when empty, so Grafana's Tempo datasource sees a stable envelope
// shape. The handler populates exemplars via a second SQL query
// against otel_traces (see chsql.EmitMetricsExemplars): one
// representative trace per (series, bucket) anchor, projected through
// the same matrix-shape time window as the metric samples. Series with
// no matching exemplar spans keep the empty-array envelope contract.
// See EF #398 for the wire-shape work this closed against.
type MetricsSeries struct {
	Labels    []MetricsLabel  `json:"labels"`
	Samples   []MetricsSample `json:"samples"`
	Exemplars []Exemplar      `json:"exemplars"`
}

// Exemplar is one trace-anchored sample point in MetricsSeries.Exemplars.
type Exemplar struct {
	Labels    []MetricsLabel `json:"labels"`
	Value     float64        `json:"value"`
	Timestamp int64          `json:"timestamp_ms"`
	TraceID   string         `json:"traceID"`
	SpanID    string         `json:"spanID,omitempty"`
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

	series := toMetricsSeries(res.Samples, metrics)
	// Zero-fill the matrix step grid for count/rate operators so the
	// response shape matches Tempo's reference engine_metrics.go: its
	// StepAggregator pre-allocates one CountOverTimeAggregator per
	// interval whose default Sample() is 0 (count starts at zero), so
	// empty buckets surface as 0-valued samples across the full
	// [Start, End] grid. Cerberus's matrix SQL emits one row per
	// (group, anchor) bucket only when at least one span falls in
	// (anchor_ts - range, anchor_ts]; without this post-step empty
	// buckets disappear, dropping samples count from N to (#observed
	// buckets) — the smoke corpus's count_over_time_groupby_service
	// case showed tempo=121 vs cerberus=2 against the same seeded
	// dataset for that reason. Tempo's OverTimeAggregator (sum / avg /
	// min / max / quantile) initialises its value to NaN, and the
	// ToProto loop skips NaN samples — so those operators already
	// match cerberus's observed-only emission without a zero-fill
	// pass.
	series = zeroFillMatrixGrid(series, metrics, start, end, step)

	exSQL, exArgs, exErr := chsql.EmitMetricsExemplars(ctx, rw, metrics,
		h.Schema.TraceIDColumn, h.Schema.SpanIDColumn, 1)
	if exErr != nil {
		// Emit failure: the matrix response still ships with an empty
		// `Exemplars` array per Tempo's wire shape. Warn so production
		// can see this rather than silently degrade.
		h.Logger.Warn("cerberus tempo metrics_query_range exemplars emit failed (matrix returns without exemplars)", "err", exErr)
	} else {
		exSamples, qErr := h.Client.Query(ctx, exSQL, exArgs...)
		if qErr != nil {
			// Execution failure: same wire shape applies. Warn so a
			// transient CH failure doesn't go unnoticed.
			h.Logger.Warn("cerberus tempo metrics_query_range exemplars query failed (matrix returns without exemplars)", "err", qErr)
		} else {
			attachExemplars(series, exSamples, metrics)
		}
	}

	writeEngineHeaders(w, res.Headers)
	writeJSON(w, http.StatusOK, MetricsQueryRangeResponse{
		Series: series,
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

// tempoMultiQuantilePhiLabel is the synthetic alias the chsql
// multi-quantile fan-out projects when MetricsAggregate.Quantiles
// holds more than one phi. Lockstep with chsql's
// `metricsMultiQuantilePhiLabel` (range_window.go) — both must agree
// on the column alias so the handler can read it out of the row
// stream. The wire-side label key is `tempoQuantileLabel` ("p"),
// mirroring Tempo's HistogramAggregator (engine_metrics.go) which
// appends `Label{"p", NewStaticFloat(q)}` to each per-quantile series.
const tempoMultiQuantilePhiLabel = "__phi__"

// tempoMetricNameLabel mirrors the Prometheus-style `__name__` label
// Tempo's UngroupedAggregator (engine_metrics.go) attaches to the
// single series an ungrouped metrics-pipeline query returns. The
// reference engine emits `{__name__="rate"}` for `{} | rate()` and
// `{__name__="avg_over_time"}` for `{} | avg_over_time(duration)`,
// rather than an empty label set, so Grafana's Tempo datasource (and
// the differ in compatibility/tempo) can always key series by at least
// one label. Cerberus mirrors that shape so an ungrouped response
// canonicalises identically across the two backends.
//
// `quantile_over_time` is the lone exception — Tempo routes it through
// HistogramAggregator rather than UngroupedAggregator, so the wire
// shape is `{p="<phi>"}` (see tempoQuantileLabel) instead of
// `{__name__="quantile_over_time"}`. wrapMetricsForSample +
// metricsLabelNames branch on Op to honour that.
const tempoMetricNameLabel = "__name__"

// tempoQuantileLabel mirrors the `p` label Tempo's HistogramAggregator
// (engine_metrics.go) appends to every series produced by
// `quantile_over_time(...)`. The label value is the phi formatted via
// `strconv.FormatFloat('f', -1, 64)` (e.g. 0.95 → "0.95") — matching
// what the differ in compatibility/tempo extracts from Tempo's
// `doubleValue` AnyValue via `fmt.Sprint(*anyV.DoubleValue)` and what
// chsql's `metricsMultiQuantileFanoutFrag` projects via `formatFloat`
// for the multi-phi fan-out column. Per-phi series are emitted whether
// or not the query carries a `by(...)` clause: ungrouped queries get
// `{p="<phi>"}` rather than `{__name__="quantile_over_time"}` because
// Tempo's UngroupedAggregator is not on the quantile path.
const tempoQuantileLabel = "p"

// wrapMetricsForSample maps the matrix-shape RangeWindow's outer SELECT
// (g0/<alias>..., anchor_ts, Value) into chclient.Sample's positional
// shape (MetricName, Attributes, TimeUnix, Value). Attributes becomes
// `map('<label>', toString(<alias>), ...)`; when the query has no
// `by(...)` clause the map carries a single
// `('__name__', '<op-name>')` entry mirroring Tempo's UngroupedAggregator
// (see `pkg/traceql.UngroupedAggregator.Series`), so an ungrouped
// response is keyed by at least one label rather than emitting the
// empty `{}` label set that previously diverged from the reference
// engine's `{__name__="rate"}` / `{__name__="count_over_time"}` shape.
// MetricName is empty on the chclient.Sample tuple (TraceQL has no
// per-row metric name beyond the synthetic `__name__` carried in
// Attributes); anchor_ts arrives as DateTime64(9) → time.Time on the
// wire.
//
// The Attributes map's keys are the Tempo-canonical wire names from
// metricsLabelNames (scope-prefixed `resource.service.name`,
// `span.http.method`, etc., mirroring upstream Tempo's response shape).
// The values still reference the SQL-side bare aliases via attrAliases
// — the SELECT-list column for `by (resource.service.name)` is aliased
// as `service.name`, while the row's Attributes map key is
// `resource.service.name`. Decoupling the two slices lets the chsql
// emitter keep emitting compact column aliases without disturbing the
// wire shape Grafana's Tempo datasource consumes.
//
// `quantile_over_time` follows the HistogramAggregator path in Tempo
// (engine_metrics.go: NewHistogramAggregator + Results) rather than
// UngroupedAggregator, so every output series carries a `p="<phi>"`
// label and never `__name__="quantile_over_time"`. The single-phi
// branch injects the phi string as an inline literal because the chsql
// emitter doesn't project a per-row phi column in that case; the
// multi-phi branch surfaces the synthetic `__phi__` column produced by
// the chsql fan-out under the wire-canonical `p` key so each
// (group × phi) pair becomes its own response series.
func wrapMetricsForSample(rw *chplan.RangeWindow, m *chplan.MetricsAggregate) chplan.Node {
	attrAliases := metricsOuterGroupAliases(m.GroupBy, m.GroupByAliases)
	labelNames := metricsLabelNames(m)
	multiPhi := len(m.Quantiles) > 1
	isQuantile := m.Op == chplan.MetricsOpQuantileOverTime

	var attrs chplan.Expr
	switch {
	case isQuantile:
		// quantile_over_time is special: Tempo's HistogramAggregator
		// appends `Label{"p", NewStaticFloat(q)}` to every per-phi
		// series, regardless of whether the query has a `by(...)`
		// clause. The chsql emitter projects a `__phi__` String column
		// for the multi-phi fan-out, but the single-phi path returns
		// just the value column — so the handler injects the phi as an
		// inline literal in that branch.
		args := make([]chplan.Expr, 0, (len(m.GroupBy)+1)*2)
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
		args = append(args, &chplan.LitString{V: tempoQuantileLabel})
		if multiPhi {
			args = append(args, &chplan.ColumnRef{Name: tempoMultiQuantilePhiLabel})
		} else if len(m.Quantiles) == 1 {
			args = append(args, &chplan.LitString{V: formatPhi(m.Quantiles[0])})
		} else {
			// MetricsAggregate.Quantiles is empty — the chsql emitter
			// would reject this earlier with ErrUnsupported, but
			// defensively project an empty string so the response shape
			// stays self-consistent rather than crashing on
			// args[len-1].
			args = append(args, &chplan.LitString{V: ""})
		}
		attrs = &chplan.FuncCall{Name: "map", Args: args}
	case len(m.GroupBy) == 0:
		// Ungrouped non-quantile: emit Tempo's UngroupedAggregator-style
		// `{__name__="<op>"}` label so the response series is keyed by
		// `__name__` rather than the empty label set. The constant
		// op-name comes from chplan.MetricsOp.String() (rate /
		// count_over_time / avg_over_time / ...), matching the upstream
		// wire form LabelsFromArgs(labels.MetricName, op.String()).
		attrs = &chplan.FuncCall{
			Name: "map",
			Args: []chplan.Expr{
				&chplan.LitString{V: tempoMetricNameLabel},
				&chplan.LitString{V: m.Op.String()},
			},
		}
	default:
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

// formatPhi renders a phi value (0 <= phi <= 1) into the wire-format
// string Tempo's HistogramAggregator surfaces on the `p` label. Mirrors
// `strconv.FormatFloat(v, 'f', -1, 64)` — what chsql's
// metricsMultiQuantileFanoutFrag uses for the inline-literal phi
// strings in the multi-phi fan-out, and what `fmt.Sprint(float64)`
// returns when the differ stringifies Tempo's `doubleValue` AnyValue.
// Stable for the phi values cerberus accepts (0 → "0", 0.5 → "0.5",
// 0.95 → "0.95", 1 → "1").
func formatPhi(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
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
// {key,value} pairs surface — the lowering's GroupByDisplayNames (the
// Tempo-canonical scope-prefixed wire form such as
// `resource.service.name`, `span.http.method`, or a bare intrinsic name
// like `kind`), with a fallback to GroupByAliases (bare attribute
// path) when the lowering didn't populate the display slice, and a
// further fallback ("group_0", ...) for any empty slot.
//
// For `quantile_over_time` the slice ends with `"p"` — every per-phi
// series carries that label (Tempo HistogramAggregator parity). This
// holds whether or not the query has a `by(...)` clause, and whether
// the chsql emit is single-phi (no extra column; wrapMetricsForSample
// injects the phi as a literal) or multi-phi (the chsql fan-out
// projects a `__phi__` String column that wrapMetricsForSample
// references and surfaces under the wire key `p`).
//
// For ungrouped non-quantile metrics queries the slice is
// `["__name__"]` so labelsFromSample preserves the order expected by
// Tempo's UngroupedAggregator wire shape (a single `__name__=<op>`
// label per series — see wrapMetricsForSample's doc-comment for the
// upstream reference).
//
// Aligning with the upstream Tempo response shape: the reference
// /api/metrics/query_range emits `resource.service.name` for a
// `by (resource.service.name)` clause — see grafana/tempo
// `pkg/traceql.Attribute.String` and the metrics-engine `labelsFor`
// loop, plus the integration test in `integration/api/query_range_test.go`
// that asserts `label.Key == "resource.res_attr"` for a
// `by (resource.res_attr)` query.
func metricsLabelNames(m *chplan.MetricsAggregate) []string {
	n := len(m.GroupBy)
	isQuantile := m.Op == chplan.MetricsOpQuantileOverTime
	if n == 0 && !isQuantile {
		return []string{tempoMetricNameLabel}
	}
	out := make([]string, 0, n+1)
	for i := 0; i < n; i++ {
		if i < len(m.GroupByDisplayNames) && m.GroupByDisplayNames[i] != "" {
			out = append(out, m.GroupByDisplayNames[i])
			continue
		}
		if i < len(m.GroupByAliases) && m.GroupByAliases[i] != "" {
			out = append(out, m.GroupByAliases[i])
			continue
		}
		out = append(out, "group_"+strconv.Itoa(i))
	}
	if isQuantile {
		out = append(out, tempoQuantileLabel)
	}
	return out
}

// toMetricsSeries pivots a flat sample stream into the Tempo
// series-of-samples envelope. Rows sharing a Labels map are coalesced
// into one series, samples sorted ascending by timestamp. Series order
// is deterministic (sorted by canonical label-set key).
func toMetricsSeries(samples []chclient.Sample, m *chplan.MetricsAggregate) []MetricsSeries {
	labelNames := metricsLabelNames(m)

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
		out = append(out, MetricsSeries{
			Labels:    b.labels,
			Samples:   b.samples,
			Exemplars: []Exemplar{},
		})
	}
	return out
}

// zeroFillMatrixGrid extends each series's Samples to the full matrix
// step grid for `| count_over_time()` and `| rate()` queries, inserting
// 0-valued samples for anchors that had no observed spans.
//
// Tempo's reference metrics engine pre-allocates one
// CountOverTimeAggregator per interval (engine_metrics.go::StepAggregator)
// whose default Sample() is 0; its ToProto path emits the full grid for
// count/rate. Cerberus's matrix SQL only emits rows for (group, anchor)
// pairs where at least one span lands in (anchor_ts - range, anchor_ts],
// so without this fill the response loses every empty bucket and the
// differ trips on `samples count tempo=N vs cerberus=M`.
//
// The fill anchors mirror the chsql emitter's
// arrayJoin(arrayMap(i -> End - i*Step, range(0, N))) grid:
// anchor[i] = end - i*step for i in [0, N), with N = (end-start)/step + 1.
// In ascending timestamp order that yields [start, start+step, ...,
// end-step, end]. Each series is filled independently; series that
// never observed any span are left absent (matches Tempo's
// SpanAggregator.Observe gating).
//
// No-op when:
//   - step <= 0 (defensive — the handler rejects step <= 0 upstream,
//     so this branch only fires under tests that bypass the request
//     validator),
//   - start / end aren't both set (the chsql matrix path falls back to
//     a single anchor at End in that case; one sample per series is
//     fine without a fill),
//   - the metrics op isn't count_over_time / rate (sum / avg / min /
//     max / quantile share Tempo's NaN-skip semantics — see
//     OverTimeAggregator initialisation in engine_metrics.go),
//   - the input slice is empty (no observed series → no zero-fill;
//     mirrors Tempo's behaviour of only initialising entries via
//     SpanAggregator.Observe).
func zeroFillMatrixGrid(series []MetricsSeries, m *chplan.MetricsAggregate, start, end time.Time, step time.Duration) []MetricsSeries {
	if len(series) == 0 || step <= 0 || start.IsZero() || end.IsZero() {
		return series
	}
	if m.Op != chplan.MetricsOpCountOverTime && m.Op != chplan.MetricsOpRate {
		return series
	}
	span := end.Sub(start)
	if span < 0 {
		return series
	}
	stepNS := step.Nanoseconds()
	if stepNS <= 0 {
		return series
	}
	// N = span/step + 1 matches chsql's numAnchors so the post-fill
	// grid exactly aligns with the matrix anchor set the emitter
	// produced.
	n := span.Nanoseconds()/stepNS + 1
	if n <= 1 {
		return series
	}
	// Pre-compute the anchor grid as DateTime64-precision unix-milli
	// values so the merge below operates on integer keys (the wire
	// shape's TimestampMs).
	anchors := make([]int64, 0, n)
	for i := int64(0); i < n; i++ {
		anchorTS := end.Add(-time.Duration(i) * step)
		anchors = append(anchors, anchorTS.UnixMilli())
	}
	// Anchors come back end-down; reorder to ascending so the merged
	// samples slice ends up sorted (toMetricsSeries already sorts each
	// series ascending; we keep the invariant after fill).
	sort.Slice(anchors, func(i, j int) bool { return anchors[i] < anchors[j] })

	for i := range series {
		present := make(map[int64]struct{}, len(series[i].Samples))
		for _, s := range series[i].Samples {
			present[s.TimestampMs] = struct{}{}
		}
		out := make([]MetricsSample, 0, len(anchors))
		for _, ts := range anchors {
			if _, ok := present[ts]; ok {
				continue
			}
			out = append(out, MetricsSample{TimestampMs: ts, Value: 0})
		}
		if len(out) == 0 {
			continue
		}
		series[i].Samples = append(series[i].Samples, out...)
		sort.Slice(series[i].Samples, func(a, b int) bool {
			return series[i].Samples[a].TimestampMs < series[i].Samples[b].TimestampMs
		})
	}
	return series
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

func attachExemplars(series []MetricsSeries, exSamples []chclient.Sample, m *chplan.MetricsAggregate) {
	labelNames := metricsLabelNames(m)
	byKey := make(map[string]*MetricsSeries, len(series))
	for i := range series {
		key := format.CanonicalKey(labelsToMap(series[i].Labels))
		byKey[key] = &series[i]
	}

	for _, s := range exSamples {
		traceID := s.Labels["trace:id"]
		spanID := s.Labels["span:id"]
		if traceID == "" {
			continue
		}
		groupLabels := make(map[string]string, len(s.Labels))
		for _, name := range labelNames {
			if v, ok := s.Labels[name]; ok {
				groupLabels[name] = v
			}
		}
		key := format.CanonicalKey(groupLabels)
		ps, ok := byKey[key]
		if !ok {
			continue
		}
		exLabels := make([]MetricsLabel, 0, 2)
		exLabels = append(exLabels, MetricsLabel{Key: "trace:id", Value: traceID})
		if spanID != "" {
			exLabels = append(exLabels, MetricsLabel{Key: "span:id", Value: spanID})
		}
		ps.Exemplars = append(ps.Exemplars, Exemplar{
			Labels:    exLabels,
			Value:     s.Value,
			Timestamp: s.Timestamp.UnixMilli(),
			TraceID:   traceID,
			SpanID:    spanID,
		})
	}

	for i := range series {
		if len(series[i].Exemplars) == 0 {
			series[i].Exemplars = []Exemplar{}
		}
		sort.Slice(series[i].Exemplars, func(j, k int) bool {
			return series[i].Exemplars[j].Timestamp < series[i].Exemplars[k].Timestamp
		})
	}
}

func labelsToMap(labels []MetricsLabel) map[string]string {
	m := make(map[string]string, len(labels))
	for _, l := range labels {
		m[l.Key] = l.Value
	}
	return m
}
