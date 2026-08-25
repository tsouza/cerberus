package promql

import (
	"fmt"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// histogram_native_mixed_or_math_fn.go lowers a single-arg instant math
// function — every entry in instant_fns.go's instantFnCH table (abs(),
// ceil(), floor(), sqrt(), the log/trig/deg-rad families, sgn(), and
// round() called with its default 1-arg form) — directly wrapping a
// mixed float/histogram `or` (histogram_native_mixed_or.go's own
// #2330/#2335 shape). Cerberus issue #2449, the third wrapper family to
// compose over that shape after `sum`/`avg` (#2346) and
// `label_replace`/`label_join` (#2449's own first pass, PR #2476) — and
// the first whose composition actually needs to READ the payload rather
// than just forward it.
//
// Reference Prometheus semantics (verified against the vendored fork's
// promql/functions.go, NOT assumed from this issue's own acceptance
// text): funcAbs and every sibling in instantFnCH's dispatch table share
// one helper, simpleFloatFunc — "for _, el := range vectorVals[0] { if
// el.H == nil { // Process only float samples ... } }". A histogram-
// valued sample is silently skipped, never computed over; there is no
// reference notion of "the absolute value of a histogram". So
// `abs(a or b)` for a mixed `or` answers exactly the FLOAT rows of the
// union with abs() applied, and drops every histogram row outright —
// the SAME "drop" family sort()/sort_desc() (#2463), the clamp family
// (#2444) and absent() (#2457) already established for a BARE histogram
// shape via dropExpHistogramSamples. It is NOT the "recompute per-sample
// over both float and histogram values" shape this issue's own
// acceptance section speculated (a discriminator-keyed chplan.Case / CH
// if()) — that speculation predates the drop-semantics precedent
// #2456/#2462/#2474 established, and no such per-sample histogram
// semantics exists in reference for this function family. So this file
// needs no chplan.Case: it filters to the float-shaped rows and
// re-projects, exactly dropExpHistogramSamples's shape but keeping the
// float rows' real values instead of answering unconditionally empty.
//
// Deliberately its own sibling recognizer registered root-only in
// lowerRoot, not a widening of mixedExpHistogramSetOp's own
// registration — see that function's and
// histogram_native_mixed_or_label.go's header comments for why.
//
// round()'s 2-arg to_nearest form ALSO composes over this shape, since
// cerberus issue #2578 — [roundToNearestOverMixedExpHistogramSetOp] and
// [lowerRoundToNearestOverMixedExpHistogramSetOp] below are its own
// recognizer/lowering pair, reusing this file's
// [floatRowsOnlyOverMixedExpHistogramSetOp] scaffolding but applying
// instant_fns.go's round-to-nearest value rewrite instead of a plain
// chFn(Value) — round()'s to_nearest bound (Args[1]) is unrelated to the
// mixed shape and is lowered exactly as [lowerRoundToNearest] lowers it.
//
// The clamp family ALSO composes over this shape, since cerberus issue
// #2587 — [clampOverMixedExpHistogramSetOp] and
// [lowerClampOverMixedExpHistogramSetOp] further below are its own
// recognizer/lowering pair, reusing the identical float-rows-only
// scaffolding but applying instant_fns.go's [lowerClamp] literal/computed
// bound logic (the least/greatest rewrite, the NaN-bound guard, and the
// maxVal < minVal degenerate-bounds Filter) instead of a plain chFn(Value)
// — clamp's bound argument(s) are unrelated to the mixed shape and are
// lowered exactly as [lowerClamp] lowers them for a bare/derived float
// vector argument.
//
// Arbitrary arithmetic binops directly wrapping a mixed `or` (`(a or b) +
// 1`, taught instead by histogram_native_mixed_or_arithmetic.go, cerberus
// issue #2449's fourth pass) are the remaining wrapper shape this file
// does not attempt; anything else still falls through to
// internal/promql/binary.go's lowerVectorSetOp rejection unchanged — see
// test/rejection-parity/catalogue's tracking entry for that site.
func mathFnOverMixedExpHistogramSetOp(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (*parser.Call, *parser.BinaryExpr, chplan.Fn, bool) {
	call, ok := peelWrappers(expr).(*parser.Call)
	if !ok {
		return nil, nil, "", false
	}
	chFn, known := instantFnCH[call.Func.Name]
	if !known || len(call.Args) != 1 {
		// round() called with 2 args (the to_nearest bound) takes a
		// further bound argument and needs its own value-rewrite —
		// [roundToNearestOverMixedExpHistogramSetOp] below handles that
		// shape instead of widening this recognizer. The clamp family
		// (further bound arguments of its own) is handled the identical
		// way by [clampOverMixedExpHistogramSetOp] further below.
		return nil, nil, "", false
	}
	b, ok := mixedExpHistogramSetOp(call.Args[0], s, ctx)
	if !ok {
		return nil, nil, "", false
	}
	return call, b, chFn, true
}

// lowerMathFnOverMixedExpHistogramSetOp lowers the shape
// [mathFnOverMixedExpHistogramSetOp] recognised: build the same Mixed
// [chplan.VectorSetOp] node the root-only leaf case does
// ([lowerMixedExpHistogramSetOp]), keep only its float-shaped rows (the
// [chplan.MixedDiscriminatorColumn] is 0 there — reference's
// simpleFloatFunc never re-admits a histogram sample), and re-project to
// the canonical float quartet with chFn(Value) applied via
// [mathFnValueExpr] (instant_fns.go) — the same rewrite
// [lowerInstantFn] uses for a bare/derived float input, dropping the
// nine Histogram*Column outputs and the discriminator column along the
// way since the result is unconditionally float-only.
//
// MetricName is forced to "" rather than forwarded, mirroring
// [projectValueOverInner]'s canonical-shape branch: every math function
// in instantFnCH derives a new sample and Prom's own DropName rule
// strips `__name__` from it (see that function's doc comment for the
// compat-lane history this matches).
func lowerMathFnOverMixedExpHistogramSetOp(call *parser.Call, b *parser.BinaryExpr, chFn chplan.Fn, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	floatRowsOnly, err := floatRowsOnlyOverMixedExpHistogramSetOp(b, s, ctx)
	if err != nil {
		return nil, err
	}

	newValue := mathFnValueExpr(chFn, &chplan.ColumnRef{Name: s.ValueColumn})
	return projectCanonicalFloatValue(floatRowsOnly, s, newValue), nil
}

// floatRowsOnlyOverMixedExpHistogramSetOp lowers the Mixed
// [chplan.VectorSetOp] node [mixedExpHistogramSetOp] recognises b as, and
// narrows it to its float-shaped rows (the [chplan.MixedDiscriminatorColumn]
// is 0 there — reference's simpleFloatFunc never re-admits a histogram
// sample). Shared by [lowerMathFnOverMixedExpHistogramSetOp] (chFn(Value))
// and [lowerRoundToNearestOverMixedExpHistogramSetOp] (round()'s
// to_nearest rewrite) — both value transforms read only Value, so both
// need the identical filtered-down-to-float-rows input before applying
// their own rewrite and re-projecting to the canonical float quartet.
func floatRowsOnlyOverMixedExpHistogramSetOp(b *parser.BinaryExpr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	inner, err := lowerMixedExpHistogramSetOp(b, s, ctx)
	if err != nil {
		return nil, err
	}
	return mixedDiscriminatorFilter(inner, mixedDiscriminatorFloat), nil
}

// projectCanonicalFloatValue re-projects a float-rows-only plan (an
// [floatRowsOnlyOverMixedExpHistogramSetOp] result) onto the canonical
// float-Sample quartet with newValue as Value. MetricName is forced to
// "" rather than forwarded, mirroring [projectValueOverInner]'s
// canonical-shape branch: every derived sample this file's callers build
// (a math function or round()'s to_nearest rewrite) matches Prom's own
// DropName rule stripping `__name__` from a derived sample (see that
// function's doc comment for the compat-lane history this matches).
func projectCanonicalFloatValue(floatRowsOnly chplan.Node, s schema.Metrics, newValue chplan.Expr) chplan.Node {
	return &chplan.Project{
		Input: floatRowsOnly,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}, Alias: s.AttributesColumn},
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}, Alias: s.TimestampColumn},
			{Expr: newValue, Alias: s.ValueColumn},
		},
	}
}

