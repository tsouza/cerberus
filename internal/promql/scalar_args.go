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
	// Aliases used by scalarStepPlan's per-step reduction: the step key,
	// the per-step value, and the two arrays they are folded into.
	scalarStepKeyAlias    = "_cerb_sts"
	scalarStepValueAlias  = "_cerb_sv"
	scalarStepKeysAlias   = "_cerb_sks"
	scalarStepValuesAlias = "_cerb_svs"
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
			if ctx.scalarsBindPerStep() {
				inner, err := lower(v.Args[0], s, ctx)
				if err != nil {
					return nil, err
				}
				return scalarStepValue(scalarStepPlan(inner, s), s, ctx), nil
			}
			// Instant mode (or a pinned embedding position): one
			// evaluation, so the single-value scalar subquery is the
			// whole story.
			scalarCtx := ctx
			scalarCtx.step = 0
			inner, err := lower(v.Args[0], s, scalarCtx)
			if err != nil {
				return nil, err
			}
			return scalarSubqueryValue(scalarValuePlan(inner, s)), nil
		case "time":
			// Mirrors lowerTime's value expression. In range mode
			// (ctx.step > 0) lowerTime binds per-step via
			// chplan.NowNano() + rewriteAnchorRefs, a rewrite pass that
			// only fires inside syntheticScalarVector — lowerScalarArg's
			// embedding sites (nested inside round()/clamp()/quantile()
			// etc.) never go through that path. The Sample contract
			// guarantees TimeUnix == anchor_ts on every row in range
			// mode (see chplan/range_lwr.go, promql/lower.go's
			// wrapRangeLatestPerSeries), so reading the row's own
			// TimeUnix column reproduces the same per-step binding
			// without the rewrite pass. Instant mode (ctx.step == 0)
			// is untouched, so every existing golden stays
			// byte-identical.
			var anchor chplan.Expr
			switch {
			case ctx.step > 0:
				anchor = &chplan.ColumnRef{Name: s.TimestampColumn}
			case !ctx.end.IsZero():
				anchor = anchorBaseExpr(evalAnchor{End: ctx.end.UTC()})
			default:
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
	// The scalar lands directly above the StepGrid this projection is
	// built on, so a per-step binding keys off the grid's own anchor
	// column — the Sample timestamp column is this projection's OUTPUT.
	v, err := lowerScalarArg(c, s, ctx.withStepGridAnchor())
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

// scalarStepPlan is [scalarValuePlan]'s range-mode sibling: instead of
// reducing the whole input to one value it reduces it to one value *per
// evaluation step*, then folds the result into a single-row lookup table.
//
// Shape:
//
//	Project [mapFromArrays(_cerb_sks, _cerb_svs) AS Value]
//	  Aggregate funcs=[groupArray(_cerb_sts) AS _cerb_sks,
//	                   groupArray(_cerb_sv)  AS _cerb_svs]  (DropEmptyOnNoGroup=false)
//	    Project [toUnixTimestamp64Nano(TimeUnix) AS _cerb_sts,
//	             toNullable(if(_cerb_scnt = 1, _cerb_sval, nan)) AS _cerb_sv]
//	      Aggregate group=[TimeUnix] funcs=[count() AS _cerb_scnt,
//	                                        any(Value) AS _cerb_sval]
//	        <input>
//
// The per-step reduction is `scalar()`'s own rule applied once per step —
// the value of the step's single sample, NaN when the step has zero or
// several samples — which is exactly what reference Prometheus computes
// when it evaluates the scalar argument at each step.
//
// Folding those per-step values into a Map rather than leaving them as
// rows is what keeps the embedding site a plain expression: the whole
// table is one value of one row, so it still satisfies
// chplan.ScalarSubquery's one-row/one-column contract, ClickHouse folds
// the subquery to a constant once per statement, and every embedding of
// the scalar reads it with an O(1) map lookup instead of a correlated
// re-execution.
//
// The map's values are Nullable so a step the input has no row for reads
// back as NULL rather than as ClickHouse's zero-valued default for the
// element type; [scalarStepValue] turns that NULL into the NaN PromQL
// defines for "no single sample at this step".
func scalarStepPlan(input chplan.Node, s schema.Metrics) chplan.Node {
	perStep := &chplan.Aggregate{
		Input:          input,
		GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: s.TimestampColumn}},
		GroupByAliases: []string{s.TimestampColumn},
		AggFuncs: []chplan.AggFunc{
			{Name: "count", Args: nil, Alias: scalarCountAlias},
			{Name: "any", Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ValueColumn}}, Alias: scalarValueAlias},
		},
	}
	keyed := &chplan.Project{
		Input: perStep,
		Projections: []chplan.Projection{
			{
				Expr: &chplan.FuncCall{
					Name: "toUnixTimestamp64Nano",
					Args: []chplan.Expr{&chplan.ColumnRef{Name: s.TimestampColumn}},
				},
				Alias: scalarStepKeyAlias,
			},
			{
				Expr: &chplan.FuncCall{
					Name: "toNullable",
					Args: []chplan.Expr{&chplan.FuncCall{
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
					}},
				},
				Alias: scalarStepValueAlias,
			},
		},
	}
	folded := &chplan.Aggregate{
		Input: keyed,
		AggFuncs: []chplan.AggFunc{
			{Name: "groupArray", Args: []chplan.Expr{&chplan.ColumnRef{Name: scalarStepKeyAlias}}, Alias: scalarStepKeysAlias},
			{Name: "groupArray", Args: []chplan.Expr{&chplan.ColumnRef{Name: scalarStepValueAlias}}, Alias: scalarStepValuesAlias},
		},
		DropEmptyOnNoGroup: false,
	}
	return &chplan.Project{
		Input: folded,
		Projections: []chplan.Projection{
			{
				Expr: &chplan.FuncCall{
					Name: "mapFromArrays",
					Args: []chplan.Expr{
						&chplan.ColumnRef{Name: scalarStepKeysAlias},
						&chplan.ColumnRef{Name: scalarStepValuesAlias},
					},
				},
				Alias: s.ValueColumn,
			},
		},
	}
}

