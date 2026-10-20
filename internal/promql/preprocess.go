package promql

import (
	"fmt"
	"time"

	"github.com/prometheus/prometheus/promql/parser"
)

// preprocessQuery returns expr as Prometheus's engine evaluates it for a
// request over [start, end] at step, before the lowering sees it:
//
//   - `start()`, `end()`, `range()` and `step()` are folded to the
//     request's start, end, span and step, in seconds, wherever they sit,
//     as foldQueryContextFunctions folds them once per query. A subquery
//     grid, a frame shift or a run of a split range never changes what
//     they name. Without a request range ([Lower]) `start()` and `end()`
//     name no instant and are left for the lowering to reject.
//   - A subquery's range, offset and step are truncated to whole
//     milliseconds, as Prometheus's subqueryTimeRange reads them
//     (durationMilliseconds): the step is the interval the inner
//     expression is evaluated at, the range and offset place the grid.
//
// A subquery whose step truncates to zero milliseconds is rejected:
// Prometheus aligns the inner evaluation's start by dividing by that
// interval and fails the query on the division by zero.
//
// expr itself is never modified: a rewritten node is a copy ([mapExpr]).
func preprocessQuery(expr parser.Expr, start, end time.Time, step time.Duration) (parser.Expr, error) {
	var err error
	out := mapExpr(expr, func(node parser.Expr) parser.Expr {
		switch n := node.(type) {
		case *parser.Call:
			if v, ok := queryContextValue(n.Func.Name, start, end, step); ok {
				return &parser.NumberLiteral{Val: v, PosRange: n.PosRange}
			}
		case *parser.SubqueryExpr:
			if err == nil {
				err = checkSubqueryStep(n.Step)
			}
			cp := *n
			cp.Range = n.Range.Truncate(time.Millisecond)
			cp.OriginalOffset = n.OriginalOffset.Truncate(time.Millisecond)
			cp.Offset = n.Offset.Truncate(time.Millisecond)
			cp.Step = n.Step.Truncate(time.Millisecond)
			if cp != *n {
				return &cp
			}
		}
		return node
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// queryContextValue returns the value Prometheus folds the query-context
// function name to for a request over [start, end] at step, and whether
// name is one preprocessQuery folds. `start()` and `end()` are the
// request's start and end in Unix seconds, truncated to whole
// milliseconds as the engine reads them (timestamp.FromTime); an instant
// query's start and end are both its evaluation time. `range()` is the
// span in seconds; `step()` is the step in seconds, or 0 for an instant
// query, whose start and end coincide.
func queryContextValue(name string, start, end time.Time, step time.Duration) (float64, bool) {
	switch name {
	case "start":
		return unixSeconds(start)
	case "end":
		return unixSeconds(end)
	case "range":
		return end.Sub(start).Seconds(), true
	case "step":
		if start.Equal(end) {
			return 0, true
		}
		return step.Seconds(), true
	}
	return 0, false
}

// unixSeconds returns t in Unix seconds at millisecond resolution, and
// false for the zero time, which names no request instant.
func unixSeconds(t time.Time) (float64, bool) {
	if t.IsZero() {
		return 0, false
	}
	return float64(t.UnixMilli()) / millisPerSecond, true
}

// checkSubqueryStep rejects a subquery step that is positive but under a
// millisecond, which truncates to an evaluation interval of zero
// milliseconds. A step of zero is a subquery that names none, which
// evaluates at the default step.
func checkSubqueryStep(step time.Duration) error {
	if step > 0 && step < time.Millisecond {
		return fmt.Errorf("promql: subquery step %s is under a millisecond, the resolution Prometheus evaluates a subquery at, so its interval is zero", step)
	}
	return nil
}

// mapExpr returns e with fn applied to every node, children first: fn
// receives a node whose children are already mapped and returns the node
// to put in its place, a copy of any node it changes. A node whose
// subtree fn leaves as it is is returned as it is, and e itself is never
// modified.
func mapExpr(e parser.Expr, fn func(parser.Expr) parser.Expr) parser.Expr {
	switch n := e.(type) {
	case *parser.MatrixSelector:
		if vs := mapExpr(n.VectorSelector, fn); vs != n.VectorSelector {
			cp := *n
			cp.VectorSelector = vs
			e = &cp
		}
	case *parser.SubqueryExpr:
		if inner := mapExpr(n.Expr, fn); inner != n.Expr {
			cp := *n
			cp.Expr = inner
			e = &cp
		}
	case *parser.Call:
		if args := mapExprs(n.Args, fn); args != nil {
			cp := *n
			cp.Args = args
			e = &cp
		}
	case *parser.AggregateExpr:
		inner, param := mapExpr(n.Expr, fn), n.Param
		if n.Param != nil {
			param = mapExpr(n.Param, fn)
		}
		if inner != n.Expr || param != n.Param {
			cp := *n
			cp.Expr, cp.Param = inner, param
			e = &cp
		}
	case *parser.BinaryExpr:
		if lhs, rhs := mapExpr(n.LHS, fn), mapExpr(n.RHS, fn); lhs != n.LHS || rhs != n.RHS {
			cp := *n
			cp.LHS, cp.RHS = lhs, rhs
			e = &cp
		}
	case *parser.ParenExpr:
		if inner := mapExpr(n.Expr, fn); inner != n.Expr {
			cp := *n
			cp.Expr = inner
			e = &cp
		}
	case *parser.UnaryExpr:
		if inner := mapExpr(n.Expr, fn); inner != n.Expr {
			cp := *n
			cp.Expr = inner
			e = &cp
		}
	case *parser.StepInvariantExpr:
		if inner := mapExpr(n.Expr, fn); inner != n.Expr {
			cp := *n
			cp.Expr = inner
			e = &cp
		}
	}
	return fn(e)
}

// mapExprs returns args with [mapExpr] applied to each, or nil when no
// argument changes.
func mapExprs(args parser.Expressions, fn func(parser.Expr) parser.Expr) parser.Expressions {
	var out parser.Expressions
	for i, a := range args {
		mapped := mapExpr(a, fn)
		if mapped == a {
			continue
		}
		if out == nil {
			out = append(parser.Expressions(nil), args...)
		}
		out[i] = mapped
	}
	return out
}
