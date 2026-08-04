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
// Both failures warn rather than degrade silently.
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
		return
	}
	exSamples, qErr := h.Client.Query(ctx, exSQL, exArgs...)
	if qErr != nil {
		h.Logger.Warn("cerberus tempo metrics exemplars query failed (matrix returns without exemplars)", "err", qErr)
		return
	}
	attachExemplars(series, exSamples, metrics)
}
