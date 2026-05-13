package promql

import (
	"fmt"

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
		return nil, fmt.Errorf("promql: predict_linear requires a scalar-literal predict horizon (computed t defers to RC3)")
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
	inner, err := lowerVectorSelector(&vsNoModifier, s, ctx)
	if err != nil {
		return nil, err
	}
	return &chplan.RangeWindow{
		Input:           inner,
		Func:            "predict_linear",
		Range:           ms.Range,
		End:             anchor.End,
		Offset:          anchor.Offset,
		Scalars:         []float64{tSeconds},
		TimestampColumn: s.TimestampColumn,
		ValueColumn:     s.ValueColumn,
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}},
	}, nil
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
	inner, err := lowerVectorSelector(&vsNoModifier, s, ctx)
	if err != nil {
		return nil, err
	}
	return &chplan.RangeWindow{
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
	}, nil
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
