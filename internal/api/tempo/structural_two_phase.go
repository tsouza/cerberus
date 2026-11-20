package tempo

import (
	"context"
	"fmt"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/schema"
	traceql_lower "github.com/tsouza/cerberus/internal/traceql"
)

// phaseARankTsAlias names the per-trace ranking key phase A computes —
// min(Timestamp) over the matched R rows, the same quantity toTraceSummaries
// uses as a trace's startTimeUnixNano. It is only ever referenced by the
// ORDER BY of the phase-A plan and never surfaces on the wire.
const phaseARankTsAlias = "rankTs"

// externalTraceIDTableName is the fixed name restrictStructural stamps onto
// chplan.StructuralJoin.TraceIDExternalTable and runTwoPhase passes
// to chclient.WithExternalTraceIDs (issue #2783). A single fixed name is safe
// even across concurrent phase-B dispatches (route-B sharding, or two
// unrelated requests in flight at once): ClickHouse scopes a native-protocol
// external table to the ONE query dispatch that declares it and tears the
// backing storage down when that query finishes (see
// chclient.WithExternalTraceIDs's doc), so there is no server-side registry
// for two dispatches sharing this name to collide in.
const externalTraceIDTableName = "cerberus_phase_b_trace_ids"

// traceIDRestrictionSiteCount is the number of independent closure scan
// sites chsql/structural_join.go's traceIDRestrictionFrag call sites splice
// TraceIDRestriction onto (the anchor seed, the recursive step, the R leaf on
// both the anti-join and direct-join arms, and the inverse-closure anchor).
// Not every phase-B shape reaches every site simultaneously, but
// useExternalTraceIDTable sizes its budget for the worst case — assuming
// every site fires — so the byte-size decision never under-shoots and lets a
// too-large literal splice through.
const traceIDRestrictionSiteCount = 5

// traceIDLiteralByteBudget bounds the TOTAL literal bytes
// useExternalTraceIDTable is willing to let restrictStructural splice across
// every closure scan site (traceIDRestrictionSiteCount of them) before
// switching to the external-table form. chsql's own emitted-SQL bound
// (internal/chsql/emit_size_bound.go's maxEmittedSQLBytes) is 262144 bytes —
// ClickHouse's own max_query_size default — and this budget reserves the
// majority of that for the REST of the query text (the recursive CTE
// structure, window predicates, projected columns), rather than letting the
// restriction alone consume it.
const traceIDLiteralByteBudget = 64 * 1024

// estimatedLiteralBytes returns the SQL-text weight of splicing ids as a
// single `IN ('id0','id1',...)` literal list — each id wrapped in one quote
// on either side plus a separator — matching what chsql's
// inStringLiteralsFrag / InlineLit render (a 1-byte overcount for the list's
// final id, which has no trailing separator, is irrelevant at threshold
// scale).
func estimatedLiteralBytes(ids []string) int {
	total := 0
	for _, id := range ids {
		total += len(id) + len(`'',`)
	}
	return total
}

// useExternalTraceIDTable reports whether restrictStructural should push ids
// as a native-protocol external table (chclient.WithExternalTraceIDs) rather
// than splice them as literals: external is only ever worth its round trip
// once the literal form's estimated total bytes — across every closure scan
// site the restriction reaches — crosses traceIDLiteralByteBudget. Below the
// budget the literal splice is cheaper and, per issue #2783's EXPLAIN
// indexes=1 verification against a real ClickHouse server, prunes
// idx_trace_id identically to the external-table form at every size tested —
// so the choice is purely about SQL-text-size risk, never pruning parity.
func useExternalTraceIDTable(ids []string) bool {
	return estimatedLiteralBytes(ids)*traceIDRestrictionSiteCount > traceIDLiteralByteBudget
}

