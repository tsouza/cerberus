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
// preserves the per-stage HTTP-status mapping the inlined handler
// used to return.
var (
	errParseStage = errors.New("traceql parse stage")
	errLowerStage = errors.New("traceql lower stage")
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
	}, nil
}

func (l *traceqlLang) ProjectSamples(plan chplan.Node, _ engine.Meta) chplan.Node {
	// Tempo's wrap-projection inspects the inner plan shape
	// (Scan / StructuralJoin / Aggregate) and materialises the
	// canonical (MetricName, Attributes, TimeUnix, Value) tuple — the
	// same logic the handler used to call inline. IsTraceByID rows
	// arrive as Filter(Scan) which falls into the canonical branch,
	// so the same helper covers both entry points.
	return wrapWithSampleProjection(plan, l.schema)
}
