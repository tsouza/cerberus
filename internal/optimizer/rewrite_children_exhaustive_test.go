package optimizer

import (
	"reflect"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// This file pins the EXHAUSTIVENESS contract of rewriteChildren (see the
// doc comment on rewriteChildren in rule.go).
//
// Background: rewriteChildren is the single recursion arm the optimizer
// Driver uses to walk a chplan tree bottom-up. Every concrete chplan.Node
// type needs an arm that recurses into its Children(); any type that falls
// through to the default `return n, false` becomes an OPAQUE LEAF and
// silently DISABLES every optimizer rule (predicate pushdown, PREWHERE,
// projection-pushdown, MV substitution, constant-fold) on its whole
// subtree. That is exactly the perf bug this test guards against
// reintroducing: the walk was previously total for only 11 of the 26 node
// types, so e.g. the TraceQL MetricsAggregate subtree was never optimized.
//
// The contract enforced here:
//
//   - GENUINE LEAVES (Children() always nil — Scan, OneRow, StepGrid):
//     rewriteChildren returns the node unchanged. Recursing would be a
//     no-op, so `changed == false` is correct and required.
//
//   - INTERIOR NODES (Children() non-empty): rewriteChildren MUST recurse
//     into every child. We feed an fn that rewrites a sentinel child and
//     assert (a) changed == true and (b) every child in the returned node
//     is the rewritten sentinel — proving the arm threaded fn through each
//     child slot rather than dropping it.
//
// A future new chplan.Node type that lacks an arm will fall through to the
// default and be returned unchanged; if it is added to the table below
// (which it must be — see the count guard) the interior-node assertion
// fires. The count guard makes "forgot to add the new type to the table"
// itself a failure.

// sentinelChild is the recognisable child planted in every interior node
// in the table. rewriteChildrenFn rewrites exactly this node.
func sentinelChild() *chplan.Scan { return &chplan.Scan{Table: "SENTINEL_CHILD"} }

// rewrittenChild is what the recursion fn turns sentinelChild into. Its
// presence in the output proves fn reached that child slot.
func rewrittenChild() *chplan.Scan { return &chplan.Scan{Table: "REWRITTEN_CHILD"} }

// rewriteChildrenFn is the recursion function handed to rewriteChildren.
// It rewrites the sentinel child (returning changed=true) and leaves
// everything else alone. This mimics what applyToTree's closure does when
// a rule fires somewhere below.
func rewriteChildrenFn(c chplan.Node) (chplan.Node, bool) {
	if s, ok := c.(*chplan.Scan); ok && s.Table == "SENTINEL_CHILD" {
		return rewrittenChild(), true
	}
	return c, false
}

// nodeExhaustivenessCase is one row in the table: a freshly-built node and
// whether it is a genuine leaf (Children() always nil).
type nodeExhaustivenessCase struct {
	name string
	node chplan.Node
	leaf bool
}

// allNodeCases enumerates EVERY concrete chplan.Node implementation. Adding
// a new Node type to chplan requires adding a row here (the count guard in
// TestRewriteChildren_TableCoversEveryNodeType fails otherwise) and an arm
// to rewriteChildren (the interior-node assertion in
// TestRewriteChildren_Exhaustive fails otherwise).
//
// Interior nodes plant sentinelChild() in every Node-typed slot so the
// recursion assertion can detect a dropped child. Leaves carry no
// children.
func allNodeCases() []nodeExhaustivenessCase {
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	return []nodeExhaustivenessCase{
		// --- genuine leaves ---
		{"Scan", &chplan.Scan{Table: "t"}, true},
		{"OneRow", &chplan.OneRow{}, true},
		{"StepGrid", &chplan.StepGrid{Start: now, End: now, Step: time.Minute}, true},

		// --- single-Input interior nodes ---
		{"Filter", &chplan.Filter{Input: sentinelChild(), Predicate: &chplan.LitBool{V: true}}, false},
		{"Project", &chplan.Project{Input: sentinelChild()}, false},
		{"NestedSetAnnotate", &chplan.NestedSetAnnotate{Input: sentinelChild()}, false},
		{"Aggregate", &chplan.Aggregate{Input: sentinelChild()}, false},
		{"RangeWindow", &chplan.RangeWindow{Input: sentinelChild(), Func: "rate"}, false},
		{"RangeWindowNative", &chplan.RangeWindowNative{Input: sentinelChild(), Func: "rate"}, false},
		{"AbsentOverTime", &chplan.AbsentOverTime{Input: sentinelChild()}, false},
		{"Limit", &chplan.Limit{Input: sentinelChild(), Count: 1}, false},
		{"OrderBy", &chplan.OrderBy{Input: sentinelChild()}, false},
		{"TopK", &chplan.TopK{Input: sentinelChild(), K: 1, SortExpr: &chplan.ColumnRef{Name: "Value"}}, false},
		{"RangeLWR", &chplan.RangeLWR{Input: sentinelChild()}, false},
		{"RangeBucketFanout", &chplan.RangeBucketFanout{Input: sentinelChild()}, false},
		{"HistogramQuantile", &chplan.HistogramQuantile{Input: sentinelChild(), Phi: 0.5}, false},
		{"HistogramQuantileNative", &chplan.HistogramQuantileNative{Input: sentinelChild(), Phi: 0.5}, false},
		{"MetricsAggregate", &chplan.MetricsAggregate{Inner: sentinelChild()}, false},
		{"MetricsHistogramOverTime", &chplan.MetricsHistogramOverTime{Inner: sentinelChild()}, false},
		{"MetricsSecondStage", &chplan.MetricsSecondStage{Input: sentinelChild()}, false},

		// --- MetricsCompare: Inner (+ optional RootLookup). Plant the
		//     sentinel in BOTH so the recursion assertion exercises the
		//     two-child path. ---
		{"MetricsCompare", &chplan.MetricsCompare{Inner: sentinelChild(), RootLookup: sentinelChild()}, false},

		// --- multi-arm interior nodes ---
		{"UnionAll", &chplan.UnionAll{Inputs: []chplan.Node{sentinelChild(), sentinelChild()}}, false},
		{"NaryVectorSetOp", &chplan.NaryVectorSetOp{Arms: []chplan.Node{sentinelChild(), sentinelChild()}}, false},

		// --- Left/Right binary interior nodes ---
		{"CrossJoin", &chplan.CrossJoin{Left: sentinelChild(), Right: sentinelChild()}, false},
		{"StructuralJoin", &chplan.StructuralJoin{Left: sentinelChild(), Right: sentinelChild()}, false},
		{"VectorJoin", &chplan.VectorJoin{Left: sentinelChild(), Right: sentinelChild()}, false},
		{"VectorSetOp", &chplan.VectorSetOp{Left: sentinelChild(), Right: sentinelChild()}, false},
		{"SetOperation", &chplan.SetOperation{Left: sentinelChild(), Right: sentinelChild()}, false},
	}
}

// expectedNodeTypeCount is the number of concrete chplan.Node
// implementations. Cross-checked against
// `grep -rn 'planNode()' internal/chplan/*.go`. Bump this (and add a table
// row + a rewriteChildren arm) when a new Node type lands.
const expectedNodeTypeCount = 28

// TestRewriteChildren_TableCoversEveryNodeType is the count guard. If a new
// chplan.Node type is added without a corresponding allNodeCases() row, the
// counts diverge and this fails, forcing the author to confront the
// exhaustiveness contract (and, via TestRewriteChildren_Exhaustive, add the
// recursion arm).
func TestRewriteChildren_TableCoversEveryNodeType(t *testing.T) {
	t.Parallel()
	cases := allNodeCases()
	if len(cases) != expectedNodeTypeCount {
		t.Fatalf("allNodeCases() has %d rows, expected %d concrete chplan.Node types; "+
			"a Node type was added/removed without updating this table", len(cases), expectedNodeTypeCount)
	}
	seen := make(map[reflect.Type]string, len(cases))
	for _, c := range cases {
		ty := reflect.TypeOf(c.node)
		if prev, dup := seen[ty]; dup {
			t.Errorf("duplicate node type %v in table (rows %q and %q)", ty, prev, c.name)
		}
		seen[ty] = c.name
	}
	if len(seen) != expectedNodeTypeCount {
		t.Fatalf("table covers %d distinct node types, expected %d", len(seen), expectedNodeTypeCount)
	}
}

// TestRewriteChildren_Exhaustive is the core contract test. For each node
// type it asserts the leaf-vs-interior behaviour described at the top of
// the file.
func TestRewriteChildren_Exhaustive(t *testing.T) {
	t.Parallel()
	for _, c := range allNodeCases() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			// Sanity: the table's `leaf` flag must agree with the node's
			// own Children() — a leaf has no children, an interior node has
			// at least one.
			children := c.node.Children()
			if c.leaf && len(children) != 0 {
				t.Fatalf("%s is marked leaf but Children() returned %d children", c.name, len(children))
			}
			if !c.leaf && len(children) == 0 {
				t.Fatalf("%s is marked interior but Children() returned 0 children", c.name)
			}

			out, changed := rewriteChildren(c.node, rewriteChildrenFn)

			if c.leaf {
				if changed {
					t.Errorf("%s: genuine leaf must be returned unchanged, got changed=true", c.name)
				}
				if out != c.node {
					t.Errorf("%s: genuine leaf must return the same node pointer", c.name)
				}
				return
			}

			// Interior node: every child must have been rewritten. This is
			// the assertion that catches an OPAQUE-LEAF regression — a
			// missing arm returns (n, false) and fails both checks.
			if !changed {
				t.Fatalf("%s: interior node returned changed=false; rewriteChildren did NOT recurse into "+
					"its children — every optimizer rule is silently disabled beneath this node type", c.name)
			}
			outChildren := out.Children()
			if len(outChildren) != len(children) {
				t.Fatalf("%s: child count changed %d -> %d after rewrite", c.name, len(children), len(outChildren))
			}
			for i, ch := range outChildren {
				s, ok := ch.(*chplan.Scan)
				if !ok || s.Table != "REWRITTEN_CHILD" {
					t.Errorf("%s: child[%d] was not rewritten (got %#v); rewriteChildren dropped fn on this "+
						"child slot", c.name, i, ch)
				}
			}
		})
	}
}

// TestRewriteChildren_NoChangeReturnsOriginal pins the clone-on-change
// contract for interior nodes: when fn reports no change, the original
// node pointer is handed back (no needless allocation). This is the
// invariant the FixedPoint driver relies on to detect its fixpoint.
func TestRewriteChildren_NoChangeReturnsOriginal(t *testing.T) {
	t.Parallel()
	noop := func(c chplan.Node) (chplan.Node, bool) { return c, false }
	for _, c := range allNodeCases() {
		if c.leaf {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			out, changed := rewriteChildren(c.node, noop)
			if changed {
				t.Errorf("%s: no-op fn must report changed=false", c.name)
			}
			if out != c.node {
				t.Errorf("%s: no-op fn must return the original node pointer", c.name)
			}
		})
	}
}