// structuralTwoPhaseTarget reports whether the lowered search plan is the
// positive recursive structural join (`A >> B` / `A << B`) the two-phase
// fetch applies to, returning the join node when so.
//
// The target is the positive OR negated descendant / ancestor closure (both
// route through the WITH RECURSIVE closure), plus a single row-preserving
// `| select(...)` Project wrapped over it (unwrapped here). The negated anti-join
// is admitted because the emitter now restricts its R side to the top-N traces
// (structural_join.go), so phase B stays a faithful subset — without that fix a
// naive gate relax would leak non-top-N traces (a WRONG result). The union
// (`&>>`) two-arm shape is admitted on the same footing: each arm INNER JOINs on
// TraceId to a restricted closure side, so neither the canonical anchor nor the
// inverse closure step leaks. Direct ops (`>` / `<` / `~`) have a far cheaper
// single-INNER-JOIN projection and are excluded. Any other wrapped shape (a set
// operation, an aggregate, a NestedSetAnnotate) falls to the single-query path —
// never a wrong result, just no memory win.
func structuralTwoPhaseTarget(plan chplan.Node) (*chplan.StructuralJoin, bool) {
	root := plan
	// Unwrap a single pure-select Project (`… >> … | select(attr)`). The select
	// re-projects columns without dropping rows or mutating Timestamp, so phase A
	// still ranks over the inner join and phase B hydrates the wrapped tree
	// (restrictStructural walks into the Project to stamp the closure).
	if p, ok := root.(*chplan.Project); ok && isPureSelectProjection(p) {
		root = p.Input
	}
	sj, ok := root.(*chplan.StructuralJoin)
	if !ok {
		return nil, false
	}
	switch sj.Op.Positive() {
	case chplan.StructuralDescendant, chplan.StructuralAncestor:
		// Positive `>>`/`<<`, negated `!>>`/`!<<`, and union `&>>`/`&<<`
		// (Positive() maps every recursive descendant/ancestor variant back to
		// Descendant/Ancestor). Direct `>`/`<`/`~` (and their negated/union forms)
		// fall through — their positive relation is not Descendant/Ancestor — so
		// the single-level emit path is never reached here. All three recursive
		// variants keep phase B a faithful top-N subset: the canonical closure
		// anchor and the inverse closure step both fold TraceIDRestriction
		// (structuralAnchorWhere / structuralStepWhere), and each union arm INNER
		// JOINs on TraceId to a restricted closure side, so no arm leaks.
		return sj, true
	}
	return nil, false
}

// isPureSelectProjection reports whether p is a row-preserving `| select(...)`
// projection directly over a structural join. A `| select(...)` is the only
// lowering that wraps a StructuralJoin in a Project, and a Project is
// row-preserving by construction — it maps one input row to one output row;
// row-collapsing lives in Aggregate nodes, never in a projection — and
// lowerSelect always emits TraceId/SpanId/Timestamp first, so phase A can still
// rank over the inner join. Requiring the input to be the bare StructuralJoin
// (NOT a NestedSetAnnotate / Aggregate / Filter / set-op, which would change the
// scan or drop rows) is what makes the unwrap safe. The projection expressions
// themselves may be map-subscripts or other per-row scalars (a selected
// attribute lowers to `ResourceAttributes['x']`, not a bare column) — those are
// fine; only the input node kind matters.
func isPureSelectProjection(p *chplan.Project) bool {
	_, ok := p.Input.(*chplan.StructuralJoin)
	return ok
}

// SearchRun is what SearchResult returns: the eager engine.Result both Search
// transports shape into trace summaries, plus the root metadata the search
// already looked up for its chosen traces. ResolveSearchRoots reuses that
// metadata instead of issuing the root lookup again.
type SearchRun struct {
	engine.Result
	// roots is the root lookup's answer for every chosen trace, keyed by trace
	// id, when rootsLooked is set.
	roots       map[string]rootMetadata
	rootsLooked bool
}

