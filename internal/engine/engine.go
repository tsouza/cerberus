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
//
// Execution strategy: route A (the default for the overwhelming majority
// of traffic) emits one optimized plan into one ClickHouse statement and
// pushes all reduction into CH. The maintainer relaxed the old "one CH
// query per request — no scatter-gather" lock on 2026-06-12 for the narrow
// memory-unbounded anchor-fan-out class: the sharded-pushdown solver
// (internal/solver, docs/solver.md) re-anchors K copies of the
// same optimized plan onto disjoint anchor slices, emits each via chsql.Emit,
// and concatenates the streams — no new evaluator, no new SQL template, and
// the all-or-nothing wire contract preserved. The solver hooks the seam
// between Optimizer.Run and chsql.Emit (see QueryPlan / QueryPlanCursor) and
// is off by default. The relaxed invariant set lives in docs/performance.md.
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
	"github.com/tsouza/cerberus/internal/solver"
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

	// HeaderRouteDecision is the ADDITIVE shadow header carrying the
	// sharded-pushdown solver's routing classification. It is stamped only
	// when the Solver is wired AND it classified the plan (PromQL head); it
	// is OMITTED entirely otherwise, so a nil-Solver engine and a non-PromQL
	// head produce a byte-identical response to the pre-solver path.
	//
	// Value grammar: "<strategy>;reason=<reason>". On a non-route the
	// strategy is the route-A label "route-a" and reason is the solver's
	// Reason vocabulary (instant / below-threshold / not-sliceable / ...);
	// on a true route (phase-2, never under Mode=single) the strategy is the
	// decomposition name (sharded-timeslice) carrying ";k=<K>" before the
	// reason. The header is OBSERVATIONAL — it never changes the X-Cerberus-
	// Strategy value or the response body.
	HeaderRouteDecision = "X-Cerberus-Route-Decision"
)

// routeStrategyA is the shadow-header strategy token for a plan the solver
// classified but did NOT route — execution stays on route A.
const routeStrategyA = "route-a"

// ChsqlEmitter adapts the package-level chsql.Emit function to the
// solver.SQLEmitter interface so the Solver's Executor can lower each
// re-anchored shard plan to SQL without internal/solver importing
// internal/chsql (the import-cycle / dependency-cone rule). It is the thin
// wrapper main.go injects into solver.New: the engine package already
// imports chsql, so the adapter composes here cleanly. Stateless — the zero
// value is ready to use.
type ChsqlEmitter struct{}

// Emit lowers a re-anchored shard plan to parameterised ClickHouse SQL,
// delegating verbatim to chsql.Emit so a shard's SQL is byte-identical to
// what route A would emit for the same (sub-grid) plan.
func (ChsqlEmitter) Emit(ctx context.Context, plan chplan.Node) (string, []any, error) {
	return chsql.Emit(ctx, plan)
}

// routeDecisionValue composes the shadow-header value from a solver Decision.
// The grammar is an ordered, semicolon-delimited list so a future composite
// strategy (e.g. "sharded-timeslice;k=4;reason=routed") never loses a signal.
// routed=false yields "route-a;reason=<reason>"; routed=true yields
// "<strategy>;k=<K>;reason=<reason>".
func routeDecisionValue(d *solver.Decision, routed bool) string {
	if d == nil {
		return ""
	}
	if !routed {
		return routeStrategyA + ";reason=" + d.Reason
	}
	strategy := d.Strategy
	if strategy == "" {
		strategy = solver.StrategyShardedTimeslice
	}
	return strategy + ";k=" + strconv.Itoa(d.K) + ";reason=" + d.Reason
}

// strategyFor picks the canonical Strategy label from meta. Centralised
// so Result and CursorResult agree on the value and so future strategies
// (mv-substituted, shadow-fallback) have one place to land.
func strategyFor(meta Meta) string {
	if meta.IsTraceByID {
		return "trace-by-id"
	}
	return "native"
}

