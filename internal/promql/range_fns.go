package promql

import (
	"fmt"
	"math"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// lowerPredictLinear handles `predict_linear(v range-vector, t scalar)`:
// fits a simple linear regression to the samples in the lookback window
// and projects the line t seconds forward from the window's anchor.
//
// IR: a RangeWindow with Func="predict_linear" and Scalars=[t_seconds].
// The chsql emitter renders the regression using CH's native
// `simpleLinearRegression(x, y)`, which returns a (slope, intercept)
// tuple.
func lowerPredictLinear(c *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if len(c.Args) != 2 {
		return nil, fmt.Errorf("promql: predict_linear expects 2 arguments, got %d", len(c.Args))
	}
	tSeconds, ok := tryScalarLiteral(c.Args[1])
	if !ok {
		return nil, fmt.Errorf("promql: predict_linear requires a scalar-literal predict horizon (computed t is unsupported)")
	}
	ms, vs, err := matrixAndSelector(c, c.Args[0])
	if err != nil {
		return nil, err
	}

	anchor, err := anchorFromSelector(vs, ctx)
	if err != nil {
		return nil, err
	}
	vsNoModifier := *vs
	vsNoModifier.Timestamp = nil
	vsNoModifier.OriginalOffset = 0
	vsNoModifier.Offset = 0
	vsNoModifier.StartOrEnd = 0
	rangeCtx := ctx
	rangeCtx.inRangeVector = true
	inner, err := lowerVectorSelector(&vsNoModifier, s, rangeCtx)
	if err != nil {
		return nil, err
	}
	rw := &chplan.RangeWindow{
		Input:           inner,
		Func:            "predict_linear",
		Range:           ms.Range,
		End:             anchor.End,
		Offset:          anchor.Offset,
		Scalars:         []float64{tSeconds},
		TimestampColumn: s.TimestampColumn,
		ValueColumn:     s.ValueColumn,
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}},
	}
	// Range mode (ctx.step > 0): fan across the request's step grid so
	// each anchor in [start, end] emits its own per-window prediction.
	// Mirrors lowerRangeVectorCall — without this gate the outer
	// `RangeWindow` defaults to Step=0 and the matrix pivot collapses
	// every step bucket onto a single anchor at end_ts.
	if ctx.step > 0 && !ctx.start.IsZero() && !ctx.end.IsZero() {
		rw.Start = ctx.start.UTC()
		rw.End = ctx.end.UTC()
		rw.Step = ctx.step
		rw.OuterRange = ctx.end.Sub(ctx.start)
	}
	return rw, nil
}

// lowerHoltWinters handles `holt_winters(v range-vector, sf scalar, tf
// scalar)`: applies double-exponential (Holt-Winters) smoothing to the
// samples in the lookback window and returns the smoothed value at the
// window's anchor.
//
// IR: a RangeWindow with Func="holt_winters" and Scalars=[sf, tf]. The
// chsql emitter renders the recurrence as an arrayMap lambda over the
// windowed array.
func lowerHoltWinters(c *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if len(c.Args) != 3 {
		return nil, fmt.Errorf("promql: holt_winters expects 3 arguments, got %d", len(c.Args))
	}
	sf, okSf := tryScalarLiteral(c.Args[1])
	tf, okTf := tryScalarLiteral(c.Args[2])
	if !okSf || !okTf {
		return nil, fmt.Errorf("promql: holt_winters requires scalar-literal smoothing and trend factors")
	}
	if sf <= 0 || sf >= 1 {
		return nil, fmt.Errorf("promql: holt_winters smoothing factor must be in (0, 1), got %v", sf)
	}
	if tf <= 0 || tf >= 1 {
		return nil, fmt.Errorf("promql: holt_winters trend factor must be in (0, 1), got %v", tf)
	}
	ms, vs, err := matrixAndSelector(c, c.Args[0])
	if err != nil {
		return nil, err
	}

	anchor, err := anchorFromSelector(vs, ctx)
	if err != nil {
		return nil, err
	}
	vsNoModifier := *vs
	vsNoModifier.Timestamp = nil
	vsNoModifier.OriginalOffset = 0
	vsNoModifier.Offset = 0
	vsNoModifier.StartOrEnd = 0
	rangeCtx := ctx
	rangeCtx.inRangeVector = true
	inner, err := lowerVectorSelector(&vsNoModifier, s, rangeCtx)
	if err != nil {
		return nil, err
	}
	rw := &chplan.RangeWindow{
		Input: inner,
		// Use the canonical "holt_winters" name in the IR regardless
		// of whether the source query used the legacy name or the new
		// `double_exponential_smoothing` alias — the emitter switches
		// on the IR name only.
		Func:            "holt_winters",
		Range:           ms.Range,
		End:             anchor.End,
		Offset:          anchor.Offset,
		Scalars:         []float64{sf, tf},
		TimestampColumn: s.TimestampColumn,
		ValueColumn:     s.ValueColumn,
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}},
	}
	// Range mode (ctx.step > 0): fan across the request's step grid so
	// each anchor in [start, end] emits its own per-window smoothed
	// value (matches the lowerRangeVectorCall matrix gate).
	if ctx.step > 0 && !ctx.start.IsZero() && !ctx.end.IsZero() {
		rw.Start = ctx.start.UTC()
		rw.End = ctx.end.UTC()
		rw.Step = ctx.step
		rw.OuterRange = ctx.end.Sub(ctx.start)
	}
	return rw, nil
}

