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

// This file exports a narrow, deliberately-small surface of the Tempo
// HTTP-handler internals the sibling `internal/api/tempo/grpc` package
// needs to answer the StreamingQuerier RPCs without duplicating the
// parse + lower + emit + execute + post-process pipelines the HTTP
// handler already encodes. Two surfaces live here:
//
//  - **Tag RPC helpers** — SearchTags / SearchTagsV2 / SearchTagValues /
//    SearchTagValuesV2 wrappers around the unexported scope-resolve +
//    DISTINCT-attribute-key lookup the HTTP handler performs.
//  - **Metrics RPC helpers** — ExecMetricsRange / ExecMetricsInstant
//    drive the full MetricsQueryRange / MetricsQueryInstant pipeline
//    end-to-end and return the post-quantile-collapse,
//    post-exemplar-attach series the HTTP handler returns. (Zero-fill
//    of empty matrix anchors lives in the SQL emitter — see
//    internal/chsql/range_window.go.)
//
// Keeping every export in a single file means PR 2 (gRPC Search),
// PR 3 (gRPC tags), and PR 4 (gRPC metrics) each touch one new gRPC
// file plus this one — the parallel rollout doesn't fan into the
// handler.go diff. See .claude/plans/tempo-grpc-streaming-design.md
// §3 + §6 for the single-frame strategy the metrics helpers enable.

// TagScope is the canonical scope keyword the V2 endpoint partitions
// results on; aliased here so the grpc handler doesn't need to import
// the package-private constants.
const (
	TagScopeNone      = tagScopeNone
	TagScopeResource  = tagScopeResource
	TagScopeSpan      = tagScopeSpan
	TagScopeIntrinsic = tagScopeIntrinsic
)

// IntrinsicTags returns a defensive copy of the static intrinsic-tag
// inventory the V2 endpoint emits in the `intrinsic` scope bucket.
// Mirrors upstream Tempo's `pkg/search.GetVirtualIntrinsicValues()`
// — see the in-package documentation on `intrinsicTags`.
func IntrinsicTags() []string {
	return append([]string(nil), intrinsicTags...)
}

// ParseTagScope normalises the SearchTagsRequest.Scope proto field
// against the upstream Tempo allowlist. Empty / "none" / "all" all
// collapse to TagScopeNone (every scope). Returns an error suitable
// for codes.InvalidArgument on any unrecognised value.
func ParseTagScope(raw string) (string, error) {
	return parseTagScope(raw)
}

// FetchTagKeys runs the DISTINCT mapKeys lookup for the given
// attribute map column (Handler.Schema.AttributesColumn for span
// keys, Handler.Schema.ResourceAttributesColumn for resource keys).
// The (sorted, de-duplicated) string slice is the same one the HTTP
// V1 / V2 envelopes are built from.
func (h *Handler) FetchTagKeys(ctx context.Context, mapCol string, start, end time.Time) ([]string, error) {
	return h.fetchTagKeys(ctx, mapCol, start, end)
}

// SortedUnique returns the de-duplicated, lexicographically sorted
// view of in (empty input → empty non-nil slice). Exported so the
// gRPC tag-list handlers can share the same final-shaping step the
// HTTP handler does.
func SortedUnique(in []string) []string {
	return sortedUnique(in)
}

// ResolvedTagName mirrors the unexported resolvedTagName layout. The
// grpc handler branches on IsIntrinsic + (IntrinsicCol /
// IntrinsicName) vs (Key, MapScope) to pick the right SQL.
type ResolvedTagName struct {
	IsIntrinsic   bool
	IntrinsicCol  string
	IntrinsicName string
	Key           string
	// MapScope encodes which attribute map(s) a dynamic-attribute
	// lookup should consult: 0 (any), 1 (resource), 2 (span). The
	// constants live as package-private values inside the tempo
	// package; the grpc handler treats them as opaque and just
	// passes the struct back into BuildTagValuesSQL.
	mapScope attrMapScope
}

// ResolveTagName parses the URL-path / RPC-field tag name as a
// TraceQL identifier and maps it onto the cerberus lookup pipeline.
// The returned error is non-nil only on a parser-rejected bare
// dotted form; callers that want V2-strict parsing reject when err
// != nil.
func (h *Handler) ResolveTagName(name string) (ResolvedTagName, error) {
	r, err := resolveTagName(name, h.Schema)
	return ResolvedTagName{
		IsIntrinsic:   r.IsIntrinsic,
		IntrinsicCol:  r.IntrinsicCol,
		IntrinsicName: r.IntrinsicName,
		Key:           r.Key,
		mapScope:      r.MapScope,
	}, err
}

// FetchTagValues runs the right CH lookup for a resolved tag — the
// intrinsic-column DISTINCT projection when r.IsIntrinsic, otherwise
// the dynamic-attribute arrayJoin form against the appropriate
// SpanAttributes / ResourceAttributes map(s). Returns the sorted,
// de-duplicated value list plus the Tempo V2 type label (see
// intrinsicType — "string" for dynamic attributes).
func (h *Handler) FetchTagValues(ctx context.Context, r ResolvedTagName, start, end time.Time) (values []string, valueType string, err error) {
	var (
		sqlStr string
		args   []any
	)
	if r.IsIntrinsic {
		sqlStr, args = buildIntrinsicValuesSQL(h.Schema, r.IntrinsicCol, start, end)
		valueType = intrinsicType(r.IntrinsicName)
	} else {
		sqlStr, args = buildAttributeValuesSQL(h.Schema, r.Key, r.mapScope, start, end)
		valueType = "string"
	}
	raw, err := h.Client.QueryStrings(ctx, sqlStr, args...)
	if err != nil {
		return nil, "", err
	}
	return sortedUnique(raw), valueType, nil
}