// SearchResult parses, routes, and executes a /api/search query. It is the
// SINGLE place the two-phase split decision lives, so the HTTP and gRPC heads
// physically cannot diverge — the HTTP-only two-phase gap this closes.
// Callers apply toTraceSummaries / TruncateSummaries / spss over the run's
// Samples, then ResolveSearchRoots.
//
// A positive recursive structural search (`A >> B` / `A << B`, plus negated
// `!>>` / union `&>>`) lowers to a WITH RECURSIVE closure INNER JOINed to the
// WIDE R projection; on a dense descendant side that projection materialises
// every matched span only to return the top-N traces — the prod OOM. Split it:
// phase A ranks narrowly to the top-N TraceIds, phase B re-runs the closure
// projecting wide but restricted to just those traces.
//
// A windowed search whose chosen traces are read over the window widened by
// the maximum trace duration (traceql.SearchDrains) returns each chosen
// trace's matching spans outside the request window too, when its plan drains
// (drainsChosenTraces):
//
//   - A structural relation (any direct or recursive one, alone or under a
//     `| select(...)`) first runs as it does over the request window alone:
//     the single query, or the recursive split. drainIfOutside then asks
//     whether any chosen trace has a span inside the drain window but outside
//     the request window. Only then does it re-read the chosen traces drained
//     (drainChosen) and answer from that read instead.
//   - Any other draining shape (a spanset operation, a pipeline) ranks its
//     top-N traces over the request window in phase A (newestTracesPlan) and
//     reads them drained in phase B.
//
// The plain shape drains inside its own plan (chplan.SearchTraceLimit's Rank)
// and stays on the single-query path, as does every other shape.
//
// The structural split is default-on, switchable off
// (CERBERUS_TEMPO_STRUCTURAL_TWO_PHASE=false); switched off, the split's
// target (structuralTwoPhaseTarget: a recursive relation at the plan's root,
// alone or under a pure select) runs as the single query over the request
// window, so it returns only its matching spans inside that window. Every
// other shape keeps its drain.
// Deliberately UNCONDITIONAL for every eligible shape — no density cost-gate:
// the OOM variable is BYTES (matched-span-count × per-span map width), and every
// cheap pre-fetch signal measures ROWS, blind to fat maps, so a selective-but-
// fat-map search would slip a gate and reintroduce the crash. The Go-heap bytes
// are instead bounded at the drain by the wide-projection byte budget
// (withSpanDrainBudget); a future upfront gate would need a byte-cost signal, not
// row counts — see TestPhaseAStaysNarrow.
func (h *Handler) SearchResult(ctx context.Context, query string, limit int) (SearchRun, error) {
	plan, meta, err := h.lang.Parse(ctx, query)
	if err != nil {
		return SearchRun{}, err
	}
	drain := traceql_lower.SearchDrains(ctx, h.Schema) && drainsChosenTraces(plan)
	if sj, ok := structuralTwoPhaseTarget(plan); ok {
		if !h.StructuralTwoPhase {
			res, err := h.Engine.QueryPlan(ctx, h.lang, plan, meta)
			return SearchRun{Result: res}, err
		}
		topN, err := h.runPhaseA(ctx, buildStructuralPhaseAPlan(sj, h.Schema, limit))
		if err != nil {
			return SearchRun{}, err
		}
		if len(topN) == 0 {
			// No trace matched: skip phase B (a literal `IN ()` is a CH syntax
			// error) and return an empty result — byte-identical to what the
			// single query would produce (zero matched rows -> zero summaries).
			return SearchRun{Result: engine.Result{Meta: meta}}, nil
		}
		// Phase B: restrict a clone of the lowered plan to the top-N traces,
		// then run the normal wide pipeline (wrap-projection + optimizer + emit
		// + execute) so the samples arrive in the exact shape toTraceSummaries
		// already consumes.
		res, err := h.runPhaseB(ctx, plan, topN, meta, false)
		if err != nil || !drain {
			return SearchRun{Result: res}, err
		}
		return h.drainIfOutside(ctx, query, res, limit)
	}
	if drain && isStructuralSearch(plan) {
		res, err := h.Engine.QueryPlan(ctx, h.lang, plan, meta)
		if err != nil {
			return SearchRun{}, err
		}
		return h.drainIfOutside(ctx, query, res, limit)
	}
	if drain {
		res, err := h.runTwoPhase(ctx, query, newestTracesPlan(plan, h.Schema, limit), meta)
		return SearchRun{Result: res}, err
	}
	res, err := h.Engine.QueryPlan(ctx, h.lang, plan, meta)
	return SearchRun{Result: res}, err
}

