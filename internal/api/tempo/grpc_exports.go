package tempo

import (
	"context"
	"fmt"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/telemetry"
	traceql_lower "github.com/tsouza/cerberus/internal/traceql"
)

// This file exports a narrow set of the Tempo HTTP-handler internals
// the sibling `internal/api/tempo/grpc` package needs to answer the
// StreamingQuerier metrics RPCs (MetricsQueryRange + MetricsQueryInstant)
// without duplicating the parse + lower + wrap + execute + post-process
// pipeline encoded in handleMetricsQueryRange / handleMetricsQueryInstant.
// The exports keep the HTTP surface as the source of truth for behaviour
// — the gRPC surface only diverges on the wire envelope (tempopb
// TimeSeries / InstantSeries vs the JSON MetricsSeries / MetricsInstantSeries
// shapes the HTTP handler returns). See .claude/plans/tempo-grpc-streaming-design.md
// §3 + §6 for the single-frame strategy this enables.

// ExecMetricsRangeResult is the post-execution intermediate shape the
// gRPC MetricsQueryRange RPC translates into tempopb.TimeSeries. Each
// MetricsSeries already carries the post-quantile-collapse,
// post-zero-fill, post-exemplar-attach view of the data; the gRPC
// handler only re-shapes labels + samples into the proto envelope.
type ExecMetricsRangeResult struct {
	// Series is the post-processed (quantile-collapse, zero-fill,
	// exemplars-attached) series list — same value the HTTP handler
	// passes to writeJSON.
	Series []MetricsSeries
}

// ExecMetricsRange runs the full metrics-pipeline evaluation that
// /api/metrics/query_range performs and returns the post-processed
// series list. Pipeline (mirrors handleMetricsQueryRange):
//
//  1. Parse the TraceQL metrics-pipeline expression — errors wrap
//     ErrParseStage so the gRPC layer maps to codes.InvalidArgument.
//  2. Lower to chplan — errors wrap ErrLowerStage (also
//     codes.InvalidArgument).
//  3. Unwrap the MetricsAggregate; reject non-metrics queries so the
//     gRPC layer surfaces InvalidArgument rather than a malformed plan.
//  4. Wrap with chplan.RangeWindow + sample projection so the inner
//     SQL emits the matrix-shape (group, anchor, value) tuples.
//  5. Run engine.QueryPlan — emit + execute against ClickHouse.
//  6. Post-process quantile buckets (no-op for non-quantile ops).
//  7. Pivot row stream → MetricsSeries, zero-fill the matrix grid
//     across [Start, End] for count/rate/quantile.
//  8. Best-effort exemplar enrichment — failure here keeps the
//     series envelope but emits a Logger.Warn (same policy as HTTP).
//
// Returns the engine error verbatim so the gRPC caller can errors.Is
// against ErrParseStage / ErrLowerStage / chclient.ErrCircuitOpen.
func (h *Handler) ExecMetricsRange(ctx context.Context, query string, start, end time.Time, step time.Duration) (ExecMetricsRangeResult, error) {
	if query == "" {
		return ExecMetricsRangeResult{}, fmt.Errorf("%w: missing query", errParseStage)
	}
	if start.IsZero() || end.IsZero() {
		return ExecMetricsRangeResult{}, fmt.Errorf("%w: 'start' and 'end' are required", errParseStage)
	}
	if step <= 0 {
		return ExecMetricsRangeResult{}, fmt.Errorf("%w: 'step' must be > 0", errParseStage)
	}

	parseT := telemetry.ObserveStage(telemetry.StageParse)
	expr, perr := parseExpr(ctx, query)
	parseT.Done(ctx)
	if perr != nil {
		return ExecMetricsRangeResult{}, fmt.Errorf("%w: %w", errParseStage, perr)
	}
	lowerT := telemetry.ObserveStage(telemetry.StageLower)
	plan, lerr := traceql_lower.Lower(ctx, expr, h.Schema)
	lowerT.Done(ctx)
	if lerr != nil {
		return ExecMetricsRangeResult{}, fmt.Errorf("%w: %w", errLowerStage, lerr)
	}

	metrics, ok := unwrapMetricsAggregate(plan)
	if !ok {
		return ExecMetricsRangeResult{}, fmt.Errorf("%w: query %q is not a TraceQL metrics-pipeline expression — MetricsQueryRange requires `| rate()`, `| count_over_time()`, `| *_over_time(...)` or `| quantile_over_time(...)`", errLowerStage, query)
	}

	rw := &chplan.RangeWindow{
		Input:           plan,
		Range:           step,
		Step:            step,
		Start:           start,
		End:             end,
		TimestampColumn: h.Schema.TimestampColumn,
	}
	wrapped := wrapMetricsForSample(rw, metrics)

	res, qerr := h.Engine.QueryPlan(ctx, metricsLang{}, wrapped, engine.Meta{
		IsMetric:      true,
		ResponseShape: "tempo-metrics-matrix",
	})
	if qerr != nil {
		return ExecMetricsRangeResult{}, qerr
	}
	h.Logger.Debug("cerberus tempo grpc metrics_query_range",
		"traceql", query, "start", start, "end", end, "step", step,
		"sql", res.SQL, "args", res.Args)

	samples := res.Samples
	if metrics.Op == chplan.MetricsOpQuantileOverTime {
		samples = postProcessQuantileBuckets(samples, metrics)
	}
	series := toMetricsSeries(samples, metrics)
	series = zeroFillMatrixGrid(series, metrics, start, end, step)

	// Best-effort exemplar enrichment. A failed emit / query keeps
	// the empty Exemplars slice already attached by toMetricsSeries
	// — the wire envelope stays well-formed.
	exSQL, exArgs, exErr := chsql.EmitMetricsExemplars(ctx, rw, metrics,
		h.Schema.TraceIDColumn, h.Schema.SpanIDColumn, 1)
	if exErr != nil {
		h.Logger.Warn("cerberus tempo grpc metrics_query_range exemplars emit failed",
			"err", exErr)
	} else {
		exSamples, qErr := h.Client.Query(ctx, exSQL, exArgs...)
		if qErr != nil {
			h.Logger.Warn("cerberus tempo grpc metrics_query_range exemplars query failed",
				"err", qErr)
		} else {
			attachExemplars(series, exSamples, metrics)
		}
	}

	return ExecMetricsRangeResult{Series: series}, nil
}