// ExecMetricsRangeResult is the post-execution intermediate shape the
// gRPC MetricsQueryRange RPC translates into tempopb.TimeSeries. Each
// MetricsSeries already carries the post-quantile-collapse,
// post-exemplar-attach view of the data; the gRPC handler only
// re-shapes labels + samples into the proto envelope. (Matrix-shape
// zero-fill lives in the SQL emitter — see
// internal/chsql/range_window.go.)
type ExecMetricsRangeResult struct {
	// Series is the post-processed (quantile-collapse,
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
//  7. Pivot row stream → MetricsSeries (matrix-grid zero-fill happens
//     SQL-side via the chsql countIf / conditional-bucket emit).
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

	// Second-stage peel + re-apply — same shape as
	// handleMetricsQueryRange: the RangeWindow wraps the aggregate, the
	// topk/bottomk/threshold wraps re-apply around the windowed result
	// partitioned by the matrix anchor column.
	stages, inner := peelMetricsSecondStages(plan)
	metrics, ok := unwrapMetricsAggregate(inner)
	if !ok {
		// `| compare({...}, topN)` routes through the BaselineAggregator
		// mirror — same post-processed series the HTTP handler returns.
		if cmp, cok := unwrapMetricsCompare(inner); cok {
			if len(stages) > 0 {
				return ExecMetricsRangeResult{}, fmt.Errorf("%w: traceql: second-stage %s over compare() is unsupported — compare() series carry the __meta_type split, not a scalar Value to rank or threshold", errLowerStage, stages[0].Op)
			}
			series, _, cerr := h.execCompareRange(ctx, query, plan, cmp, start, end, step)
			if cerr != nil {
				return ExecMetricsRangeResult{}, cerr
			}
			return ExecMetricsRangeResult{Series: series}, nil
		}
		return ExecMetricsRangeResult{}, fmt.Errorf("%w: query %q is not a TraceQL metrics-pipeline expression — MetricsQueryRange requires `| rate()`, `| count_over_time()`, `| *_over_time(...)` or `| quantile_over_time(...)`", errLowerStage, query)
	}
	if len(stages) > 0 && metrics.Op == chplan.MetricsOpQuantileOverTime {
		return ExecMetricsRangeResult{}, fmt.Errorf("%w: traceql: second-stage %s over quantile_over_time is unsupported — quantiles are computed from bucket rows after SQL execution", errLowerStage, stages[0].Op)
	}

	rw := &chplan.RangeWindow{
		Input:           inner,
		Range:           step,
		Step:            step,
		Start:           start,
		End:             end,
		TimestampColumn: h.Schema.TimestampColumn,
	}
	wrapped := wrapMetricsForSample(
		applyMetricsSecondStages(rw, stages, []string{chsql.RangeWindowAnchorAlias}),
		metrics,
	)

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
	// Matrix-shape zero-fill is owned by the SQL emitter (see
	// internal/chsql/range_window.go's countIf / conditional-bucket
	// emit). No Go-side post-pass.
	series := toMetricsSeries(samples, metrics)

	// Best-effort exemplar enrichment. A failed emit / query keeps
	// the empty Exemplars slice already attached by toMetricsSeries
	// — the wire envelope stays well-formed.
	exSQL, exArgs, exErr := chsql.EmitMetricsExemplars(ctx, rw, metrics,
		h.Schema.TraceIDColumn, h.Schema.SpanIDColumn, 1, h.Schema.SpansTable)
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
// The instant path produces one anchor only (no matrix grid) and does NOT
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

	// Second-stage peel + re-apply — instant path: no PartitionBy, a
	// single anchor means one global selection (mirrors
	// handleMetricsQueryInstant).
	stages, inner := peelMetricsSecondStages(plan)
	metrics, ok := unwrapMetricsAggregate(inner)
	if !ok {
		// compare() instant — single anchor at `end` (Start = End, same
		// rationale as handleMetricsQueryInstant's RangeWindow comment),
		// then the per-series sample collapses to InstantSeries.Value.
		if cmp, cok := unwrapMetricsCompare(inner); cok {
			if len(stages) > 0 {
				return ExecMetricsInstantResult{}, fmt.Errorf("%w: traceql: second-stage %s over compare() is unsupported — compare() series carry the __meta_type split, not a scalar Value to rank or threshold", errLowerStage, stages[0].Op)
			}
			series, _, cerr := h.execCompareRange(ctx, query, plan, cmp, end, end, step)
			if cerr != nil {
				return ExecMetricsInstantResult{}, cerr
			}
			return ExecMetricsInstantResult{Series: compareSeriesToInstant(series)}, nil
		}
		return ExecMetricsInstantResult{}, fmt.Errorf("%w: query %q is not a TraceQL metrics-pipeline expression — MetricsQueryInstant requires `| rate()`, `| count_over_time()`, `| *_over_time(...)` or `| quantile_over_time(...)`", errLowerStage, query)
	}
	if len(stages) > 0 && metrics.Op == chplan.MetricsOpQuantileOverTime {
		return ExecMetricsInstantResult{}, fmt.Errorf("%w: traceql: second-stage %s over quantile_over_time is unsupported — quantiles are computed from bucket rows after SQL execution", errLowerStage, stages[0].Op)
	}

	rw := &chplan.RangeWindow{
		Input:           inner,
		Range:           step,
		Step:            step,
		Start:           start,
		End:             end,
		TimestampColumn: h.Schema.TimestampColumn,
	}
	wrapped := wrapMetricsForSample(applyMetricsSecondStages(rw, stages, nil), metrics)

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