// drainsChosenTraces reports whether a lowered search plan reads its chosen
// traces' matching spans in a phase B of its own: a spanset operation (`&&`,
// `||`), a positive or union structural relation of any kind, a pipeline
// filter or a `| select(...)`, over span filters with any trace-scoped
// intrinsic or spanset-aggregate operand they carry. Every such shape is
// monotone in the window — reading more of a chosen trace's spans only adds
// matching spans, never removes one — so each chosen trace keeps the spans it
// matched inside the request window and gains its matching spans outside it.
//
// Excluded: the plain shape (a chplan.SearchTraceLimit root), which drains
// inside its own plan; a negated structural relation, whose matches a wider
// read can remove (a span outside the window can be the ancestor that makes a
// span inside it no longer match); the nested-set annotation; and a
// top-level spanset aggregate, which returns one row per trace.
func drainsChosenTraces(n chplan.Node) bool {
	switch v := n.(type) {
	case *chplan.Scan:
		return true
	case *chplan.Filter:
		return drainsChosenTraces(v.Input)
	case *chplan.Project:
		return drainsChosenTraces(v.Input)
	case *chplan.TraceLookupFilter:
		return drainsChosenTraces(v.Input)
	case *chplan.SetOperation:
		switch v.Op {
		case chplan.SetIntersect, chplan.SetUnion:
			return drainsChosenTraces(v.Left) && drainsChosenTraces(v.Right)
		}
	case *chplan.StructuralJoin:
		return !v.Op.IsNegated() && drainsChosenTraces(v.Left) && drainsChosenTraces(v.Right)
	}
	return false
}

// isStructuralSearch reports whether a lowered search plan is a structural
// relation, alone or under a row-preserving `| select(...)`.
func isStructuralSearch(plan chplan.Node) bool {
	if p, ok := plan.(*chplan.Project); ok && isPureSelectProjection(p) {
		plan = p.Input
	}
	_, ok := plan.(*chplan.StructuralJoin)
	return ok
}

// drainIfOutside finishes a draining structural search from res, its result
// over the request window. It chooses the response traces from res as the
// callers do (toTraceSummaries, TruncateSummaries) and asks ClickHouse whether
// any of them has a span inside the drain window but outside the request
// window (traceql.SearchDrainOutside). When none does, every chosen trace's
// drained answer is its request-window answer, and res answers the search.
// When one does, or the question cannot be answered, the chosen traces are
// read drained (drainChosen) and that read answers the search in place of
// res.
//
// The question rides the root lookup when a chosen trace lacks its root row,
// the lookup the callers would otherwise issue for those traces: it covers
// every chosen trace, and the run carries its answer for ResolveSearchRoots.
// Otherwise it is a statement of its own that reads the chosen traces'
// Timestamp alone and stops at the first span it finds.
func (h *Handler) drainIfOutside(ctx context.Context, query string, res engine.Result, limit int) (SearchRun, error) {
	run := SearchRun{Result: res}
	outside := traceql_lower.SearchDrainOutside(ctx, h.Schema)
	summaries, missing := toTraceSummaries(res.Samples, DefaultSpansPerSpanSet, res.Meta)
	summaries, missing = TruncateSummaries(summaries, missing, limit)
	if outside == nil || len(summaries) == 0 {
		return run, nil
	}
	chosen := make([]string, len(summaries))
	for i := range summaries {
		chosen[i] = summaries[i].TraceID
	}
	extends := true
	if len(missing) > 0 {
		roots, err := h.resolveTraceRootsWithin(ctx, chosen, outside)
		if err != nil {
			h.Logger.Warn("cerberus tempo search root and extent lookup failed; reading the chosen traces drained",
				"err", err, "traces", len(chosen))
		} else {
			run.roots, run.rootsLooked = roots, true
			extends = anyExtendsOutside(chosen, roots)
		}
	} else {
		ids, err := h.queryTraceIDs(ctx, extendingTracePlan(h.Schema, chosen, outside), "search extent")
		if err != nil {
			h.Logger.Warn("cerberus tempo search extent lookup failed; reading the chosen traces drained",
				"err", err, "traces", len(chosen))
		} else {
			extends = len(ids) > 0
		}
	}
	if !extends {
		return run, nil
	}
	drained, err := h.drainChosen(ctx, query, chosen, res.Meta)
	if err != nil {
		return SearchRun{}, err
	}
	run.Result = drained
	return run, nil
}

// anyExtendsOutside reports whether any chosen trace has a span inside the
// drain window but outside the request window, by the root lookup's answer. A
// trace the lookup returned no row for counts as one that does.
func anyExtendsOutside(chosen []string, roots map[string]rootMetadata) bool {
	for _, id := range chosen {
		if root, ok := roots[id]; !ok || root.SpansOutsideWindow {
			return true
		}
	}
	return false
}

