package promql

import (
	"fmt"
	"math"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// Aliases used by scalarValuePlan's one-row reduction. The `_cerb_`
// prefix mirrors absent.go's `_cerb_n` convention — nothing else in
// the pipeline writes `_cerb_*` columns, so the aliases can't collide
// with user-supplied labels.
const (
	scalarCountAlias = "_cerb_scnt"
	scalarValueAlias = "_cerb_sval"
)

// lowerScalarArg lowers a scalar-typed PromQL expression — the type
// the parser enforces for clamp bounds, `round(v, to_nearest)`,
// `quantile(phi, ...)`, `histogram_quantile(phi, ...)`,
// `predict_linear(v, t)`, `quantile_over_time(phi, v)` and
// `vector(s)` arguments — into a chplan.Expr.
//
// The scalar-typed expression space is closed by the parser's type
// checker: literals and literal arithmetic (folded by TryFoldScalar),
// ParenExpr / UnaryExpr / BinaryExpr over scalar operands, and exactly
// three scalar-returning calls — `scalar(<vector>)`, `time()`, `pi()`.
// Vector-typed subtrees can appear only inside `scalar()`.
//
// Shapes:
//
//   - Anything TryFoldScalar reduces → LitFloat (the common literal
//     path; keeps existing fixtures byte-stable because the literal
//     fast paths at each call site fire before this function).
//   - `scalar(v)` → ScalarSubquery over scalarValuePlan(lower(v)) —
//     PromQL semantics: the value of v's single sample, NaN when v has
//     zero or multiple samples. The vector argument is lowered in
//     instant context (step = 0): `scalar()` produces one value per
//     evaluation, and the scalar-subquery shape binds a single value
//     per statement. Range-mode queries therefore see the scalar
//     evaluated once (at the eval anchor) rather than per step — the
//     same documented posture as topk's computed-K lowering
//     (lowerTopKComputed).
//   - `time()` → the eval anchor as Unix seconds (same value expr as
//     lowerTime's instant path).
//   - Unary / Binary / Paren compositions recurse; arithmetic maps
//     through promBinaryOp (atan2, pow and Go-modulo included);
//     comparisons (always `bool`-flagged on scalars — the parser
//     rejects the bare form) wrap in toFloat64 so the UInt8 CH
//     comparison result projects as 1.0 / 0.0.
func lowerScalarArg(e parser.Expr, s schema.Metrics, ctx lowerCtx) (chplan.Expr, error) {
	if v, ok := TryFoldScalar(e); ok {
		return &chplan.LitFloat{V: v}, nil
	}
	switch v := e.(type) {
	case *parser.ParenExpr:
		return lowerScalarArg(v.Expr, s, ctx)
	case *parser.UnaryExpr:
		inner, err := lowerScalarArg(v.Expr, s, ctx)
		if err != nil {
			return nil, err
		}
		if v.Op == parser.SUB {
			return &chplan.Binary{Op: chplan.OpSub, Left: &chplan.LitFloat{V: 0}, Right: inner}, nil
		}
		return inner, nil
	case *parser.BinaryExpr:
		left, err := lowerScalarArg(v.LHS, s, ctx)
		if err != nil {
			return nil, err
		}
		right, err := lowerScalarArg(v.RHS, s, ctx)
		if err != nil {
			return nil, err
		}
		op, err := promBinaryOp(v.Op)
		if err != nil {
			return nil, err
		}
		bin := &chplan.Binary{Op: op, Left: left, Right: right}
		if isComparison(op) {
			// Scalar-scalar comparisons require the `bool` modifier at
			// parse time, so the result is always the 1.0/0.0 fold.
			return &chplan.FuncCall{Name: "toFloat64", Args: []chplan.Expr{bin}}, nil
		}
		return bin, nil
	case *parser.Call:
		switch v.Func.Name {
		case "scalar":
			if len(v.Args) != 1 {
				return nil, fmt.Errorf("promql: scalar() expects 1 argument, got %d", len(v.Args))
			}
			scalarCtx := ctx
			scalarCtx.step = 0
			inner, err := lower(v.Args[0], s, scalarCtx)
			if err != nil {
				return nil, err
			}
			return &chplan.ScalarSubquery{Input: scalarValuePlan(inner, s)}, nil
		case "time":
			// Mirrors lowerTime's instant value expression: the eval
			// anchor as Unix seconds with the fraction preserved.
			var anchor chplan.Expr
			if !ctx.end.IsZero() {
				anchor = anchorBaseExpr(evalAnchor{End: ctx.end.UTC()})
			} else {
				anchor = anchorBaseExpr(evalAnchor{})
			}
			return &chplan.FuncCall{
				Name: "toFloat64",
				Args: []chplan.Expr{
					&chplan.Binary{
						Op: chplan.OpDiv,
						Left: &chplan.FuncCall{
							Name: "toUnixTimestamp64Nano",
							Args: []chplan.Expr{anchor},
						},
						Right: &chplan.LitInt{V: chplan.NanoToSecondDivisor},
					},
				},
			}, nil
		}
		// `pi()` folds via TryFoldScalar above; scalar() / time() are
		// handled; the parser's type checker admits no other
		// scalar-returning call, so this branch is unreachable from a
		// parseable query (kept as an invariant guard for future
		// upstream scalar functions).
		return nil, fmt.Errorf("promql: %s() is not a supported scalar argument", v.Func.Name)
	}
	return nil, fmt.Errorf("promql: unsupported scalar argument %T", e)
}

// lowerScalarTopLevel lowers a bare top-level scalar-returning call —
// `scalar(<vector>)` or `pi()` — into the canonical single-sample
// synthetic-vector shape.
//
// PromQL `scalar(v)` returns the value of v's single sample, or NaN when
// v has zero or != 1 elements; the result type is scalar. `pi()` is the
// constant π. The /api/v1/query handler renders a top-level scalar as
// resultType "scalar" (a single [ts, value] pair) and query_range
// renders it as a one-series matrix; in both cases cerberus materialises
// the value as a one-row vector with empty labels — the same shape the
// already-supported `vector(scalar(v))` / `time()` lowerings produce.
//
// We reuse lowerScalarArg, which folds `pi()` to a LitFloat and lowers
// `scalar(v)` to a ScalarSubquery over scalarValuePlan (the
// count()==1 ? any(Value) : NaN reduction). Wrapping that scalar
// expression in syntheticScalarVector gives the canonical
// MetricName/Attributes/TimeUnix/Value row, fanned across the step grid
// in range mode.
func lowerScalarTopLevel(c *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	v, err := lowerScalarArg(c, s, ctx)
	if err != nil {
		return nil, err
	}
	return syntheticScalarVector(v, nil, s, ctx), nil
}

// scalarValuePlan wraps an instant-lowered vector plan with PromQL's
// `scalar()` reduction: exactly one sample → its value; zero or many
// samples → NaN. The shape is
//
//	Project [if(_cerb_scnt = 1, _cerb_sval, nan) AS Value]
//	  Aggregate funcs=[count() AS _cerb_scnt, any(Value) AS _cerb_sval]  (DropEmptyOnNoGroup=false)
//	    <input>
//
// The no-GROUP-BY Aggregate with DropEmptyOnNoGroup=false always
// returns exactly one row (CH's aggregate-only-query semantics emit a
// `count = 0` row even over an empty input), which is precisely the
// one-row contract chplan.ScalarSubquery requires — an empty scalar
// subquery is a CH-side error, and `scalar(<empty vector>)` must be
// NaN, not a 5xx.
func scalarValuePlan(input chplan.Node, s schema.Metrics) chplan.Node {
	agg := &chplan.Aggregate{
		Input: input,
		AggFuncs: []chplan.AggFunc{
			{Name: "count", Args: nil, Alias: scalarCountAlias},
			{Name: "any", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ValueColumn}}, Alias: scalarValueAlias},
		},
		DropEmptyOnNoGroup: false,
	}
	return &chplan.Project{
		Input: agg,
		Projections: []chplan.Projection{
			{
				Expr: &chplan.FuncCall{
					Name: "if",
					Args: []chplan.Expr{
						&chplan.Binary{
							Op:    chplan.OpEq,
							Left:  &chplan.ColumnRef{Name: scalarCountAlias},
							Right: &chplan.LitInt{V: 1},
						},
						&chplan.ColumnRef{Name: scalarValueAlias},
						&chplan.LitFloat{V: math.NaN()},
					},
				},
				Alias: s.ValueColumn,
			},
		},
	}
}

