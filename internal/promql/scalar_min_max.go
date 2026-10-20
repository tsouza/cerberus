package promql

import (
	"fmt"
	"math"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// Names of the experimental scalar functions `min_of(a, b)` and
// `max_of(a, b)`: Prometheus answers them with math.Min / math.Max of
// its two scalar arguments (funcMinOf / funcMaxOf).
const (
	fnMinOf = "min_of"
	fnMaxOf = "max_of"
)

// scalarMinMaxArity is the argument count the parser enforces for
// `min_of` and `max_of`.
const scalarMinMaxArity = 2

// scalarMinMaxFold maps each function to the Go function the reference
// evaluates it with, for [TryFoldScalar]'s literal-only fold.
var scalarMinMaxFold = map[string]func(a, b float64) float64{
	fnMinOf: math.Min,
	fnMaxOf: math.Max,
}

// negOne is the factor that negates a float while keeping the sign of
// zero (`x * -1` is -0 for x = +0, which `0 - x` is not).
const negOne = -1

// lowerScalarMinMax lowers `min_of(a, b)` / `max_of(a, b)` over computed
// scalar arguments to the ClickHouse expression that answers exactly
// what math.Min / math.Max do. ClickHouse's least / greatest order NaN
// and ignore the sign of zero, so the expression spells out Go's special
// cases in Go's own order:
//
//	max_of: either +Inf → +Inf; else either NaN → NaN;
//	        both zero → a + b (+0 unless both are -0); else greatest(a, b)
//	min_of: either -Inf → -Inf; else either NaN → NaN;
//	        both zero → -(-a + -b) (-0 unless both are +0); else least(a, b)
func lowerScalarMinMax(c *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Expr, error) {
	if len(c.Args) != scalarMinMaxArity {
		return nil, fmt.Errorf("promql: %s expects %d arguments, got %d", c.Func.Name, scalarMinMaxArity, len(c.Args))
	}
	a, err := lowerScalarArg(c.Args[0], s, ctx)
	if err != nil {
		return nil, err
	}
	b, err := lowerScalarArg(c.Args[1], s, ctx)
	if err != nil {
		return nil, err
	}
	return bindScalarPair(a, b, func(a, b chplan.Expr) chplan.Expr {
		return scalarMinMaxExpr(c.Func.Name == fnMaxOf, a, b)
	}), nil
}

// Lambda parameter names [bindScalarPair] binds the two arguments to. The
// `_cerb_` prefix keeps them clear of any column a plan carries.
const (
	scalarPairLeftParam  = "_cerb_mma"
	scalarPairRightParam = "_cerb_mmb"
)

// bindScalarPair returns body over a and b with each argument evaluated
// once: `arrayMap((l, r) -> body(l, r), [a], [b])[1]`. body reads each
// argument several times, and a computed scalar argument is a scalar
// subquery (or a per-step lookup over one), which inlining would repeat
// in the SQL once per read.
func bindScalarPair(a, b chplan.Expr, body func(a, b chplan.Expr) chplan.Expr) chplan.Expr {
	mapped := &chplan.FuncCall{Fn: chplan.FnArrayMap, Args: []chplan.Expr{
		&chplan.Lambda{
			Params: []string{scalarPairLeftParam, scalarPairRightParam},
			Body:   body(&chplan.BareIdent{Name: scalarPairLeftParam}, &chplan.BareIdent{Name: scalarPairRightParam}),
		},
		&chplan.FuncCall{Fn: chplan.FnArray, Args: []chplan.Expr{a}},
		&chplan.FuncCall{Fn: chplan.FnArray, Args: []chplan.Expr{b}},
	}}
	return &chplan.FuncCall{Fn: chplan.FnArrayElement, Args: []chplan.Expr{mapped, &chplan.LitInt{V: 1}}}
}

// scalarMinMaxExpr builds the math.Max (isMax) or math.Min expression
// over a and b; see [lowerScalarMinMax].
func scalarMinMaxExpr(isMax bool, a, b chplan.Expr) chplan.Expr {
	inf, pick, zeros := math.Inf(-1), chplan.FnLeast, negate(&chplan.Binary{Op: chplan.OpAdd, Left: negate(a), Right: negate(b)})
	if isMax {
		inf, pick, zeros = math.Inf(1), chplan.FnGreatest, &chplan.Binary{Op: chplan.OpAdd, Left: a, Right: b}
	}
	isValue := func(e chplan.Expr, v float64) chplan.Expr {
		return &chplan.Binary{Op: chplan.OpEq, Left: e, Right: &chplan.LitFloat{V: v}}
	}
	either := func(cond func(chplan.Expr) chplan.Expr) chplan.Expr {
		return &chplan.Binary{Op: chplan.OpOr, Left: cond(a), Right: cond(b)}
	}
	return &chplan.FuncCall{Fn: chplan.FnIf, Args: []chplan.Expr{
		either(func(e chplan.Expr) chplan.Expr { return isValue(e, inf) }),
		&chplan.LitFloat{V: inf},
		nanIfExpr(either(isNaNExpr), &chplan.FuncCall{Fn: chplan.FnIf, Args: []chplan.Expr{
			&chplan.Binary{Op: chplan.OpAnd, Left: isValue(a, 0), Right: isValue(b, 0)},
			zeros,
			&chplan.FuncCall{Fn: pick, Args: []chplan.Expr{a, b}},
		}}),
	}}
}

// negate returns e * -1, which flips the sign of zero as IEEE negation does.
func negate(e chplan.Expr) chplan.Expr {
	return &chplan.Binary{Op: chplan.OpMul, Left: e, Right: &chplan.LitFloat{V: negOne}}
}