// extendingTracePlan selects at most one trace id among ids whose trace has a
// span matching outside:
//
//	Project[TraceId]
//	  Limit(1)
//	    Filter(TraceId IN ids AND outside)
//	      Scan
func extendingTracePlan(s schema.Traces, ids []string, outside chplan.Expr) chplan.Node {
	return &chplan.Project{
		Input: &chplan.Limit{
			Input: &chplan.Filter{
				Input:     &chplan.Scan{Table: s.SpansTable},
				Predicate: &chplan.Binary{Op: chplan.OpAnd, Left: traceIDsIn(s.TraceIDColumn, ids), Right: outside},
			},
			Count: 1,
		},
		Projections: []chplan.Projection{{Expr: &chplan.ColumnRef{Name: s.TraceIDColumn}}},
	}
}

// runTwoPhase executes a draining search in two phases and returns the
// engine.Result the response is shaped from: phase A runs the ranking plan
// phaseA (newestTracesPlan) — traces by min(Timestamp) DESC, TraceId ASC LIMIT
// N over the request window, the ranking the single query applies — and
// drainChosen reads those traces drained.
func (h *Handler) runTwoPhase(ctx context.Context, query string, phaseA chplan.Node, meta engine.Meta) (engine.Result, error) {
	topN, err := h.runPhaseA(ctx, phaseA)
	if err != nil {
		return engine.Result{}, err
	}
	if len(topN) == 0 {
		// No trace matched: skip phase B (a literal `IN ()` is a CH syntax
		// error) and return an empty result — byte-identical to what the
		// single query would produce (zero matched rows -> zero summaries).
		return engine.Result{Meta: meta}, nil
	}
	return h.drainChosen(ctx, query, topN, meta)
}

// drainChosen reads the chosen traces ids drained: the query re-lowered under
// traceql.WithSearchDrain, which reads the window widened by the maximum trace
// duration, restricted to those traces (runPhaseB), so each chosen trace
// returns its matching spans outside the request window too. Every physical
// spans scan is confined to the chosen traces, so idx_trace_id granule-prunes
// the fetch to just the response traces — spliced as literals, or, on the
// structural closure scans, pushed as a native-protocol external table above
// useExternalTraceIDTable's threshold when h.ExternalTraceIDPush is set (issue
// #2783). toTraceSummaries over the rows computes the summaries;
// TruncateSummaries is then a no-op safety net (≤ N traces already). A drained
// summary starts at its trace's earliest matching span in the widened window,
// so the response orders the same N traces by that start.
func (h *Handler) drainChosen(ctx context.Context, query string, ids []string, meta engine.Meta) (engine.Result, error) {
	plan, _, err := h.lang.Parse(traceql_lower.WithSearchDrain(ctx), query)
	if err != nil {
		return engine.Result{}, err
	}
	// A drain's span rows are not all read through a structural closure: a
	// spanset operation's arms and a pipeline's span filter are plain span
	// scans, so every leaf scan — per-trace subqueries and lookups included —
	// takes a literal id list of its own. Each list costs SQL text at every
	// site it is spliced into, so the chosen traces are read in batches whose
	// lists fit traceIDLiteralByteBudget. Every
	// trace's spans depend on that trace's rows alone, so the batches'
	// results together are the single phase B's. The merged result's
	// statement fields describe the first batch's statement; its ClickHouse
	// time sums every batch's.
	var res engine.Result
	for i, batch := range drainBatches(ids, drainLiteralSites(plan, h.ExternalTraceIDPush)) {
		part, err := h.runPhaseB(ctx, plan, batch, meta, true)
		if err != nil {
			return engine.Result{}, err
		}
		if i == 0 {
			res = part
			continue
		}
		res.Samples = append(res.Samples, part.Samples...)
		res.CHMillis += part.CHMillis
	}
	return res, nil
}

