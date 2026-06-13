package prom

import (
	"context"
	"time"

	promparser "github.com/prometheus/prometheus/promql/parser"
	"go.opentelemetry.io/otel/trace"

	"github.com/tsouza/cerberus/internal/cerbtrace"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/telemetry"
)

// lang is the PromQL adapter for engine.Engine. One value per request
// (so Start / End can carry the query's evaluation window through to
// promql.LowerAt for `@ start()` / `@ end()` resolution) — the heavier
// pieces (Parser, Schema) come from the long-lived Handler.
//
// The adapter is intentionally collocated with the Prom handler rather
// than living under `internal/promql/` because ProjectSamples wraps the
// plan into the per-handler Sample shape (an HTTP-layer concern); the
// promql package stays focused on parser → chplan lowering.
//
// Step is non-zero only on the /api/v1/query_range path: it threads
// the request's `step` parameter into the lowering ctx so the
// no-driving-vector shapes (`time()`, `vector(N)`, zero-arg date fns,
// `absent(...)`) materialise a step-grid source instead of OneRow.
// Instant queries leave Step at 0; the lowering then keeps the
// existing OneRow shape (one row at the eval anchor).
type lang struct {
	// Parser is borrowed by reference from the owning Handler
	// (handler.go executeInstant / executeRangeStreaming both pass
	// `Parser: h.parser`). It MUST be the same instance the handler's
	// scalar-fold short-circuit uses via parseExpr, otherwise the two
	// parse paths could drift on promparser.Options (e.g. only one
	// having EnableExperimentalFunctions=true would route experimental-
	// function queries inconsistently). The invariant is pinned by
	// TestHandlerLang_ParserOptionsAligned.
	Parser promparser.Parser
	Schema schema.Metrics
	Start  time.Time
	End    time.Time
	Step   time.Duration

	// ExperimentalTSGridRange opts the eligible rate query_range shape
	// into the ClickHouse-native timeSeriesRateToGrid lowering. Threaded
	// from Handler.ExperimentalTSGridRange (which reads
	// Config.ExperimentalTSGridRange). Only the range path
	// (executeRangeStreaming) sets it true; instant queries leave it
	// false. Default false → the default arrayJoin fan-out.
	ExperimentalTSGridRange bool
}

// Compile-time check that *lang satisfies engine.Lang.
var _ engine.Lang = (*lang)(nil)

// Name returns the stable QL identifier the engine uses for
// progress-context keying, telemetry labels, and span attributes.
func (l *lang) Name() string { return "promql" }

// parseStageError tags an error with the pipeline stage that produced
// it so the handler-side error mapper can preserve the pre-port
// errorType / HTTP-status classification (parse → 400 bad_data, lower
// → 422 execution).
type parseStageError struct {
	stage string // "parse" or "lower"
	err   error
}

func (e *parseStageError) Error() string { return e.err.Error() }
func (e *parseStageError) Unwrap() error { return e.err }

// Parse runs the upstream PromQL parser and lowers the AST into a
// chplan tree. It owns the `parse` + `lower` pipeline-stage spans so
// the cross-handler trace shape (parse → lower → optimize → emit →
// execute) matches what the old inline orchestration emitted.
//
// Errors are wrapped in parseStageError so the handler can map parse
// failures to 400 bad_data and lower failures to 422 execution, the
// classification the inline pipeline used pre-port.
//
// Meta.IsMetric is set to true unconditionally — every PromQL query
// produces matrix / vector / scalar output (the scalar-fold path is
// short-circuited in the handler before the engine is invoked, so a
// query reaching here is guaranteed to lower to chplan).
func (l *lang) Parse(ctx context.Context, query string) (chplan.Node, engine.Meta, error) {
	parseT := telemetry.ObserveStage(telemetry.StageParse)
	_, span := tracer.Start(ctx, cerbtrace.SpanParse,
		trace.WithAttributes(cerbtrace.ParseAttrs("promql", query)...))
	// Rewrite OTel-style dotted metric names to `{__name__="..."}` form
	// before handing to the parser. The handler-level parseExpr path
	// does the same rewrite for the scalar-fold short-circuit; doing it
	// here makes the engine path (Query / QueryCursor) symmetric, so a
	// query like `rate(http.server.request.duration[5m])` parses on
	// both paths instead of failing with `unexpected character: '.'`.
	expr, err := l.Parser.ParseExpr(normalizeDottedSelectors(query))
	if err != nil {
		span.RecordError(err)
		span.End()
		parseT.Done(ctx)
		return nil, engine.Meta{}, &parseStageError{stage: "parse", err: err}
	}
	span.End()
	parseT.Done(ctx)

	lowerT := telemetry.ObserveStage(telemetry.StageLower)
	plan, err := promql.LowerAtRangeOpts(ctx, expr, l.Schema, l.Start, l.End, l.Step,
		promql.LowerOpts{ExperimentalTSGridRange: l.ExperimentalTSGridRange})
	lowerT.Done(ctx)
	if err != nil {
		return nil, engine.Meta{}, &parseStageError{stage: "lower", err: err}
	}
	return plan, engine.Meta{IsMetric: true}, nil
}

// ProjectSamples wraps plan with the Sample-shape Project the Prom
// handler used to apply inline via wrapWithSampleProjection. The
// per-handler derived-vs-canonical branch (RangeWindow / Aggregate /
// Scan / Filter shapes) lives in wrapWithSampleProjection and is
// re-used verbatim — moving the projection behind Lang keeps the
// engine generic without forcing prom's per-shape switch into the
// shared layer.
func (l *lang) ProjectSamples(plan chplan.Node, _ engine.Meta) chplan.Node {
	return wrapWithSampleProjection(plan, l.Schema)
}
