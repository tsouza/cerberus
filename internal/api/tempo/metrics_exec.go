package tempo

import (
	"context"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/telemetry"
)

// This file owns the ONE scalar-aggregate metrics execution path both
// Tempo entrypoints run: the HTTP handlers in metrics_query_range.go /
// metrics_query_instant.go, and the gRPC StreamingQuerier exports in
// grpc_exports.go. Everything downstream of "the query lowered to a
// chplan.MetricsAggregate" lives here — RangeWindow construction,
// second-stage re-application, engine execution, quantile-bucket
// collapse, series pivot, and (range only) exemplar enrichment.
//
// What stays in the entrypoints is exactly what is genuinely
// transport-specific: request-parameter decoding, the wording of the
// rejection messages (which name the endpoint the caller used), and
// response encoding.
//
// The split matters beyond deduplication. The two copies of this
// pipeline had drifted: the gRPC copy built its instant RangeWindow
// with Start=start rather than Start=end, fanning out a second anchor
// entirely before the requested window — see execMetricsInstant's
// window comment, and issue #1577. A single construction site makes
// that divergence unrepresentable rather than merely fixed.

const (
	// Response-shape tags threaded into engine.Meta. They pick the
	// corpus/telemetry bucket a query is recorded under, so they are
	// wire-visible in the observability output and must stay stable.
	metricsMatrixShape  = "tempo-metrics-matrix"
	metricsInstantShape = "tempo-metrics-instant"

	// exemplarsPerSeries is the per-series exemplar budget handed to
	// chsql.EmitMetricsExemplars. Tempo's matrix envelope carries at
	// most one exemplar per series per anchor.
	exemplarsPerSeries = 1
)

// execMetricsRange runs the matrix-shape pipeline for a lowered
// scalar-aggregate metrics query and returns the fully post-processed
// series list plus the engine headers the HTTP layer echoes.
//
// Range = Step → each bucket spans exactly one step width, matching
// Tempo's reference metrics semantics where `count_over_time` over a
// step-sized bucket is the per-step count. Matrix-shape zero-fill of
// empty anchors is the SQL emitter's concern, not this function's:
// internal/chsql/range_window.go swaps the outer WHERE for a
// `countIf(<window pred>)` on the count_over_time / rate paths and
// emits a phantom 0-bucket / 0-count row per (group, anchor) on the
// quantile path, so the row stream already carries one row per
// (group, anchor) tuple the inner fanout materialises.
func (h *Handler) execMetricsRange(
	ctx context.Context,
	q string,
	inner chplan.Node,
	stages []*chplan.MetricsSecondStage,
	metrics *chplan.MetricsAggregate,
	start, end time.Time,
	step time.Duration,
) ([]MetricsSeries, map[string]string, error) {
	rw := &chplan.RangeWindow{
		Input:           inner,
		Range:           step,
		Step:            step,
		Start:           start,
		End:             end,
		TimestampColumn: h.Schema.TimestampColumn,
	}
	// The second-stage wraps re-apply around the WINDOWED result,
	// partitioned by the matrix anchor column — that is the LIMIT-BY
	// partition key that gives Tempo's per-timestamp topk semantics.
	samples, headers, err := h.runMetricsWindow(ctx, q, rw, stages,
		[]string{chsql.RangeWindowAnchorAlias}, metrics, metricsMatrixShape)
	if err != nil {
		return nil, nil, err
	}

	series := toMetricsSeries(samples, metrics)
	h.attachMetricsExemplars(ctx, series, rw, metrics)
	return series, headers, nil
}