// runPhaseB runs plan restricted to the traces topN: every structural join
// through restrictStructural and, for a drain, every leaf span scan through a
// literal id list (chplan.PushLeafPredicate). Both carry topN as phase A read
// it from the trace-id column, so they match that column's stored values
// whatever form they take.
func (h *Handler) runPhaseB(ctx context.Context, plan chplan.Node, topN []string, meta engine.Meta, drain bool) (engine.Result, error) {
	restricted, ids, external := restrictStructural(plan, topN, h.ExternalTraceIDPush)
	if drain {
		restricted = chplan.PushLeafPredicate(restricted, traceIDsIn(h.Schema.TraceIDColumn, topN))
	}
	if external {
		var err error
		ctx, err = chclient.WithExternalTraceIDs(ctx, externalTraceIDTableName, h.Schema.TraceIDColumn, ids)
		if err != nil {
			return engine.Result{}, fmt.Errorf("engine: execute: search phase B external trace-id table: %w", err)
		}
	}
	return h.Engine.QueryPlan(ctx, h.lang, restricted, meta)
}

// drainLiteralSites is the number of places a drain phase B splices its
// literal trace-id list into: one per leaf scan chplan.PushLeafPredicate
// restricts, plus traceIDRestrictionSiteCount per structural join when the
// joins take the literal form (no external-table push).
func drainLiteralSites(plan chplan.Node, externalPush bool) int {
	marker := &chplan.LitBool{V: true}
	sites := countSplices(chplan.PushLeafPredicate(chplan.CloneNode(plan), marker), marker)
	if !externalPush {
		eachStructuralJoin(plan, func(*chplan.StructuralJoin) { sites += traceIDRestrictionSiteCount })
	}
	return sites
}

// countSplices counts the Filter conjuncts of n that are pred itself, the
// predicate chplan.PushLeafPredicate shares between every leaf it restricts:
// in n's nodes and in every per-trace subquery their predicates carry.
func countSplices(n chplan.Node, pred chplan.Expr) int {
	count := 0
	var predicate chplan.Expr
	switch v := n.(type) {
	case *chplan.Filter:
		predicate = v.Predicate
		for _, c := range flattenAnd(v.Predicate) {
			if c == pred {
				count++
			}
		}
	case *chplan.TraceLookupFilter:
		predicate = v.Predicate
	}
	for _, in := range chplan.CohortSubqueries(predicate) {
		count += countSplices(in.Subquery, pred)
	}
	for _, c := range n.Children() {
		count += countSplices(c, pred)
	}
	return count
}

// flattenAnd splits an AND tree into its conjuncts.
func flattenAnd(e chplan.Expr) []chplan.Expr {
	if b, ok := e.(*chplan.Binary); ok && b.Op == chplan.OpAnd {
		return append(flattenAnd(b.Left), flattenAnd(b.Right)...)
	}
	return []chplan.Expr{e}
}

// drainBatches splits the chosen trace ids, in rank order, into batches whose
// literal list, spliced at sites places, stays within
// traceIDLiteralByteBudget. Every batch holds at least one id; a plan with no
// literal site (a structural relation under an external-table push) reads
// every id in one batch.
func drainBatches(ids []string, sites int) [][]string {
	if sites == 0 {
		return [][]string{ids}
	}
	var batches [][]string
	start, bytes := 0, 0
	for i, id := range ids {
		idBytes := estimatedLiteralBytes([]string{id}) * sites
		if i > start && bytes+idBytes > traceIDLiteralByteBudget {
			batches = append(batches, ids[start:i])
			start, bytes = i, 0
		}
		bytes += idBytes
	}
	return append(batches, ids[start:])
}

// traceIDsIn is the `<traceIDColumn> IN (<ids>)` restriction phase B pushes
// onto a drain's leaf span scans.
func traceIDsIn(traceIDColumn string, ids []string) chplan.Expr {
	list := make([]chplan.Expr, len(ids))
	for i, id := range ids {
		list[i] = &chplan.LitString{V: id}
	}
	return &chplan.InList{Left: &chplan.ColumnRef{Name: traceIDColumn}, List: list}
}

