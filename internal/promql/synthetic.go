package promql

import (
	"fmt"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// secondsPerMilli scales a Unix-millisecond timestamp to fractional
// Unix seconds (matching upstream's `timestamp.FromTime(t) / 1000`).
const secondsPerMilli = 1000.0

// lowerQueryContextFold implements the four query-context PromQL
// functions — `start()`, `end()`, `range()`, `step()` — that the
// reference engine constant-folds per query execution
// (engine.foldQueryContextFunctions). Their values depend only on the
// query's eval range (ctx.start / ctx.end / ctx.step), not on any
// series data, so cerberus folds them to a single synthetic scalar
// vector at lowering time — exactly mirroring how `time()` and
// `vector(N)` materialise a constant-per-step stream via
// [syntheticScalarVector].
//
// Reference semantics (Unix-second floats, matching upstream exactly):
//
//   - start() → epoch seconds of the query start  (FromTime(start)/1000)
//   - end()   → epoch seconds of the query end    (FromTime(end)/1000)
//   - range() → (end - start) in seconds
//   - step()  → the query_range step in seconds, or 0 for an instant
//     query (start == end). Upstream gates on `!start.Equal(end)`.
//
// All four take zero arguments (the parser's function table pins
// ArgTypes to the empty slice); a non-zero arg count is a caller /
// parser bug and surfaces as an error rather than a silent fold.
//
// The folded float flows through [syntheticScalarVector] as a
// LitFloat, so the central Builder.Expr path wraps it in `toFloat64(?)`
// for the Float64 wire-shape pin — identical to `vector(N)`. In range
// mode the constant is fanned across the StepGrid; in instant mode it
// is a single OneRow sample.
func lowerQueryContextFold(c *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if len(c.Args) != 0 {
		return nil, fmt.Errorf("promql: %s() takes no arguments, got %d", c.Func.Name, len(c.Args))
	}
	var val float64
	switch c.Func.Name {
	case "start":
		val = float64(ctx.start.UnixMilli()) / secondsPerMilli
	case "end":
		val = float64(ctx.end.UnixMilli()) / secondsPerMilli
	case "range":
		val = ctx.end.Sub(ctx.start).Seconds()
	case "step":
		// Instant queries (start == end) have no step; upstream folds
		// step() to 0 in that case. Range queries fold to the request's
		// step duration in seconds.
		if !ctx.start.Equal(ctx.end) {
			val = ctx.step.Seconds()
		}
	default:
		return nil, fmt.Errorf("promql: %s is not a query-context function", c.Func.Name)
	}
	return syntheticScalarVector(&chplan.LitFloat{V: val}, nil, s, ctx), nil
}

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
//
// LitFloat value expressions flow through unchanged: the central
// `Builder.Expr` LitFloat path wraps every emitted LitFloat in
// `toFloat64(?)` so the Float64 wire-shape pin happens at the SQL
// layer, not at every lowering callsite. Pre-wrapped expressions
// (`time()`'s `toFloat64(toUnixTimestamp64Nano(...))`, the date-fn
// `asFloat64(...)` path, the scalar-binop fold's
// [foldSyntheticBinary] output) also pass through unchanged.
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
		timeExpr = chplan.NowNano()
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

// isSyntheticScalarPlan reports whether plan is the shape produced by
// [syntheticScalarVector]: a `Project` whose source is `OneRow` /
// `StepGrid` and whose projections match the canonical 4-slot synthetic
// vector layout — `LitString("") AS MetricName`, `<empty-map> AS
// Attributes`, `<ts_expr> AS TimeUnix`, `<value_expr> AS Value`.
//
// Synthetic-scalar lowerings (`time()`, `vector(scalar)`, zero-arg
// date functions) emit this exact shape: a one-per-step (range mode)
// or one-row (instant mode) "constant per step" stream with no input
// data dependency. When BOTH legs of a vector-vector binop lower to
// this shape, the [lowerVectorVector] path would join two N-row
// per-step streams via an INNER JOIN keyed on
// `(MetricName, Attributes)` — and because the per-side argMax wrap
// collapses each side to one row first, the join degenerates to
// 1-row × 1-row even though Prom would emit N rows. The fix is to
// detect this pair and fold to a single `Project` over a shared
// `StepGrid` / `OneRow` source, mirroring how [TryFoldScalar] folds
// pure literal-literal binops.
//
// The predicate is intentionally narrow: it matches only the shape
// `syntheticScalarVector` writes. Any callsite that mints a Project
// with a non-empty literal `MetricName`, a non-empty Attributes map,
// or a different projection order falls through to the regular
// V-V join path.
func isSyntheticScalarPlan(plan chplan.Node, s schema.Metrics) bool {
	p, ok := plan.(*chplan.Project)
	if !ok {
		return false
	}
	switch p.Input.(type) {
	case *chplan.OneRow, *chplan.StepGrid:
	default:
		return false
	}
	if len(p.Projections) != 4 {
		return false
	}
	// Projection 0 — MetricName slot: must be `LitString("")` aliased
	// to the metric-name column. Any other shape (e.g. a CAST or a
	// non-empty literal) means the plan carries identifying labels
	// the fold would erase.
	mn := p.Projections[0]
	if mn.Alias != s.MetricNameColumn {
		return false
	}
	if lit, ok := mn.Expr.(*chplan.LitString); !ok || lit.V != "" {
		return false
	}
	// Projection 1 — Attributes slot: must be the canonical empty-map
	// expression. We compare against a freshly-built emptyAttrsMap()
	// to avoid coupling to its internal shape (CAST(map(),
	// 'Map(String,String)')).
	at := p.Projections[1]
	if at.Alias != s.AttributesColumn {
		return false
	}
	if !at.Expr.Equal(emptyAttrsMap()) {
		return false
	}
	// Projection 2 — TimeUnix slot: alias-only check (the value
	// expression varies between `now64(9)` / a `toDateTime64` literal
	// / a `ColumnRef{anchor_ts}` depending on instant/range mode).
	if p.Projections[2].Alias != s.TimestampColumn {
		return false
	}
	// Projection 3 — Value slot: alias-only check (the value
	// expression is exactly what we want to extract).
	return p.Projections[3].Alias == s.ValueColumn
}

// syntheticValueExpr returns the Value-slot expression from a plan
// previously matched by [isSyntheticScalarPlan]. The caller MUST gate
// on `isSyntheticScalarPlan(plan, s) == true` before invoking —
// otherwise the function panics (caller bug).
func syntheticValueExpr(plan chplan.Node) chplan.Expr {
	return plan.(*chplan.Project).Projections[3].Expr
}

// syntheticSource returns the underlying `OneRow` / `StepGrid` source
// of a plan previously matched by [isSyntheticScalarPlan]. Used by the
// V-V synthetic-fold path to attach the combined Project to one of
// the two legs' sources (both legs share identical Start/End/Step in
// the StepGrid case, since they thread through the same lowerCtx; the
// instant-mode OneRow has no state).
func syntheticSource(plan chplan.Node) chplan.Node {
	return plan.(*chplan.Project).Input
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
		anchor = chplan.NowNano()
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
	if v, ok := TryFoldScalar(c.Args[0]); ok {
		return syntheticScalarVector(&chplan.LitFloat{V: v}, nil, s, ctx), nil
	}
	// Computed scalar (`vector(scalar(x))`, `vector(scalar(x) + 1)`):
	// bind the argument as a scalar-subquery expression in the same
	// synthetic one-row shape. `scalar()` over zero / many series is
	// NaN, and `vector(NaN)` is a one-sample vector whose value is NaN
	// — the scalarValuePlan reduction inside lowerScalarArg produces
	// exactly that. Range mode evaluates the scalar once at the eval
	// anchor (documented lowerScalarArg posture) and fans the constant
	// across the step grid.
	v, err := lowerScalarArg(c.Args[0], s, ctx)
	if err != nil {
		return nil, err
	}
	return syntheticScalarVector(v, nil, s, ctx), nil
}
