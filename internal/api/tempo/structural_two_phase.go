package tempo

import (
	"context"
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/schema"
)

// phaseARankTsAlias names the per-trace ranking key phase A computes —
// min(Timestamp) over the matched R rows, the same quantity toTraceSummaries
// uses as a trace's startTimeUnixNano. It is only ever referenced by the
// ORDER BY of the phase-A plan and never surfaces on the wire.
const phaseARankTsAlias = "rankTs"

// structuralTwoPhaseTarget reports whether the lowered search plan is the
// positive recursive structural join (`A >> B` / `A << B`) the two-phase
// fetch applies to, returning the join node when so.
//
// The target is deliberately narrow: only the bare positive descendant /
// ancestor closure, whose emitted shape is the WITH RECURSIVE closure INNER
// JOINed to the wide R projection that OOMs on a dense descendant side. Direct
// ops (`>` / `<` / `~`) have a far cheaper single-INNER-JOIN projection;
// negated (`!>>`) and union (`&>>`) variants have anti-join / two-arm shapes
// with different phase-A ranking semantics; and any wrapped shape (a Project,
// a set operation, a `| select(...)` pipeline over the join) is not a bare
// StructuralJoin root. All of those fall to the single-query path — never a
// wrong result, just no memory win. They can be added behind the same seam
// later if their projections turn out to matter.
func structuralTwoPhaseTarget(plan chplan.Node) (*chplan.StructuralJoin, bool) {
	sj, ok := plan.(*chplan.StructuralJoin)
	if !ok {
		return nil, false
	}
	if sj.Op.IsNegated() || sj.Op.IsUnion() {
		return nil, false
	}
	switch sj.Op.Positive() {
	case chplan.StructuralDescendant, chplan.StructuralAncestor:
		return sj, true
	}
	return nil, false
}

// runStructuralTwoPhase executes a positive recursive structural search in two
// phases and returns the same engine.Result the single wide query would have
// produced — the identical ordered top-N traces with identical per-trace
// spansets, only bounded in memory.
//
// Phase A runs the closure NARROW (join keys + Timestamp only, no wide
// attribute maps), groups by TraceId, and ranks by min(Timestamp) DESC,
// TraceId ASC LIMIT N — the exact ranking sortSummariesStartDesc +
// TruncateSummaries apply downstream — yielding the top-N on-disk TraceIds.
// Phase B re-runs the closure projecting WIDE but with those N TraceIds
// spliced as literals onto every physical spans scan, so idx_trace_id
// granule-prunes the wide fetch to just the response traces. toTraceSummaries
// over phase B's rows recomputes the identical summaries; TruncateSummaries is
// then a no-op safety net (≤ N traces already).
func (h *Handler) runStructuralTwoPhase(ctx context.Context, sj *chplan.StructuralJoin, fullPlan chplan.Node, meta engine.Meta, limit int) (engine.Result, error) {
	topN, err := h.runStructuralPhaseA(ctx, sj, limit)
	if err != nil {
		return engine.Result{}, err
	}
	if len(topN) == 0 {
		// No trace matched the closure: skip phase B (a literal `IN ()` is a CH
		// syntax error) and return an empty result — byte-identical to what the
		// single query would produce (zero matched rows -> zero summaries).
		return engine.Result{Meta: meta}, nil
	}
	// Phase B: clone the lowered join and restrict every closure scan to the
	// top-N traces, then run the normal wide pipeline (wrap-projection +
	// optimizer + emit + execute) so res.Samples arrive in the exact shape
	// toTraceSummaries already consumes.
	restricted := restrictStructural(fullPlan, topN)
	return h.Engine.QueryPlan(ctx, h.lang, restricted, meta)
}

// runStructuralPhaseA emits and runs the narrow ranking query, returning the
// top-N on-disk TraceIds newest-first (min(Timestamp) DESC, TraceId ASC). It
// emits directly through chsql.Emit + Client.QueryStrings (bypassing
// wrap-projection + the optimizer) because the result is a bare list of trace
// ids, not a Sample stream — the same direct-emit pattern resolveTraceRoots
// uses. The spans table is threaded onto the emit context so the resource-
// bound gate verifies the closure's scans stay windowed.
func (h *Handler) runStructuralPhaseA(ctx context.Context, sj *chplan.StructuralJoin, limit int) ([]string, error) {
	phaseA := buildStructuralPhaseAPlan(sj, h.Schema, limit)
	sql, args, err := chsql.Emit(chsql.WithSpansTable(ctx, h.Schema.SpansTable), phaseA)
	if err != nil {
		// Mirror the engine's `emit:` wrapping so classifySearchErr maps this
		// to HTTP 500 like any other emit failure.
		return nil, fmt.Errorf("engine: emit: structural phase A: %w", err)
	}
	ids, err := h.Client.QueryStrings(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("engine: execute: structural phase A: %w", err)
	}
	return ids, nil
}

// buildStructuralPhaseAPlan builds the narrow ranking plan over a lowered
// structural join:
//
//	Project[TraceId]
//	  Limit(N)
//	    OrderBy(rankTs DESC, TraceId ASC)
//	      Aggregate(GroupBy=[TraceId], min(Timestamp) AS rankTs)
//	        StructuralJoin(narrow: ExtraProjectionColumns=[Timestamp])
//
// The narrow join is a clone of the lowered join with the wide attribute
// columns dropped from the projection — the closure, R match, window, depth
// cap, and candidate prefilter are all preserved, so the set of matched R rows
// is IDENTICAL to the wide query's; only the projected columns differ. Grouping
// by TraceId and taking min(Timestamp) reproduces exactly the per-trace startNS
// toTraceSummaries computes, and the ORDER BY + LIMIT reproduce exactly the
// top-N sortSummariesStartDesc + TruncateSummaries would keep — so phase A's
// ids are byte-for-byte the traces the single query would have returned.
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

	agg := &chplan.Aggregate{
		Input:          narrow,
		GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: s.TraceIDColumn}},
		GroupByAliases: []string{s.TraceIDColumn},
		AggFuncs: []chplan.AggFunc{{
			Name:  "min",
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

// restrictStructural clones the lowered structural-join plan and stamps the
// phase-A top-N TraceIds onto it as the closure's TraceIDRestriction, so phase
// B's wide fetch reads only those traces. The ids are routed through
// padTraceIDs (the root-lookup literal-splice helper) — idempotent for the
// on-disk 32-char form phase A returns, and a defensive left-pad for any short
// id — so the spliced literals match otel_traces.TraceId exactly.
func restrictStructural(plan chplan.Node, topN []string) chplan.Node {
	clone := chplan.CloneNode(plan).(*chplan.StructuralJoin)
	ids := padTraceIDs(topN)
	// Stamp EVERY structural join in the plan, not just the root: a chain
	// `A >> B >> C` is left-associative, so the root's Left is itself a
	// StructuralJoin (the inner `A >> B` closure). Restricting only the root
	// leaves the inner closure scanning + projecting the full window in both
	// phases (the exact OOM this fix removes). Every result trace is in the
	// top-N set, so confining an inner closure to those trace-ids is loss-free.
	eachStructuralJoin(clone, func(sj *chplan.StructuralJoin) {
		sj.TraceIDRestriction = ids
	})
	return clone
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