// lowerQuantileOverTime handles `quantile_over_time(phi, v[range])`:
// PromQL's range-vector quantile reducer with phi as a scalar-literal
// first argument and the range vector as the second. CH emits it as
// `quantile(phi)(window_vals)` inside the standard windowed-array
// idiom.
//
// IR: a RangeWindow with Func="quantile_over_time" and
// Scalars=[phi]. The chsql emitter switches on Func and reads
// Scalars[0] for the quantile parameter.
//
// Out-of-range phi: PromQL's funcQuantileOverTime delegates to the
// shared `quantile()` helper, which returns -Inf for phi < 0 and +Inf
// for phi > 1 (see prometheus/promql/quantile.go). ClickHouse's
// `quantile` aggregate rejects phi outside [0, 1], so the lowerer
// detects the out-of-range case at compile time, builds the
// RangeWindow with phi clamped to a valid sentinel (0.5 — the actual
// computed value is discarded), and post-Projects the Value column to
// +Inf / -Inf. The empty-window branch still produces NaN per Prom
// semantics (matching the unconditional `if(length(...) > 0, ..., nan)`
// the emitter already writes).
func lowerQuantileOverTime(c *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if len(c.Args) != 2 {
		return nil, fmt.Errorf("promql: quantile_over_time expects 2 arguments, got %d", len(c.Args))
	}
	phi, ok := tryScalarLiteral(c.Args[0])
	if !ok {
		return nil, fmt.Errorf("promql: quantile_over_time requires a scalar-literal phi (computed phi is unsupported)")
	}
	ms, vs, err := matrixAndSelector(c, c.Args[1])
	if err != nil {
		return nil, err
	}

	anchor, err := anchorFromSelector(vs, ctx)
	if err != nil {
		return nil, err
	}
	vsNoModifier := *vs
	vsNoModifier.Timestamp = nil
	vsNoModifier.OriginalOffset = 0
	vsNoModifier.Offset = 0
	vsNoModifier.StartOrEnd = 0
	rangeCtx := ctx
	rangeCtx.inRangeVector = true
	inner, err := lowerVectorSelector(&vsNoModifier, s, rangeCtx)
	if err != nil {
		return nil, err
	}

	// Detect compile-time out-of-range phi. CH's quantile aggregate
	// errors on phi outside [0, 1]; substitute a safe phi for the
	// inner aggregate and post-Project the Value column to the
	// PromQL-spec Inf-valued constant. NaN phi rides the same path
	// (CH would also error on `quantile(NaN)`) and is materialised
	// as a NaN literal in the post-Project — empty windows still
	// produce `nan` via the emitter's length-guarded `if`.
	infValue, replaceValue := outOfRangePhiInf(phi)

	emitPhi := phi
	if replaceValue {
		emitPhi = 0.5
	}

	window := &chplan.RangeWindow{
		Input:           inner,
		Func:            "quantile_over_time",
		Range:           ms.Range,
		End:             anchor.End,
		Offset:          anchor.Offset,
		Scalars:         []float64{emitPhi},
		TimestampColumn: s.TimestampColumn,
		ValueColumn:     s.ValueColumn,
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}},
	}
	// Range mode (ctx.step > 0): fan across the request's step grid so
	// each anchor in [start, end] emits its own per-window quantile
	// (matches the lowerRangeVectorCall matrix gate).
	if ctx.step > 0 && !ctx.start.IsZero() && !ctx.end.IsZero() {
		window.Start = ctx.start.UTC()
		window.End = ctx.end.UTC()
		window.Step = ctx.step
		window.OuterRange = ctx.end.Sub(ctx.start)
	}
	if !replaceValue {
		return window, nil
	}
	return projectValueOverInner(window, s, &chplan.LitFloat{V: infValue}), nil
}

// outOfRangePhiInf reports whether phi falls outside PromQL's valid
// quantile range [0, 1] and returns the PromQL-spec replacement value:
//
//   - phi < 0  → (-Inf, true)
//   - phi > 1  → (+Inf, true)
//   - NaN       → (NaN,  true)   (mirrors Prom's `quantile()` helper)
//   - otherwise → (0,    false)
//
// Callers fold a `true` result into a post-Project on the aggregate /
// range-window output so CH's `quantile()` aggregate never sees an
// out-of-range phi (CH errors on phi outside [0, 1]). The two phi-
// taking call sites — `quantile_over_time(phi, v[range])` and
// `quantile(phi, V)` aggregator — share this single fold.
func outOfRangePhiInf(phi float64) (float64, bool) {
	switch {
	case math.IsNaN(phi):
		return math.NaN(), true
	case phi < 0:
		return math.Inf(-1), true
	case phi > 1:
		return math.Inf(+1), true
	}
	return 0, false
}

// matrixAndSelector extracts the *parser.MatrixSelector and inner
// *parser.VectorSelector from a function call's first arg, with the
// canonical error messages the other range-vector functions use.
func matrixAndSelector(c *parser.Call, arg parser.Expr) (*parser.MatrixSelector, *parser.VectorSelector, error) {
	ms, ok := arg.(*parser.MatrixSelector)
	if !ok {
		return nil, nil, fmt.Errorf("promql: %s first argument must be a range-vector selector, got %T",
			c.Func.Name, arg)
	}
	vs, ok := ms.VectorSelector.(*parser.VectorSelector)
	if !ok {
		return nil, nil, fmt.Errorf("promql: matrix selector's inner must be a VectorSelector, got %T",
			ms.VectorSelector)
	}
	return ms, vs, nil
}
