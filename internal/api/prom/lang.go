package prom

import (
	"context"
	"time"

	promparser "github.com/prometheus/prometheus/promql/parser"
	"go.opentelemetry.io/otel/trace"

	"github.com/tsouza/cerberus/internal/cerbtrace"
	"github.com/tsouza/cerberus/internal/chclient"
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

	// Lowerers is the BOOT-WIRED polymorphic dispatch table for the
	// ClickHouse-native timeSeries*ToGrid family (rate / staleness). Threaded
	// from Handler.Lowerers (built once at boot in cmd/cerberus from the
	// resolved chopt.EnabledSet). Only the range path (executeRangeStreaming)
	// sets it; instant queries leave it zero. The zero value is the all-fan-out
	// default. The lowering dispatches through this table with NO per-query
	// feature-flag / version read.
	Lowerers promql.RangeLowerers
}

// Compile-time check that *lang satisfies engine.Lang.
var _ engine.Lang = (*lang)(nil)

// Name returns the stable QL identifier the engine uses for
// progress-context keying, telemetry labels, and span attributes.
func (l *lang) Name() string { return telemetry.QLPromQL }

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
	parseT := telemetry.ObserveStage(telemetry.StageParse, l.Name())
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

	lowerT := telemetry.ObserveStage(telemetry.StageLower, l.Name())
	// Collect the scalar-parameter domain checks lowering cannot settle
	// on its own. This is the query path — it can run the extra statement
	// each guard needs — so it asks for them; the plan-only callers of
	// promql.Lower leave the sink nil.
	var guards []promql.ScalarGuard
	plan, err := promql.LowerAtRangeOpts(ctx, expr, l.Schema, l.Start, l.End, l.Step,
		promql.LowerOpts{Lowerers: l.Lowerers, Guards: &guards})
	lowerT.Done(ctx)
	if err != nil {
		return nil, engine.Meta{}, &parseStageError{stage: "lower", err: err}
	}
	meta := engine.Meta{IsMetric: true, Guards: engineGuards(guards)}
	// Step is non-zero only on the /api/v1/query_range path (see the lang
	// doc), which is the only PromQL caller that reaches engine.QueryCursor —
	// executeInstant always goes through the eager engine.Query, which never
	// reads ResponseShape. Declaring the matrix shape here lets chclient's
	// columnar decode confirm caller intent as defense-in-depth (#1429).
	if l.Step > 0 {
		meta.ResponseShape = chclient.ResponseShapeMatrix
	}
	return plan, meta, nil
}

// engineGuards adapts the lowering's scalar guards to the engine's
// language-agnostic shape. The two types are deliberately separate: the
// engine knows only "run this plan, judge these values", while the
// promql side owns which PromQL domain each one enforces.
func engineGuards(guards []promql.ScalarGuard) []engine.Guard {
	if len(guards) == 0 {
		return nil
	}
	out := make([]engine.Guard, len(guards))
	for i, g := range guards {
		out[i] = engine.Guard{Name: g.Name, Plan: g.Plan, Check: g.Check}
	}
	return out
}

// ProjectSamples wraps plan with the Sample-shape Project via
// wrapWithSampleProjection. The per-handler derived-vs-canonical branch
// (RangeWindow / Aggregate / Scan / Filter shapes) lives in
// wrapWithSampleProjection — keeping the projection behind Lang keeps the
// engine generic without forcing prom's per-shape switch into the
// shared layer.
//
// This runs BEFORE the solver's Optimizer.Run / classify — every route
// (A and B) sees the identical wrapped plan here. The series-key ordering
// matrixFromCursor's streaming pivot wants is deliberately NOT added at
// this seam; see RangeSeriesOrder's doc comment for why it has to land
// later, at emit time, route-A only.
func (l *lang) ProjectSamples(plan chplan.Node, _ engine.Meta) chplan.Node {
	return wrapWithSampleProjection(plan, l.Schema)
}

// RangeSeriesOrder implements the engine's route-A-only, emit-time
// ordering hook (mirrors spansTabler / lateMatTabler — see
// internal/engine.emitForHead). On the /api/v1/query_range path (Step > 0)
// it wraps plan in a chplan.OrderBy on (series, timestamp) so the cursor
// matrixFromCursor drains hands it rows already grouped by series,
// ascending by timestamp within each group — the precondition its
// per-series streaming accumulation is BUILT for (see that function's doc
// comment for the full memory argument). Instant queries (Step == 0) never
// reach matrixFromCursor (they drain eagerly via engine.Query into
// matrixFromSamples), so there is nothing for the ordering to buy; this
// returns plan unchanged for those.
//
// Why emit-time and route-A-only, not part of ProjectSamples. Wrapping the
// plan the SOLVER classifies would put a chplan.OrderBy node on the spine
// every eligibility / slicing pass walks:
//
//   - internal/chplan.IsSliceInvariant is a closed, opt-in-only registry
//     (see its doc comment) — an unregistered node kind anywhere fails
//     classify()'s allSliceInvariant gate, so EVERY query_range plan would
//     have silently stopped being route-B-eligible, not just ones this
//     change was meant to touch.
//   - internal/chplan.ReanchorRange's per-shard re-anchor has no case for
//     OrderBy either; its default arm shares an unrecognised node
//     VERBATIM WITHOUT RECURSING into its input, so even if the
//     eligibility gate above were satisfied, every shard would silently
//     emit the SAME un-re-anchored (zeroed-grid) plan instead of its own
//     sub-window — a wrong-answer bug, not a refusal.
//
// Teaching both of those chokepoints to pass an OrderBy through is its own
// unit of work (chplan/sliceinvariant.go's own docstring requires a
// slice-invariance proof + the §Parity fixture family per node kind before
// registering one) — out of scope here. Applying the wrap at
// internal/engine.emitForHead instead sidesteps both: classify() and
// ReanchorRange run on the plan BEFORE this hook fires (emitForHead is
// route A's own emit call, downstream of the routing decision), and a
// route-B shard's SQL is emitted straight from chplan.Emit by
// engine.ChsqlEmitter, which never calls emitForHead at all — so a shard
// never sees this node either. matrixFromCursor does not depend on that
// for CORRECTNESS: it defensively re-sorts a series' accumulated points if
// a later row's timestamp ever regresses, so an unordered route-B shard
// (or any future route-A shape that skips this hook) still produces the
// right answer, just without the peak-memory win this ordering buys route
// A's common case.
func (l *lang) RangeSeriesOrder(plan chplan.Node) chplan.Node {
	if l.Step <= 0 {
		return plan
	}
	return &chplan.OrderBy{
		Input: plan,
		Keys: []chplan.OrderKey{
			{Expr: chplan.CanonicalAttributesExpr(&chplan.ColumnRef{Name: l.Schema.AttributesColumn})},
			{Expr: &chplan.ColumnRef{Name: l.Schema.TimestampColumn}},
		},
	}
}