// isNaNExpr returns `isNaN(<e>)` — used by the computed-scalar call
// sites to reproduce Go's NaN-propagation where the matching CH
// function (greatest / least) treats NaN as orderable instead.
func isNaNExpr(e chplan.Expr) chplan.Expr {
	return &chplan.FuncCall{Name: "isNaN", Args: []chplan.Expr{e}}
}

// nanIfExpr returns `if(<cond>, nan, <e>)`.
func nanIfExpr(cond, e chplan.Expr) chplan.Expr {
	return &chplan.FuncCall{
		Name: "if",
		Args: []chplan.Expr{cond, &chplan.LitFloat{V: math.NaN()}, e},
	}
}

// outOfRangePhiGuardExpr wraps a quantile output value with PromQL's
// runtime phi-domain rules (prometheus/promql/quantile.go::quantile):
//
//	NaN phi → NaN; phi < 0 → -Inf; phi > 1 → +Inf; else <value>.
//
// Used by the computed-phi paths where the literal-phi lowering
// resolves the same rules at compile time via outOfRangePhiInf.
func outOfRangePhiGuardExpr(phi, value chplan.Expr) chplan.Expr {
	return &chplan.FuncCall{
		Name: "multiIf",
		Args: []chplan.Expr{
			isNaNExpr(phi), &chplan.LitFloat{V: math.NaN()},
			&chplan.Binary{Op: chplan.OpLt, Left: phi, Right: &chplan.LitFloat{V: 0}}, &chplan.LitFloat{V: math.Inf(-1)},
			&chplan.Binary{Op: chplan.OpGt, Left: phi, Right: &chplan.LitFloat{V: 1}}, &chplan.LitFloat{V: math.Inf(1)},
			value,
		},
	}
}