// scalarStepValue reads one evaluation step's value out of a
// [scalarStepPlan] lookup table:
//
//	ifNull((SELECT …)[toUnixTimestamp64Nano(<anchor>)], nan)
//
// The anchor is whatever names the current step in the scope the
// expression lands in — the canonical Sample timestamp column for a
// row-position embedding, `anchor_ts` inside a synthetic step grid (see
// lowerCtx.scalarAnchorColumn).
//
// A step the table has no entry for reads back NULL, which becomes the
// NaN PromQL gives `scalar()` when a step has no single sample; the
// Nullable never escapes this expression.
//
// The anchor ColumnRef is built here, per call, rather than carried on
// the ctx: one ctx lowers arbitrarily many scalar arguments and each
// needs its own node so the IR stays a tree.
func scalarStepValue(plan chplan.Node, s schema.Metrics, ctx lowerCtx) chplan.Expr {
	anchorColumn := ctx.scalarAnchorColumn
	if anchorColumn == "" {
		anchorColumn = s.TimestampColumn
	}
	lookup := &chplan.FuncCall{
		Name: "arrayElement",
		Args: []chplan.Expr{
			&chplan.ScalarSubquery{Input: plan},
			&chplan.FuncCall{
				Name: "toUnixTimestamp64Nano",
				Args: []chplan.Expr{&chplan.ColumnRef{Name: anchorColumn}},
			},
		},
	}
	return &chplan.FuncCall{
		Name: "ifNull",
		Args: []chplan.Expr{lookup, &chplan.LitFloat{V: math.NaN()}},
	}
}

// scalarSubqueryValue wraps a [scalarValuePlan] subtree in the scalar
// subquery that reads its single value, then strips the Nullable that
// ClickHouse's static type inference attaches to EVERY scalar-subquery
// position (a subquery might return no rows, so `(SELECT x FROM …)` is
// Nullable(typeof(x)) regardless of x).
//
// That inference is what has to go, not a real null: scalarValuePlan's
// Aggregate carries DropEmptyOnNoGroup=false, so it emits exactly one
// row even over an empty input, and its projection is the total
// `if(count = 1, value, NaN)` — there is no input for which this
// subquery yields SQL NULL. Prometheus's own "not exactly one series"
// signal is NaN, which passes through assumeNotNull untouched.
//
// Leaving the Nullable in place poisons any consumer whose ClickHouse
// function demands an exact type match. `arrayFold`, which
// double_exponential_smoothing's recurrence runs on, compares its
// lambda's return type against the seed accumulator's and raises
// TYPE_MISMATCH when a computed smoothing factor makes the lambda
// Tuple(Nullable(Float64), Nullable(Float64)) against a
// Tuple(Float64, Float64) seed.
func scalarSubqueryValue(plan chplan.Node) chplan.Expr {
	return &chplan.FuncCall{
		Name: "assumeNotNull",
		Args: []chplan.Expr{&chplan.ScalarSubquery{Input: plan}},
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

// quantileSentinelPhi is the in-domain phi substituted for an
// out-of-domain one so the quantile machinery never sees a phi it would
// reject. PromQL defines those shapes as constant NaN / ±Inf results, so
// the sentinel's quantile is computed but always discarded by the
// matching output guard — its value is arbitrary beyond being in [0, 1].
const quantileSentinelPhi = 0.5

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
		Args: []chplan.Expr{outOfDomain, &chplan.LitFloat{V: quantileSentinelPhi}, phi},
	}
}
