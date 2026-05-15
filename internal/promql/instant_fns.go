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
var instantFnCH = map[string]string{
	"abs":   "abs",
	"ceil":  "ceil",
	"floor": "floor",
	"round": "round",
	"sqrt":  "sqrt",
	"exp":   "exp",
	"ln":    "log",
	"log2":  "log2",
	"log10": "log10",
	"sgn":   "sign",
}

// lowerInstantFn handles single-arg math functions like abs / sqrt / ln. The
// arg is expected to be an instant-vector expression; we lower it, then
// wrap with a Project that maps the Value column through the CH function.
//
// Multi-arg variants of round and the clamp family are handled separately.
func lowerInstantFn(c *parser.Call, s schema.Metrics, chFn string, ctx lowerCtx) (chplan.Node, error) {
	switch c.Func.Name {
	case "round":
		if len(c.Args) == 2 {
			return lowerRoundToNearest(c, s, ctx)
		}
	}

	if len(c.Args) != 1 {
		return nil, fmt.Errorf("promql: %s with %d arguments is not yet supported (instant math fns are unary)",
			c.Func.Name, len(c.Args))
	}

	inner, err := lower(c.Args[0], s, ctx)
	if err != nil {
		return nil, err
	}

	newValue := &chplan.FuncCall{
		Name: chFn,
		Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ValueColumn}},
	}
	return projectValueOverInner(inner, s, newValue), nil
}

// lowerRoundToNearest implements PromQL `round(v, to_nearest)` as
// `round(Value / to_nearest) * to_nearest`. CH's native `round(v, N)`
// rounds to N decimal places, not to a multiple, so we synthesise the
// multiple-rounding semantics explicitly.
func lowerRoundToNearest(c *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	toNearest, ok := tryScalarLiteral(c.Args[1])
	if !ok {
		return nil, fmt.Errorf("promql: round(v, to_nearest) requires a scalar literal to_nearest")
	}

	inner, err := lower(c.Args[0], s, ctx)
	if err != nil {
		return nil, err
	}

	valueRef := &chplan.ColumnRef{Name: s.ValueColumn}
	tn := &chplan.LitFloat{V: toNearest}

	rounded := &chplan.FuncCall{
		Name: "round",
		Args: []chplan.Expr{&chplan.Binary{Op: chplan.OpDiv, Left: valueRef, Right: tn}},
	}
	newValue := &chplan.Binary{Op: chplan.OpMul, Left: rounded, Right: tn}
	return projectValueOverInner(inner, s, newValue), nil
}

// lowerClamp implements the PromQL clamp family:
//
//	clamp_max(v, max) → least(Value, max)
//	clamp_min(v, min) → greatest(Value, min)
//	clamp(v, min, max) → greatest(min, least(max, Value))
//
// Bounds must be scalar literals at lowering time (computed bounds defer
// to RC2 when scalars are first-class chplan nodes).
func lowerClamp(c *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	switch c.Func.Name {
	case "clamp_max", "clamp_min":
		if len(c.Args) != 2 {
			return nil, fmt.Errorf("promql: %s expects 2 arguments, got %d", c.Func.Name, len(c.Args))
		}
		bound, ok := tryScalarLiteral(c.Args[1])
		if !ok {
			return nil, fmt.Errorf("promql: %s requires a scalar-literal bound", c.Func.Name)
		}
		inner, err := lower(c.Args[0], s, ctx)
		if err != nil {
			return nil, err
		}
		fnName := "least"
		if c.Func.Name == "clamp_min" {
			fnName = "greatest"
		}
		newValue := &chplan.FuncCall{
			Name: fnName,
			Args: []chplan.Expr{
				&chplan.ColumnRef{Name: s.ValueColumn},
				&chplan.LitFloat{V: bound},
			},
		}
		return projectValueOverInner(inner, s, newValue), nil

	case "clamp":
		if len(c.Args) != 3 {
			return nil, fmt.Errorf("promql: clamp expects 3 arguments, got %d", len(c.Args))
		}
		minB, okMin := tryScalarLiteral(c.Args[1])
		maxB, okMax := tryScalarLiteral(c.Args[2])
		if !okMin || !okMax {
			return nil, fmt.Errorf("promql: clamp requires scalar-literal bounds for min and max")
		}
		inner, err := lower(c.Args[0], s, ctx)
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
		valueRef := &chplan.ColumnRef{Name: s.ValueColumn}
		newValue := &chplan.FuncCall{
			Name: "greatest",
			Args: []chplan.Expr{
				&chplan.LitFloat{V: minB},
				&chplan.FuncCall{
					Name: "least",
					Args: []chplan.Expr{&chplan.LitFloat{V: maxB}, valueRef},
				},
			},
		}
		return projectValueOverInner(inner, s, newValue), nil
	}
	return nil, fmt.Errorf("promql: unknown clamp function %s", c.Func.Name)
}

// projectValueOverInner wraps inner with a Project that keeps the
// label-bearing columns and replaces Value with newValue.
//
// The set of forwarded columns depends on the inner shape:
//
//   - LWR / Aggregate / Project / Filter / Scan: MetricName / Attributes
//     / Timestamp are all in scope, so we forward all four columns.
//
//   - RangeWindow: only `Attributes` + `Value` survive the windowed
//     groupArray — MetricName and TimeUnix never make it through.
//
// The text-equality goldens in test/spec/promql/ track both shapes; see
// e.g. `edge_abs_over_rate.txtar` (instant fn over rate) and
// `unary_minus_rate.txtar` (unary minus over rate).
func projectValueOverInner(inner chplan.Node, s schema.Metrics, newValue chplan.Expr) chplan.Node {
	if rw, ok := inner.(*chplan.RangeWindow); ok {
		projections := []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}},
		}
		// Matrix-shape RangeWindow (range-mode subqueries + range-mode
		// `rate`/`*_over_time` queries with Step > 0) exposes
		// `anchor_ts` as the per-row per-anchor timestamp. The outer
		// wrapWithSampleProjection reads it back through this Project
		// (when `isMatrixRangeWindow` walks past the value-rewrite
		// Project layer); forwarding the column keeps the per-anchor
		// time-bucketing intact for callers like `abs(avg_over_time(…))`.
		if rw.OuterRange > 0 {
			projections = append(projections, chplan.Projection{
				Expr: &chplan.ColumnRef{Name: "anchor_ts"},
			})
		}
		projections = append(projections, chplan.Projection{Expr: newValue, Alias: s.ValueColumn})
		return &chplan.Project{
			Input:       inner,
			Projections: projections,
		}
	}
	return &chplan.Project{
		Input: inner,
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: s.MetricNameColumn}},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}},
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}},
			{Expr: newValue, Alias: s.ValueColumn},
		},
	}
}