// sanitizedPhiParamExpr clamps a computed phi into CH's accepted
// quantile-parameter domain: `if(isNaN(phi) OR phi < 0 OR phi > 1,
// 0.5, phi)`. CH's parameterised `quantile(phi)(...)` aggregate errors
// at runtime on out-of-domain phi (PARAMETER_OUT_OF_BOUND), while
// PromQL defines those shapes as NaN / ±Inf results — the caller pairs
// this sanitised parameter with outOfRangePhiGuardExpr on the output
// so the sentinel 0.5 quantile is computed but never observed.
func sanitizedPhiParamExpr(phi chplan.Expr) chplan.Expr {
	outOfDomain := &chplan.Binary{
		Op:   chplan.OpOr,
		Left: isNaNExpr(phi),
		Right: &chplan.Binary{
			Op:    chplan.OpOr,
			Left:  &chplan.Binary{Op: chplan.OpLt, Left: phi, Right: &chplan.LitFloat{V: 0}},
			Right: &chplan.Binary{Op: chplan.OpGt, Left: phi, Right: &chplan.LitFloat{V: 1}},
		},
	}
	return &chplan.FuncCall{
		Name: "if",
		Args: []chplan.Expr{outOfDomain, &chplan.LitFloat{V: 0.5}, phi},
	}
}
