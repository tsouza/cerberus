package traceql

import (
	"context"
	"testing"

	tempoql "github.com/grafana/tempo/pkg/traceql"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// lowerSearch parses + lowers a TraceQL query with the given /api/search
// limit threaded through the context, mirroring the handler path.
func lowerSearch(t *testing.T, query string, limit int) chplan.Node {
	t.Helper()
	expr, err := tempoql.Parse(query)
	if err != nil {
		t.Fatalf("parse %q: %v", query, err)
	}
	ctx := WithSearchTraceLimit(context.Background(), limit)
	plan, err := Lower(ctx, expr, schema.DefaultOTelTraces())
	if err != nil {
		t.Fatalf("lower %q: %v", query, err)
	}
	return plan
}

// findNestedSetAnnotate descends the (small) plan shape a select() with
// nested-set intrinsics produces and returns the NestedSetAnnotate node.
func findNestedSetAnnotate(t *testing.T, n chplan.Node) *chplan.NestedSetAnnotate {
	t.Helper()
	for n != nil {
		if ns, ok := n.(*chplan.NestedSetAnnotate); ok {
			return ns
		}
		ch := n.Children()
		if len(ch) == 0 {
			break
		}
		n = ch[0]
	}
	t.Fatalf("no NestedSetAnnotate in plan")
	return nil
}

const drilldownStructureQuery = `({ nestedSetParent < 0 } &>> { kind = server }) || ({ nestedSetParent < 0 }) | select(status, name, nestedSetParent, nestedSetLeft, nestedSetRight)`

// boundedTraceGates collects every chplan.BoundedTraceScope conjunct reachable
// from the leaf Filter predicates of n (walking the row-source spine + the
// conjunction tree of each Filter). Used to assert the row-source bound is
// stamped (or declined) in lock-step with NestedSetAnnotate.TraceLimit.
func boundedTraceGates(n chplan.Node) []*chplan.BoundedTraceScope {
	var out []*chplan.BoundedTraceScope
	var walkNode func(chplan.Node)
	walkNode = func(n chplan.Node) {
		if n == nil {
			return
		}
		if f, ok := n.(*chplan.Filter); ok {
			chplan.InspectExpr(f.Predicate, func(e chplan.Expr) bool {
				if g, ok := e.(*chplan.BoundedTraceScope); ok {
					out = append(out, g)
				}
				return true
			})
		}
		for _, c := range n.Children() {
			walkNode(c)
		}
	}
	walkNode(n)
	return out
}

// TestSearchLimit_StampsDrilldownShape verifies the Drilldown structure
// query (union with a root-filter arm) gets its NestedSetAnnotate bounded
// to the search limit, so the numbering walk only numbers the returned N.
func TestSearchLimit_StampsDrilldownShape(t *testing.T) {
	t.Parallel()
	ns := findNestedSetAnnotate(t, lowerSearch(t, drilldownStructureQuery, 200))
	if ns.TraceLimit != 200 {
		t.Fatalf("TraceLimit = %d, want 200 (Drilldown shape must be bounded)", ns.TraceLimit)
	}
	// The row source must be bounded too: one BoundedTraceScope gate per leaf
	// scan. The structure shape has three leaves (the `&>>` arm's two scans +
	// the bare-root `||` arm), so the numbering bound AND the closure bound
	// both fire. Every gate must carry the SAME params as the annotate node,
	// so the leaf gate and the numbering scope emit the identical top-N set.
	gates := boundedTraceGates(ns.Input)
	if len(gates) != 3 {
		t.Fatalf("got %d BoundedTraceScope leaf gates, want 3 (one per row-source leaf scan)", len(gates))
	}
	want := &chplan.BoundedTraceScope{
		SpansTable:         ns.SpansTable,
		TraceIDColumn:      ns.TraceIDColumn,
		ParentSpanIDColumn: ns.ParentSpanIDColumn,
		TimestampColumn:    ns.TimestampColumn,
		TraceLimit:         ns.TraceLimit,
	}
	for i, g := range gates {
		if !g.Equal(want) {
			t.Errorf("gate[%d] = %+v, want %+v (must match the annotate node so numbering==row-source set)", i, g, want)
		}
	}
}

// TestSearchLimit_UnboundedWithoutLimit verifies a query lowered with no
// search limit on the context (metrics, tests, property harness) keeps the
// numbering unbounded — byte-identical to today.
func TestSearchLimit_UnboundedWithoutLimit(t *testing.T) {
	t.Parallel()
	ns := findNestedSetAnnotate(t, lowerSearch(t, drilldownStructureQuery, 0))
	if ns.TraceLimit != 0 {
		t.Fatalf("TraceLimit = %d, want 0 (no ctx limit ⇒ unbounded)", ns.TraceLimit)
	}
	// No limit ⇒ no row-source gate either; the two bounds are stamped in
	// lock-step inside the same precondition branch.
	if gates := boundedTraceGates(ns.Input); len(gates) != 0 {
		t.Fatalf("got %d BoundedTraceScope gates, want 0 (no ctx limit ⇒ unbounded row source)", len(gates))
	}
}

// TestSearchLimit_NonGuaranteedRootStaysUnbounded verifies a select() over
// a shape that does NOT guarantee each trace's root is in the result (a
// plain non-root filter, no union root-readd arm) is left unbounded — the
// root-Timestamp ranking would not match TruncateSummaries' result-min
// ranking, so bounding it could drop a kept trace. Safe default: no bound.
func TestSearchLimit_NonGuaranteedRootStaysUnbounded(t *testing.T) {
	t.Parallel()
	// `{ nestedSetLeft > 0 }` is a position filter over a single scan —
	// not a union with a root-readd arm — so the gate must decline.
	q := `{ nestedSetLeft > 0 } | select(nestedSetParent, nestedSetLeft, nestedSetRight)`
	ns := findNestedSetAnnotate(t, lowerSearch(t, q, 200))
	if ns.TraceLimit != 0 {
		t.Fatalf("TraceLimit = %d, want 0 (non-root-guaranteed shape must stay unbounded)", ns.TraceLimit)
	}
	// The gate must decline on the SAME precondition as TraceLimit — the two
	// can never fire on different shapes (a bounded row source under an
	// unbounded numbering would strand rows at 0/0/0).
	if gates := boundedTraceGates(ns.Input); len(gates) != 0 {
		t.Fatalf("got %d BoundedTraceScope gates, want 0 (non-root-guaranteed shape must stay unbounded)", len(gates))
	}
}

// TestSearchLimit_RootlessAdmittingUnionStaysUnbounded pins the D4b
// correctness gate: a `||` union whose non-bare-root arm is a plain
// `{ kind = server }` filter (NOT a root-seeded structural join) can admit
// rootless traces to the result via that arm. Bounding such a shape would let
// the root-keyed gate silently DROP those rootless traces — a wrong result,
// not a boundary reorder. inputGuaranteesRootInResult must therefore decline
// it: neither TraceLimit nor any BoundedTraceScope gate may be stamped.
func TestSearchLimit_RootlessAdmittingUnionStaysUnbounded(t *testing.T) {
	t.Parallel()
	const q = `({ nestedSetParent < 0 }) || ({ kind = server }) | select(status, nestedSetParent, nestedSetLeft, nestedSetRight)`
	ns := findNestedSetAnnotate(t, lowerSearch(t, q, 200))
	if ns.TraceLimit != 0 {
		t.Fatalf("TraceLimit = %d, want 0 (root || server admits rootless traces — must stay unbounded)", ns.TraceLimit)
	}
	if gates := boundedTraceGates(ns.Input); len(gates) != 0 {
		t.Fatalf("got %d BoundedTraceScope gates, want 0 (rootless-admitting union must not be gated)", len(gates))
	}
}

// TestWithSearchTraceLimit_NonPositiveNoOp verifies a non-positive limit
// leaves the context value unset (searchTraceLimit reads 0).
func TestWithSearchTraceLimit_NonPositiveNoOp(t *testing.T) {
	t.Parallel()
	for _, n := range []int{0, -1} {
		ctx := WithSearchTraceLimit(context.Background(), n)
		if got := searchTraceLimit(ctx); got != 0 {
			t.Errorf("searchTraceLimit after WithSearchTraceLimit(%d) = %d, want 0", n, got)
		}
	}
}
