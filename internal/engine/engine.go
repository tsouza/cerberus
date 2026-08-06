// Package engine orchestrates the shared cerberus query pipeline:
//
//	parse → lower (inside Lang.Parse) → wrap-projection → optimize →
//	emit → execute
//
// Engine owns this loop so the per-API handlers (prom / loki / tempo)
// stay thin: each handler is just (a) HTTP routing, (b) per-language
// adapter wiring, and (c) the response-shape pivot.
//
// Per-language differences live behind the Lang interface: the parser
// type stays inside the adapter, lowering happens inside Lang.Parse,
// and the sample-row reshaping lives behind Lang.ProjectSamples.
//
// Execution strategy: route A (the default for the overwhelming majority
// of traffic) emits one optimized plan into one ClickHouse statement and
// pushes all reduction into CH. For the narrow memory-unbounded
// anchor-fan-out class, the sharded-pushdown solver (internal/solver,
// docs/solver.md) re-anchors K copies of the same optimized plan onto
// disjoint anchor slices, emits each via chsql.Emit, and concatenates the
// streams — no new evaluator, no new SQL template, and the all-or-nothing
// wire contract preserved. The solver hooks the seam between
// Optimizer.Run and chsql.Emit (see QueryPlan / QueryPlanCursor) and is
// off by default. The invariant set lives in docs/performance.md.
package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/tsouza/cerberus/internal/cerbtrace"
	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/routememo"
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
	// on a true route (never under Mode=single) the strategy is the
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

// spansTabler is implemented by a Lang whose plans scan a spans table (the
// Tempo head). The engine threads that table name onto the emit context so
// chsql.Emit's RequireSpansScansBounded chokepoint runs over the whole plan.
type spansTabler interface{ SpansTable() string }

// lateMatTabler is implemented by a Lang that knows its own resolved
// late-materialisation shape: the table it scans plus that table's wide
// columns and row-key columns, straight from the request's actually-resolved
// schema.Logs / schema.Traces value (which may differ from the OTel default
// table name under CERBERUS_SCHEMA_LOGS_TABLE / CERBERUS_SCHEMA_TRACES_TABLE
// or the equivalent config key). The engine threads that triple onto the
// emit context so chsql's late-materialisation gate (see
// internal/chsql/late_mat.go) matches even when the table has been renamed
// — see #1703.
type lateMatTabler interface {
	LateMatShape() (table string, wide, rowKey []string)
}

