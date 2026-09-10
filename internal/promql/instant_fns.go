package promql

import (
	"fmt"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// instantFnCH maps PromQL instant-vector functions to the ClickHouse
// function that implements the same transform on `Value`. PromQL `ln` is
// the natural log; CH spells that `log`. Everything else is 1:1.
//
// Each entry is a 1-arg function over a vector; we wrap the lowered vector
// with a Project that replaces ValueColumn with `<chFn>(Value)`.
var instantFnCH = map[string]chplan.Fn{
	"abs":   chplan.FnAbs,
	"ceil":  chplan.FnCeil,
	"floor": chplan.FnFloor,
	"round": chplan.FnRound,
	"sqrt":  chplan.FnSqrt,
	"exp":   chplan.FnExp,
	"ln":    chplan.FnLn,
	"log2":  chplan.FnLog2,
	"log10": chplan.FnLog10,
	"sgn":   chplan.FnSign,

	// Trigonometric family. PromQL's trig functions operate per-row on
	// `Value` and interpret/return angles in RADIANS — exactly CH's
	// convention — so each maps 1:1 to the same-named CH builtin. All are
	// Float64-in/Float64-out (unlike `sgn`, which needs a toFloat64 wrap).
	"acos":  chplan.FnAcos,
	"acosh": chplan.FnAcosh,
	"asin":  chplan.FnAsin,
	"asinh": chplan.FnAsinh,
	"atan":  chplan.FnAtan,
	"atanh": chplan.FnAtanh,
	"cos":   chplan.FnCos,
	"cosh":  chplan.FnCosh,
	"sin":   chplan.FnSin,
	"sinh":  chplan.FnSinh,
	"tan":   chplan.FnTan,
	"tanh":  chplan.FnTanh,

	// Degrees ↔ radians conversion. PromQL `deg(x)` = `x * 180/π` and
	// `rad(x)` = `x * π/180`; CH spells these `degrees(x)` / `radians(x)`.
	"deg": chplan.FnDegrees,
	"rad": chplan.FnRadians,
}

// lowerInstantFn handles single-arg math functions like abs / sqrt / ln. The
// arg is expected to be an instant-vector expression; we lower it, then
// wrap with a Project that maps the Value column through the CH function.
//
// Multi-arg variants of round and the clamp family are handled separately.
func lowerInstantFn(c *parser.Call, s schema.Metrics, chFn chplan.Fn, ctx lowerCtx) (chplan.Node, error) {
	switch c.Func.Name {
	case "round":
		if len(c.Args) == 2 {
			// Nested drop-family argument recognised too (cerberus issue
			// #2528) — see the unary-function branch below for the full
			// rationale.
			if node, ok, err := lowerExpHistogramArgAsCanonicalFloat(c.Args[0], s, ctx); ok {
				return node, err
			}
			return lowerRoundToNearest(c, s, ctx)
		}
	}

	if len(c.Args) != 1 {
		return nil, fmt.Errorf("promql: %s with %d arguments is unsupported (instant math fns are unary)",
			c.Func.Name, len(c.Args))
	}
	// A histogram-VALUED argument reprojects to the canonical empty float
	// quartet (Prom's simpleFloatFunc skips every H-set sample). A nested
	// drop-family argument (cerberus issue #2528) — e.g.
	// `abs(demo_latency_exp_hist + 0)` — is already that same empty shape,
	// so no chFn(Value) rewrite is needed either: no row survives to read
	// it.
	if node, ok, err := lowerExpHistogramArgAsCanonicalFloat(c.Args[0], s, ctx); ok {
		return node, err
	}

	inner, err := lowerMathOperand(c.Args[0], s, ctx)
	if err != nil {
		return nil, err
	}

	return guardedValueProjection(inner, c.Args[0], s, ctx, mixedMathFamily, func(refs sampleRoleRefs) chplan.Expr {
		return mathFnValueExpr(chFn, refs.Value)
	})
}

// mathFnValueExpr wraps valueExpr with the CH function instantFnCH maps
// chFn to, applying the one dtype fixup the table needs: CH's sign()
// returns Int8, while every other entry in instantFnCH is
// Float64-in/Float64-out, and the wire scanner reads Value as *float64 —
// an unwrapped sign() 502s with "converting Int8 to *float64 is
// unsupported" (surfaced by the showcase-promql sgn() panel). Shared by
// [lowerInstantFn] (a bare/derived float input) and
// histogram_native_mixed_or_math_fn.go's mixed-`or` composition (cerberus
// issue #2449), which needs the identical chFn(Value) rewrite over a
// differently-shaped input.
func mathFnValueExpr(chFn chplan.Fn, valueExpr chplan.Expr) chplan.Expr {
	var newValue chplan.Expr = &chplan.FuncCall{
		Fn:   chFn,
		Args: []chplan.Expr{valueExpr},
	}
	if chFn == chplan.FnSign {
		newValue = &chplan.FuncCall{Fn: chplan.FnToFloat64, Args: []chplan.Expr{newValue}}
	}
	return newValue
}

// lowerRoundToNearest implements PromQL `round(v, to_nearest)` as
// `round(Value / to_nearest) * to_nearest`. CH's native `round(v, N)`
// rounds to N decimal places, not to a multiple, so we synthesise the
// multiple-rounding semantics explicitly.
//
// to_nearest may be a scalar literal (the common case — folded at
// lowering time) or any computed scalar expression
// (`round(v, scalar(x))`): lowerScalarArg binds the computed shape as
// a scalar subquery and the same division/multiplication arithmetic
// applies. A NaN to_nearest (scalar() over 0 or many series)
// propagates NaN through the arithmetic, matching Prom's
// `math.Floor(v/toNearest+0.5)*toNearest` with a NaN operand.
func lowerRoundToNearest(c *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	tn, err := lowerRoundToNearestBound(c.Args[1], s, ctx)
	if err != nil {
		return nil, err
	}

	inner, err := lowerMathOperand(c.Args[0], s, ctx)
	if err != nil {
		return nil, err
	}

	return guardedValueProjection(inner, c.Args[0], s, ctx, mixedMathFamily, func(refs sampleRoleRefs) chplan.Expr {
		return roundToNearestValueExpr(refs.Value, tn)
	})
}

// lowerRoundToNearestBound lowers round()'s second argument — the
// to_nearest bound — shared by [lowerRoundToNearest] (a bare/derived
// float vector argument) and
// histogram_native_mixed_or_math_fn.go's [lowerRoundToNearestOverMixedExpHistogramSetOp]
// (cerberus issue #2578), which needs the identical literal/computed
// split over a differently-shaped vector argument.
func lowerRoundToNearestBound(arg parser.Expr, s schema.Metrics, ctx lowerCtx) (chplan.Expr, error) {
	if toNearest, ok := tryScalarLiteral(arg); ok {
		return &chplan.LitFloat{V: toNearest}, nil
	}
	return lowerScalarArg(arg, s, ctx)
}

// roundToNearestValueExpr builds `round(valueExpr / tn) * tn` — the CH
// expression for PromQL's `round(v, to_nearest)` kernel. Shared by
// [lowerRoundToNearest] and histogram_native_mixed_or_math_fn.go's mixed-
// `or` composition, which apply it over differently-shaped inputs.
func roundToNearestValueExpr(valueExpr, tn chplan.Expr) chplan.Expr {
	rounded := &chplan.FuncCall{
		Fn:   chplan.FnRound,
		Args: []chplan.Expr{&chplan.Binary{Op: chplan.OpDiv, Left: valueExpr, Right: tn}},
	}
	return &chplan.Binary{Op: chplan.OpMul, Left: rounded, Right: tn}
}

// lowerMathOperand checks admission before any wrapper adds a filter or returns
// an empty result. A bound filter's legacy Sample shape must not conceal an
// originally mixed input from the wrapper policy. Callers keep their existing
// argument evaluation order by invoking this at their original lower seam.
func lowerMathOperand(arg parser.Expr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	inner, err := lower(arg, s, ctx)
	if err != nil {
		return nil, err
	}
	if err := requireMixedPlanPolicy(inner, mixedMathFamily); err != nil {
		return nil, err
	}
	return inner, nil
}

// lowerClamp implements the PromQL clamp family:
//
//	clamp_max(v, max) → least(Value, max)
//	clamp_min(v, min) → greatest(Value, min)
//	clamp(v, min, max) → greatest(min, least(max, Value))
//
// Bounds may be scalar literals (the common case — folded at lowering
// time, byte-stable SQL) or computed scalar expressions
// (`clamp_min(v, scalar(x))`): lowerScalarArg binds the computed shape
// as a scalar subquery. Two semantic gaps between CH's least/greatest
// and Prom's math.Min/math.Max are bridged on the computed path:
//
//   - NaN bounds: Go's math.Min/Max NaN-propagate (clamp_min(v, NaN)
//     is a NaN series), while CH's least/greatest order NaN; the
//     computed path wraps the value in `if(isNaN(bound), nan, ...)`.
//   - Degenerate 3-arg bounds: Prom's funcClamp returns an EMPTY
//     vector when maxVal < minVal. The literal path folds that at
//     lowering time; the computed path emits a runtime
//     `NOT (max < min)` Filter (NaN bounds compare false, so they
//     keep the rows and resolve to NaN values — exactly Prom's
//     behaviour).
//
// Prom's shared clamp() helper (promql/functions.go) skips every sample
// whose H field is set before applying math.Max/math.Min — the exact
// "process only float samples" rule simpleFloatFunc applies for
// abs/ceil/floor/round/etc. Unlike those, the clamp family didn't route
// its vector arg through lowerExpHistogramValuedShape, so a histogram
// selector fell through to lower()'s generic dispatch and hit
// expHistogramSelectorRouting's catch-all rejection instead of Prom's
// drop-and-answer-empty semantics (cerberus issue #2345). Checking it
// here, before the literal/computed bound split below, makes all three
// clamp forms answer an empty float vector for a histogram-valued arg
// exactly as dropExpHistogramSamples does for the unary math functions.
func lowerClamp(c *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if len(c.Args) >= 1 {
		// See lowerInstantFn's own comment for the shared rationale
		// (histogram-valued and drop-family arguments alike, cerberus
		// issues #2345 and #2528): an already-empty argument composes for
		// free.
		if node, ok, err := lowerExpHistogramArgAsCanonicalFloat(c.Args[0], s, ctx); ok {
			return node, err
		}
	}
	switch c.Func.Name {
	case "clamp_max", "clamp_min":
		if len(c.Args) != 2 {
			return nil, fmt.Errorf("promql: %s expects 2 arguments, got %d", c.Func.Name, len(c.Args))
		}
		fnName := chplan.FnLeast
		if c.Func.Name == "clamp_min" {
			fnName = chplan.FnGreatest
		}
		if bound, ok := tryScalarLiteral(c.Args[1]); ok {
			inner, err := lowerMathOperand(c.Args[0], s, ctx)
			if err != nil {
				return nil, err
			}
			return guardedValueProjection(inner, c.Args[0], s, ctx, mixedMathFamily, func(refs sampleRoleRefs) chplan.Expr {
				return &chplan.FuncCall{
					Fn: fnName,
					Args: []chplan.Expr{
						refs.Value,
						&chplan.LitFloat{V: bound},
					},
				}
			})
		}
		boundE, err := lowerScalarArg(c.Args[1], s, ctx)
		if err != nil {
			return nil, err
		}
		inner, err := lowerMathOperand(c.Args[0], s, ctx)
		if err != nil {
			return nil, err
		}
		return guardedValueProjection(inner, c.Args[0], s, ctx, mixedMathFamily, func(refs sampleRoleRefs) chplan.Expr {
			return nanIfExpr(isNaNExpr(boundE), &chplan.FuncCall{
				Fn: fnName,
				Args: []chplan.Expr{
					refs.Value,
					boundE,
				},
			})
		})

	case "clamp":
		if len(c.Args) != 3 {
			return nil, fmt.Errorf("promql: clamp expects 3 arguments, got %d", len(c.Args))
		}
		minB, okMin := tryScalarLiteral(c.Args[1])
		maxB, okMax := tryScalarLiteral(c.Args[2])
		if okMin && okMax {
			inner, err := lowerMathOperand(c.Args[0], s, ctx)
			if err != nil {
				return nil, err
			}
			// Prom's funcClamp short-circuits to an empty Vector when
			// `maxVal < minVal` (see prometheus/promql/functions.go::clamp).
			// The CH-side `greatest(min, least(max, V))` doesn't replicate
			// that — it would force every sample to `min` — so detect the
			// degenerate-bounds case at lowering and Filter the inner tree
			// to zero rows. Surfaced as the compat-lane diff on
			// `clamp(demo_memory_usage_bytes, 1e12, 0)`: cerberus emitted a
			// constant 1e12 series across every step while Prom emitted no
			// series at all.
			if maxB < minB {
				return &chplan.Filter{
					Input:     inner,
					Predicate: &chplan.LitBool{V: false},
				}, nil
			}
			return guardedValueProjection(inner, c.Args[0], s, ctx, mixedMathFamily, func(refs sampleRoleRefs) chplan.Expr {
				return &chplan.FuncCall{
					Fn: chplan.FnGreatest,
					Args: []chplan.Expr{
						&chplan.LitFloat{V: minB},
						&chplan.FuncCall{
							Fn:   chplan.FnLeast,
							Args: []chplan.Expr{&chplan.LitFloat{V: maxB}, refs.Value},
						},
					},
				}
			})
		}

		// At least one computed bound: bind both sides through
		// lowerScalarArg (the literal side folds to a LitFloat) and
		// resolve Prom's degenerate-bounds + NaN rules at runtime.
		minE, err := lowerScalarArg(c.Args[1], s, ctx)
		if err != nil {
			return nil, err
		}
		maxE, err := lowerScalarArg(c.Args[2], s, ctx)
		if err != nil {
			return nil, err
		}
		inner, err := lowerMathOperand(c.Args[0], s, ctx)
		if err != nil {
			return nil, err
		}
		// Narrow while the operand still carries its Mixed shape. The bound
		// Filter below has a legacy Sample shape and would otherwise hide
		// histogram rows from guardedValueProjection's float-only check.
		inner = mixedRowsFloatOnly(inner)
		// Runtime mirror of the literal path's maxB < minB fold: keep
		// rows only while NOT (max < min). NaN bounds compare false —
		// rows survive and the NaN guard below turns the values NaN,
		// matching Prom's math.Max(min, math.Min(max, v)).
		filtered := &chplan.Filter{
			Input: inner,
			Predicate: &chplan.FuncCall{
				Fn: chplan.FnNot,
				Args: []chplan.Expr{
					&chplan.Binary{Op: chplan.OpLt, Left: maxE, Right: minE},
				},
			},
		}
		return guardedValueProjection(filtered, c.Args[0], s, ctx, mixedMathFamily, func(refs sampleRoleRefs) chplan.Expr {
			return nanIfExpr(
				&chplan.Binary{Op: chplan.OpOr, Left: isNaNExpr(minE), Right: isNaNExpr(maxE)},
				&chplan.FuncCall{
					Fn: chplan.FnGreatest,
					Args: []chplan.Expr{
						minE,
						&chplan.FuncCall{
							Fn:   chplan.FnLeast,
							Args: []chplan.Expr{maxE, refs.Value},
						},
					},
				},
			)
		})
	}
	return nil, fmt.Errorf("promql: unknown clamp function %s", c.Func.Name)
}

// projectValueOverInner applies the wrapper's float-only, drop-name policy.
// The temporary legacy adapter selects only its temporal/materialization
// envelope; actual input names are resolved from roles before build runs.
func projectValueOverInner(inner chplan.Node, s schema.Metrics, layout sampleProjectionLayout, build func(sampleRoleRefs) chplan.Expr) chplan.Node {
	return projectSampleRoles(inner, s,
		sampleProjectionPolicy{name: dropSampleName, payload: floatSamplePayload},
		layout,
		func(refs sampleRoleRefs) sampleRoleRewrite { return sampleRoleRewrite{value: build(refs)} })
}

// legacySampleProjectionLayout preserves the existing temporal/materialization
// boundary until the legacy shape contract is reconciled. It must not choose
// a wrapper's name or payload policy, and it never supplies input column names.
func legacySampleProjectionLayout(inner chplan.Node) sampleProjectionLayout {
	shape := chplan.RowShapeOf(inner)
	return sampleProjectionLayout{
		canonical: shape != chplan.GridWindowRowShape && shape != chplan.ReducedWindowRowShape,
		anchored:  shape == chplan.GridWindowRowShape,
	}
}
