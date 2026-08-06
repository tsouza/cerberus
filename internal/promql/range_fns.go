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
	switch rangeGridShapeFor(vs, ctx) {
	case gridFanout:
		// Fan across the request's step grid so each anchor in
		// [start, end] emits its own per-window prediction. Without this
		// the outer `RangeWindow` defaults to Step=0 and the matrix pivot
		// collapses every step bucket onto a single anchor at end_ts.
		applyStepGridFanout(rw, ctx)
	case gridBroadcast:
		// `@`-pinned: one prediction from the pinned window, broadcast
		// across the grid. The native PredictLinear strategy is skipped
		// deliberately — timeSeriesPredictLinearToGrid computes a
		// per-grid-anchor regression, which is the shape the pin forbids.
		return wrapRangeWindowAtBroadcast(rw, ctx, s, nil, &chplan.ColumnRef{Name: s.ValueColumn}), nil
	case gridSingleAnchor:
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
// IR: a RangeWindow with Func="holt_winters" and Scalars=[sf, tf], or
// ScalarExprs=[sfExpr, tfExpr] when either factor is computed. The chsql
// emitter renders the recurrence as an arrayFold over the windowed array.
//
// Factor domain: reference Prometheus's funcDoubleExponentialSmoothing
// requires 0 < sf < 1 and 0 < tf < 1, and errors the QUERY when a factor
// falls outside. A literal (or constant-foldable) factor is known at
// lowering time, so cerberus rejects it here — the same accept/reject
// boundary upstream draws, just moved earlier. A COMPUTED factor
// (`double_exponential_smoothing(v[5m], scalar(x), 0.3)`) is not known
// until execution, and upstream accepts that query shape and evaluates
// it, so cerberus lowers it too; the (0, 1) check rides into SQL as a
// per-row guard (see chsql.holtWintersFactorInDomain).
func lowerHoltWinters(c *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if len(c.Args) != 3 {
		return nil, fmt.Errorf("promql: holt_winters expects 3 arguments, got %d", len(c.Args))
	}
	sf, tf, sfExpr, tfExpr, err := holtWintersFactors(c.Args[1], c.Args[2], s, ctx)
	if err != nil {
		return nil, err
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
		TimestampColumn: s.TimestampColumn,
		ValueColumn:     s.ValueColumn,
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}},
	}
	bindHoltWintersFactors(rw, sf, tf, sfExpr, tfExpr)
	switch rangeGridShapeFor(vs, ctx) {
	case gridFanout:
		// Fan across the request's step grid so each anchor in
		// [start, end] emits its own per-window smoothed value.
		applyStepGridFanout(rw, ctx)
	case gridBroadcast:
		// `@`-pinned: smooth the pinned window once, broadcast the
		// per-series result across the grid.
		return wrapRangeWindowAtBroadcast(rw, ctx, s, nil, &chplan.ColumnRef{Name: s.ValueColumn}), nil
	case gridSingleAnchor:
	}
	return rw, nil
}

// holtWintersFactors resolves the smoothing and trend arguments of
// `double_exponential_smoothing` (and its legacy `holt_winters`
// spelling) to either a literal pair (sf, tf — both scalar-foldable) or
// a computed pair (sfExpr, tfExpr — scalar-subquery expressions). The
// two forms are mutually exclusive per factor slot, but the RangeWindow
// carries the pair as a unit, so ONE computed factor promotes BOTH to
// the expression binding: [chplan.RangeWindow.ScalarExprs] is positional
// and the emitter reads index 0 as sf and index 1 as tf.
//
// A literal factor outside (0, 1) is rejected here — the exact boundary
// reference Prometheus draws at eval time. A computed factor's value is
// unknowable at lowering time, so it passes and the emitter's per-row
// guard handles the domain.
//
// Shared by [lowerHoltWinters] (matrix-selector argument) and
// threadOuterRangeFnScalars (subquery argument) so the two argument
// forms cannot drift apart.
func holtWintersFactors(sfArg, tfArg parser.Expr, s schema.Metrics, ctx lowerCtx) (sf, tf float64, sfExpr, tfExpr chplan.Expr, err error) {
	sf, okSf := tryScalarLiteral(sfArg)
	tf, okTf := tryScalarLiteral(tfArg)
	if okSf && (sf <= 0 || sf >= 1) {
		return 0, 0, nil, nil, fmt.Errorf("promql: holt_winters smoothing factor must be in (0, 1), got %v", sf)
	}
	if okTf && (tf <= 0 || tf >= 1) {
		return 0, 0, nil, nil, fmt.Errorf("promql: holt_winters trend factor must be in (0, 1), got %v", tf)
	}
	if okSf && okTf {
		return sf, tf, nil, nil, nil
	}
	// At least one factor is computed: lower BOTH into the expression
	// slots so the positional ScalarExprs pair stays complete. A literal
	// alongside a computed sibling lowers through the same
	// scalar-argument path, which folds it straight back to a literal.
	sfExpr, err = lowerScalarArg(sfArg, s, ctx)
	if err != nil {
		return 0, 0, nil, nil, err
	}
	tfExpr, err = lowerScalarArg(tfArg, s, ctx)
	if err != nil {
		return 0, 0, nil, nil, err
	}
	return 0, 0, sfExpr, tfExpr, nil
}

// bindHoltWintersFactors writes the factor pair [holtWintersFactors]
// resolved onto the RangeWindow, choosing the literal Scalars slot or
// the computed ScalarExprs slot. Extracted so the matrix-selector and
// subquery lowerings bind the IR identically.
func bindHoltWintersFactors(rw *chplan.RangeWindow, sf, tf float64, sfExpr, tfExpr chplan.Expr) {
	if sfExpr != nil {
		rw.ScalarExprs = []chplan.Expr{sfExpr, tfExpr}
		rw.Scalars = nil
		return
	}
	rw.Scalars = []float64{sf, tf}
	rw.ScalarExprs = nil
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
	// The Value column the output carries: the window's own quantile, or
	// the PromQL-spec ±Inf / NaN constant for an out-of-range literal phi
	// (the sentinel-phi window still decides WHICH series exist, only its
	// computed value is discarded).
	value := chplan.Expr(&chplan.ColumnRef{Name: s.ValueColumn})
	if replaceValue {
		value = &chplan.LitFloat{V: infValue}
	}
	switch rangeGridShapeFor(vs, ctx) {
	case gridFanout:
		// Fan across the request's step grid so each anchor in
		// [start, end] emits its own per-window quantile.
		applyStepGridFanout(window, ctx)
	case gridBroadcast:
		// `@`-pinned: one quantile over the pinned window, broadcast
		// across the grid. The value substitution folds into the
		// broadcast projection rather than going through
		// projectValueOverInner, which has no branch for that shape.
		return wrapRangeWindowAtBroadcast(window, ctx, s, nil, value), nil
	case gridSingleAnchor:
	}
	if !replaceValue {
		return window, nil
	}
	return projectValueOverInner(window, s, value), nil
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