// execContext wraps the execute-stage ctx with any per-plan ClickHouse
// settings the emitted plan requires. Today the single rule is: when the
// optimized plan contains a chplan.RangeWindowNative node (the
// experimental timeSeriesRateToGrid lowering), mark the ctx with
// chclient.WithTSGridSetting so the chclient query path adds
// `allow_experimental_time_series_aggregate_functions=1` to THAT query's
// settings. Plans without the native node return ctx unchanged, so the
// experimental setting never rides an unrelated query (a plain unknown
// setting can itself error on a ClickHouse < 25.6).
//
// Applied identically on the eager (QueryPlan) and streaming
// (QueryPlanCursor) execute sites so the native path is gated the same
// way regardless of which one runs.
//
// On top of the always-on ts-grid gate, execContext layers the DARK,
// flag-gated settings rules from e.Settings (optimize_aggregation_in_order,
// log_comment shape id). Each rule is OFF unless its CERBERUS_* flag is set,
// so the default ctx is byte-identical to before these rules existed. Every
// rule writes through chclient.WithQuerySetting, so a plan that triggers more
// than one rule carries all of them on the one per-request settings map.
func (e *Engine) execContext(ctx context.Context, plan chplan.Node, language string) context.Context {
	if planHasTSGridNative(plan) {
		ctx = chclient.WithTSGridSetting(ctx)
	}
	ctx = e.Settings.apply(ctx, plan)
	// Fix the per-dispatch ClickHouse query_id ONCE here, on the ctx that
	// flows into the chclient dispatch, so the corpus reconciler records the
	// exact same id the chclient query path later stamps via WithQueryID. The
	// id is non-deterministic (a process-global counter keeps it unique per
	// dispatch, avoiding ClickHouse code 216), so it MUST be generated once and
	// shared rather than recomputed by each consumer.
	queryID, ctx := chclient.EnsureQueryID(ctx)
	e.observeQuery(queryID, plan, language)
	return ctx
}

// observeQuery feeds the corpus reconciler (when registered) the dispatch-seam
// tuple for plan: the per-dispatch CH query_id (fixed once in execContext via
// chclient.EnsureQueryID, the SAME id the chclient query path stamps via
// WithQueryID), the literal-free plan shape-id, the resolved enabled-opts, and
// the query language. It is a no-op when no observer is registered (the
// default) or when there is no valid trace id to join on, so the hot path is
// byte-unchanged unless the corpus is enabled.
func (e *Engine) observeQuery(queryID string, plan chplan.Node, language string) {
	if e.QueryObserver == nil || queryID == "" {
		return
	}
	e.QueryObserver.ObserveQuery(queryID, planShapeID(plan), e.Settings.enabledOpts(), language)
}

// planHasTSGridNative reports whether plan contains a node from the
// experimental timeSeries*ToGrid family anywhere in the tree — either a
// chplan.RangeWindowNative (timeSeriesRateToGrid for Func="rate",
// timeSeriesChangesToGrid for Func="changes", timeSeriesResetsToGrid for
// Func="resets") or a chplan.RangeWindowResample
// (timeSeriesResampleToGridWithStaleness). All share the
// allow_experimental_time_series_aggregate_functions gate, so the engine stamps
// the experimental setting on a query carrying ANY such node — the changes /
// resets matrix functions ride the RangeWindowNative match with no engine
// change.
func planHasTSGridNative(plan chplan.Node) bool {
	found := false
	chplan.Walk(plan, func(n chplan.Node) bool {
		switch n.(type) {
		case *chplan.RangeWindowNative, *chplan.RangeWindowResample:
			found = true
			return false // stop descending this branch
		}
		return true
	})
	return found
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
	// Solver is the OPTIONAL sharded-pushdown query orchestrator
	// (internal/solver, docs/solver.md). When nil the feature
	// is fully off and every existing call path is byte-unchanged — the
	// classification branch, the shadow header, and the Executor are all
	// dead code. When non-nil the engine classifies the optimized plan at
	// the seam between Optimizer.Run and chsql.Emit and stamps the
	// additive X-Cerberus-Route-Decision shadow header; under the phase-1
	// default (Mode=single) the Planner never routes, so EXECUTION STAYS ON
	// ROUTE A and the Executor is never invoked. The routed branch is wired
	// (so the phase-2 flip is a config change) but dormant at the default
	// config.
	Solver *solver.Solver

	// Settings carries the optional, DARK-by-default per-query ClickHouse
	// settings rules the engine evaluates against the post-optimize plan
	// (optimize_aggregation_in_order, log_comment shape id). The zero value
	// is "every rule off": every existing call path is byte-unchanged. Wired
	// from the CERBERUS_* flags in cmd/cerberus. See SettingsRules.
	Settings SettingsRules

	// QueryObserver is the OPTIONAL hook the async query_log performance-corpus
	// reconciler registers to learn, at the dispatch seam, the (query_id,
	// shape-id, enabled-opts, language) tuple of each query cerberus sends. It
	// is nil unless CERBERUS_CH_OPT_CORPUS_ENABLED is set, so the default path
	// is byte-unchanged. The engine calls ObserveQuery exactly where the
	// query_id (trace id on ctx) and shape-id (planShapeID) are already
	// computed; the reconciler later joins those ids back to system.query_log.
	QueryObserver QueryObserver
}