// runPhaseA emits and runs a narrow ranking plan, returning the top-N on-disk
// TraceIds newest-first (min(Timestamp) DESC, TraceId ASC). It
// emits directly through chsql.Emit + Client.QueryStrings (bypassing
// wrap-projection + the optimizer) because the result is a bare list of trace
// ids, not a Sample stream — the same direct-emit pattern resolveTraceRoots
// uses. The spans table is threaded onto the emit context so the resource-
// bound gate verifies the closure's scans stay windowed.
//
// The traces SchemaStrategies are threaded here too (cerberus issue #3062)
// precisely because this bypasses the engine's entry points — the ones
// that duck-type h.lang against schemaStrategier and call
// chsql.WithSchemaStrategies automatically. Every other emit path in this
// package reaches them through h.Engine.QueryPlan and picks the
// strategies up for free; this hand-rolled direct-emit path is the
// exception the #2777 PR body's own risk note anticipated ("structural-
// join/nested-set emitters that may not route through the emitSelect
// chokepoint"), and a real chDB differential
// (TestTraceQL_JSONAttrStrategy_StructuralJoin_ChDB) caught it: the
// closure's own predicate against a JSON-typed span attribute column
// failed with ILLEGAL_TYPE_OF_ARGUMENT until this line was added — the
// narrow ranking query still evaluates every closure predicate (it is
// the SAME closure, just projecting fewer columns), so a JSON-typed
// attribute referenced anywhere in it needs the strategy exactly like
// phase B and the single-query path do.
func (h *Handler) runPhaseA(ctx context.Context, phaseA chplan.Node) ([]string, error) {
	return h.queryTraceIDs(ctx, phaseA, "search phase A")
}

// queryTraceIDs emits and runs plan, a plan that projects one trace-id column,
// and returns the ids it selects, the way runPhaseA documents. stage names the
// statement in a returned error.
func (h *Handler) queryTraceIDs(ctx context.Context, plan chplan.Node, stage string) ([]string, error) {
	ctx = chsql.WithSpansTable(ctx, h.Schema.SpansTable)
	ctx = chsql.WithSchemaStrategies(ctx, h.tracesStrategies(ctx))
	sql, args, physicalScans, err := chsql.EmitCounted(ctx, plan)
	if err != nil {
		// Mirror the engine's `emit:` wrapping so ClassifyErr maps this
		// to HTTP 500 like any other emit failure.
		return nil, fmt.Errorf("engine: emit: %s: %w", stage, err)
	}
	// This dispatch bypasses the engine, so it stamps the data-shard fan-out
	// weight itself: a structural join renders the spans table once per
	// closure arm (five to seven times for the committed shapes), and each
	// render is its own Distributed fan-out on a multi-data-shard deployment
	// (chclient.WithDataShardFanoutMultiplier's doc).
	ctx = chclient.WithDataShardFanoutMultiplier(ctx, physicalScans)
	ids, err := h.Client.QueryStrings(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("engine: execute: %s: %w", stage, err)
	}
	return ids, nil
}

// buildStructuralPhaseAPlan builds the narrow ranking plan over a lowered
// structural join: newestTracesPlan over
// StructuralJoin(narrow: ExtraProjectionColumns=[Timestamp]).
//
// The narrow join is a clone of the lowered join with the wide attribute
// columns dropped from the projection — the closure, R match, window, depth
// cap, and candidate prefilter are all preserved, so the set of matched R rows
// is IDENTICAL to the wide query's; only the projected columns differ.
func buildStructuralPhaseAPlan(sj *chplan.StructuralJoin, s schema.Traces, limit int) chplan.Node {
	narrow := chplan.CloneNode(sj).(*chplan.StructuralJoin)
	// Project only the ranking column. The three join keys are always emitted
	// by structuralProjectionFrags; ExtraProjectionColumns adds Timestamp so
	// min(Timestamp) resolves — and drops every wide map column the OOM came
	// from. Applied to EVERY structural join (chains are left-associative, so
	// the inner `A >> B` closure must be narrowed too, else phase A re-projects
	// the wide window on the inner hop).
	eachStructuralJoin(narrow, func(j *chplan.StructuralJoin) {
		j.ExtraProjectionColumns = []string{s.TimestampColumn}
	})

	return newestTracesPlan(narrow, s, limit)
}

