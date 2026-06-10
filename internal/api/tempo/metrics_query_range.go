package tempo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	upstreamTraceql "github.com/grafana/tempo/pkg/traceql"

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

// alignMetricsWindow snaps a metrics-range window to the step grid the
// way Tempo's api.AlignRequest does: start rounds down to the nearest
// step multiple, end rounds up (unchanged when already a multiple).
// Range queries only — Tempo's IsInstant path skips alignment, and so
// does cerberus's handleMetricsQueryInstant.
func alignMetricsWindow(start, end time.Time, step time.Duration) (time.Time, time.Time) {
	alignedStart := start.Truncate(step)
	alignedEnd := end.Truncate(step)
	if alignedEnd.Before(end) {
		alignedEnd = alignedEnd.Add(step)
	}
	return alignedStart, alignedEnd
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
	// `step` is optional, matching reference Tempo: the query-frontend
	// defaults a zero step via traceql.DefaultQueryRangeStep (~240
	// points across the window, see modules/frontend/
	// metrics_query_range_handler.go). Grafana's Traces Drilldown app
	// (preinstalled since Grafana 12.x) relies on this — its
	// issue-detector query omits step entirely.
	var step time.Duration
	if stepStr := r.URL.Query().Get("step"); stepStr == "" {
		ns := upstreamTraceql.DefaultQueryRangeStep(
			uint64(start.UnixNano()), uint64(end.UnixNano()),
		)
		// DefaultQueryRangeStep targets ~240 points across the window,
		// so ns is far below MaxInt64 for any representable time range;
		// clamp anyway so the uint64 → Duration conversion is provably
		// overflow-free (gosec G115).
		if ns > math.MaxInt64 {
			ns = math.MaxInt64
		}
		step = time.Duration(ns)
	} else {
		var err error
		step, err = parseMetricsStep(stepStr)
		if err != nil {
			writeError(w, http.StatusBadRequest, "", "", err)
			return
		}
	}
	// Align the eval grid to the step the way Tempo's api.AlignRequest
	// does: start rounds down to a step multiple, end rounds up (or
	// stays put when already aligned). Tempo additionally subtracts one
	// step from the aligned start to force an initial bucket, then
	// stamps each right-closed interval with its right edge
	// (start + (k+1)*step) — netting out to a sample grid of step
	// multiples spanning [floor(start), ceil(end)], which is exactly
	// the anchor set the [Start, End] range below fans out. Without
	// this, the anchors sat at start + k*step — offset from Tempo's
	// grid by (start mod step) — and every range-shape compat case
	// diff'd on sample timestamps.
	start, end = alignMetricsWindow(start, end, step)

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
		// `| histogram_over_time(<attr>)` lowers to its own plan node
		// (chplan.MetricsHistogramOverTime) because the per-bucket value
		// is a distribution, not a scalar — route it through the
		// histogram response shape (one series per (group, __bucket)).
		if hist, hok := unwrapMetricsHistogram(plan); hok {
			h.serveMetricsQueryRangeHistogram(ctx, w, q, plan, hist, start, end, step)
			return
		}
		writeError(w, http.StatusBadRequest, "", "",
			fmt.Errorf("query %q is not a TraceQL metrics-pipeline expression — /api/metrics/query_range requires `| rate()`, `| count_over_time()`, `| *_over_time(...)`, `| quantile_over_time(...)` or `| histogram_over_time(...)`", q))
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

	// quantile_over_time: the matrix SQL emits `(group, anchor, bucket,
	// count)` tuples (with synthetic 0-bucket / 0-count phantom rows
	// per (group, anchor) so empty anchors survive the GROUP BY).
	// Collapse the bucket rows into the per-(group, phi, anchor) scalar
	// wire shape via Tempo's `Log2QuantileWithBucket` — empty anchors
	// resolve to 0 because the phantom-bucket totalCount is zero.
	samples := res.Samples
	if metrics.Op == chplan.MetricsOpQuantileOverTime {
		samples = postProcessQuantileBuckets(samples, metrics)
	}

	// Matrix-shape zero-fill is the SQL emitter's concern, not the
	// handler's: `internal/chsql.emitRangeWindowMetrics` swaps the
	// outer WHERE clause for `countIf(<window pred>)` on the
	// count_over_time / rate paths, and
	// `emitRangeWindowMetricsQuantileBuckets` emits a phantom
	// 0-bucket / 0-count row per (group, anchor) — both produce one
	// row per (group, anchor) tuple the inner fanout materialises,
	// matching Tempo's StepAggregator + HistogramAggregator
	// emit-every-anchor wire shape without a Go-side post-pass. See
	// `metricsOpZeroFillsEmptyBuckets` in internal/chsql/range_window.go
	// for the per-op rationale (NaN-skip operators — sum / avg / min /
	// max — keep the WHERE-filtered "observed-only" shape).
	series := toMetricsSeries(samples, metrics)

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

// tempoQuantileBucketLabel mirrors the `__bucket` synthetic label
// Tempo's HistogramAggregator uses internally on the metrics-engine
// hot path (pkg/traceql/engine_metrics.go, `internalLabelBucket`).
// The chsql matrix-path quantile emitter projects each row's
// power-of-two bucket edge under this label so the post-processor in
// the Tempo handler can recover the bucket alongside the count and
// drive `pkg/traceql.Log2QuantileWithBucket` per phi. The
// post-processor strips this label before emitting the final wire
// shape — Tempo's reference engine never surfaces `__bucket` to the
// client; it's purely an internal-routing key.
const tempoQuantileBucketLabel = "__bucket"

// tempoQuantileLabel mirrors the `p` label Tempo's HistogramAggregator
// (engine_metrics.go) appends to every series produced by
// `quantile_over_time(...)`. The label value is the phi formatted via
// `strconv.FormatFloat('f', -1, 64)` (e.g. 0.95 → "0.95") — matching
// what the differ in compatibility/tempo extracts from Tempo's
// `doubleValue` AnyValue via `fmt.Sprint(*anyV.DoubleValue)` and what
// the post-processor (`postProcessQuantileBuckets`) writes onto each
// per-phi series after collapsing the bucket-shape row stream. Per-phi
// series are emitted whether
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
// label and never `__name__="quantile_over_time"`. The matrix SQL
// emits one row per (group, anchor, bucket) tuple — the chsql side
// projects each row's power-of-two bucket edge under the synthetic
// `__bucket` label; the handler's `postProcessQuantileBuckets` then
// collapses those rows via `traceql.Log2QuantileWithBucket(phi,
// buckets)` per phi (independent of len(m.Quantiles)) and synthesises
// the wire `p="<phi>"` label on each emitted series.
func wrapMetricsForSample(rw *chplan.RangeWindow, m *chplan.MetricsAggregate) chplan.Node {
	attrAliases := metricsOuterGroupAliases(m.GroupBy, m.GroupByAliases)
	labelNames := metricsLabelNames(m)
	isQuantile := m.Op == chplan.MetricsOpQuantileOverTime

	var attrs chplan.Expr
	switch {
	case isQuantile:
		// quantile_over_time emits `(group, anchor, bucket, count)` rows
		// out of the chsql matrix path — see
		// `emitRangeWindowMetricsQuantileBuckets`. The post-processor
		// (`postProcessQuantileBuckets`) folds the per-bucket counts into
		// Tempo's `Log2QuantileWithBucket` per phi to produce the
		// per-(group, phi, anchor) wire value. Until that fold happens,
		// the row stream needs the bucket-edge `__bucket` column
		// surfaced as a label so the post-processor can pluck it out of
		// `chclient.Sample.Labels`; the post-processor strips the
		// `__bucket` label before emitting the final series.
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
		args = append(
			args,
			&chplan.LitString{V: tempoQuantileBucketLabel},
			&chplan.FuncCall{
				Name: "toString",
				Args: []chplan.Expr{&chplan.ColumnRef{Name: tempoQuantileBucketLabel}},
			},
		)
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
// `strconv.FormatFloat(v, 'f', -1, 64)` — what
// `postProcessQuantileBuckets` writes onto each per-phi series and what
// `fmt.Sprint(float64)` returns when the differ stringifies Tempo's
// `doubleValue` AnyValue. Stable for the phi values cerberus accepts
// (0 → "0", 0.5 → "0.5", 0.95 → "0.95", 1 → "1").
func formatPhi(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// postProcessQuantileBuckets folds the chsql matrix-path quantile row
// stream — one row per (group, anchor, bucket) tuple carrying a count
// and the synthetic `__bucket` label — into the per-(group, phi,
// anchor) wire shape Tempo's HistogramAggregator emits.
//
// For each (group_labels_minus_bucket, anchor) pair, the per-bucket
// counts feed `pkg/traceql.Log2QuantileWithBucket(phi, buckets)` from
// the cerberus-accessors Tempo fork — the same power-of-two
// bucket-interpolation routine `HistogramAggregator.Results` runs
// upstream. The post-processor then emits one `chclient.Sample` per
// (group_labels_minus_bucket, phi, anchor) with a synthetic
// `p="<phi>"` label.
//
// Why a post-processor rather than rendering the algorithm as CH SQL:
// the upstream algorithm is non-trivial (boundary handling for
// p100, exponential interpolation between bucket maxima, plus an
// edge-case sample-count rounding rule) and is being tweaked over
// time in Tempo upstream. Reusing the upstream function via the
// cerberus-accessors fork keeps the two backends bit-for-bit aligned
// even as Tempo refines the algorithm.
//
// Returns a fresh `[]chclient.Sample`; the input slice is read-only.
func postProcessQuantileBuckets(samples []chclient.Sample, m *chplan.MetricsAggregate) []chclient.Sample {
	if len(samples) == 0 || len(m.Quantiles) == 0 {
		return samples
	}

	// Group key: (canonical-label-set-minus-bucket, anchor-ts-unix-nanos)
	// → ordered list of (bucket-edge, count). The canonical key uses the
	// same separator format format.CanonicalKey produces so series keying
	// stays consistent with toMetricsSeries downstream.
	type bucketEntry struct {
		max   float64
		count int
	}
	type groupKey struct {
		labelsKey string
		anchorNS  int64
	}
	type groupState struct {
		labels  map[string]string
		anchor  time.Time
		buckets []bucketEntry
	}
	groups := map[groupKey]*groupState{}
	keyOrder := []groupKey{}

	for _, s := range samples {
		bucketStr, ok := s.Labels[tempoQuantileBucketLabel]
		if !ok {
			// Defensive: a row without the synthetic bucket label is
			// either an upstream chsql-emit bug or a leaked input from
			// a different code path. Skip the row rather than
			// double-counting under a zero bucket; the differ surfaces
			// the missing samples as a count gap, which is the
			// debuggable signal.
			continue
		}
		bucket, err := strconv.ParseFloat(bucketStr, 64)
		if err != nil {
			continue
		}
		// Strip the bucket label from the group-identifying set so two
		// rows that share (group, anchor) but differ in bucket coalesce
		// into the same group.
		groupLabels := make(map[string]string, len(s.Labels))
		for k, v := range s.Labels {
			if k == tempoQuantileBucketLabel {
				continue
			}
			groupLabels[k] = v
		}
		gk := groupKey{
			labelsKey: format.CanonicalKey(groupLabels),
			anchorNS:  s.Timestamp.UnixNano(),
		}
		g, ok := groups[gk]
		if !ok {
			g = &groupState{labels: groupLabels, anchor: s.Timestamp}
			groups[gk] = g
			keyOrder = append(keyOrder, gk)
		}
		g.buckets = append(g.buckets, bucketEntry{max: bucket, count: int(s.Value)})
	}

	// Deterministic emission order: sort keyOrder by (labelsKey,
	// anchorNS) so the post-processed stream is stable run-to-run.
	sort.Slice(keyOrder, func(i, j int) bool {
		if keyOrder[i].labelsKey != keyOrder[j].labelsKey {
			return keyOrder[i].labelsKey < keyOrder[j].labelsKey
		}
		return keyOrder[i].anchorNS < keyOrder[j].anchorNS
	})

	out := make([]chclient.Sample, 0, len(keyOrder)*len(m.Quantiles))
	for _, gk := range keyOrder {
		g := groups[gk]
		// Tempo's Log2QuantileWithBucket walks buckets in ascending
		// `Max` order — the upstream HistogramAggregator records into a
		// `Histogram.Buckets` slice in insertion order and the per-
		// quantile loop assumes ascending Max. Sort here so a row
		// stream that surfaces buckets in arbitrary CH-internal order
		// still feeds the algorithm correctly.
		sort.Slice(g.buckets, func(i, j int) bool {
			return g.buckets[i].max < g.buckets[j].max
		})
		buckets := make([]upstreamTraceql.HistogramBucket, len(g.buckets))
		for i, b := range g.buckets {
			buckets[i] = upstreamTraceql.HistogramBucket{Max: b.max, Count: b.count}
		}
		for _, phi := range m.Quantiles {
			value, _ := upstreamTraceql.Log2QuantileWithBucket(phi, buckets)
			labels := make(map[string]string, len(g.labels)+1)
			for k, v := range g.labels {
				labels[k] = v
			}
			labels[tempoQuantileLabel] = formatPhi(phi)
			out = append(out, chclient.Sample{
				Labels:    labels,
				Timestamp: g.anchor,
				Value:     value,
			})
		}
	}
	return out
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
	return toMetricsSeriesWithNames(samples, metricsLabelNames(m))
}

// toMetricsSeriesWithNames is the label-name-parameterised core of
// toMetricsSeries, shared with the histogram_over_time path (whose
// label-name derivation lives on chplan.MetricsHistogramOverTime
// rather than MetricsAggregate — see histogramLabelNames).
func toMetricsSeriesWithNames(samples []chclient.Sample, labelNames []string) []MetricsSeries {
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
