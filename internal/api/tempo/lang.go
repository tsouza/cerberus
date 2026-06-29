package tempo

import (
	"context"
	"errors"
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/telemetry"
	traceql_lower "github.com/tsouza/cerberus/internal/traceql"
)

// errParseStage / errLowerStage are sentinel markers the Lang adapter
// uses so the handler-side error classifier can distinguish parser
// failures (HTTP 400) from lowering failures (HTTP 422). The engine
// wraps Lang.Parse errors with `engine: parse:` which collapses both
// into a single bucket; carrying the stage in the wrapped error chain
// preserves the per-stage HTTP-status mapping.
//
// ErrParseStage / ErrLowerStage are the exported aliases the sibling
// gRPC handler (internal/api/tempo/grpc) chains via errors.Is to map
// user-facing query errors onto codes.InvalidArgument (parser + lower)
// while keeping emit/execute on codes.Internal — sibling of
// classifySearchErr's HTTP-status mapping.
var (
	errParseStage = errors.New("traceql parse stage")
	errLowerStage = errors.New("traceql lower stage")

	// ErrParseStage / ErrLowerStage re-export the parse / lower stage
	// markers so external callers (e.g. the gRPC StreamingQuerier
	// surface) can errors.Is against them without depending on the
	// unexported sentinels.
	ErrParseStage = errParseStage
	ErrLowerStage = errLowerStage
)

// traceqlLang adapts the TraceQL head to engine.Lang. Parse runs the
// Tempo parser + lowering (each wrapped in its pipeline-stage span +
// stopwatch so the trace shape matches what the inlined handler emits
// today); ProjectSamples delegates to wrapWithSampleProjection so the
// canonical chclient.Sample row shape is materialised before the
// optimizer pass.
//
// Tempo's /traces/{id} short-circuit bypasses Parse entirely — the
// handler constructs the lookup plan via lowerTraceByID and calls
// engine.QueryPlan with Meta.IsTraceByID = true; that path still goes
// through ProjectSamples (which keeps the same wrap-projection rule).
//
// All trace responses are span-summary shaped (no metric matrix), so
// Meta.IsMetric stays false. ResponseShape is "tempo-trace" — purely
// informational since the handler picks its envelope by route, not by
// the meta flag.
type traceqlLang struct {
	schema schema.Traces
}

func (l *traceqlLang) Name() string { return "traceql" }

// SpansTable exposes the spans table so the engine threads it onto the emit
// context (chsql.WithSpansTable), letting RequireSpansScansBounded verify every
// otel_traces scan in the search / structural / nested-set / trace-by-id plans
// is resource-bounded.
func (l *traceqlLang) SpansTable() string { return l.schema.SpansTable }

func (l *traceqlLang) Parse(ctx context.Context, query string) (chplan.Node, engine.Meta, error) {
	// Parse pipeline-stage stopwatch — mirrors the inlined handler so
	// cerberus.queries.parse_duration_ms keeps its per-head label.
	parseT := telemetry.ObserveStage(telemetry.StageParse)
	expr, err := parseExpr(ctx, query)
	parseT.Done(ctx)
	if err != nil {
		return nil, engine.Meta{}, fmt.Errorf("%w: %w", errParseStage, err)
	}

	// Lower pipeline-stage stopwatch. traceql.Lower opens its own
	// cerbtrace.SpanLower span internally.
	lowerT := telemetry.ObserveStage(telemetry.StageLower)
	plan, err := traceql_lower.Lower(ctx, expr, l.schema)
	lowerT.Done(ctx)
	if err != nil {
		return nil, engine.Meta{}, fmt.Errorf("%w: %w", errLowerStage, err)
	}

	return plan, engine.Meta{
		IsMetric:      false,
		IsTraceByID:   false,
		ResponseShape: "tempo-trace",
		// Carry the /api/search trace limit so ProjectSamples can cap a
		// spanset-aggregation search to the newest N traces server-side
		// (the parity counterpart to plain search's SearchTraceLimit node).
		Extra: map[string]any{metaKeySearchTraceLimit: traceql_lower.SearchTraceLimit(ctx)},
	}, nil
}

func (l *traceqlLang) ProjectSamples(plan chplan.Node, meta engine.Meta) chplan.Node {
	// Tempo's wrap-projection inspects the inner plan shape
	// (Scan / StructuralJoin / Aggregate / Project) and materialises
	// the canonical (MetricName, Attributes, TimeUnix, Value) tuple.
	// IsTraceByID is threaded through so the Filter(Scan) branch can
	// enrich the Attributes map with the span-detail fields Grafana's
	// trace-view UI consumes (TraceId / SpanId / ParentSpanId /
	// SpanKind / StatusCode + SpanAttributes); the search-path
	// branches use the leaner canonical projection unchanged.
	return wrapWithSampleProjection(plan, l.schema, meta)
}
