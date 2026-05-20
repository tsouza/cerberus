// Package engine orchestrates the shared cerberus query pipeline:
//
//	parse → lower (inside Lang.Parse) → wrap-projection → optimize →
//	emit → execute
//
// The three per-API handlers (prom / loki / tempo) each used to inline
// this loop with copy-pasted telemetry plumbing. Engine extracts the
// loop so the handlers shrink to (a) HTTP routing, (b) per-language
// adapter wiring, and (c) the response-shape pivot.
//
// Per-language differences live behind the Lang interface: the parser
// type stays inside the adapter, lowering happens inside Lang.Parse,
// and the sample-row reshaping that used to live in each handler's
// wrapWithSampleProjection helper moves behind Lang.ProjectSamples.
package engine

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/tsouza/cerberus/internal/cerbtrace"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/telemetry"
)

// Canonical X-Cerberus-* response-header names the engine populates on
// every Result / CursorResult.Headers map. Handlers iterate the bag and
// stamp each (k, v) onto w.Header() before WriteHeader fires.
//
//   - HeaderStrategy   — execution-path label. "trace-by-id" for the
//     Tempo /traces/{id} short-circuit, "native" otherwise. Reserved
//     values for future strategies: "mv-substituted" (when the rule
//     fires) and "shadow-fallback" (oracle pivot).
//   - HeaderPlanNodes  — post-optimize plan node count (chplan tree
//     walked depth-first). Useful for debug dashboards + cost-shape
//     telemetry.
//   - HeaderCHMillis   — ClickHouse execute wall-clock in milliseconds.
//     Only stamped on the eager Result (not CursorResult — the cursor
//     keeps the connection open and the wall-clock isn't known until
//     the caller drains).
const (
	HeaderStrategy  = "X-Cerberus-Strategy"
	HeaderPlanNodes = "X-Cerberus-Plan-Nodes"
	HeaderCHMillis  = "X-Cerberus-CH-Millis"
)

// strategyFor picks the canonical Strategy label from meta. Centralised
// so Result and CursorResult agree on the value and so future strategies
// (mv-substituted, shadow-fallback) have one place to land.
func strategyFor(meta Meta) string {
	if meta.IsTraceByID {
		return "trace-by-id"
	}
	return "native"
}

// Querier is the subset of *chclient.Client Engine needs. Each handler
// already declares a (broader) Querier in its own package; Engine
// requires only the row-returning Query method since adapters lower to
// a plan that emits chclient.Sample rows. Streaming / strings /
// label-set callers go straight to their handler's Querier — the
// engine's surface is intentionally narrow.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) ([]chclient.Sample, error)
}

// CursorQuerier is the optional streaming sibling of Querier. When the
// engine's Client implements it, Engine.QueryCursor / QueryPlanCursor
// route through it for the prom /query_range matrix path; otherwise
// those entry points return an error. The split keeps the engine's
// minimum surface narrow (one method on Querier) while still allowing
// per-language adapters to opt into streaming on a per-call basis.
type CursorQuerier interface {
	QueryCursor(ctx context.Context, sql string, args ...any) (chclient.Cursor, error)
}

// Engine owns the shared dependencies (optimizer, ClickHouse client)
// and runs the pipeline loop. One Engine instance lives in each
// handler; the per-language differences are supplied by the Lang
// argument to each Query / QueryPlan call.
type Engine struct {
	// Optimizer rewrites the post-projection plan. Required.
	Optimizer *optimizer.Driver
	// Client executes the emitted ClickHouse SQL. Required.
	Client Querier
}

// Lang adapts a query-language head (PromQL / LogQL / TraceQL) to
// Engine. The parser type and the lowering call stay inside the
// adapter — Engine sees only a plan plus a Meta carrying the
// per-language flags downstream rendering needs.
type Lang interface {
	// Name identifies the QL for spans, progress-context keying, and
	// logs. Stable strings: "promql", "logql", "traceql".
	Name() string

	// Parse runs the upstream parser, lowers the AST into a chplan
	// tree, and returns the plan plus any per-language semantic flags
	// the engine cannot infer from the plan alone. Parse SHOULD open
	// the cerbtrace.SpanParse / SpanLower spans itself so trace
	// shapes match what the per-handler pipelines emit today.
	Parse(ctx context.Context, query string) (chplan.Node, Meta, error)

	// ProjectSamples wraps plan with whatever projection the adapter
	// needs so that the executed SQL emits rows in the canonical
	// chclient.Sample shape — (MetricName, Attributes, TimeUnix,
	// Value). Each existing handler hand-rolls this; the adapter
	// owns it after the port.
	ProjectSamples(plan chplan.Node, meta Meta) chplan.Node
}