// ExecMetricsInstantResult is the post-execution intermediate the
// gRPC MetricsQueryInstant RPC translates into tempopb.InstantSeries.
// Mirrors ExecMetricsRangeResult but carries the single-bucket
// projection — one (labels, scalar value) tuple per series.
type ExecMetricsInstantResult struct {
	Series []MetricsInstantSeries
}

// ExecMetricsInstant runs the full instant evaluation that
// /api/metrics/query performs. Same pipeline shape as ExecMetricsRange
// but with step = end - start so the chplan.RangeWindow emits exactly
// one anchor per series — Tempo's translateQueryRangeToInstant
// semantics (one (labels, scalar) tuple per series at end-of-window).
//
// The instant path does NOT zero-fill (one anchor only) and does NOT
// attach exemplars (Tempo's instant envelope carries no Exemplars
// field — see tempopb.InstantSeries). Quantile post-processing still
// runs so the per-phi label shape is honoured.
func (h *Handler) ExecMetricsInstant(ctx context.Context, query string, start, end time.Time) (ExecMetricsInstantResult, error) {
	if query == "" {
		return ExecMetricsInstantResult{}, fmt.Errorf("%w: missing query", errParseStage)
	}
	if start.IsZero() || end.IsZero() {
		return ExecMetricsInstantResult{}, fmt.Errorf("%w: 'start' and 'end' are required", errParseStage)
	}
	step := end.Sub(start)
	if step <= 0 {
		return ExecMetricsInstantResult{}, fmt.Errorf("%w: 'end' must be after 'start'", errParseStage)
	}

	parseT := telemetry.ObserveStage(telemetry.StageParse)
	expr, perr := parseExpr(ctx, query)
	parseT.Done(ctx)
	if perr != nil {
		return ExecMetricsInstantResult{}, fmt.Errorf("%w: %w", errParseStage, perr)
	}
	lowerT := telemetry.ObserveStage(telemetry.StageLower)
	plan, lerr := traceql_lower.Lower(ctx, expr, h.Schema)
	lowerT.Done(ctx)
	if lerr != nil {
		return ExecMetricsInstantResult{}, fmt.Errorf("%w: %w", errLowerStage, lerr)
	}

	metrics, ok := unwrapMetricsAggregate(plan)
	if !ok {
		return ExecMetricsInstantResult{}, fmt.Errorf("%w: query %q is not a TraceQL metrics-pipeline expression — MetricsQueryInstant requires `| rate()`, `| count_over_time()`, `| *_over_time(...)` or `| quantile_over_time(...)`", errLowerStage, query)
	}

	rw := &chplan.RangeWindow{
		Input:           plan,
		Range:           step,
		Step:            step,
		Start:           start,
		End:             end,
		TimestampColumn: h.Schema.TimestampColumn,
	}
	wrapped := wrapMetricsForSample(rw, metrics)

	res, qerr := h.Engine.QueryPlan(ctx, metricsLang{}, wrapped, engine.Meta{
		IsMetric:      true,
		ResponseShape: "tempo-metrics-instant",
	})
	if qerr != nil {
		return ExecMetricsInstantResult{}, qerr
	}
	h.Logger.Debug("cerberus tempo grpc metrics_query_instant",
		"traceql", query, "start", start, "end", end, "step", step,
		"sql", res.SQL, "args", res.Args)

	samples := res.Samples
	if metrics.Op == chplan.MetricsOpQuantileOverTime {
		samples = postProcessQuantileBuckets(samples, metrics)
	}

	return ExecMetricsInstantResult{
		Series: toMetricsInstantSeries(samples, metrics),
	}, nil
}
