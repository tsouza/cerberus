package traceql

import (
	"slices"

	"github.com/prometheus/prometheus/model/labels"

	"github.com/tsouza/cerberus/internal/chplan"
)

// rootlessRootIdentity is the value reference Tempo gives rootName and
// rootServiceName on a trace that has no root span: vparquet4's
// traceToParquet resets the trace-level RootSpanName / RootServiceName
// columns to the empty string and only overwrites them when it finds a
// root span (tempodb/encoding/vparquet4/schema.go), and the trace
// collector surfaces whatever the column holds as a non-nil string Static
// (block_traceql.go, the columnPathRootSpanName / columnPathRootServiceName
// arms). The condition is then evaluated against that string like any
// other — so `!= "x"`, `= ""` and `!~ "x"` all match a rootless trace.
const rootlessRootIdentity = ""

// rootlessTraceMatches reports whether pred — a predicate over
// traceScopedValueAlias, as lowerTraceScopedBinary, lowerInOperation and
// lowerRegexMatchArray build it — holds when the alias is
// rootlessRootIdentity, i.e. for a trace with no root span.
//
// The predicate is evaluated here, at plan time, rather than in the
// emitted SQL, so the lowering can pick the membership form that keeps
// the root-only GROUP BY (traceScopedMembership). Regexes use the
// anchored matcher reference Tempo's own regex evaluator is built on
// (labels.NewFastRegexMatcher), the same semantics chsql's anchored
// match() reproduces in ClickHouse.
//
// A shape it cannot decide reports false, which keeps the plain
// `TraceId IN (...)` form. Two exist: a pattern that does not compile,
// which validation rejects before lowering, and a per-span operand
// (`rootName = name`), whose comparison the per-trace subquery cannot
// evaluate in either form, tracked in #4012.
func rootlessTraceMatches(pred chplan.Expr) bool {
	v, ok := evalRootlessPredicate(pred)
	return ok && v
}

// evalRootlessPredicate evaluates pred with traceScopedValueAlias bound to
// rootlessRootIdentity; ok is false for any shape outside the closed set
// the trace-scoped lowerings produce.
func evalRootlessPredicate(pred chplan.Expr) (value, ok bool) {
	switch p := pred.(type) {
	case *chplan.FuncCall:
		if p.Fn != chplan.FnNot || len(p.Args) != 1 {
			return false, false
		}
		v, ok := evalRootlessPredicate(p.Args[0])
		return !v, ok
	case *chplan.InList:
		if !isTraceScopedValueRef(p.Left) {
			return false, false
		}
		elems := make([]string, 0, len(p.List))
		for _, e := range p.List {
			lit, ok := e.(*chplan.LitString)
			if !ok {
				return false, false
			}
			elems = append(elems, lit.V)
		}
		return slices.Contains(elems, rootlessRootIdentity), true
	case *chplan.Binary:
		switch p.Op {
		case chplan.OpAnd, chplan.OpOr:
			l, lok := evalRootlessPredicate(p.Left)
			r, rok := evalRootlessPredicate(p.Right)
			if !lok || !rok {
				return false, false
			}
			if p.Op == chplan.OpAnd {
				return l && r, true
			}
			return l || r, true
		}
		lit, isLit := p.Right.(*chplan.LitString)
		if !isTraceScopedValueRef(p.Left) || !isLit {
			return false, false
		}
		return compareRootless(p.Op, lit.V)
	}
	return false, false
}

// compareRootless evaluates `rootlessRootIdentity <op> rhs` for one
// string comparison operator.
func compareRootless(op chplan.BinaryOp, rhs string) (value, ok bool) {
	v := rootlessRootIdentity
	switch op {
	case chplan.OpEq:
		return v == rhs, true
	case chplan.OpNe:
		return v != rhs, true
	case chplan.OpLt:
		return v < rhs, true
	case chplan.OpLe:
		return v <= rhs, true
	case chplan.OpGt:
		return v > rhs, true
	case chplan.OpGe:
		return v >= rhs, true
	case chplan.OpMatch, chplan.OpNotMatch:
		m, err := labels.NewFastRegexMatcher(rhs)
		if err != nil {
			return false, false
		}
		return m.MatchString(v) == (op == chplan.OpMatch), true
	}
	return false, false
}

// isTraceScopedValueRef reports whether e is the bare reference to the
// per-trace value column traceScopedValueNode projects.
func isTraceScopedValueRef(e chplan.Expr) bool {
	ref, ok := e.(*chplan.ColumnRef)
	return ok && ref.Name == traceScopedValueAlias
}