// execMetricsRangeHistogram runs the matrix-shape pipeline for a lowered
// `| histogram_over_time(<attr>)` plan and returns the per-(group,
// __bucket) series list plus the engine headers — the histogram sibling
// of execMetricsRange. Unlike the scalar-aggregate path, a histogram
// plan node carries its own group/value/bucket column aliases
// (chplan.MetricsHistogramOverTime) rather than the shared
// chplan.MetricsAggregate shape, so it gets its own wrap
// (wrapHistogramForSample) and label-name derivation
// (histogramLabelNames) — see metrics_query_range_histogram.go.
//
// Every Tempo entrypoint that can reach a histogram plan calls this one
// function through metricsPipelineRouter.Histogram, so
// `| histogram_over_time(...)` behaves identically on all of them. Before
// the shared router, three of the five never checked
// unwrapMetricsHistogram at all and rejected every histogram_over_time
// query as "not a TraceQL metrics-pipeline expression" — a real
// entrypoint-only divergence (#1453 caught the gRPC-range half of it,
// #1484 the remaining three).
//
// `shape` is the engine.Meta response-shape tag: the matrix tag for a
// range request, the instant tag for the single-anchor collapse, so the
// telemetry/corpus bucket names the request the caller actually made.
func (h *Handler) execMetricsRangeHistogram(
	ctx context.Context,
	q string,
	plan chplan.Node,
	hist *chplan.MetricsHistogramOverTime,
	start, end time.Time,
	step time.Duration,
	shape string,
) ([]MetricsSeries, map[string]string, error) {
	rw := &chplan.RangeWindow{
		Input:           plan,
		Range:           step,
		Step:            step,
		Start:           start,
		End:             end,
		TimestampColumn: h.Schema.TimestampColumn,
	}
	wrapped := wrapHistogramForSample(rw, hist)

	res, qerr := h.Engine.QueryPlan(ctx, metricsLang{spansTable: h.Schema.SpansTable}, wrapped, engine.Meta{
		IsMetric:      true,
		ResponseShape: shape,
	})
	if qerr != nil {
		return nil, nil, qerr
	}
	h.Logger.Debug("cerberus tempo metrics histogram exec",
		"traceql", telemetry.SanitizeForLog(q), "shape", shape,
		"start", start, "end", end, "step", step,
		"sql", res.SQL, "args", res.Args)

	normalizeHistogramBucketLabels(res.Samples)
	series := toMetricsSeriesWithNames(res.Samples, histogramLabelNames(hist))
	return series, res.Headers, nil
}

// execMetricsInstant runs the single-anchor pipeline for a lowered
// scalar-aggregate metrics query — Tempo's translateQueryRangeToInstant
// shape, one (labels, scalar) tuple per series at end-of-window.
//
// Instant carries no exemplars: tempopb.InstantSeries has no Exemplars
// field, so there is nothing to attach them to.
func (h *Handler) execMetricsInstant(
	ctx context.Context,
	q string,
	inner chplan.Node,
	stages []*chplan.MetricsSecondStage,
	metrics *chplan.MetricsAggregate,
	end time.Time,
	step time.Duration,
) ([]MetricsInstantSeries, map[string]string, error) {
	// Start == End on purpose: the matrix emitter generates
	// (End-Start)/Step + 1 anchors at `End - i*Step`, so Start==End
	// yields the single anchor at `end` whose right-closed window
	// (end - Range, end] IS the instant bucket Tempo's
	// IntervalMapperInstant evaluates. Passing Start=start here fans
	// out a SECOND anchor at `start` covering (start-step, start] —
	// entirely before the requested window — which evaluates to 0 and,
	// being the earliest sample, wins the first-sample-by-timestamp
	// pick in toMetricsInstantSeries: every series comes back 0
	// instead of its real value. The inner-scan time-bound pushdown
	// still prunes on (Start - Range, End] = (start, end], so
	// partition pruning is unaffected.
	rw := &chplan.RangeWindow{
		Input:           inner,
		Range:           step,
		Step:            step,
		Start:           end,
		End:             end,
		TimestampColumn: h.Schema.TimestampColumn,
	}
	// No PartitionBy on the instant path: a single anchor means a
	// single global selection (`ORDER BY Value LIMIT K`), matching
	// Tempo's translateQueryRangeToInstant collapse.
	samples, headers, err := h.runMetricsWindow(ctx, q, rw, stages, nil, metrics, metricsInstantShape)
	if err != nil {
		return nil, nil, err
	}
	return toMetricsInstantSeries(samples, metrics), headers, nil
}

