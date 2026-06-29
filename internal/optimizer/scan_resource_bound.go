package optimizer

import "github.com/tsouza/cerberus/internal/chplan"

// RequireScanResourceBound is the must-run, verify-only analyzer rule that acts
// as an EARLY signal for the spans-scan resource-bound invariant. The sufficient
// enforcement is the chsql.Emit chokepoint (chplan.RequireSpansScansBounded over
// the whole tree, plus the per-site fromSpansScan gates for the emitter-
// synthetic recursive scans); this rule catches the highest-value regression —
// a broken lock-step between a bounded nested-set numbering walk and its
// trace-scope gate — at plan-build time rather than at emit.
//
// It is purely a fact-lift: search_limit.go's stampNestedSetTraceLimit sets a
// NestedSetAnnotate's TraceLimit > 0 and pushes a matching BoundedTraceScope
// onto the row-source leaves IN LOCK-STEP (both under the same
// inputGuaranteesRootInResult precondition). If a NestedSetAnnotate reaches the
// optimizer with TraceLimit > 0 but no BoundedTraceScope on its input, the two
// bounds drifted apart — the numbering scope would be bounded while the row
// source is not (or vice versa), stranding kept rows at the 0/0/0 LEFT-JOIN
// default and unbounding the structural closures. That is a real bug, so the
// rule fails closed.
//
// It mutates nothing (trivially idempotent) and reads no ctx / schema. The
// TraceLimit == 0 numbering shapes (single-trace /traces/{id}, non-search
// traceScopeFrag supersets) carry no such fact and are left untouched — this
// rule never invents a bound a query was not supposed to have.
type RequireScanResourceBound struct{}

func (RequireScanResourceBound) Name() string { return "require-scan-resource-bound" }

func (RequireScanResourceBound) isAnalyzerRule() {}

func (RequireScanResourceBound) Apply(n chplan.Node) (chplan.Node, bool) {
	nsa, ok := n.(*chplan.NestedSetAnnotate)
	if !ok || nsa.TraceLimit <= 0 {
		return n, false
	}
	if !inputCarriesBoundedTraceScope(nsa.Input, nsa.TraceLimit) {
		panic(&chplan.ScanResourceBoundViolation{Table: nsa.SpansTable})
	}
	return n, false
}

// inputCarriesBoundedTraceScope reports whether the numbering walk's row source
// carries a BoundedTraceScope conjunct with the same TraceLimit — the lock-step
// partner of NestedSetAnnotate.TraceLimit.
func inputCarriesBoundedTraceScope(input chplan.Node, limit int64) bool {
	found := false
	chplan.Walk(input, func(node chplan.Node) bool {
		f, ok := node.(*chplan.Filter)
		if !ok {
			return true
		}
		chplan.InspectExpr(f.Predicate, func(e chplan.Expr) bool {
			if bts, ok := e.(*chplan.BoundedTraceScope); ok && bts.TraceLimit == limit {
				found = true
			}
			return true
		})
		return true
	})
	return found
}
