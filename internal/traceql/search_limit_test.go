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

// TestSearchLimit_StampsDrilldownShape verifies the Drilldown structure
// query (union with a root-filter arm) gets its NestedSetAnnotate bounded
// to the search limit, so the numbering walk only numbers the returned N.
func TestSearchLimit_StampsDrilldownShape(t *testing.T) {
	t.Parallel()
	ns := findNestedSetAnnotate(t, lowerSearch(t, drilldownStructureQuery, 200))
	if ns.TraceLimit != 200 {
		t.Fatalf("TraceLimit = %d, want 200 (Drilldown shape must be bounded)", ns.TraceLimit)
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