// Meta carries per-query semantic flags the engine needs but cannot
// infer from the plan. Adapters populate it during Parse / when
// building a plan directly for QueryPlan.
type Meta struct {
	// IsMetric distinguishes matrix-shaped responses (PromQL always;
	// LogQL when the query is a metric query rather than a log
	// stream). The handler-side response pivot reads it.
	IsMetric bool
	// IsTraceByID flags the Tempo /traces/{id} short-circuit: the
	// plan is built without a parser and the optimizer is skipped
	// because the row-by-id fetch has no rewrites worth running.
	IsTraceByID bool
	// ResponseShape is the handler-side pivot key — one of
	// "prom-vector" / "prom-matrix" / "loki-streams" / "tempo-traces"
	// etc. The engine doesn't read it; it's threaded through Result
	// so the handler can switch on it without re-deriving.
	ResponseShape string
	// Extra is an adapter-specific bag so per-language knobs can ride
	// through Meta without bloating the type. Engine doesn't read it.
	Extra map[string]any
}

// Result is what Engine.Query / Engine.QueryPlan return on success.
type Result struct {
	// Samples is the row stream from ClickHouse decoded as
	// chclient.Sample. Handlers pivot it into the upstream wire
	// shape (Prom vector / matrix, Loki streams, Tempo trace
	// summaries).
	Samples []chclient.Sample
	// SQL is the parameterised ClickHouse SQL the engine emitted.
	// Surfaced for debug logging and the future
	// X-Cerberus-SQL-Length header.
	SQL string
	// Args is the positional argument list bound to SQL's `?`
	// placeholders.
	Args []any
	// Strategy is a free-form label for the execution path taken.
	// Empty today; reserved for future fallback-evaluator wiring.
	Strategy string
	// CHMillis is the wall-clock time spent in Client.Query, in
	// milliseconds. Replaces the per-handler chMillisCounter for
	// loki / tempo (prom keeps its middleware until the port).
	CHMillis int64
	// PlanNodeCount is the optimised plan's node count, surfaced
	// for the X-Cerberus-Plan-Nodes header.
	PlanNodeCount int
	// Headers is a bag of HTTP response headers the engine wants
	// the handler to stamp on the response — keeps the engine free
	// of http.ResponseWriter. Empty today; populated as the
	// per-head ports move the X-Cerberus-* headers off the
	// handlers.
	Headers map[string]string
	// Meta is the per-language Meta the adapter returned from
	// Parse (or that QueryPlan was called with), threaded through
	// so the handler-side response pivot can switch on it.
	Meta Meta
}

// Query runs the full pipeline for an upstream query string: it asks
// the Lang adapter to parse + lower, then delegates to QueryPlan.
//
// Returns a wrapped error from each pipeline stage so callers can
// errors.Is / errors.As to classify (parse → bad-data, emit →
// internal, execute → bad-gateway, etc.).
func (e *Engine) Query(ctx context.Context, lang Lang, query string) (Result, error) {
	if lang == nil {
		return Result{}, fmt.Errorf("engine: nil Lang")
	}
	plan, meta, err := lang.Parse(ctx, query)
	if err != nil {
		return Result{}, fmt.Errorf("engine: parse: %w", err)
	}
	return e.QueryPlan(ctx, lang, plan, meta)
}

// QueryPlan runs the post-parse half of the pipeline for a plan the
// adapter built directly. The Tempo /traces/{id} path is the canonical
// caller: it hand-rolls a plan instead of running a TraceQL parser, so
// Engine.Query is skipped and QueryPlan is entered with
// Meta.IsTraceByID = true.
//
// IsTraceByID also short-circuits the optimizer pass — the row-by-id
// fetch has no rewrites worth running and skipping the pass keeps the
// trace flat (no `optimize` span on a probe that ought to be one
// SELECT against the spans table).
func (e *Engine) QueryPlan(ctx context.Context, lang Lang, plan chplan.Node, meta Meta) (Result, error) {
	if lang == nil {
		return Result{}, fmt.Errorf("engine: nil Lang")
	}
	if plan == nil {
		return Result{}, fmt.Errorf("engine: nil plan")
	}

	// Wrap-projection. The adapter owns the per-language switch
	// (canonical vs. derived vs. structural-join shape); the engine
	// applies it unconditionally.
	plan = lang.ProjectSamples(plan, meta)

	// Optimize — unless the adapter signalled a fetch-by-id where
	// rewriting buys nothing. Each branch keeps the rest of the
	// pipeline identical.
	if !meta.IsTraceByID {
		optT := telemetry.ObserveStage(telemetry.StageOptimize)
		plan = e.Optimizer.Run(ctx, plan)
		optT.Done(ctx)
	}

	// Emit.
	emitT := telemetry.ObserveStage(telemetry.StageEmit)
	sql, args, err := chsql.Emit(ctx, plan)
	emitT.Done(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("engine: emit: %w", err)
	}

	// Execute. The progress-context key matches the upstream QL so
	// the cerberus.clickhouse.{rows,bytes}_read histograms keep
	// their per-head labels.
	execT := telemetry.ObserveStage(telemetry.StageExecute)
	start := time.Now()
	samples, err := e.Client.Query(chclient.WithProgressFor(ctx, lang.Name()), sql, args...)
	chMillis := time.Since(start).Milliseconds()
	execT.Done(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("engine: execute: %w", err)
	}

	nodes := cerbtrace.CountNodes(plan)
	strategy := strategyFor(meta)
	return Result{
		Samples:       samples,
		SQL:           sql,
		Args:          args,
		Strategy:      strategy,
		CHMillis:      chMillis,
		PlanNodeCount: nodes,
		Headers: map[string]string{
			HeaderStrategy:  strategy,
			HeaderPlanNodes: strconv.Itoa(nodes),
			HeaderCHMillis:  strconv.FormatInt(chMillis, 10),
		},
		Meta: meta,
	}, nil
}

