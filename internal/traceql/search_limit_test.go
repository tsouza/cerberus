package traceql

import (
	"context"
	"testing"
	"time"

	tempoql "github.com/grafana/tempo/pkg/traceql"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// countWindowBounds walks the plan and counts Filter predicates that carry a
// time-window bound (a fromUnixTimestamp64Nano call — what tsBound emits). Used
// to assert stampSearchWindow folds the window onto the right leaves and never
// double-folds.
func countWindowBounds(n chplan.Node) int {
	if n == nil {
		return 0
	}
	count := 0
	if f, ok := n.(*chplan.Filter); ok {
		chplan.InspectExpr(f.Predicate, func(e chplan.Expr) bool {
			if c, ok := e.(*chplan.FuncCall); ok && c.Name == "fromUnixTimestamp64Nano" {
				count++
			}
			return true
		})
	}
	for _, c := range n.Children() {
		count += countWindowBounds(c)
	}
	return count
}

// lowerSearchWindowed lowers a TraceQL query with both the /api/search limit
// AND the request window threaded through ctx, mirroring the handler path.
func lowerSearchWindowed(t *testing.T, query string, limit int, start, end time.Time) chplan.Node {
	t.Helper()
	expr, err := tempoql.Parse(query)
	if err != nil {
		t.Fatalf("parse %q: %v", query, err)
	}
	ctx := WithSearchTraceLimit(context.Background(), limit)
	ctx = WithSearchWindow(ctx, start, end)
	plan, err := Lower(ctx, expr, schema.DefaultOTelTraces())
	if err != nil {
		t.Fatalf("lower %q: %v", query, err)
	}
	return plan
}

var (
	winStart = time.Unix(1782571392, 0).UTC()
	winEnd   = time.Unix(1782573192, 0).UTC()
)

// TestSearchWindow_CompoundLeavesWindowed — a `&&` compound search, which
// stampSearchTraceLimit leaves untouched, gets the window folded onto BOTH leaf
// scans (2 leaves × the >= and <= bounds = 4 fromUnixTimestamp64Nano calls).
func TestSearchWindow_CompoundLeavesWindowed(t *testing.T) {
	t.Parallel()
	plan := lowerSearchWindowed(t, `{ resource.service.name = "a" } && { span.http.status_code = 500 }`, 20, winStart, winEnd)
	if got := countWindowBounds(plan); got != 4 {
		t.Fatalf("compound && window bounds = %d, want 4 (both leaves, >= and <=)", got)
	}
}

// TestSearchWindow_MetricsGuard — stampSearchWindow is gated on limit > 0 (the
// search-not-metrics discriminator). A query lowered WITHOUT a search limit (the
// metrics / test path) must get NO window fold, even when a window is in ctx,
// so the metrics pipeline's own RangeWindow time bound is the sole authority.
func TestSearchWindow_MetricsGuard(t *testing.T) {
	t.Parallel()
	expr, err := tempoql.Parse(`{ resource.service.name = "a" } && { span.http.status_code = 500 }`)
	if err != nil {
		t.Fatal(err)
	}
	// Window in ctx but NO search limit ⇒ stampSearchWindow must no-op.
	ctx := WithSearchWindow(context.Background(), winStart, winEnd)
	plan, err := Lower(ctx, expr, schema.DefaultOTelTraces())
	if err != nil {
		t.Fatal(err)
	}
	if got := countWindowBounds(plan); got != 0 {
		t.Fatalf("no-limit (metrics-path) window bounds = %d, want 0 (stampSearchWindow must gate on limit>0)", got)
	}
}

// TestSearchWindow_PlainNotDoubleFolded — plain search is already windowed by
// stampSearchTraceLimit, which folds the window into its single Filter(Scan)
// predicate (2 bounds at the PLAN level; the SearchTraceLimit node re-emits it
// into the inner ranking subquery only at SQL-emit time, not in the plan).
// stampSearchWindow runs after it and must NOT descend SearchTraceLimit, so the
// plan count stays 2 — not 4 (a double-fold would conjoin a second window onto
// the same leaf and regress the #1109/#1110 plain-search goldens).
func TestSearchWindow_PlainNotDoubleFolded(t *testing.T) {
	t.Parallel()
	plan := lowerSearchWindowed(t, `{ resource.service.name = "a" }`, 20, winStart, winEnd)
	if got := countWindowBounds(plan); got != 2 {
		t.Fatalf("plain search window bounds = %d, want 2 (stampSearchTraceLimit fold only, no double-fold)", got)
	}
}

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

// TestSearchWindow_AggregateLeavesWindowed — GAP-3 regression. A spanset
// aggregation search (`| count() > N`) lowers to Aggregate over the span row
// source. Pre-fix, pushLeafPredicate hit `default: return n` on Aggregate and
// dropped the window → all-time scan + unbounded GROUP BY. Now the generic
// recurse folds the window onto the leaf below the aggregate: 1 leaf × (>=, <=).
func TestSearchWindow_AggregateLeavesWindowed(t *testing.T) {
	t.Parallel()
	plan := lowerSearchWindowed(t, `{ resource.service.name = "a" } | count() > 1`, 200, winStart, winEnd)
	if got := countWindowBounds(plan); got != 2 {
		t.Fatalf("aggregate window bounds = %d, want 2 (pre-fix 0 = all-time scan)", got)
	}
}

// TestSearchWindow_StructuralAggregateWindowed — the structural + aggregate
// combo: two leaves under the join, both windowed below the GROUP BY → 4.
func TestSearchWindow_StructuralAggregateWindowed(t *testing.T) {
	t.Parallel()
	plan := lowerSearchWindowed(t, `{ resource.service.name = "a" } >> { span.http.status_code = 500 } | count() > 1`, 200, winStart, winEnd)
	if got := countWindowBounds(plan); got != 4 {
		t.Fatalf("structural-aggregate window bounds = %d, want 4", got)
	}
}

// TestSearchWindow_MetricsNotFolded — a metrics-pipeline query must NOT get the
// request window folded onto its leaves even when routed with a search limit;
// metrics get their time bound from the /api/metrics/query_range RangeWindow
// wrap. The lower.go gate skips the search stamps for MetricsPipeline so the
// generic leaf recurse never touches a metrics leaf.
func TestSearchWindow_MetricsNotFolded(t *testing.T) {
	t.Parallel()
	plan := lowerSearchWindowed(t, `{ resource.service.name = "a" } | rate()`, 200, winStart, winEnd)
	if got := countWindowBounds(plan); got != 0 {
		t.Fatalf("metrics query window bounds = %d, want 0 (metrics carries its own bound)", got)
	}
}

// TestSearchWindow_GroupByAggregateWindowed — a by(...) grouping aggregate is
// windowed like the bare aggregate (same chplan.Aggregate node, same recurse).
func TestSearchWindow_GroupByAggregateWindowed(t *testing.T) {
	t.Parallel()
	plan := lowerSearchWindowed(t, `{ resource.service.name = "a" } | by(name)`, 200, winStart, winEnd)
	if got := countWindowBounds(plan); got != 2 {
		t.Fatalf("group-by aggregate window bounds = %d, want 2", got)
	}
}
