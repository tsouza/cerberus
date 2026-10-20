package promql

import (
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// lowerIsolatedSubquery lowers sub, a subquery the fit isolated
// ([domainFit.isolated]), whose inner expression Prometheus evaluates with
// the evaluator state ev.
//
// Prometheus evaluates the inner expression of a subquery as a range
// query over the subquery's grid. The isolated lowering does exactly
// that: it lowers the inner expression as a range query over the grid's
// instants in Prometheus's own frame, split into runs of at most
// [maxRangeSpan] as [lowerRangeInParts] splits a long range query, each
// run fitted and lowered on its own ([lowerFitted]), in a frame of its
// own when it needs one. Each run reports its rows at the grid's instants
// in the frame of the region enclosing sub ([lowerCtx.rootFrame]), and the
// runs' union is the subquery's matrix, shaped as every grid-evaluated
// subquery is ([subqueryAnchorShape]).
//
// The runs' plans are sealed: the enclosing lowering moves none of their
// reads into its own frame ([frameStorage]).
func lowerIsolatedSubquery(sub *parser.SubqueryExpr, ev promEval, step time.Duration, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	grid, state, err := subqueryGridCtx(sub, step, ctx)
	if err != nil {
		return nil, err
	}
	inner := sub.Expr
	if ev.req != nil {
		inner = resolveRequestPins(inner, ev.req[0], ev.req[1])
	}
	first := grid.start.UnixMilli() - ctx.shiftMillis
	last := grid.end.UnixMilli() - ctx.shiftMillis
	stepMillis := step.Milliseconds()
	// A step longer than a run evaluates one instant per run.
	runSteps := max(maxRangeSpan.Milliseconds()/stepMillis, 1)
	var arms []chplan.Node
	for from := first; from <= last; from += runSteps * stepMillis {
		to := min(from+(runSteps-1)*stepMillis, last)
		run := grid
		run.start, run.end = time.UnixMilli(from).UTC(), time.UnixMilli(to).UTC()
		run.request, run.domain, run.shiftMillis, run.guards = nil, nil, 0, nil
		run.isolatedEval, run.rootFrame, run.run = &ev, ctx.shiftMillis, true
		// A run records its guards on a sink of its own, whose plans are
		// sealed before they join the request's.
		var guards []ScalarGuard
		if ctx.guards != nil {
			run.guards = &guards
		}
		plan, err := lowerFitted(inner, s, run)
		if err != nil {
			return nil, err
		}
		arm := subqueryAnchorShape(plan, s)
		ctx.domain.seal(arm)
		for _, g := range guards {
			ctx.domain.seal(g.Plan)
			*ctx.guards = append(*ctx.guards, g)
		}
		arms = append(arms, arm)
	}
	out := arms[0]
	// A union of one run answers that run's rows: a lone run is used as
	// it is only to keep the plan small.
	if len(arms) > 1 {
		out = &chplan.UnionAll{Inputs: arms}
	}
	if state == subqueryGridEmpty {
		out = emptySubqueryGrid(out)
	}
	return out, nil
}