// roundToNearestOverMixedExpHistogramSetOp recognises `round(v, n)` (the
// 2-arg to_nearest form) directly wrapping a mixed float/histogram `or`
// — the shape [mathFnOverMixedExpHistogramSetOp] above deliberately
// leaves unattempted since it takes a further bound argument (cerberus
// issue #2578). v is checked against [mixedExpHistogramSetOp]; n (the
// to_nearest bound) is unconstrained and lowered independently by
// [lowerRoundToNearestOverMixedExpHistogramSetOp] exactly as
// [lowerRoundToNearest] (instant_fns.go) lowers it for a bare/derived
// float vector argument.
func roundToNearestOverMixedExpHistogramSetOp(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (*parser.Call, *parser.BinaryExpr, bool) {
	call, ok := peelWrappers(expr).(*parser.Call)
	if !ok || call.Func.Name != "round" || len(call.Args) != 2 {
		return nil, nil, false
	}
	b, ok := mixedExpHistogramSetOp(call.Args[0], s, ctx)
	if !ok {
		return nil, nil, false
	}
	return call, b, true
}

// lowerRoundToNearestOverMixedExpHistogramSetOp lowers the shape
// [roundToNearestOverMixedExpHistogramSetOp] recognised: narrow the Mixed
// [chplan.VectorSetOp] node to its float-shaped rows exactly as
// [lowerMathFnOverMixedExpHistogramSetOp] does, then apply
// instant_fns.go's `round(Value / to_nearest) * to_nearest` rewrite
// ([roundToNearestValueExpr]) instead of a plain chFn(Value) — the
// to_nearest bound (call.Args[1]) is lowered via
// [lowerRoundToNearestBound], identical to the bare/derived-vector
// path's own literal/computed split.
func lowerRoundToNearestOverMixedExpHistogramSetOp(call *parser.Call, b *parser.BinaryExpr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	floatRowsOnly, err := floatRowsOnlyOverMixedExpHistogramSetOp(b, s, ctx)
	if err != nil {
		return nil, err
	}

	tn, err := lowerRoundToNearestBound(call.Args[1], s, ctx)
	if err != nil {
		return nil, err
	}

	newValue := roundToNearestValueExpr(&chplan.ColumnRef{Name: s.ValueColumn}, tn)
	return projectCanonicalFloatValue(floatRowsOnly, s, newValue), nil
}

// clampOverMixedExpHistogramSetOp recognises `clamp`/`clamp_min`/
// `clamp_max` directly wrapping a mixed float/histogram `or` as their
// vector argument (cerberus issue #2587 — the clamp family's own
// instance of the further-bound-argument shape
// [roundToNearestOverMixedExpHistogramSetOp] already composes for
// round()'s 2-arg form). call.Args[0] is checked against
// [mixedExpHistogramSetOp]; the bound argument(s) — Args[1] for
// clamp_min/clamp_max, Args[1] and Args[2] for the 3-arg clamp — are
// unconstrained and lowered independently by
// [lowerClampOverMixedExpHistogramSetOp] exactly as [lowerClamp]
// (instant_fns.go) lowers them for a bare/derived float vector argument.
func clampOverMixedExpHistogramSetOp(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (*parser.Call, *parser.BinaryExpr, bool) {
	call, ok := peelWrappers(expr).(*parser.Call)
	if !ok {
		return nil, nil, false
	}
	switch call.Func.Name {
	case "clamp_min", "clamp_max":
		if len(call.Args) != 2 {
			return nil, nil, false
		}
	case "clamp":
		if len(call.Args) != 3 {
			return nil, nil, false
		}
	default:
		return nil, nil, false
	}
	b, ok := mixedExpHistogramSetOp(call.Args[0], s, ctx)
	if !ok {
		return nil, nil, false
	}
	return call, b, true
}

// lowerClampOverMixedExpHistogramSetOp lowers the shape
// [clampOverMixedExpHistogramSetOp] recognised: narrow the Mixed
// [chplan.VectorSetOp] node to its float-shaped rows exactly as
// [lowerMathFnOverMixedExpHistogramSetOp] does, then apply the SAME
// literal/computed bound logic [lowerClamp] (instant_fns.go) applies for
// a bare/derived float vector argument — the least/greatest rewrite, the
// NaN-bound guard, and (for the 3-arg form) the maxVal < minVal
// degenerate-bounds Filter that answers an empty vector — over that
// float-rows-only input instead of over [lower](call.Args[0], ...)'s
// ordinary result.
//
// Unlike [lowerClamp]'s own bare-argument path, this composition does not
// route the result through [guardedValueProjection]'s duplicate-labelset
// Aggregate: [projectCanonicalFloatValue] is used instead, identical to
// [lowerMathFnOverMixedExpHistogramSetOp] and
// [lowerRoundToNearestOverMixedExpHistogramSetOp] just above — see
// [projectCanonicalFloatValue]'s own doc comment for why every wrapper
// composed over this Mixed-set-op shape shares that choice.
func lowerClampOverMixedExpHistogramSetOp(call *parser.Call, b *parser.BinaryExpr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	floatRowsOnly, err := floatRowsOnlyOverMixedExpHistogramSetOp(b, s, ctx)
	if err != nil {
		return nil, err
	}

	valueRef := &chplan.ColumnRef{Name: s.ValueColumn}

	switch call.Func.Name {
	case "clamp_max", "clamp_min":
		fnName := chplan.FnLeast
		if call.Func.Name == "clamp_min" {
			fnName = chplan.FnGreatest
		}
		if bound, ok := tryScalarLiteral(call.Args[1]); ok {
			newValue := &chplan.FuncCall{
				Fn:   fnName,
				Args: []chplan.Expr{valueRef, &chplan.LitFloat{V: bound}},
			}
			return projectCanonicalFloatValue(floatRowsOnly, s, newValue), nil
		}
		boundE, err := lowerScalarArg(call.Args[1], s, ctx)
		if err != nil {
			return nil, err
		}
		newValue := nanIfExpr(isNaNExpr(boundE), &chplan.FuncCall{
			Fn:   fnName,
			Args: []chplan.Expr{valueRef, boundE},
		})
		return projectCanonicalFloatValue(floatRowsOnly, s, newValue), nil

	case "clamp":
		minB, okMin := tryScalarLiteral(call.Args[1])
		maxB, okMax := tryScalarLiteral(call.Args[2])
		if okMin && okMax {
			// Mirrors lowerClamp's own maxVal < minVal fold (cerberus
			// issue #2345's compat-lane finding): Prom's funcClamp
			// short-circuits to an empty Vector rather than clamping
			// every sample to minB.
			if maxB < minB {
				empty := &chplan.Filter{
					Input:     floatRowsOnly,
					Predicate: &chplan.LitBool{V: false},
				}
				return projectCanonicalFloatValue(empty, s, valueRef), nil
			}
			newValue := &chplan.FuncCall{
				Fn: chplan.FnGreatest,
				Args: []chplan.Expr{
					&chplan.LitFloat{V: minB},
					&chplan.FuncCall{
						Fn:   chplan.FnLeast,
						Args: []chplan.Expr{&chplan.LitFloat{V: maxB}, valueRef},
					},
				},
			}
			return projectCanonicalFloatValue(floatRowsOnly, s, newValue), nil
		}

		minE, err := lowerScalarArg(call.Args[1], s, ctx)
		if err != nil {
			return nil, err
		}
		maxE, err := lowerScalarArg(call.Args[2], s, ctx)
		if err != nil {
			return nil, err
		}
		// Runtime mirror of the literal path's maxB < minB fold: keep
		// rows only while NOT (max < min). NaN bounds compare false —
		// rows survive and the NaN guard below turns the values NaN,
		// matching Prom's math.Max(min, math.Min(max, v)).
		filtered := &chplan.Filter{
			Input: floatRowsOnly,
			Predicate: &chplan.FuncCall{
				Fn: chplan.FnNot,
				Args: []chplan.Expr{
					&chplan.Binary{Op: chplan.OpLt, Left: maxE, Right: minE},
				},
			},
		}
		newValue := nanIfExpr(
			&chplan.Binary{Op: chplan.OpOr, Left: isNaNExpr(minE), Right: isNaNExpr(maxE)},
			&chplan.FuncCall{
				Fn: chplan.FnGreatest,
				Args: []chplan.Expr{
					minE,
					&chplan.FuncCall{
						Fn:   chplan.FnLeast,
						Args: []chplan.Expr{maxE, valueRef},
					},
				},
			},
		)
		return projectCanonicalFloatValue(filtered, s, newValue), nil
	}
	return nil, fmt.Errorf("promql: unknown clamp function %s", call.Func.Name)
}