// newestTracesPlan builds the ranking plan selecting the limit newest traces
// of a span row source:
//
//	Project[TraceId]
//	  Limit(N)
//	    OrderBy(rankTs DESC, TraceId ASC)
//	      Aggregate(GroupBy=[TraceId], min(Timestamp) AS rankTs)
//	        input
//
// Grouping by TraceId and taking min(Timestamp) reproduces exactly the
// per-trace startNS toTraceSummaries computes over input's rows, and the
// ORDER BY + LIMIT reproduce exactly the top-N sortSummariesStartDesc +
// TruncateSummaries would keep of them — so the ids are byte-for-byte the
// traces the single query over input would have returned.
func newestTracesPlan(input chplan.Node, s schema.Traces, limit int) chplan.Node {
	agg := &chplan.Aggregate{
		Input:          input,
		GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: s.TraceIDColumn}},
		GroupByAliases: []string{s.TraceIDColumn},
		AggFuncs: []chplan.AggFunc{{
			Fn:    chplan.FnMin,
			Args:  []chplan.Expr{&chplan.ColumnRef{Name: s.TimestampColumn}},
			Alias: phaseARankTsAlias,
		}},
	}
	ordered := &chplan.OrderBy{
		Input: agg,
		Keys: []chplan.OrderKey{
			{Expr: &chplan.ColumnRef{Name: phaseARankTsAlias}, Desc: true},
			{Expr: &chplan.ColumnRef{Name: s.TraceIDColumn}, Desc: false},
		},
	}
	limited := &chplan.Limit{Input: ordered, Count: int64(limit)}
	return &chplan.Project{
		Input:       limited,
		Projections: []chplan.Projection{{Expr: &chplan.ColumnRef{Name: s.TraceIDColumn}}},
	}
}

// restrictStructural clones the lowered plan and stamps the phase-A top-N
// TraceIds onto every structural join in it as the closure's restriction, so
// phase B's wide fetch reads only those traces. The ids are the trace-id
// column's values exactly as phase A read them, so the restriction matches
// every stored id, including one shorter than the canonical 32 hex
// characters; a canonicalised spelling of a short stored id matches no row.
//
// externalEligible is h.ExternalTraceIDPush: when false (the pre-#2783
// default) every restriction is the literal splice, byte-identical to
// before. When true, restrictStructural ALSO checks useExternalTraceIDTable —
// only once BOTH hold does it stamp TraceIDExternalTable instead of
// TraceIDRestriction, and reports external=true so the caller
// (runTwoPhase) knows to attach the actual id rows via
// chclient.WithExternalTraceIDs. Returns the ids alongside the clone either
// way, since the caller needs them again for that attach.
func restrictStructural(plan chplan.Node, topN []string, externalEligible bool) (restricted chplan.Node, ids []string, external bool) {
	// Clone as a plain Node (not *StructuralJoin): the plan may be a pure-select
	// Project wrapped over the join (`… >> … | select(...)`), in which case the
	// root is the Project and the join sits beneath it. eachStructuralJoin walks
	// into it either way, so the restriction is stamped whether the root is the
	// join or a wrapper over it.
	clone := chplan.CloneNode(plan)
	ids = topN
	external = externalEligible && useExternalTraceIDTable(ids)
	// Stamp EVERY structural join in the plan, not just the root: a chain
	// `A >> B >> C` is left-associative, so the root's Left is itself a
	// StructuralJoin (the inner `A >> B` closure). Restricting only the root
	// leaves the inner closure scanning + projecting the full window in both
	// phases (the exact OOM this fix removes). Every result trace is in the
	// top-N set, so confining an inner closure to those trace-ids is loss-free.
	joins := 0
	eachStructuralJoin(clone, func(sj *chplan.StructuralJoin) {
		joins++
		if external {
			sj.TraceIDExternalTable = externalTraceIDTableName
		} else {
			sj.TraceIDRestriction = ids
		}
	})
	// A plan with no structural join (a spanset operation's phase B) reads no
	// external table, so none is attached.
	return clone, ids, external && joins > 0
}

// eachStructuralJoin visits every StructuralJoin in n (root-first), so a
// left-associative chain's inner closures are reached, not just the root.
func eachStructuralJoin(n chplan.Node, fn func(*chplan.StructuralJoin)) {
	if n == nil {
		return
	}
	if sj, ok := n.(*chplan.StructuralJoin); ok {
		fn(sj)
	}
	for _, c := range n.Children() {
		eachStructuralJoin(c, fn)
	}
}