// QueryObserver is the narrow seam the corpus reconciler registers on the
// Engine. ObserveQuery is called once per dispatched query with the CH
// query_id (the join key into system.query_log), the literal-free plan
// shape-id, the resolved enabled-opts that rode the query, and the query
// language. It must be non-blocking and cheap (the reconciler ring-buffers).
type QueryObserver interface {
	ObserveQuery(queryID, shapeID string, opts []string, language string)
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
	// Inspected is the number of rows the engine pulled from ClickHouse
	// for this request — the size of the buffer a result-buffering
	// handler accumulates before it truncates / reshapes in Go. On the
	// eager path it equals len(Samples) (Client.Query drains the whole
	// result into the slice), the same quantity Tempo already reports as
	// SearchMetrics.InspectedTraces. It is the uniform per-response drain
	// counter the boundsdrain harness asserts stays O(output) as the
	// input axis scales; the streaming sibling lives on the cursor as
	// chclient.Cursor.Inspected (CursorResult carries the cursor, so the
	// caller reads the count off it after the drain).
	Inspected int64
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

	// Inflight bookkeeping. Deferred decrement balances the counter
	// across panics, early returns, and context cancellations. Sibling
	// instrumentation lives on QueryPlanCursor so the streaming path
	// gets the same gauge bump.
	defer telemetry.ObserveQueryInflight(ctx, lang.Name())()

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

	// Solver classification (DARK). When the Solver is wired it classifies
	// the optimized plan into a routing Decision between Optimizer.Run and
	// chsql.Emit. Under Mode=single routed is always false: the Decision is
	// read ONLY for the additive shadow header and EXECUTION CONTINUES ON
	// ROUTE A below, byte-unchanged. The routed branch (Mode=sharded /
	// test-only force) drains the Executor's composed cursor instead — it is
	// wired but dormant at the default config.
	decision, routed := e.classify(plan, lang)
	if routed {
		return e.executeRouted(ctx, lang, meta, plan, decision)
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
	samples, err := e.Client.Query(e.execContext(chclient.WithProgressFor(ctx, lang.Name()), plan, lang.Name()), sql, args...)
	chMillis := time.Since(start).Milliseconds()
	execT.Done(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("engine: execute: %w", err)
	}

	nodes := cerbtrace.CountNodes(plan)
	strategy := strategyFor(meta)
	headers := map[string]string{
		HeaderStrategy:  strategy,
		HeaderPlanNodes: strconv.Itoa(nodes),
		HeaderCHMillis:  strconv.FormatInt(chMillis, 10),
	}
	if v := routeDecisionValue(decision, false); v != "" {
		headers[HeaderRouteDecision] = v
	}
	return Result{
		Samples:       samples,
		SQL:           sql,
		Args:          args,
		Strategy:      strategy,
		CHMillis:      chMillis,
		PlanNodeCount: nodes,
		Headers:       headers,
		Meta:          meta,
		// Eager path: Client.Query drained the whole result into samples,
		// so the slice length IS the rows-from-CH drain count.
		Inspected: int64(len(samples)),
	}, nil
}

// classify runs the Solver over the optimized plan, gated on a non-nil
// Solver. It derives the solver.RequestMeta from the plan's OUTER grid
// carrier (solver.GridOf) plus the language name, then asks the Planner to
// classify. The returned Decision is nil (and routed false) when the Solver
// is off OR the head is not PromQL — both cases make the engine omit the
// shadow header and stay byte-identical to the pre-solver path.
func (e *Engine) classify(plan chplan.Node, lang Lang) (*solver.Decision, bool) {
	if e.Solver == nil {
		return nil, false
	}
	start, end, step := solver.GridOf(plan)
	rm := solver.RequestMeta{
		Lang:  lang.Name(),
		Start: start,
		End:   end,
		Step:  step,
	}
	return e.Solver.Classify(plan, rm)
}

// executeRouted runs the dormant route-B path: it dispatches the K shard
// cursors through the Solver's Executor and drains the composed cursor into
// the eager Result slice. It is NEVER reached under Mode=single (classify
// returns routed=false there); it is wired so the phase-2 flip is a config
// change. A nil Executor on a routed Decision is a wiring bug — fail closed
// to an error rather than panic.
func (e *Engine) executeRouted(
	ctx context.Context,
	lang Lang,
	meta Meta,
	plan chplan.Node,
	decision *solver.Decision,
) (Result, error) {
	if e.Solver == nil || e.Solver.Executor == nil {
		return Result{}, fmt.Errorf("engine: solver routed without an Executor")
	}
	execT := telemetry.ObserveStage(telemetry.StageExecute)
	start := time.Now()
	cursor, info, err := e.Solver.Executor.Execute(
		chclient.WithProgressFor(ctx, lang.Name()), lang.Name(), decision, chclient.SampleBudgetFromContext(ctx),
	)
	if err != nil {
		execT.Done(ctx)
		return Result{}, fmt.Errorf("engine: solver execute: %w", err)
	}
	defer func() { _ = cursor.Close() }()

	var samples []chclient.Sample
	for cursor.Next() {
		samples = append(samples, cursor.Sample())
	}
	if cerr := cursor.Err(); cerr != nil {
		execT.Done(ctx)
		return Result{}, fmt.Errorf("engine: solver drain: %w", cerr)
	}
	chMillis := time.Since(start).Milliseconds()
	execT.Done(ctx)

	nodes := cerbtrace.CountNodes(plan)
	strategy := strategyFor(meta)
	sql, args := routedSQLArgs(info)
	headers := map[string]string{
		HeaderStrategy:  strategy,
		HeaderPlanNodes: strconv.Itoa(nodes),
		HeaderCHMillis:  strconv.FormatInt(chMillis, 10),
	}
	if v := routeDecisionValue(decision, true); v != "" {
		headers[HeaderRouteDecision] = v
	}
	return Result{
		Samples:       samples,
		SQL:           sql,
		Args:          args,
		Strategy:      strategy,
		CHMillis:      chMillis,
		PlanNodeCount: nodes,
		Headers:       headers,
		Meta:          meta,
		// Routed eager path drained the composed shard cursor into samples,
		// so the slice length is the rows-from-CH drain count (equal to the
		// cursor's Inspected/emitted).
		Inspected: int64(len(samples)),
	}, nil
}

// routedSQLArgs surfaces the FIRST shard's SQL + args on the Result for
// debug logging parity with route A (which carries the single emitted SQL).
// The full per-shard list lives on the ExecInfo the tracing path reads; the
// eager Result keeps the single-string contract its callers expect.
func routedSQLArgs(info *solver.ExecInfo) (string, []any) {
	if info == nil || len(info.SQLs) == 0 {
		return "", nil
	}
	return info.SQLs[0], info.ShardArgs[0]
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

	// Inflight bookkeeping — symmetrical with QueryPlan so the gauge
	// covers both the eager and streaming pipelines. Cursor consumers
	// hold the gauge for the duration of the engine call only (until
	// QueryPlanCursor returns); the cursor's subsequent drain isn't
	// "in engine" anymore and shouldn't double-count.
	defer telemetry.ObserveQueryInflight(ctx, lang.Name())()

	plan = lang.ProjectSamples(plan, meta)
	if !meta.IsTraceByID {
		optT := telemetry.ObserveStage(telemetry.StageOptimize)
		plan = e.Optimizer.Run(ctx, plan)
		optT.Done(ctx)
	}

	// Solver classification (DARK) — symmetrical with QueryPlan. Under
	// Mode=single routed is always false and the streaming path below is
	// byte-unchanged; the Decision is read only for the additive shadow
	// header. The routed branch returns the Executor's composed cursor
	// instead — wired but dormant at the default config.
	decision, routed := e.classify(plan, lang)
	if routed {
		return e.executeRoutedCursor(ctx, lang, meta, plan, decision)
	}

	emitT := telemetry.ObserveStage(telemetry.StageEmit)
	sql, args, err := chsql.Emit(ctx, plan)
	emitT.Done(ctx)
	if err != nil {
		return CursorResult{}, fmt.Errorf("engine: emit: %w", err)
	}

	execT := telemetry.ObserveStage(telemetry.StageExecute)
	cursor, err := cq.QueryCursor(e.execContext(chclient.WithProgressFor(ctx, lang.Name()), plan, lang.Name()), sql, args...)
	execT.Done(ctx)
	if err != nil {
		return CursorResult{}, fmt.Errorf("engine: execute: %w", err)
	}

	nodes := cerbtrace.CountNodes(plan)
	strategy := strategyFor(meta)
	headers := map[string]string{
		HeaderStrategy:  strategy,
		HeaderPlanNodes: strconv.Itoa(nodes),
		// CH-Millis is omitted on the cursor path — wall-clock for
		// the execute stage isn't known until the caller drains
		// the cursor + Close()s it. Streaming consumers that want
		// per-request CH timing should plug into the
		// cerberus.clickhouse.* histograms instead.
	}
	if v := routeDecisionValue(decision, false); v != "" {
		headers[HeaderRouteDecision] = v
	}
	return CursorResult{
		Cursor:        cursor,
		SQL:           sql,
		Args:          args,
		Strategy:      strategy,
		PlanNodeCount: nodes,
		Headers:       headers,
		Meta:          meta,
	}, nil
}

// executeRoutedCursor is the streaming sibling of executeRouted: it
// dispatches the K shard cursors through the Solver's Executor and returns
// the composed cursor directly (the caller drives the drain + Close, exactly
// as route A's single cursor). NEVER reached under Mode=single; wired so the
// phase-2 flip is a config change.
func (e *Engine) executeRoutedCursor(
	ctx context.Context,
	lang Lang,
	meta Meta,
	plan chplan.Node,
	decision *solver.Decision,
) (CursorResult, error) {
	if e.Solver == nil || e.Solver.Executor == nil {
		return CursorResult{}, fmt.Errorf("engine: solver routed without an Executor")
	}
	execT := telemetry.ObserveStage(telemetry.StageExecute)
	cursor, info, err := e.Solver.Executor.Execute(
		chclient.WithProgressFor(ctx, lang.Name()), lang.Name(), decision, chclient.SampleBudgetFromContext(ctx),
	)
	execT.Done(ctx)
	if err != nil {
		return CursorResult{}, fmt.Errorf("engine: solver execute: %w", err)
	}

	nodes := cerbtrace.CountNodes(plan)
	strategy := strategyFor(meta)
	sql, args := routedSQLArgs(info)
	headers := map[string]string{
		HeaderStrategy:  strategy,
		HeaderPlanNodes: strconv.Itoa(nodes),
	}
	if v := routeDecisionValue(decision, true); v != "" {
		headers[HeaderRouteDecision] = v
	}
	return CursorResult{
		Cursor:        cursor,
		SQL:           sql,
		Args:          args,
		Strategy:      strategy,
		PlanNodeCount: nodes,
		Headers:       headers,
		Meta:          meta,
	}, nil
}
