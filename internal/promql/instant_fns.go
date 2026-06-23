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

	// Trigonometric family. PromQL's trig functions operate per-row on
	// `Value` and interpret/return angles in RADIANS — exactly CH's
	// convention — so each maps 1:1 to the same-named CH builtin. All are
	// Float64-in/Float64-out (unlike `sgn`, which needs a toFloat64 wrap).
	"acos":  "acos",
	"acosh": "acosh",
	"asin":  "asin",
	"asinh": "asinh",
	"atan":  "atan",
	"atanh": "atanh",
	"cos":   "cos",
	"cosh":  "cosh",
	"sin":   "sin",
	"sinh":  "sinh",
	"tan":   "tan",
	"tanh":  "tanh",

	// Degrees ↔ radians conversion. PromQL `deg(x)` = `x * 180/π` and
	// `rad(x)` = `x * π/180`; CH spells these `degrees(x)` / `radians(x)`.
	"deg": "degrees",
	"rad": "radians",
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
		return nil, fmt.Errorf("promql: %s with %d arguments is unsupported (instant math fns are unary)",
			c.Func.Name, len(c.Args))
	}

	inner, err := lower(c.Args[0], s, ctx)
	if err != nil {
		return nil, err
	}

	var newValue chplan.Expr = &chplan.FuncCall{
		Name: chFn,
		Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ValueColumn}},
	}
	if chFn == "sign" {
		// CH's sign() returns Int8; every other function in
		// instantFnCH is Float64-in/Float64-out. The wire scanner
		// reads Value as *float64, so an unwrapped sign() 502s with
		// "converting Int8 to *float64 is unsupported" — surfaced by
		// the showcase-promql sgn() panel.
		newValue = &chplan.FuncCall{Name: "toFloat64", Args: []chplan.Expr{newValue}}
	}
	return projectValueOverInner(inner, s, newValue), nil
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
	var tn chplan.Expr
	if toNearest, ok := tryScalarLiteral(c.Args[1]); ok {
		tn = &chplan.LitFloat{V: toNearest}
	} else {
		computed, err := lowerScalarArg(c.Args[1], s, ctx)
		if err != nil {
			return nil, err
		}
		tn = computed
	}

	inner, err := lower(c.Args[0], s, ctx)
	if err != nil {
		return nil, err
	}

	valueRef := &chplan.ColumnRef{Name: s.ValueColumn}

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
func lowerClamp(c *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	switch c.Func.Name {
	case "clamp_max", "clamp_min":
		if len(c.Args) != 2 {
			return nil, fmt.Errorf("promql: %s expects 2 arguments, got %d", c.Func.Name, len(c.Args))
		}
		fnName := "least"
		if c.Func.Name == "clamp_min" {
			fnName = "greatest"
		}
		if bound, ok := tryScalarLiteral(c.Args[1]); ok {
			inner, err := lower(c.Args[0], s, ctx)
			if err != nil {
				return nil, err
			}
			newValue := &chplan.FuncCall{
				Name: fnName,
				Args: []chplan.Expr{
					&chplan.ColumnRef{Name: s.ValueColumn},
					&chplan.LitFloat{V: bound},
				},
			}
			return projectValueOverInner(inner, s, newValue), nil
		}
		boundE, err := lowerScalarArg(c.Args[1], s, ctx)
		if err != nil {
			return nil, err
		}
		inner, err := lower(c.Args[0], s, ctx)
		if err != nil {
			return nil, err
		}
		newValue := nanIfExpr(isNaNExpr(boundE), &chplan.FuncCall{
			Name: fnName,
			Args: []chplan.Expr{
				&chplan.ColumnRef{Name: s.ValueColumn},
				boundE,
			},
		})
		return projectValueOverInner(inner, s, newValue), nil

	case "clamp":
		if len(c.Args) != 3 {
			return nil, fmt.Errorf("promql: clamp expects 3 arguments, got %d", len(c.Args))
		}
		minB, okMin := tryScalarLiteral(c.Args[1])
		maxB, okMax := tryScalarLiteral(c.Args[2])
		if okMin && okMax {
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
		inner, err := lower(c.Args[0], s, ctx)
		if err != nil {
			return nil, err
		}
		// Runtime mirror of the literal path's maxB < minB fold: keep
		// rows only while NOT (max < min). NaN bounds compare false —
		// rows survive and the NaN guard below turns the values NaN,
		// matching Prom's math.Max(min, math.Min(max, v)).
		filtered := &chplan.Filter{
			Input: inner,
			Predicate: &chplan.FuncCall{
				Name: "not",
				Args: []chplan.Expr{
					&chplan.Binary{Op: chplan.OpLt, Left: maxE, Right: minE},
				},
			},
		}
		valueRef := &chplan.ColumnRef{Name: s.ValueColumn}
		newValue := nanIfExpr(
			&chplan.Binary{Op: chplan.OpOr, Left: isNaNExpr(minE), Right: isNaNExpr(maxE)},
			&chplan.FuncCall{
				Name: "greatest",
				Args: []chplan.Expr{
					minE,
					&chplan.FuncCall{
						Name: "least",
						Args: []chplan.Expr{maxE, valueRef},
					},
				},
			},
		)
		return projectValueOverInner(filtered, s, newValue), nil
	}
	return nil, fmt.Errorf("promql: unknown clamp function %s", c.Func.Name)
}

// projectValueOverInner wraps inner with a Project that keeps the
// label-bearing columns and replaces Value with newValue.
//
// The set of forwarded columns depends on the inner shape:
//
//   - LWR / Aggregate / Project / Filter / Scan: Attributes / Timestamp
//     flow through unchanged; MetricName is replaced with an empty
//     string to match PromQL's "drop __name__ on derived samples"
//     rule — every caller (instant math fns, clamp family, unary
//     minus, date fns, quantile-out-of-range fold) produces a derived
//     sample that Prom strips `__name__` from. The previous shape
//     (forwarding `MetricName` verbatim) caused ~30+ of the 107
//     compat-lane diffs in Pool-AU's audit (#355) for queries like
//     `abs(metric)` showing Metric: metric{...} on cerberus vs
//     Metric: {...} on reference Prometheus.
//
//   - RangeWindow: only `Attributes` + `Value` survive the windowed
//     groupArray — MetricName and TimeUnix never make it through, so
//     this branch already matches Prom semantics by construction.
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
		//
		// The matrix RangeWindow ALSO surfaces `anchor_ts AS TimeUnix`
		// (the schema timestamp column — see range_window.go
		// `outer.Select(As(verbatim("anchor_ts"), r.TimestampColumn))`).
		// A step-aligned vector↔vector join reads that column off each
		// arm via `argMax(Value, TimeUnix)` / `As(Col(TimeUnix), …)`;
		// when a scalar-wrapped arm (`100 * rate(…)`) feeds the join,
		// dropping TimeUnix here makes the join wrapper reference a
		// column that never materialises, so CH fails with code 47
		// `Unknown expression identifier 'TimeUnix'` / `_join_TimeUnix`.
		// Forward it so the scalar arm carries the same join-key columns
		// the bare-rate arm does. The non-join path is unaffected: its
		// outer wrapWithSampleProjection reads `anchor_ts`, not TimeUnix,
		// and an extra subquery column is harmless.
		if rw.OuterRange > 0 {
			projections = append(
				projections,
				chplan.Projection{Expr: &chplan.ColumnRef{Name: "anchor_ts"}},
				chplan.Projection{
					Expr:  &chplan.ColumnRef{Name: s.TimestampColumn},
					Alias: s.TimestampColumn,
				},
			)
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
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}},
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}},
			{Expr: newValue, Alias: s.ValueColumn},
		},
	}
}
