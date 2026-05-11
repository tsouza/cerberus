package promql

import (
	"fmt"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// Lower turns a parsed PromQL expression into a chplan tree, using s for
// table and column name conventions.
//
// v0.1 scope: VectorSelector, MatrixSelector (only as a Call argument),
// Call with `rate` / `increase` / `delta` / `*_over_time`, AggregateExpr
// with `by (...)`, ParenExpr. Subqueries, scalar arithmetic, `without`,
// and BinaryExpr are out of scope for the seed.
func Lower(expr parser.Expr, s schema.Metrics) (chplan.Node, error) {
	return lower(expr, s)
}

func lower(expr parser.Expr, s schema.Metrics) (chplan.Node, error) {
	switch e := expr.(type) {
	case *parser.VectorSelector:
		return lowerVectorSelector(e, s), nil
	case *parser.Call:
		return lowerCall(e, s)
	case *parser.AggregateExpr:
		return lowerAggregate(e, s)
	case *parser.ParenExpr:
		return lower(e.Expr, s)
	default:
		return nil, fmt.Errorf("promql: unsupported expression %T", expr)
	}
}

// lowerVectorSelector turns `metric{label="val"}` into Scan + Filter.
func lowerVectorSelector(v *parser.VectorSelector, s schema.Metrics) chplan.Node {
	metricName := metricNameFromMatchers(v.LabelMatchers)
	table := s.GaugeTable
	if metricName != "" {
		table = s.TableFor(metricName)
	}

	scan := &chplan.Scan{Table: table}

	pred := buildPredicate(v.LabelMatchers, s)
	if pred == nil {
		return scan
	}
	return &chplan.Filter{Input: scan, Predicate: pred}
}

// metricNameFromMatchers returns the value of the __name__ matcher (if any
// exists with MatchType == Equal); empty string otherwise. Used to pick the
// CH table for VectorSelectors that name a specific metric.
func metricNameFromMatchers(ms []*labels.Matcher) string {
	for _, m := range ms {
		if m.Name == model.MetricNameLabel && m.Type == labels.MatchEqual {
			return m.Value
		}
	}
	return ""
}

// buildPredicate AND-folds the label matchers into a single chplan.Expr.
// __name__ goes against the MetricName column; everything else goes against
// `Attributes[<label>]` via MapAccess.
func buildPredicate(matchers []*labels.Matcher, s schema.Metrics) chplan.Expr {
	var out chplan.Expr
	for _, m := range matchers {
		cond := matcherToExpr(m, s)
		if out == nil {
			out = cond
			continue
		}
		out = &chplan.Binary{Op: chplan.OpAnd, Left: out, Right: cond}
	}
	return out
}

func matcherToExpr(m *labels.Matcher, s schema.Metrics) chplan.Expr {
	var lhs chplan.Expr
	if m.Name == model.MetricNameLabel {
		lhs = &chplan.ColumnRef{Name: s.MetricNameColumn}
	} else {
		lhs = &chplan.MapAccess{
			Map: &chplan.ColumnRef{Name: s.AttributesColumn},
			Key: &chplan.LitString{V: m.Name},
		}
	}
	return &chplan.Binary{
		Op:    matchOp(m.Type),
		Left:  lhs,
		Right: &chplan.LitString{V: m.Value},
	}
}

func matchOp(t labels.MatchType) chplan.BinaryOp {
	switch t {
	case labels.MatchEqual:
		return chplan.OpEq
	case labels.MatchNotEqual:
		return chplan.OpNe
	case labels.MatchRegexp:
		return chplan.OpMatch
	case labels.MatchNotRegexp:
		return chplan.OpNotMatch
	}
	// Any new labels.MatchType added upstream would land here as Equal —
	// safer than panicking, and we'd notice via the spec tests.
	return chplan.OpEq
}

// lowerCall handles range-vector functions: rate, increase, delta, and the
// `*_over_time` family. The single argument is a MatrixSelector wrapping a
// VectorSelector; we lower the VectorSelector and wrap the result in a
// RangeWindow capturing the function name + range duration.
func lowerCall(c *parser.Call, s schema.Metrics) (chplan.Node, error) {
	if len(c.Args) != 1 {
		return nil, fmt.Errorf("promql: %s expects exactly 1 argument, got %d", c.Func.Name, len(c.Args))
	}
	ms, ok := c.Args[0].(*parser.MatrixSelector)
	if !ok {
		return nil, fmt.Errorf("promql: %s argument must be a range-vector selector, got %T",
			c.Func.Name, c.Args[0])
	}
	vs, ok := ms.VectorSelector.(*parser.VectorSelector)
	if !ok {
		return nil, fmt.Errorf("promql: matrix selector's inner must be a VectorSelector, got %T",
			ms.VectorSelector)
	}

	inner := lowerVectorSelector(vs, s)
	return &chplan.RangeWindow{
		Input:           inner,
		Func:            c.Func.Name,
		Range:           ms.Range,
		TimestampColumn: s.TimestampColumn,
		ValueColumn:     s.ValueColumn,
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}},
	}, nil
}

// lowerAggregate handles `sum by (job) (...)`, `count(...)`, etc.
// Only the `by` form is supported in v0.1; `without` requires schema-wide
// label introspection that lands later.
func lowerAggregate(a *parser.AggregateExpr, s schema.Metrics) (chplan.Node, error) {
	if a.Without {
		return nil, fmt.Errorf("promql: 'without' aggregation is not yet supported (v0.1 supports 'by' only)")
	}
	if a.Param != nil {
		return nil, fmt.Errorf("promql: parameterised aggregation (%s) is not yet supported", a.Op.String())
	}

	input, err := lower(a.Expr, s)
	if err != nil {
		return nil, err
	}

	groupBy := make([]chplan.Expr, 0, len(a.Grouping))
	for _, label := range a.Grouping {
		groupBy = append(groupBy, &chplan.MapAccess{
			Map: &chplan.ColumnRef{Name: s.AttributesColumn},
			Key: &chplan.LitString{V: label},
		})
	}

	chFunc, err := chAggFunc(a.Op)
	if err != nil {
		return nil, err
	}
	return &chplan.Aggregate{
		Input:   input,
		GroupBy: groupBy,
		AggFuncs: []chplan.AggFunc{{
			Name:  chFunc,
			Args:  []chplan.Expr{&chplan.ColumnRef{Name: s.ValueColumn}},
			Alias: s.ValueColumn,
		}},
	}, nil
}

func chAggFunc(op parser.ItemType) (string, error) {
	switch op {
	case parser.SUM:
		return "sum", nil
	case parser.COUNT:
		return "count", nil
	case parser.AVG:
		return "avg", nil
	case parser.MIN:
		return "min", nil
	case parser.MAX:
		return "max", nil
	}
	return "", fmt.Errorf("promql: aggregation op %s is not yet supported", op.String())
}
