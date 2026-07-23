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
	var (
		tSeconds float64
		tExpr    chplan.Expr
	)
	if t, ok := tryScalarLiteral(c.Args[1]); ok {
		tSeconds = t
	} else {
		// Computed horizon (`predict_linear(v[r], scalar(x))`): bind t
		// as a scalar-subquery expression on RangeWindow.ScalarExprs.
		// A NaN t (scalar() over 0 / many series) propagates NaN
		// through `intercept + slope * t`, matching Prom's arithmetic.
		computed, err := lowerScalarArg(c.Args[1], s, ctx)
		if err != nil {
			return nil, err
		}
		tExpr = computed
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
		TimestampColumn: s.TimestampColumn,
		ValueColumn:     s.ValueColumn,
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}},
	}
	if tExpr != nil {
		rw.ScalarExprs = []chplan.Expr{tExpr}
	} else {
		rw.Scalars = []float64{tSeconds}
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
	// Route through the boot-wired PredictLinear strategy: the native impl
	// emits a RangeWindowNative (timeSeriesPredictLinearToGrid) for an eligible
	// range-mode window with a whole-second literal horizon, the fan-out impl
	// returns rw unchanged. The feature/version decision lives in WHICH strategy
	// cmd/cerberus wired — there is NO feature-flag / version read here, and the
	// strategy always returns a valid lowering (never nil). Mirrors the
	// rate/changes/resets dispatch in lowerRangeVectorCall.
	return ctx.lowerers.PredictLinear.LowerPredictLinear(rw, s), nil
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
	var (
		phi     float64
		phiOK   bool
		phiExpr chplan.Expr
	)
	if v, ok := tryScalarLiteral(c.Args[0]); ok {
		phi, phiOK = v, true
	} else {
		// Computed phi (`quantile_over_time(scalar(x), v[r])`): bind
		// phi as a scalar-subquery expression on
		// RangeWindow.ScalarExprs. The emitter switches to a manual
		// arraySort interpolation (Prom's quantile() formula) with the
		// NaN / out-of-range domain rules resolved at runtime — CH's
		// parameterised `quantile(<literal>)` arrayReduce spelling
		// can't bind a computed parameter.
		computed, err := lowerScalarArg(c.Args[0], s, ctx)
		if err != nil {
			return nil, err
		}
		phiExpr = computed
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

	window := &chplan.RangeWindow{
		Input:           inner,
		Func:            "quantile_over_time",
		Range:           ms.Range,
		End:             anchor.End,
		Offset:          anchor.Offset,
		TimestampColumn: s.TimestampColumn,
		ValueColumn:     s.ValueColumn,
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}},
	}

	// Literal phi: detect compile-time out-of-range phi. PromQL's
	// quantile() returns -Inf for phi<0, +Inf for phi>1, NaN for NaN.
	// For an OUT-OF-RANGE / NaN literal the inner aggregate is fed a
	// safe sentinel phi (0.5 — value discarded) and the Value column is
	// post-Projected to the PromQL-spec constant; empty windows still
	// produce `nan` via the emitter's length-guarded `if`.
	//
	// For an IN-RANGE literal phi we route through the SAME exact
	// arraySort + linear-interpolation branch the computed-phi path
	// uses (RangeWindow.ScalarExprs), materialising phi as a LitFloat.
	// This replaces CH's approximate t-digest `quantile()` aggregate —
	// which diverges from reference PromQL on larger windows — with
	// Prometheus's exact `quantile()` formula (rank = phi*(N-1), linear
	// interpolation between the two nearest ranks). The emitter's
	// multiIf domain guards (isNaN / <0 / >1) are constant-folded by CH
	// for the literal; the single-sample window falls out naturally
	// (rank=0 → sorted[0]).
	//
	// Computed phi: the emitter's ScalarExprs path embeds the runtime
	// domain rules (multiIf over isNaN / <0 / >1) directly in the
	// per-window value expression, so no post-Project is needed.
	var (
		infValue     float64
		replaceValue bool
	)
	if phiOK {
		infValue, replaceValue = outOfRangePhiInf(phi)
		if replaceValue {
			// Out-of-range / NaN literal: feed a safe sentinel to the
			// inner aggregate and fold the spec value via post-Project.
			window.Scalars = []float64{0.5}
		} else {
			// In-range literal: exact interpolation via ScalarExprs.
			window.ScalarExprs = []chplan.Expr{&chplan.LitFloat{V: phi}}
		}
	} else {
		window.ScalarExprs = []chplan.Expr{phiExpr}
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
