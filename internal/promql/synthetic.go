package promql

import (
	"fmt"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// syntheticScalarVector builds a Project-over-OneRow plan that
// materialises a single sample with empty labels and the supplied
// value/timestamp expressions. Used by `time()`, `vector(scalar)`, and
// the scalar-only binop fold path — all three are PromQL constructs
// that produce one labelled-empty sample per evaluation.
//
// The output shape matches the canonical chclient.Sample contract:
// MetricName / Attributes / TimeUnix / Value in that order so the API
// layer can stream rows through chclient.Sample.Scan without a per-
// query projection.
//
// timeExpr defaults to `now64(9)` when nil — every callsite except the
// `time()` lowering wants the eval anchor's wall-clock representation
// rather than the timestamp-as-value reflection that `time()` uses.
func syntheticScalarVector(valueExpr, timeExpr chplan.Expr, s schema.Metrics) chplan.Node {
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
	anchor := anchorBaseExpr(evalAnchor{End: ctx.end.UTC()})
	if ctx.end.IsZero() {
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
	return syntheticScalarVector(valueExpr, nil, s), nil
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
	return syntheticScalarVector(&chplan.LitFloat{V: v}, nil, s), nil
}
