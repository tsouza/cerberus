package promql

import (
	"fmt"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// syntheticScalarVector builds a Project-over-(OneRow|StepGrid) plan
// that materialises a synthetic sample with empty labels and the
// supplied value/timestamp expressions. Used by `time()`,
// `vector(scalar)`, and the scalar-only binop fold path — all three
// are PromQL constructs that produce one labelled-empty sample per
// evaluation.
//
// In instant mode (ctx.step == 0) the source is OneRow — exactly one
// sample at the eval anchor (matches Prom's instant query semantics
// and keeps existing byte-stable SQL fixtures unchanged).
//
// In range mode (ctx.step > 0) the source is a StepGrid emitting one
// row per step in `[ctx.start, ctx.end]`; the TimeUnix projection
// references `anchor_ts` so each step's row lands at the right
// bucket. The value expression is rewritten by [rewriteAnchorRefs]
// before being plugged in — any `now64(9)` it carries is replaced
// with the matching `anchor_ts` reference so per-step values reflect
// each step's evaluation timestamp rather than wall-clock now.
//
// The output shape matches the canonical chclient.Sample contract:
// MetricName / Attributes / TimeUnix / Value in that order so the API
// layer can stream rows through chclient.Sample.Scan without a per-
// query projection.
//
// timeExpr defaults to the eval-anchor (now64(9) instant / anchor_ts
// range) when nil — every callsite except the `time()` lowering wants
// the eval anchor's wall-clock representation rather than the
// timestamp-as-value reflection that `time()` uses.
func syntheticScalarVector(valueExpr, timeExpr chplan.Expr, s schema.Metrics, ctx lowerCtx) chplan.Node {
	if ctx.step > 0 {
		if timeExpr == nil {
			timeExpr = &chplan.ColumnRef{Name: "anchor_ts"}
		} else {
			timeExpr = rewriteAnchorRefs(timeExpr)
		}
		valueExpr = rewriteAnchorRefs(valueExpr)
		return &chplan.Project{
			Input: &chplan.StepGrid{Start: ctx.start.UTC(), End: ctx.end.UTC(), Step: ctx.step},
			Projections: []chplan.Projection{
				{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
				{Expr: emptyAttrsMap(), Alias: s.AttributesColumn},
				{Expr: timeExpr, Alias: s.TimestampColumn},
				{Expr: valueExpr, Alias: s.ValueColumn},
			},
		}
	}
	if timeExpr == nil {
		timeExpr = &chplan.FuncCall{
			Name: "now64",
			Args: []chplan.Expr{&chplan.LitInt{V: 9}},
		}
	}
	return &chplan.Project{
		Input: &chplan.OneRow{},
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: emptyAttrsMap(), Alias: s.AttributesColumn},
			{Expr: timeExpr, Alias: s.TimestampColumn},
			{Expr: valueExpr, Alias: s.ValueColumn},
		},
	}
}

// rewriteAnchorRefs walks expr and replaces every `now64(9)` /
// `now()` FuncCall with a ColumnRef to `anchor_ts`. Used by the
// range-mode synthetic-vector lowerings so the per-step value
// expression resolves to each step's anchor (the StepGrid's
// fanned-out `anchor_ts` column) rather than wall-clock now.
//
// `now64(9)` (DateTime64(9)) and `now()` (DateTime) both line up with
// the StepGrid's `anchor_ts` column type — CH narrows DateTime64 to
// DateTime implicitly inside the date-component builtins so the
// rewrite is type-safe across both shapes.
//
// Returns a shallow-copy expression tree; the caller's input is
// untouched.
func rewriteAnchorRefs(expr chplan.Expr) chplan.Expr {
	if expr == nil {
		return nil
	}
	switch v := expr.(type) {
	case *chplan.FuncCall:
		if v.Name == "now64" || v.Name == "now" {
			return &chplan.ColumnRef{Name: "anchor_ts"}
		}
		newArgs := make([]chplan.Expr, len(v.Args))
		for i, a := range v.Args {
			newArgs[i] = rewriteAnchorRefs(a)
		}
		return &chplan.FuncCall{Name: v.Name, Args: newArgs}
	case *chplan.Binary:
		return &chplan.Binary{Op: v.Op, Left: rewriteAnchorRefs(v.Left), Right: rewriteAnchorRefs(v.Right)}
	}
	return expr
}