// emitForHead lowers plan to SQL, threading the spans-table scope and the
// late-materialisation shape onto the emit context for a head that exposes
// them. Heads without a spans table (PromQL) emit unchanged —
// RequireSpansScansBounded is a table-scoped no-op for them, and a Lang that
// doesn't implement lateMatTabler leaves chsql to fall back to its
// default-OTel-name-keyed static registry (pre-#1703 behaviour, unchanged).
func emitForHead(ctx context.Context, lang Lang, plan chplan.Node) (string, []any, error) {
	if st, ok := lang.(spansTabler); ok {
		ctx = chsql.WithSpansTable(ctx, st.SpansTable())
	}
	if lt, ok := lang.(lateMatTabler); ok {
		table, wide, rowKey := lt.LateMatShape()
		ctx = chsql.WithLateMatShape(ctx, table, wide, rowKey)
	}
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
// flag-gated settings rules from e.settings() (optimize_aggregation_in_order,
// log_comment shape id). Each rule is OFF unless its CERBERUS_* flag is set,
// so the default ctx is byte-identical to before these rules existed. Every
// rule writes through chclient.WithQuerySetting, so a plan that triggers more
// than one rule carries all of them on the one per-request settings map.
func (e *Engine) execContext(ctx context.Context, plan chplan.Node, language string, decision *solver.Decision) (context.Context, string) {
	if planHasTSGridNative(plan) {
		ctx = chclient.WithTSGridSetting(ctx)
	}
	// Always-on, result-equivalent: let any GROUP BY / sort spill to disk
	// rather than blow the per-query memory cap (MEMORY_LIMIT_EXCEEDED / 241).
	memCap := e.queryMemoryCap()
	ctx = applySpillSettings(ctx, memCap)
	// Compare()-only: cap read parallelism so the concurrent S3 read buffers
	// for the wide attribute Map columns can't blow the budget even after the
	// aggregation spills. Fires only on the metrics-compare plan shape.
	ctx = applyCompareMemoryBound(ctx, plan, memCap)
	ctx = e.settings().apply(ctx, plan)
	// Fix the per-dispatch ClickHouse query_id ONCE here, on the ctx that
	// flows into the chclient dispatch, so the corpus reconciler records the
	// exact same id the chclient query path later stamps via WithQueryID. The
	// id is non-deterministic (a process-global counter keeps it unique per
	// dispatch, avoiding ClickHouse code 216), so it MUST be generated once and
	// shared rather than recomputed by each consumer.
	queryID, ctx := chclient.EnsureQueryID(ctx)
	e.observeQuery(queryID, plan, language, decision)
	// Return the queryID so the caller can later stamp a cerberus-side terminal
	// outcome (e.g. the sample-budget 422 surfacing through this dispatch) onto
	// the same corpus record via observeOutcomeForErr.
	return ctx, queryID
}

// observeQuery feeds the corpus reconciler (when registered) the dispatch-seam
// tuple for plan: the per-dispatch CH query_id (fixed once in execContext via
// chclient.EnsureQueryID, the SAME id the chclient query path stamps via
// WithQueryID), the literal-free plan shape-id, the resolved enabled-opts, the
// query language, and the routing classifier read-out (decision). It is a no-op
// when no observer is registered (the default) or when there is no valid trace
// id to join on, so the hot path is byte-unchanged unless the corpus is
// enabled.
//
// decision is the route A/B classifier's output for this dispatch (always
// non-nil on the classified head, nil otherwise — see classify). Its RAW
// cost-grid scalars are passed through verbatim so the corpus can join each
// routing DECISION to its OBSERVED cost and replay the classifier offline. A
// nil decision means no classification ran (Solver off / unclassified head):
// routePresent is then false and the routing columns stay zero.
func (e *Engine) observeQuery(queryID string, plan chplan.Node, language string, decision *solver.Decision) {
	if e.QueryObserver == nil || queryID == "" {
		return
	}
	present, route, nAnchors, fanout, cumD, outerRange, step, kShards, reason := routeFeatures(language, decision)
	e.QueryObserver.ObserveQuery(
		queryID, planShapeID(plan), e.settings().enabledOpts(), language,
		present, route, nAnchors, fanout, cumD, outerRange, step, kShards, reason,
	)
}

// observeRoutedQuery is observeQuery's route-B counterpart: it records the
// routed dispatch as ONE corpus observation carrying the K per-shard query_ids
// the Executor minted, so a routed query lands in the corpus with its real
// route ("B"), k_shards and decision reason. Without it the corpus can only
// ever see route A — every routed path returns before reaching the route-A
// dispatch seam — and an empty route-B population reads as "route B never
// fires" when it really means "route B is unobservable".
//
// ONE observation per routed QUERY, not per shard: the corpus row is the unit
// of A/B comparison and carries no request identifier to group shards by after
// the fact, so a per-shard row would compare a fraction of route B against a
// whole route-A query. The observer folds the K query_log rows into that one
// row, and needs the Executor's EFFECTIVE concurrency to do it — the wall-clock
// and memory columns depend on how many of the K shards actually overlapped.
//
// The shape-id is the WHOLE plan's — the same plan route A would have run — so
// an A row and a B row for the same query shape share a shape_id and join
// directly in the calibration analysis.
//
// Like observeQuery it is a no-op without a registered observer and cannot fail
// the query: the seam returns nothing, is non-blocking, and drops under burst.
// A shard whose id is empty (the request carries no trace) has no join key and
// is left out; with none left there is nothing to record.
func (e *Engine) observeRoutedQuery(info *solver.ExecInfo, plan chplan.Node, language string, decision *solver.Decision) {
	if e.QueryObserver == nil || info == nil {
		return
	}
	ids := make([]string, 0, len(info.ShardQueryIDs))
	for _, id := range info.ShardQueryIDs {
		if id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return
	}
	present, route, nAnchors, fanout, cumD, outerRange, step, kShards, reason := routeFeatures(language, decision)
	e.QueryObserver.ObserveRoutedQuery(
		ids, info.Parallelism, planShapeID(plan), e.settings().enabledOpts(), language,
		present, route, nAnchors, fanout, cumD, outerRange, step, kShards, reason,
	)
}

// outcomeTokenForErr classifies a dispatch error into the corpus exit-status
// token for the CERBERUS-side terminal outcome it represents, or "" when the
// error is not a resource rejection the corpus records in-process (CH-side
// timeout is derived from query_log by the reconciler instead). It is the
// single error→outcome mapping shared by the eager and cursor paths and by the
// breaker-vs-drain split, so the classification lives in one place.
//
//   - sample-budget exceedance (chclient.ErrTooManySamples) → "sample_budget":
//     the CH query finished cleanly but cerberus rejected the drain. Stamped
//     onto the dispatched record (cost retained, exit overridden).
//   - circuit-breaker open (chclient.ErrCircuitOpen) → "breaker": no CH query
//     ran. Recorded as a decision-only rejection (no cost).
//   - memory-cap rejection (chclient.ErrMemoryLimitExceeded, CH code 241) →
//     "oom": cerberus's per-query max_memory_usage cap aborted the query ON
//     ClickHouse. A query_log row MAY land, but the corpus records the row
//     terminally at the engine site (zero cost) so it does not depend on the
//     join — without this the memory-cap rejection is invisible to the corpus,
//     which is the observability gap this seam closes.
func outcomeTokenForErr(err error) string {
	switch {
	case errors.Is(err, chclient.ErrTooManySamples):
		return optcorpusExitSampleBudget
	case errors.Is(err, chclient.ErrCircuitOpen):
		return optcorpusExitBreaker
	case errors.Is(err, chclient.ErrMemoryLimitExceeded):
		return optcorpusExitOOM
	default:
		return ""
	}
}

// Exit-status tokens the engine stamps through the QueryObserver seam. They
// duplicate optcorpus's ExitToken* constants by value (not by import) on
// purpose: the engine declares the QueryObserver interface in primitive terms
// so it never imports optcorpus (the nil-interface decoupling the rest of the
// seam relies on). The corpus parses these back; a drift between the two sets
// would simply be ignored by the corpus parser rather than mislabel a row.
const (
	optcorpusExitSampleBudget = "sample_budget"
	optcorpusExitBreaker      = "breaker"
	optcorpusExitRejected     = "rejected"
	optcorpusExitOOM          = "oom"
)

// observeOutcomeForErr maps a dispatch error to its cerberus-side outcome and
// records it on the corpus. A dispatched query whose drain hit the sample
// budget (queryID known) is stamped via ObserveOutcome so the reconciler keeps
// the joined CH cost but overrides exit_status. A breaker rejection (no CH
// query ran) is recorded as a decision-only rejection carrying the routing
// read-out. A memory-cap rejection (CH code 241) is recorded as a DISPATCHED
// rejection: it carries the dispatch query_id (so the reconciler forgets it and
// the query_log join cannot double-write the same abort) plus the routing
// read-out, with zero cost. Any other error is left to the query_log-derived
// path. No-op when no observer is registered (the default hot path is
// byte-unchanged).
func (e *Engine) observeOutcomeForErr(queryID, language string, plan chplan.Node, decision *solver.Decision, err error) {
	if e.QueryObserver == nil || err == nil {
		return
	}
	switch outcomeTokenForErr(err) {
	case optcorpusExitSampleBudget:
		if queryID != "" {
			e.QueryObserver.ObserveOutcome(queryID, optcorpusExitSampleBudget)
		}
	case optcorpusExitBreaker:
		e.observeRejection(language, plan, decision, optcorpusExitBreaker)
	case optcorpusExitOOM:
		e.observeDispatchedRejection(queryID, language, plan, decision, optcorpusExitOOM)
	}
}

// observeRejection records a decision-only corpus row for a request rejected
// before any CH dispatch (the breaker; the handler-side cap rejections call the
// observer directly via the engine's exported seam). It carries the routing
// read-out known at classify time and zero cost.
func (e *Engine) observeRejection(language string, plan chplan.Node, decision *solver.Decision, token string) {
	if e.QueryObserver == nil {
		return
	}
	present, route, nAnchors, fanout, cumD, outerRange, step, kShards, reason := routeFeatures(language, decision)
	e.QueryObserver.ObserveRejection(
		planShapeID(plan), e.settings().enabledOpts(), language, token,
		present, route, nAnchors, fanout, cumD, outerRange, step, kShards, reason,
	)
}

// observeDispatchedRejection records a terminal corpus row for a query that DID
// dispatch a CH query but was aborted by a resource cap the engine recognises at
// the error site — the per-query memory cap (CH code 241, token "oom"). Unlike
// observeRejection (pre-dispatch, no query_id), it passes the dispatch query_id
// so the reconciler drops it from the join index and the query_log reconcile
// cannot ALSO emit a row for the same abort. The cost columns stay zero (the
// rows/bytes/memory CH actually paid before aborting are unknowable here — kept
// unset honestly), and the routing read-out is carried as on the dispatch.
func (e *Engine) observeDispatchedRejection(queryID, language string, plan chplan.Node, decision *solver.Decision, token string) {
	if e.QueryObserver == nil {
		return
	}
	present, route, nAnchors, fanout, cumD, outerRange, step, kShards, reason := routeFeatures(language, decision)
	e.QueryObserver.ObserveDispatchedRejection(
		queryID, planShapeID(plan), e.settings().enabledOpts(), language, token,
		present, route, nAnchors, fanout, cumD, outerRange, step, kShards, reason,
	)
}

// routeSecond is the divisor that converts a time.Duration's nanoseconds to
// the whole seconds the corpus stores its grid columns in.
const routeSecond = int64(time.Second)

// corpusRouteA / corpusRouteB are the two values of the corpus row's `route`
// column: A is the single-query dispatch, B the sharded fan-out. The mining
// rules and the pinned corpus invariants key on these exact tokens.
const (
	corpusRouteA = "A"
	corpusRouteB = "B"

	// CorpusReasonNonPromQL is the corpus decision_reason for a row that has
	// no route decision because its head never enters the solver. It is corpus
	// TAXONOMY, not a solver Reason: LogQL and TraceQL keep an empty route,
	// zero solver geometry, route-A execution and NO shadow header — only the
	// reason column distinguishes them, and only so a mining rule can name the
	// unclassified population instead of inferring it from an absence.
	//
	// Exported because the column's writer-side vocabulary is a closed wire
	// contract that two other packages must agree with, and both derive it from
	// here rather than restating the literal (internal/routerrules'
	// decision_reason enum domain, and the corpus fixture invariants in
	// test/regression). The engine declares it because the engine is where the
	// "no decision, and here is why" call is made.
	CorpusReasonNonPromQL = "non-promql"
)

// routeFeatures unpacks a solver Decision into the primitive routing-feature
// scalars the QueryObserver seam takes. Durations (D / OuterRange / Step) are
// reported in whole seconds to match the UInt32 corpus columns. The Route enum
// is "B" on a true route (Strategy set), "A" otherwise — read from the recorded
// Strategy, never the Reason string, so a Reason added to the solver later
// cannot be misread as a route.
//
// A nil decision always reports present=false with zero geometry: nothing was
// classified, so there is no route and no grid to report. It splits on LANGUAGE
// only to name WHY, and the two arms are not interchangeable:
//
//   - a non-PromQL head can never be classified — solver.Classify is gated on
//     LangPromQL — so the absence is structural and permanent, and the corpus
//     records CorpusReasonNonPromQL to say so;
//   - a PromQL head with a nil decision merely had the Solver switched off, a
//     deployment setting that can change tomorrow. It records NO reason, because
//     claiming "non-promql" for the head that IS PromQL would mislabel the only
//     population that ever carries a route.
//
// Keying on the language rather than on "decision == nil" is therefore the whole
// correctness of the split: the default deployment runs the Solver off, so the
// nil-decision case is dominated by PromQL rows.
func routeFeatures(language string, d *solver.Decision) (present bool, route string, nAnchors, fanout, cumulativeD, outerRange, step uint32, kShards uint8, reason string) {
	if d == nil {
		if language != solver.LangPromQL {
			return false, "", 0, 0, 0, 0, 0, 0, CorpusReasonNonPromQL
		}
		return false, "", 0, 0, 0, 0, 0, 0, ""
	}
	route = corpusRouteA
	if d.Strategy != "" {
		route = corpusRouteB
	}
	return true,
		route,
		clampU32(int64(d.NAnchors)),
		clampU32(d.Fanout),
		clampU32(int64(d.CumulativeD) / routeSecond),
		clampU32(int64(d.OuterRange) / routeSecond),
		clampU32(int64(d.Step) / routeSecond),
		clampU8(int64(d.K)),
		d.Reason
}

// clampU32 narrows a non-negative int64 grid scalar to uint32, clamping a
// negative value to 0 and an over-range value to the uint32 max so the
// conversion is provably overflow-free (gosec G115). The classifier's grid
// scalars are always small non-negative values; the clamp documents that
// invariant rather than trusting it silently.
func clampU32(v int64) uint32 {
	if v < 0 {
		return 0
	}
	if v > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(v)
}

// clampU8 narrows the shard count to uint8 the same way; K is clamped to MaxK
// (<= 255) by the Planner, so this only restates the bound.
func clampU8(v int64) uint8 {
	if v < 0 {
		return 0
	}
	if v > math.MaxUint8 {
		return math.MaxUint8
	}
	return uint8(v)
}

// planHasTSGridNative reports whether plan contains a node from the
// experimental timeSeries*ToGrid family anywhere in the tree — either a
// chplan.RangeWindowNative (timeSeriesRateToGrid for Func="rate",
// timeSeriesChangesToGrid for Func="changes", timeSeriesResetsToGrid for
// Func="resets", timeSeriesDerivToGrid for Func="deriv",
// timeSeriesPredictLinearToGrid for Func="predict_linear") or a
// chplan.RangeWindowResample (timeSeriesResampleToGridWithStaleness). All share
// the allow_experimental_time_series_aggregate_functions gate, so the engine
// stamps the experimental setting on a query carrying ANY such node — the
// changes / resets / deriv / predict_linear matrix functions ride the
// RangeWindowNative match with no engine change.
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

// memoryCapQuerier is the optional surface a Client exposes when it can report
// its per-query memory cap (max_memory_usage, bytes). *chclient.Client
// implements it; test fakes that don't simply report no cap (0). The engine
// reads it so the compare() spill threshold sizes itself relative to the SAME
// cap the data-plane query path stamps, never a hard-coded value that could sit
// at or above a lowered cap and silently disable the spill.
type memoryCapQuerier interface {
	MaxQueryMemoryBytes() int64
}

// queryMemoryCap returns the engine Client's per-query memory cap in bytes, or
// 0 when the Client doesn't expose one. A 0 cap means "no max_memory_usage
// configured", which spillThreshold treats as "use the absolute no-cap spill
// threshold" rather than taking a fraction of a non-positive value.
func (e *Engine) queryMemoryCap() int64 {
	if mc, ok := e.Client.(memoryCapQuerier); ok {
		return mc.MaxQueryMemoryBytes()
	}
	return 0
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
	// additive X-Cerberus-Route-Decision shadow header; under the default
	// (Mode=single) the Planner never routes, so EXECUTION STAYS ON
	// ROUTE A and the Executor is never invoked. The routed branch is wired
	// (so flipping to routed mode is a config change) but dormant at the
	// default config.
	Solver *solver.Solver

	// Settings carries the optional, DARK-by-default per-query ClickHouse
	// settings rules the engine evaluates against the post-optimize plan
	// (optimize_aggregation_in_order, log_comment shape id). The zero value
	// is "every rule off": every existing call path is byte-unchanged. Wired
	// from the CERBERUS_* flags in cmd/cerberus. See SettingsRules.
	//
	// It is the value in force until SetSettings installs a replacement, and it
	// is never read directly on the query path — see settings.
	Settings SettingsRules

	// liveSettings holds the replacement rules installed by SetSettings, or nil
	// while the engine is still on the wired Settings value. Two of the rules
	// (OptimizeAggregationInOrder, ConditionCache) are gated on the ClickHouse
	// server's CAPABILITIES, which change under the running process when the
	// cluster is upgraded, so the value the query path reads has to be
	// swappable without a restart. It is a pointer swap rather than a field
	// mutation because the read happens concurrently on every dispatch.
	liveSettings atomic.Pointer[SettingsRules]

	// QueryObserver is the OPTIONAL hook the async query_log performance-corpus
	// reconciler registers to learn, at the dispatch seam, the (query_id,
	// shape-id, enabled-opts, language) tuple of each query cerberus sends. It
	// is nil unless CERBERUS_CH_OPT_CORPUS_ENABLED is set, so the default path
	// is byte-unchanged. The engine calls ObserveQuery exactly where the
	// query_id (trace id on ctx) and shape-id (planShapeID) are already
	// computed; the reconciler later joins those ids back to system.query_log.
	QueryObserver QueryObserver

	// MaxQuerySamples mirrors chclient.Config.MaxQuerySamples (wired from the
	// same CERBERUS_QUERY_MAX_SAMPLES in cmd/cerberus). The chclient enforces it
	// on the Go-side RESULT drain; the engine enforces it on the post-optimize
	// PLAN — rejecting a subquery whose anchor grid alone (RangeWindow.NumAnchors)
	// would exceed the budget, which the result drain never sees (resource-bound
	// audit GAP-2, see requireSubquerySampleBudget). 0 disables the gate.
	MaxQuerySamples int64

	// RouteMemo is the OPTIONAL failure-driven route memo (internal/routememo,
	// docs/solver.md §"Failure-driven route memo"). Nil unless wired from
	// cmd/cerberus, so the default path is byte-unchanged. When non-nil AND
	// Solver is non-nil AND the head is PromQL, the engine consults it before
	// dispatching route A (a live PreferB verdict routes B directly) and,
	// symmetrically, when a route-A dispatch fails with a resource-exhaustion
	// error (a retry on route B, at most once, see route_memo_wiring.go).
	RouteMemo *routememo.Memo
}

// SetSettings installs rules as the per-query settings the engine evaluates
// from the NEXT dispatch on, superseding the wired [Engine.Settings] value.
// It is how a re-resolved ClickHouse capability set reaches the query path:
// the capability-gated rules (aggregation-in-order, condition cache) answer a
// question about the SERVER, and a cluster upgraded under a running cerberus
// changes that answer without the process restarting.
//
// The swap is a single pointer store, so a dispatch already in flight keeps
// the rules it started with and every later one sees the new value whole —
// there is no window in which half the rules are old and half are new.
func (e *Engine) SetSettings(rules SettingsRules) {
	e.liveSettings.Store(&rules)
}

// settings returns the rules in force right now: the replacement installed by
// [Engine.SetSettings] when there is one, else the wired [Engine.Settings].
// Every query-path read goes through here, so a live swap needs no lock on the
// hot path and no second read site can go on consulting a stale value.
func (e *Engine) settings() SettingsRules {
	if live := e.liveSettings.Load(); live != nil {
		return *live
	}
	return e.Settings
}

// QueryObserver is the narrow seam the corpus reconciler registers on the
// Engine. ObserveQuery is called once per dispatched query with the CH
// query_id (the join key into system.query_log), the literal-free plan
// shape-id, the resolved enabled-opts that rode the query, the query
// language, and the routing classifier read-out for the dispatch. It must be
// non-blocking and cheap (the reconciler ring-buffers).
//
// The routing read-out is passed as primitive scalars rather than a shared
// struct so the engine does not import the corpus package (the concrete
// observer is *optcorpus.Reconciler; an engine→optcorpus import would couple
// the two and invite the nil-interface trap the QueryObserver==nil guard
// guards against). routePresent is false when no routing classification ran
// for the dispatch (Solver off / unclassified head), in which case route is ""
// and the scalar features are 0. This is a pure additive read-out: it joins
// each routing DECISION to its OBSERVED cost for the route A/B calibration
// corpus and changes no routing behavior.
type QueryObserver interface {
	ObserveQuery(
		queryID, shapeID string,
		opts []string,
		language string,
		routePresent bool,
		route string,
		nAnchors, fanout, cumulativeD, outerRange, step uint32,
		kShards uint8,
		decisionReason string,
	)

	// ObserveRoutedQuery is ObserveQuery's route-B sibling: it records ONE
	// dispatched query that fanned out into K physical ClickHouse queries, one
	// per shard, identified by shardQueryIDs (the join keys into
	// system.query_log). The observer folds the K query_log rows into a SINGLE
	// corpus row, because the corpus exists to compare route A against route B
	// per REQUEST — a row per shard would compare one shard against a whole
	// route-A query and make B look K times cheaper than it is.
	//
	// parallelism is the EFFECTIVE shard concurrency the Executor ran the
	// fan-out at, which is routinely below K (the configured P, further clamped
	// by the admission top-up and the shard gate). The observer needs it to fold
	// the wall-clock and memory columns: at concurrency P the K shards' peaks do
	// not all coexist and their durations do not all overlap, so a fold that
	// assumed full parallelism would understate route B's latency and overstate
	// its memory by roughly K/P.
	//
	// It is a distinct method rather than a widened ObserveQuery so route A's
	// seam — the overwhelmingly hot one — stays byte-identical. The remaining
	// parameters carry the same meaning as ObserveQuery's; route is "B" and
	// kShards is K on every call that reaches here. It must be non-blocking and
	// cheap.
	ObserveRoutedQuery(
		shardQueryIDs []string,
		parallelism int,
		shapeID string,
		opts []string,
		language string,
		routePresent bool,
		route string,
		nAnchors, fanout, cumulativeD, outerRange, step uint32,
		kShards uint8,
		decisionReason string,
	)

	// ObserveOutcome stamps a CERBERUS-side terminal outcome onto an
	// already-observed DISPATCHED query (matched by queryID) — currently the
	// query.maxSamples 422, which fires during the Go-side result drain AFTER
	// the CH query finished cleanly. statusToken is a stable exit-status token
	// (e.g. "sample_budget"); the observer ignores a token that is not a
	// cerberus-side outcome. It must be non-blocking and cheap.
	ObserveOutcome(queryID, statusToken string)

	// ObserveRejection records a decision-only corpus row for a request
	// cerberus rejected BEFORE any CH dispatch (breaker 503 / cap 400): there
	// is no query_id and no CH cost, but the routing read-out is known. The
	// scalars mirror ObserveQuery; statusToken is the cerberus-side outcome
	// ("breaker" / "rejected"). It must be non-blocking and cheap.
	ObserveRejection(
		shapeID string,
		opts []string,
		language string,
		statusToken string,
		routePresent bool,
		route string,
		nAnchors, fanout, cumulativeD, outerRange, step uint32,
		kShards uint8,
		decisionReason string,
	)

	// ObserveDispatchedRejection records a terminal corpus row for a query that
	// DID dispatch a CH query but was aborted by a resource cap recognised at the
	// engine error site — currently the per-query memory cap (CH code 241,
	// statusToken "oom"). It carries the dispatch queryID so the observer drops it
	// from the query_log join index (no double-count with the CH-derived row) and
	// records the row TERMINALLY with the routing read-out and zero cost (the cost
	// CH paid before aborting is unknowable here). The scalars mirror ObserveQuery.
	// It must be non-blocking and cheap.
	ObserveDispatchedRejection(
		queryID, shapeID string,
		opts []string,
		language string,
		statusToken string,
		routePresent bool,
		route string,
		nAnchors, fanout, cumulativeD, outerRange, step uint32,
		kShards uint8,
		decisionReason string,
	)
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
	// Guards are the value-domain checks the adapter's lowering could not
	// settle on its own — see [Guard]. The engine runs every one of them,
	// in order, BEFORE the main statement and fails the request with the
	// first violation. Nil for every language and query shape with
	// nothing to check, which is the overwhelming majority.
	Guards []Guard
	// Extra is an adapter-specific bag so per-language knobs can ride
	// through Meta without bloating the type. Engine doesn't read it.
	Extra map[string]any
}

// Guard is one value-domain check a language could not settle while
// lowering, because the value it judges only exists once ClickHouse has
// produced it.
//
// PromQL's evaluation-time parameter domains are the motivating case: a
// `topk(scalar(x), v)` K that turns out to be NaN, a
// `double_exponential_smoothing(v[5m], scalar(x), 0.3)` smoothing factor
// that turns out to be 1.5. Reference Prometheus evaluates the parameter
// expression first, inspects the resulting value series, and aborts the
// query with a message quoting the offending value. Neither half of that
// survives being folded into the main statement: ClickHouse's `throwIf`
// requires a constant message, so the value cannot be interpolated into
// it, and a predicate that silently substitutes a sentinel answers a
// query the reference rejects — a wrong answer, not a lenient one.
//
// So the check rides beside the plan as its own query. The engine runs
// Plan, hands the values it produced to Check in the order they came
// back, and fails the request with a [GuardError] wrapping Check's error
// — never reaching the main statement, exactly as reference never
// reaches its aggregation.
type Guard struct {
	// Name identifies the guarded quantity in the wrapped error and in
	// traces, e.g. "topk K".
	Name string
	// Plan projects the guarded quantity's value series as canonical
	// Samples: one row per evaluation step for a range query, one row for
	// an instant query.
	Plan chplan.Node
	// Check applies the domain to the values Plan produced and returns
	// the reference-shaped error for a violation, or nil to proceed.
	Check func(values []float64) error
}

// GuardError reports a [Guard] whose Check rejected the values its plan
// produced. It carries the guard's own message verbatim, because that
// message is the user-visible contract — it is what reference Prometheus
// would have answered — and handlers map it to the reference's
// evaluation-error status rather than to an upstream-failure 5xx.
type GuardError struct {
	// Guard is the failing guard's Name.
	Guard string
	// Err is the domain violation, in reference Prometheus's own wording.
	Err error
}

func (e *GuardError) Error() string { return e.Err.Error() }

func (e *GuardError) Unwrap() error { return e.Err }

// runGuards evaluates meta's guards, in order, and returns the first
// violation as a [GuardError].
//
// Each guard is a full query in its own right, so it goes through the
// same optimize → resource-bound → execContext → emit → execute pipeline
// the main statement does. That is not bookkeeping. A guard plan is
// lowered with the caller's own lowerers, so it can carry a
// RangeWindowNative that only runs with the experimental ts-grid setting
// execContext attaches; it scans the parameter's whole series, so it
// wants the same spill settings and the same subquery sample budget; and
// it is a real ClickHouse dispatch, so it belongs in the corpus the same
// way the main statement does.
//
// A guard plan that could not be emitted or executed is an internal
// failure, not a domain violation, and keeps its own error shape.
//
// The values are handed to Check in the order ClickHouse returned them,
// sorted by timestamp so a domain whose message names the FIRST
// offending step (double_exponential_smoothing's does) names the same
// step reference would.
func (e *Engine) runGuards(ctx context.Context, lang Lang, meta Meta) error {
	for _, g := range meta.Guards {
		plan := e.Optimizer.Run(ctx, g.Plan)
		if err := requireSubquerySampleBudget(plan, e.MaxQuerySamples); err != nil {
			return err
		}
		guardCtx, _ := e.execContext(ctx, plan, lang.Name(), nil)
		sql, args, err := emitForHead(guardCtx, lang, plan)
		if err != nil {
			return fmt.Errorf("engine: emit: guard %s: %w", g.Name, err)
		}
		samples, err := e.Client.Query(chclient.WithProgressFor(guardCtx, lang.Name()), sql, args...)
		if err != nil {
			return fmt.Errorf("engine: execute: guard %s: %w", g.Name, err)
		}
		sort.SliceStable(samples, func(i, j int) bool {
			return samples[i].Timestamp.Before(samples[j].Timestamp)
		})
		values := make([]float64, len(samples))
		for i, s := range samples {
			values[i] = s.Value
		}
		if err := g.Check(values); err != nil {
			return &GuardError{Guard: g.Name, Err: err}
		}
	}
	return nil
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

// DryRun is the offline result of DryRunSQL: the parameterised ClickHouse SQL
// the pipeline would execute for a query, plus the optimized plan and Meta,
// WITHOUT running it.
type DryRun struct {
	// SQL is the parameterised ClickHouse SQL that Query would execute. It is
	// empty when emit failed (see the error DryRunSQL returned alongside it).
	SQL string
	// Args is the positional bind list for SQL's `?` placeholders.
	Args []any
	// Plan is the optimized plan the SQL was emitted from. It is populated even
	// when emit fails, so a caller can still inspect WHY (e.g. an unbounded
	// scan that the emit-time chokepoint rejected).
	Plan chplan.Node
	// Meta is the per-language semantic flags the adapter returned from Parse.
	Meta Meta
}

// DryRunSQL runs the read-side of the pipeline — parse, project, optimize,
// emit — and returns the ClickHouse SQL WITHOUT executing it. It reuses the
// exact stages QueryPlan runs before Execute, so the SQL is byte-identical to
// what Query would send to ClickHouse. The optimizer pass, the subquery
// sample-budget gate, and the emit-time chokepoints all fire here just as they
// do on a live run, so an unbounded query returns their error rather than a
// misleadingly-clean preview.
//
// It exists so offline tooling (the migration preview) can show operators the
// SQL cerberus will run for a query without a ClickHouse connection — the
// Engine's Client is never touched. Solver routing is intentionally skipped:
// the dry run reports the standard emit, which is what executes under the
// default single-route mode.
func (e *Engine) DryRunSQL(ctx context.Context, lang Lang, query string) (DryRun, error) {
	if lang == nil {
		return DryRun{}, fmt.Errorf("engine: nil Lang")
	}
	plan, meta, err := lang.Parse(ctx, query)
	if err != nil {
		return DryRun{}, fmt.Errorf("engine: parse: %w", err)
	}
	dr := DryRun{Meta: meta}

	plan = lang.ProjectSamples(plan, meta)
	if !meta.IsTraceByID {
		plan = e.Optimizer.Run(ctx, plan)
	}
	// Record the optimized plan before the gate + emit, which may return early:
	// the plan is useful for offline inspection even when emit rejects it.
	dr.Plan = plan

	if err := requireSubquerySampleBudget(plan, e.MaxQuerySamples); err != nil {
		return dr, err
	}

	sql, args, err := emitForHead(ctx, lang, plan)
	if err != nil {
		return dr, fmt.Errorf("engine: emit: %w", err)
	}
	dr.SQL, dr.Args = sql, args
	return dr, nil
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

	// Scalar-parameter value domains, before anything else touches the
	// main statement: a guard violation rejects the whole query, so
	// running the query first and discarding it would be wasted work and
	// would report the wrong thing on the way out.
	if err := e.runGuards(ctx, lang, meta); err != nil {
		return Result{}, err
	}

	// Wrap-projection. The adapter owns the per-language switch
	// (canonical vs. derived vs. structural-join shape); the engine
	// applies it unconditionally.
	plan = lang.ProjectSamples(plan, meta)

	// Optimize — unless the adapter signalled a fetch-by-id where
	// rewriting buys nothing. Each branch keeps the rest of the
	// pipeline identical.
	if !meta.IsTraceByID {
		optT := telemetry.ObserveStage(telemetry.StageOptimize, lang.Name())
		plan = e.Optimizer.Run(ctx, plan)
		optT.Done(ctx)
	}

	// Resource-bound gate: reject a subquery whose anchor grid alone busts the
	// per-query sample budget before any SQL is sent (GAP-2). Honest 422, never
	// an OOM.
	if err := requireSubquerySampleBudget(plan, e.MaxQuerySamples); err != nil {
		return Result{}, err
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
	emitT := telemetry.ObserveStage(telemetry.StageEmit, lang.Name())
	sql, args, err := emitForHead(ctx, lang, plan)
	emitT.Done(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("engine: emit: %w", err)
	}

	// Execute. The progress-context key matches the upstream QL so
	// the cerberus.clickhouse.{rows,bytes}_read histograms keep
	// their per-head labels.
	execT := telemetry.ObserveStage(telemetry.StageExecute, lang.Name())
	start := time.Now()
	execCtx, queryID := e.execContext(chclient.WithProgressFor(ctx, lang.Name()), plan, lang.Name(), decision)
	samples, err := e.Client.Query(execCtx, sql, args...)
	chMillis := time.Since(start).Milliseconds()
	execT.Done(ctx)
	if err != nil {
		// The eager path drains the whole result inside Client.Query, so a
		// sample-budget 422 (or a breaker fast-fail) surfaces here. Stamp the
		// cerberus-side outcome onto the corpus before wrapping the error.
		e.observeOutcomeForErr(queryID, lang.Name(), plan, decision, err)
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
// returns routed=false there); it is wired so flipping to routed mode is a
// config change. A nil Executor on a routed Decision is a wiring bug — fail closed
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
	execT := telemetry.ObserveStage(telemetry.StageExecute, lang.Name())
	start := time.Now()
	cursor, info, err := e.Solver.Executor.Execute(
		chclient.WithProgressFor(ctx, lang.Name()), lang.Name(), decision, chclient.SampleBudgetFromContext(ctx),
	)
	if err != nil {
		execT.Done(ctx)
		return Result{}, fmt.Errorf("engine: solver execute: %w", err)
	}
	// Record the fan-out at its dispatch seam, symmetrically with route A's
	// execContext: the shards are running, so the corpus row is registered
	// before the drain rather than after (a drain that never finishes would
	// otherwise lose the observation entirely).
	e.observeRoutedQuery(info, plan, lang.Name(), decision)
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
	// QueryID is this cursor's corpus-record key. It is the dispatch query_id
	// for route A and one shard query_id for route B; the reconciler indexes all
	// of a routed query's shard ids onto its one logical record. The handler
	// passes it to ObserveDrainOutcome so a sample-budget 422 surfacing during
	// the drain is stamped onto that record. Empty when the dispatch carried no
	// trace id (un-instrumented caller) or when the corpus is off.
	QueryID string

	// Retry is the failure-driven route memo's A->B retry hook (docs
	// §"Failure-driven route memo"). Nil unless Engine.RouteMemo is wired AND
	// this CursorResult's dispatch went through route A AND was structurally
	// eligible for route B — a nil Retry is the ordinary "this mechanism does
	// not apply here" case, not an error.
	//
	// The DRAIN happens in the caller (the handler owns cursor.Next() /
	// cursor.Err()), so a resource-exhaustion failure surfacing mid-drain is
	// necessarily discovered outside this package — Retry is how the engine
	// still gets to classify it and decide whether to dispatch route B,
	// without the handler needing to know anything about routes, keys, or
	// the memo itself. On drainErr classifying as a genuine resource
	// failure AND every other gate passing (eligibility, freshness, breaker,
	// corroboration), Retry dispatches route B and returns a FRESH
	// CursorResult (with its own nil Retry — at most one retry per request,
	// no loop) for the caller to drain instead; retried=false means "keep
	// using drainErr", the safe default.
	//
	// The caller MUST release ownership of the ORIGINAL cursor (Close it)
	// before calling Retry, and must drop any partial result it accumulated
	// from draining the original cursor before beginning to drain the new
	// one — Retry itself does neither, since both are properties of
	// whatever the caller was accumulating into, not of the cursor.
	Retry func(ctx context.Context, drainErr error) (CursorResult, bool)

	// ObserveDrainOutcome is the failure-driven route memo's UNCONDITIONAL
	// bookkeeping hook — distinct from Retry, which fires ONLY on a drain
	// failure and offers an alternate cursor. This Cursor came from route
	// B's very first probe for its Key (Retry's own A->B retry path): a
	// CLEAN drain is what creates the memo's positive verdict in the first
	// place (there is no pre-existing verdict here to fall back on if the
	// caller skips this), so the caller MUST call this exactly once with the
	// drain error (nil on a clean finish) whenever it is non-nil — silently
	// skipping it leaves the Key permanently unlearned no matter how many
	// times route A goes on failing it. Nil whenever it does not apply
	// (RouteMemo unset, or this Cursor did not come from a first-time
	// probe).
	ObserveDrainOutcome func(drainErr error)
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

	// Inflight bookkeeping — symmetrical with QueryPlan so the gauge
	// covers both the eager and streaming pipelines. Cursor consumers
	// hold the gauge for the duration of the engine call only (until
	// QueryPlanCursor returns); the cursor's subsequent drain isn't
	// "in engine" anymore and shouldn't double-count.
	defer telemetry.ObserveQueryInflight(ctx, lang.Name())()

	// Scalar-parameter value domains — symmetrical with QueryPlan, so the
	// streaming path rejects exactly what the eager path rejects.
	if err := e.runGuards(ctx, lang, meta); err != nil {
		return CursorResult{}, err
	}

	plan = lang.ProjectSamples(plan, meta)
	if !meta.IsTraceByID {
		optT := telemetry.ObserveStage(telemetry.StageOptimize, lang.Name())
		plan = e.Optimizer.Run(ctx, plan)
		optT.Done(ctx)
	}

	// Resource-bound gate (symmetrical with QueryPlan): reject a subquery whose
	// anchor grid alone busts the per-query sample budget before any SQL is sent.
	if err := requireSubquerySampleBudget(plan, e.MaxQuerySamples); err != nil {
		return CursorResult{}, err
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

	// Failure-driven route memo (DARK — nil RouteMemo is a no-op, byte-
	// unchanged below). A live PreferB verdict routes B directly, subject
	// to every gate re-checked at THIS call; any route-B failure here falls
	// back to the ordinary route-A dispatch below exactly as if this had
	// never run (Major-2's symmetric fallback).
	budget := chclient.SampleBudgetFromContext(ctx)
	if cur, info, usedDecision, key, ok := e.tryRouteMemoHit(chclient.WithProgressFor(ctx, lang.Name()), lang.Name(), plan, decision, budget); ok {
		result := e.buildRoutedCursorResult(meta, plan, lang.Name(), usedDecision, cur, info, "memo-hit")
		result.Retry = func(retryCtx context.Context, drainErr error) (CursorResult, bool) {
			// The verdict already existed before this dispatch (that is
			// what made it a memo-hit); a clean drain needed no
			// re-confirmation, but this closure only ever runs when the
			// drain FAILED, so report that failure now rather than waiting
			// for the entry to age out on its own TTL clock.
			e.RouteMemo.Observe(key, routememo.RouteB, classifyRouteOutcome(routememo.RouteB, drainErr))
			// The memo chose B on B's own past recommendation, not on
			// evidence about THIS request — fall back to the ordinary,
			// always-safe route-A dispatch, memo bypassed entirely (no
			// further retry chaining: an open failure here just surfaces
			// as "no retry available", not a second retry attempt).
			fallback, ferr := e.dispatchRouteACursor(retryCtx, lang, meta, plan, decision)
			if ferr != nil || fallback.openErr != nil {
				return CursorResult{}, false
			}
			return fallback.CursorResult, true
		}
		return result, nil
	}

	result, err := e.dispatchRouteACursor(ctx, lang, meta, plan, decision)
	if err != nil {
		return CursorResult{}, err
	}

	if err := result.openErr; err != nil {
		// Open-time failure (e.g. a breaker fast-fail, or a 241 on the
		// first block) — try the A->B retry before giving up. On success
		// this returns a routed CursorResult from route B instead; on
		// failure (including "retry does not apply here") the original
		// route-A error is what surfaces, unchanged.
		execCtx := chclient.WithProgressFor(ctx, lang.Name())
		if cur, info, usedDecision, observeFn, retried := e.retryOnRouteAResourceFailure(execCtx, lang.Name(), plan, decision, budget, err); retried {
			retryResult := e.buildRoutedCursorResult(meta, plan, lang.Name(), usedDecision, cur, info, "retry")
			retryResult.ObserveDrainOutcome = observeFn
			return retryResult, nil
		}
		// The sample-budget 422 instead surfaces later during the handler's
		// drain via ObserveDrainOutcome. Stamp any cerberus-side open-time
		// outcome here.
		e.observeOutcomeForErr(result.queryID, lang.Name(), plan, decision, err)
		return CursorResult{}, fmt.Errorf("engine: execute: %w", err)
	}

	// Attach the drain-failure retry hook only when the memo mechanism is
	// active AND this dispatch could plausibly need it (a nil seed decision
	// means the head isn't PromQL / Solver is off, in which case
	// retryOnRouteAResourceFailure's own guards would refuse anyway — the
	// nil check here just avoids handing the caller a closure that always
	// declines).
	if e.routeMemoActive() && decision != nil {
		result.Retry = func(retryCtx context.Context, drainErr error) (CursorResult, bool) {
			cur, info, usedDecision, observeFn, retried := e.retryOnRouteAResourceFailure(retryCtx, lang.Name(), plan, decision, budget, drainErr)
			if !retried {
				return CursorResult{}, false
			}
			retryResult := e.buildRoutedCursorResult(meta, plan, lang.Name(), usedDecision, cur, info, "retry")
			retryResult.ObserveDrainOutcome = observeFn
			return retryResult, true
		}
	}
	return result.CursorResult, nil
}

// routeACursorAttempt wraps the ordinary (non-memo) route-A cursor dispatch:
// either a genuine CursorResult (openErr nil), or — when the OPEN itself
// failed — the empty CursorResult plus openErr and queryID, so the caller
// can still classify/retry/stamp the open-time failure without
// dispatchRouteACursor needing to know anything about the route memo.
type routeACursorAttempt struct {
	CursorResult
	openErr error
	queryID string
}

// dispatchRouteACursor is the ordinary, always-safe route-A cursor dispatch
// — emit, open. Factored out of QueryPlanCursor so both the normal path AND
// the failure-driven route memo's memo-hit fallback (route B failed mid-
// drain -> fall back to plain route A, memo bypassed) can share it, rather
// than risk the two drifting apart.
func (e *Engine) dispatchRouteACursor(
	ctx context.Context,
	lang Lang,
	meta Meta,
	plan chplan.Node,
	decision *solver.Decision,
) (routeACursorAttempt, error) {
	cq, ok := e.Client.(CursorQuerier)
	if !ok {
		return routeACursorAttempt{}, fmt.Errorf("engine: client does not implement CursorQuerier")
	}

	emitT := telemetry.ObserveStage(telemetry.StageEmit, lang.Name())
	sql, args, err := emitForHead(ctx, lang, plan)
	emitT.Done(ctx)
	if err != nil {
		return routeACursorAttempt{}, fmt.Errorf("engine: emit: %w", err)
	}

	execT := telemetry.ObserveStage(telemetry.StageExecute, lang.Name())
	execCtx, queryID := e.execContext(chclient.WithProgressFor(ctx, lang.Name()), plan, lang.Name(), decision)
	// Thread the adapter's declared response shape onto the execute ctx so
	// chclient's columnar matrix decode can confirm (defense-in-depth, on top
	// of its own structural name/type check) that this query really is the
	// matrix projection before it engages — see chclient.ResponseShapeMatrix
	// (#1429). meta.ResponseShape is "" for adapters that haven't opted in,
	// which chclient treats as "unknown, defer to the structural check
	// alone" — byte-unchanged behaviour for those callers.
	execCtx = chclient.WithResponseShape(execCtx, meta.ResponseShape)
	cursor, err := cq.QueryCursor(execCtx, sql, args...)
	execT.Done(ctx)
	if err != nil {
		return routeACursorAttempt{openErr: err, queryID: queryID}, nil
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
	return routeACursorAttempt{
		CursorResult: CursorResult{
			Cursor:        cursor,
			SQL:           sql,
			Args:          args,
			Strategy:      strategy,
			PlanNodeCount: nodes,
			Headers:       headers,
			Meta:          meta,
			QueryID:       queryID,
		},
	}, nil
}

// buildRoutedCursorResult composes the CursorResult for a cursor obtained
// via the Solver's Executor — shared by the failure-driven route memo's
// memo-hit and its two retry dispatch sites, so header and strategy
// construction cannot drift between the three. via is a short label folded
// into the shadow header's decision-reason position ("memo-hit" / "retry")
// distinguishing a memo-driven route from an ordinary threshold-triggered one;
// it never appears on the ordinary executeRoutedCursor path, which builds its
// own headers from its decision's real Reason (ReasonRouted) via
// routeDecisionValue.
//
// It is also the single corpus dispatch seam for those three route-B sites, so
// none of them can be added or moved without the routed observation coming
// along — and, because executeRoutedCursor observes for itself and never calls
// here, no routed dispatch is ever recorded twice. The corpus records the
// DECISION's own Reason (ReasonRouted: the memo re-derives its decision through
// the Planner), never via — decision_reason carries the classifier's vocabulary
// and "route B iff reason=routed" is a pinned corpus invariant, so via stays a
// response-header detail.
func (e *Engine) buildRoutedCursorResult(
	meta Meta,
	plan chplan.Node,
	language string,
	decision *solver.Decision,
	cursor chclient.Cursor,
	info *solver.ExecInfo,
	via string,
) CursorResult {
	e.observeRoutedQuery(info, plan, language, decision)
	nodes := cerbtrace.CountNodes(plan)
	strategy := strategyFor(meta)
	sql, args := routedSQLArgs(info)
	headers := map[string]string{
		HeaderStrategy:  strategy,
		HeaderPlanNodes: strconv.Itoa(nodes),
	}
	headers[HeaderRouteDecision] = solver.StrategyShardedTimeslice + ";k=" + strconv.Itoa(decision.K) + ";reason=" + via
	return CursorResult{
		Cursor:        cursor,
		SQL:           sql,
		Args:          args,
		Strategy:      strategy,
		PlanNodeCount: nodes,
		Headers:       headers,
		Meta:          meta,
		QueryID:       routedQueryID(info),
	}
}

// routedQueryID selects a shard join key for a logical route-B record. The
// reconciler indexes every shard id onto that one record, so one non-empty id
// is sufficient to stamp a terminal drain outcome onto the whole fan-out.
func routedQueryID(info *solver.ExecInfo) string {
	if info == nil {
		return ""
	}
	for _, id := range info.ShardQueryIDs {
		if id != "" {
			return id
		}
	}
	return ""
}

// ObserveDrainOutcome stamps a CERBERUS-side terminal outcome that surfaced
// while the handler drained a cursor onto the corpus record for queryID. It is
// the cursor-path sibling of the eager path's in-engine observeOutcomeForErr:
// the drain happens in the handler, so the handler calls this with the
// CursorResult.QueryID and the drain error. The sample-budget 422 (fires after a
// clean CH finish) is stamped via ObserveOutcome so the reconciler keeps the
// joined cost and overrides exit_status; a memory-cap abort (CH code 241,
// "oom") surfacing mid-drain is recorded as a dispatched rejection so the row
// lands even if the query_log join misses. No-op when no observer is registered,
// the queryID is empty, or the error is not a recorded outcome. The drain site
// has no plan/decision, so the dispatched-rejection row carries the language but
// no routing read-out (routePresent=false) — exit_status stays the operator signal.
func (e *Engine) ObserveDrainOutcome(queryID, language string, err error) {
	if e.QueryObserver == nil || queryID == "" || err == nil {
		return
	}
	switch outcomeTokenForErr(err) {
	case optcorpusExitSampleBudget:
		e.QueryObserver.ObserveOutcome(queryID, optcorpusExitSampleBudget)
	case optcorpusExitOOM:
		e.QueryObserver.ObserveDispatchedRejection(
			queryID, "", nil, language, optcorpusExitOOM,
			false, "", 0, 0, 0, 0, 0, 0, "",
		)
	}
}

// ObserveCapRejection records a decision-only "rejected" corpus row for a
// request cerberus rejected with a 400 BEFORE the pipeline ran (the
// resolution-cap / body-limit guards fire pre-parse, so there is no plan and no
// routing classification). The row carries the language and the cerberus-side
// outcome with no cost and no routing features (routePresent=false) — it still
// captures that the request was rejected, which is a misroute signal the
// query_log can never show. No-op when no observer is registered.
func (e *Engine) ObserveCapRejection(language string) {
	if e.QueryObserver == nil {
		return
	}
	// No plan / decision at the pre-parse cap site: empty shape-id, no opts,
	// absent routing read-out. The "rejected" token is the discriminator.
	e.QueryObserver.ObserveRejection(
		"", nil, language, optcorpusExitRejected,
		false, "", 0, 0, 0, 0, 0, 0, "",
	)
}

// executeRoutedCursor is the streaming sibling of executeRouted: it
// dispatches the K shard cursors through the Solver's Executor and returns
// the composed cursor directly (the caller drives the drain + Close, exactly
// as route A's single cursor). NEVER reached under Mode=single; wired so the
// flip to routed mode is a config change.
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
	execT := telemetry.ObserveStage(telemetry.StageExecute, lang.Name())
	cursor, info, err := e.Solver.Executor.Execute(
		chclient.WithProgressFor(ctx, lang.Name()), lang.Name(), decision, chclient.SampleBudgetFromContext(ctx),
	)
	execT.Done(ctx)
	if err != nil {
		return CursorResult{}, fmt.Errorf("engine: solver execute: %w", err)
	}
	// Dispatch seam for the streaming route-B path (the caller owns the drain,
	// so there is no later engine-side site to record from).
	e.observeRoutedQuery(info, plan, lang.Name(), decision)

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
		QueryID:       routedQueryID(info),
	}, nil
}
