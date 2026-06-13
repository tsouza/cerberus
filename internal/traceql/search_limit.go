// See aggregate.go for the no-reflection / no-pointer-aliasing rule
// covering this file.

package traceql

import (
	"context"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// searchTraceLimitKey types the context value carrying /api/search's
// per-request `limit` (max trace summaries the response keeps) down into
// lowering. A dedicated unexported key type avoids collisions with any
// other context value.
type searchTraceLimitKey struct{}

// WithSearchTraceLimit returns a context carrying the /api/search `limit`
// (max returned trace summaries). lowerRoot reads it to bound the
// nested-set numbering walk to exactly the traces the response will keep
// (see stampNestedSetTraceLimit). n <= 0 leaves the numbering unbounded —
// the behaviour for every caller that doesn't return a bounded trace set
// (the metrics pipelines, the spec/property harnesses, /traces/{id}).
//
// The Tempo /api/search + gRPC Search handlers wrap their request context
// with this before calling Engine.Query / Engine.QueryCursor; the value
// rides through Lang.Parse → traceql.Lower (both already ctx-aware) to the
// stamping pass, so no lowering function signature changes.
func WithSearchTraceLimit(ctx context.Context, n int) context.Context {
	if n <= 0 {
		return ctx
	}
	return context.WithValue(ctx, searchTraceLimitKey{}, n)
}

// searchTraceLimit recovers the value WithSearchTraceLimit stored, or 0
// when unset (unbounded).
func searchTraceLimit(ctx context.Context) int64 {
	if n, ok := ctx.Value(searchTraceLimitKey{}).(int); ok && n > 0 {
		return int64(n)
	}
	return 0
}

// stampNestedSetTraceLimit walks the lowered plan and sets TraceLimit on
// every NestedSetAnnotate whose input plan guarantees each returned trace's
// root span is in the result set — the precondition under which ranking the
// numbering scope by root-span Timestamp equals /api/search's result-min-
// Timestamp ranking, so the bounded scope keeps exactly the traces
// TruncateSummaries keeps (exact parity). For any other shape the annotate
// is left untouched (TraceLimit stays 0, numbering byte-identical to today).
//
// limit <= 0 (no /api/search limit on the context — metrics, tests, the
// property harness) is a no-op: the plan is returned unchanged.
//
// The walk only descends the node families a select()-with-nested-set plan
// can produce above the NestedSetAnnotate: the Project the select lowering
// emits, and any chained second select()'s Project. NestedSetAnnotate never
// appears under a metrics pipeline (select is span-shaped, not metric), so
// the metric node families are deliberately not traversed.
func stampNestedSetTraceLimit(plan chplan.Node, limit int64, s schema.Traces) chplan.Node {
	if limit <= 0 || plan == nil {
		return plan
	}
	switch v := plan.(type) {
	case *chplan.Project:
		v.Input = stampNestedSetTraceLimit(v.Input, limit, s)
	case *chplan.NestedSetAnnotate:
		if inputGuaranteesRootInResult(v.Input, s.ParentSpanIDColumn) {
			v.TraceLimit = limit
		}
	}
	return plan
}

// inputGuaranteesRootInResult reports whether every trace n emits is
// guaranteed to carry its own root span (ParentSpanId = ”) in the result —
// the precondition for bounding the numbering walk by root-span Timestamp.
//
// The recognised shape is the Grafana Traces Drilldown structure-tab input:
// a `||` SetOperation one of whose arms is a bare root-span filter
// (`{ nestedSetParent < 0 }`, lowered to Filter(ParentSpanId = "") over a
// Scan, optionally re-projected). The union re-adds every matched trace's
// root, so result-min(Timestamp) per trace == root.Timestamp. This is the
// only OOM-prone shape; gating on it keeps the bound exact-parity-safe by
// construction and leaves all other selects unbounded.
func inputGuaranteesRootInResult(n chplan.Node, parentSpanIDCol string) bool {
	set, ok := n.(*chplan.SetOperation)
	if !ok || set.Op != chplan.SetUnion {
		return false
	}
	return isRootSpanFilter(set.Left, parentSpanIDCol) || isRootSpanFilter(set.Right, parentSpanIDCol)
}

// isRootSpanFilter reports whether n is a root-span filter
// (`ParentSpanId = ”`) over a Scan, looking through a bare passthrough
// Project (the union-arm-alignment lowering re-projects the plain arm to
// match the structural arm's column list without changing its rows).
func isRootSpanFilter(n chplan.Node, parentSpanIDCol string) bool {
	if p, ok := n.(*chplan.Project); ok {
		n = p.Input
	}
	f, ok := n.(*chplan.Filter)
	if !ok {
		return false
	}
	if _, ok := f.Input.(*chplan.Scan); !ok {
		return false
	}
	b, ok := f.Predicate.(*chplan.Binary)
	if !ok || b.Op != chplan.OpEq {
		return false
	}
	col, ok := b.Left.(*chplan.ColumnRef)
	if !ok {
		return false
	}
	lit, ok := b.Right.(*chplan.LitString)
	return ok && col.Name == parentSpanIDCol && lit.V == ""
}