// CursorResult is what Engine.QueryCursor / QueryPlanCursor return on
// success. Mirrors Result but carries a chclient.Cursor instead of a
// []chclient.Sample slice — the caller drives row consumption and is
// responsible for cursor.Close(). CHMillis is intentionally absent
// because the execute stage's wall-clock isn't known until the caller
// drains the cursor; the chclient.Cursor implementation closes its own
// `execute` span on Close, so timing instrumentation stays consistent.
type CursorResult struct {
	Cursor        chclient.Cursor
	SQL           string
	Args          []any
	Strategy      string
	PlanNodeCount int
	Headers       map[string]string
	Meta          Meta
}

// QueryCursor runs the full pipeline through emit, then opens a
// streaming cursor against the emitted SQL instead of draining rows
// into a slice. Caller MUST defer Cursor.Close() on the returned
// CursorResult on the happy path. The handler-side /query_range
// matrix pivot is the canonical consumer.
//
// Errors: returns ErrNoCursorQuerier when Engine.Client doesn't
// implement CursorQuerier (configuration mistake); otherwise the
// per-stage wrapped errors mirror Query.
func (e *Engine) QueryCursor(ctx context.Context, lang Lang, query string) (CursorResult, error) {
	if lang == nil {
		return CursorResult{}, fmt.Errorf("engine: nil Lang")
	}
	plan, meta, err := lang.Parse(ctx, query)
	if err != nil {
		return CursorResult{}, fmt.Errorf("engine: parse: %w", err)
	}
	return e.QueryPlanCursor(ctx, lang, plan, meta)
}

// QueryPlanCursor is the streaming sibling of QueryPlan. Same wrap +
// optimize + emit pipeline; opens a cursor instead of executing
// eagerly. The IsTraceByID short-circuit (skip optimizer) applies
// identically.
func (e *Engine) QueryPlanCursor(ctx context.Context, lang Lang, plan chplan.Node, meta Meta) (CursorResult, error) {
	if lang == nil {
		return CursorResult{}, fmt.Errorf("engine: nil Lang")
	}
	if plan == nil {
		return CursorResult{}, fmt.Errorf("engine: nil plan")
	}
	cq, ok := e.Client.(CursorQuerier)
	if !ok {
		return CursorResult{}, fmt.Errorf("engine: client does not implement CursorQuerier")
	}

	plan = lang.ProjectSamples(plan, meta)
	if !meta.IsTraceByID {
		optT := telemetry.ObserveStage(telemetry.StageOptimize)
		plan = e.Optimizer.Run(ctx, plan)
		optT.Done(ctx)
	}

	emitT := telemetry.ObserveStage(telemetry.StageEmit)
	sql, args, err := chsql.Emit(ctx, plan)
	emitT.Done(ctx)
	if err != nil {
		return CursorResult{}, fmt.Errorf("engine: emit: %w", err)
	}

	execT := telemetry.ObserveStage(telemetry.StageExecute)
	cursor, err := cq.QueryCursor(chclient.WithProgressFor(ctx, lang.Name()), sql, args...)
	execT.Done(ctx)
	if err != nil {
		return CursorResult{}, fmt.Errorf("engine: execute: %w", err)
	}

	nodes := cerbtrace.CountNodes(plan)
	strategy := strategyFor(meta)
	return CursorResult{
		Cursor:        cursor,
		SQL:           sql,
		Args:          args,
		Strategy:      strategy,
		PlanNodeCount: nodes,
		Headers: map[string]string{
			HeaderStrategy:  strategy,
			HeaderPlanNodes: strconv.Itoa(nodes),
			// CH-Millis is omitted on the cursor path — wall-clock for
			// the execute stage isn't known until the caller drains
			// the cursor + Close()s it. Streaming consumers that want
			// per-request CH timing should plug into the
			// cerberus.clickhouse.* histograms instead.
		},
		Meta: meta,
	}, nil
}