// runMetricsWindow applies the second-stage chain + sample projection
// over rw, executes the plan, and returns the post-quantile-collapse
// row stream. The engine error is returned verbatim so callers can
// classify it through ClassifyErr rather than re-deriving a status.
func (h *Handler) runMetricsWindow(
	ctx context.Context,
	q string,
	rw *chplan.RangeWindow,
	stages []*chplan.MetricsSecondStage,
	partitionBy []string,
	metrics *chplan.MetricsAggregate,
	shape string,
) ([]chclient.Sample, map[string]string, error) {
	wrapped := wrapMetricsForSample(applyMetricsSecondStages(rw, stages, partitionBy), metrics)

	// metricsLang.ProjectSamples is a passthrough (the Sample
	// projection is already wrapped above) so engine.QueryPlan runs
	// optimize → emit → execute without re-wrapping the shape.
	res, qerr := h.Engine.QueryPlan(ctx, metricsLang{spansTable: h.Schema.SpansTable}, wrapped, engine.Meta{
		IsMetric:      true,
		ResponseShape: shape,
	})
	if qerr != nil {
		return nil, nil, qerr
	}
	h.Logger.Debug("cerberus tempo metrics exec",
		"traceql", telemetry.SanitizeForLog(q), "shape", shape,
		"start", rw.Start, "end", rw.End, "step", rw.Step,
		"sql", res.SQL, "args", res.Args)

	samples := res.Samples
	if metrics.Op == chplan.MetricsOpQuantileOverTime {
		// quantile_over_time emits (group, anchor, bucket, count)
		// tuples with synthetic 0-bucket / 0-count phantom rows per
		// (group, anchor) so empty anchors survive the GROUP BY.
		// Collapse them into the per-(group, phi, anchor) scalar wire
		// shape via Tempo's Log2QuantileWithBucket — empty anchors
		// resolve to 0 because the phantom rows are all the anchor
		// has, so its histogram is empty once they are discarded.
		// Tempo's reference engine routes the instant shape through
		// the same HistogramAggregator, so this runs on both paths.
		samples = postProcessQuantileBuckets(samples, metrics)
	}
	return samples, res.Headers, nil
}

// attachMetricsExemplars enriches series in place, best-effort: an emit
// or execute failure keeps the empty Exemplars slice toMetricsSeries
// already attached, so the wire envelope stays well-formed either way.
// Both failures warn AND increment telemetry.ExemplarFailuresTotal
// (#1460) — a log line alone left a systematic exemplar outage
// wire-indistinguishable from "no exemplars in this window", visible
// only by reading logs rather than on a dashboard. Shared by both the
// HTTP and gRPC Tempo metrics entrypoints via execMetricsRange, so one
// counter covers both transports.
func (h *Handler) attachMetricsExemplars(
	ctx context.Context,
	series []MetricsSeries,
	rw *chplan.RangeWindow,
	metrics *chplan.MetricsAggregate,
) {
	exSQL, exArgs, exErr := chsql.EmitMetricsExemplars(ctx, rw, metrics,
		h.Schema.TraceIDColumn, h.Schema.SpanIDColumn, exemplarsPerSeries, h.Schema.SpansTable)
	if exErr != nil {
		h.Logger.Warn("cerberus tempo metrics exemplars emit failed (matrix returns without exemplars)", "err", exErr)
		telemetry.RecordTempoExemplarFailure(ctx, telemetry.StageEmit)
		return
	}
	exSamples, qErr := h.Client.Query(ctx, exSQL, exArgs...)
	if qErr != nil {
		h.Logger.Warn("cerberus tempo metrics exemplars query failed (matrix returns without exemplars)", "err", qErr)
		telemetry.RecordTempoExemplarFailure(ctx, telemetry.StageExecute)
		return
	}
	attachExemplars(series, exSamples, metrics)
}

// metricsRangeRouter is the ONE range-shape evaluation both range
// entrypoints run: the HTTP /api/metrics/query_range handler and the gRPC
// MetricsQueryRange RPC. It implements metricsPipelineRouter, so the
// per-kind branch exists exactly once and both transports reach the same
// exec function for the same plan kind.
//
// The result lands on the struct rather than in the return value because
// metricsPipelineRouter's methods return only an error — the two transports
// encode the series differently (JSON envelope vs proto stream) but compute
// them identically, which is precisely the split this router preserves.
type metricsRangeRouter struct {
	h          *Handler
	q          string
	pipeline   metricsPipeline
	start, end time.Time
	step       time.Duration

	// Series / Headers carry the evaluated result out to the caller after
	// metricsPipeline.Route returns nil.
	Series  []MetricsSeries
	Headers map[string]string
}