// lowerTime implements PromQL `time()`. The function returns the
// query-evaluation timestamp as a scalar float (Unix seconds, with
// nanosecond precision retained on the fraction).
//
// Lowered as a single row whose Value column reflects the eval anchor
// resolved by [LowerAt]: when ctx.end is non-zero we emit a literal
// `toFloat64(toUnixTimestamp64Nano(<eval_ts>) / 1000000000)`; when it
// is zero (plain [Lower] without range threading) we fall back to
// `toUnixTimestamp64Nano(now64(9)) / 1000000000`. CH renders both as
// Float64; toFloat64 around the integer division would lose nanosecond
// precision so we wrap toFloat64 around the whole division.
//
// The synthesised row's TimeUnix column itself stays at `now64(9)` /
// the literal eval timestamp — Prom canonicalises this and downstream
// `sum(time())`, `floor(time())`, etc. only read Value.
func lowerTime(c *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if len(c.Args) != 0 {
		return nil, fmt.Errorf("promql: time() takes no arguments, got %d", len(c.Args))
	}
	// In range mode the anchor is per-step — leave the value expression
	// referencing `now64(9)` so [syntheticScalarVector] swaps it for the
	// StepGrid's `anchor_ts` column. In instant mode keep the existing
	// shape (literal eval_ts when threaded, now64(9) otherwise) so
	// per-fixture SQL stays byte-stable.
	var anchor chplan.Expr
	switch {
	case ctx.step > 0:
		anchor = &chplan.FuncCall{Name: "now64", Args: []chplan.Expr{&chplan.LitInt{V: 9}}}
	case !ctx.end.IsZero():
		anchor = anchorBaseExpr(evalAnchor{End: ctx.end.UTC()})
	default:
		anchor = anchorBaseExpr(evalAnchor{})
	}
	// toFloat64(toUnixTimestamp64Nano(anchor) / 1000000000) — the
	// outer toFloat64 widens the integer division to Float64 so the
	// sub-second fraction (when present) survives the cast.
	valueExpr := &chplan.FuncCall{
		Name: "toFloat64",
		Args: []chplan.Expr{
			&chplan.Binary{
				Op: chplan.OpDiv,
				Left: &chplan.FuncCall{
					Name: "toUnixTimestamp64Nano",
					Args: []chplan.Expr{anchor},
				},
				Right: &chplan.LitInt{V: 1_000_000_000},
			},
		},
	}
	return syntheticScalarVector(valueExpr, nil, s, ctx), nil
}

// lowerVector implements PromQL `vector(scalar)`. The function
// promotes a scalar to a 1-element instant vector with no labels.
// Per Prom spec the result has MetricName empty, an empty label set,
// the eval timestamp, and Value = scalar.
//
// The argument is either:
//
//   - A scalar-foldable expression — TryFoldScalar walks NumberLiteral /
//     ParenExpr / UnaryExpr / arithmetic BinaryExpr / bool-comparison
//     BinaryExpr, so `vector(1+2)` and `vector(-3)` and
//     `vector(1 < bool 2)` reduce to a single literal here.
//
//   - A scalar-returning function call like `time()` — Prom's `time()`
//     returns a scalar, and `vector(time())` is equivalent to `time()`
//     (both produce a 1-element vector with the eval timestamp as
//     Value). We forward to the existing lowering rather than
//     re-implementing it.
//
// Vector arguments (e.g. `vector(rate(m[5m]))`) would just be the
// inner vector unchanged, but in practice PromQL syntax doesn't allow
// passing a vector to `vector()` (the parser rejects it at parse
// time), so we don't need to handle that path.
func lowerVector(c *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if len(c.Args) != 1 {
		return nil, fmt.Errorf("promql: vector() expects 1 argument, got %d", len(c.Args))
	}
	// `vector(time())` — forward to the existing scalar-returning
	// lowering. The result shape is identical: a 1-row synthetic
	// vector with the eval timestamp as Value.
	if inner, ok := c.Args[0].(*parser.Call); ok && inner.Func.Name == "time" {
		return lowerTime(inner, s, ctx)
	}
	v, ok := TryFoldScalar(c.Args[0])
	if !ok {
		return nil, fmt.Errorf("promql: vector() requires a scalar-foldable argument or time()")
	}
	return syntheticScalarVector(&chplan.LitFloat{V: v}, nil, s, ctx), nil
}