// newMetricsRangeRouter binds a classified pipeline to the range window it
// is evaluated over.
func (h *Handler) newMetricsRangeRouter(
	q string, p metricsPipeline, start, end time.Time, step time.Duration,
) *metricsRangeRouter {
	return &metricsRangeRouter{h: h, q: q, pipeline: p, start: start, end: end, step: step}
}

func (r *metricsRangeRouter) Scalar(ctx context.Context, m *chplan.MetricsAggregate) error {
	series, headers, err := r.h.execMetricsRange(
		ctx, r.q, r.pipeline.Inner, r.pipeline.Stages, m, r.start, r.end, r.step,
	)
	return r.capture(series, headers, err)
}

func (r *metricsRangeRouter) Histogram(ctx context.Context, m *chplan.MetricsHistogramOverTime) error {
	series, headers, err := r.h.execMetricsRangeHistogram(
		ctx, r.q, r.pipeline.Inner, m, r.start, r.end, r.step, metricsMatrixShape,
	)
	return r.capture(series, headers, err)
}

func (r *metricsRangeRouter) Compare(ctx context.Context, m *chplan.MetricsCompare) error {
	series, headers, err := r.h.execCompareRange(
		ctx, r.q, r.pipeline.Inner, m, r.start, r.end, r.step, metricsMatrixShape,
	)
	return r.capture(series, headers, err)
}

func (r *metricsRangeRouter) capture(series []MetricsSeries, headers map[string]string, err error) error {
	if err != nil {
		return err
	}
	r.Series, r.Headers = series, headers
	return nil
}

// metricsInstantRouter is the instant-shape counterpart: the ONE evaluation
// both instant entrypoints run (HTTP /api/metrics/query and the gRPC
// MetricsQueryInstant RPC).
//
// Every kind evaluates over the single anchor at `end` — see
// execMetricsInstant's window comment for why Start == End is load-bearing
// rather than incidental — and collapses to one (labels, scalar) tuple per
// series, which is Tempo's translateQueryRangeToInstant semantics.
type metricsInstantRouter struct {
	h        *Handler
	q        string
	pipeline metricsPipeline
	end      time.Time
	step     time.Duration

	Series  []MetricsInstantSeries
	Headers map[string]string
}

// newMetricsInstantRouter binds a classified pipeline to the single-bucket
// window ending at `end` (step = end - start, set by the caller).
func (h *Handler) newMetricsInstantRouter(
	q string, p metricsPipeline, end time.Time, step time.Duration,
) *metricsInstantRouter {
	return &metricsInstantRouter{h: h, q: q, pipeline: p, end: end, step: step}
}

func (r *metricsInstantRouter) Scalar(ctx context.Context, m *chplan.MetricsAggregate) error {
	series, headers, err := r.h.execMetricsInstant(
		ctx, r.q, r.pipeline.Inner, r.pipeline.Stages, m, r.end, r.step,
	)
	if err != nil {
		return err
	}
	r.Series, r.Headers = series, headers
	return nil
}

func (r *metricsInstantRouter) Histogram(ctx context.Context, m *chplan.MetricsHistogramOverTime) error {
	series, headers, err := r.h.execMetricsRangeHistogram(
		ctx, r.q, r.pipeline.Inner, m, r.end, r.end, r.step, metricsInstantShape,
	)
	return r.collapse(series, headers, err)
}

func (r *metricsInstantRouter) Compare(ctx context.Context, m *chplan.MetricsCompare) error {
	series, headers, err := r.h.execCompareRange(
		ctx, r.q, r.pipeline.Inner, m, r.end, r.end, r.step, metricsInstantShape,
	)
	return r.collapse(series, headers, err)
}

// collapse folds a matrix-shape result (the histogram and compare paths
// have no instant-specific emitter — they run the same single-anchor window
// and collapse afterwards) into the instant envelope.
func (r *metricsInstantRouter) collapse(series []MetricsSeries, headers map[string]string, err error) error {
	if err != nil {
		return err
	}
	r.Series, r.Headers = metricsSeriesToInstant(series), headers
	return nil
}
