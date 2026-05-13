package promql

import (
	"fmt"
	"time"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// Lower turns a parsed PromQL expression into a chplan tree, using s for
// table and column name conventions.
//
// Supports: VectorSelector, MatrixSelector (only as a Call argument),
// range-vector Call (`rate` / `increase` / `delta` / `*_over_time`),
// instant-vector Call (`abs`, `sqrt`, `ln`, ...), AggregateExpr with
// `by (...)`, ParenExpr, BinaryExpr with scalar/vector arithmetic,
// SubqueryExpr (P0 4.5–4.7: bare-vector, over range-vector calls,
// outer reducer over subquery).
//
// Deferred to RC3 / later milestones: nested subqueries, subquery
// over AggregateExpr, subquery `@ start()`/`@ end()`, native-histogram
// `histogram_quantile`, `predict_linear`/`holt_winters`, exemplars.
func Lower(expr parser.Expr, s schema.Metrics) (chplan.Node, error) {
	return lower(expr, s, lowerCtx{})
}

// LowerAt is the time-aware variant of [Lower] used by handlers that
// know the query's evaluation range (start / end). It threads those
// times through to the `@ start()` / `@ end()` modifier resolution so
// `metric @ start()` lowers against the request's start time instead
// of erroring out.
//
// For an instant query the API layer passes start == end == ts; for a
// query_range it passes the request's start / end.
func LowerAt(expr parser.Expr, s schema.Metrics, start, end time.Time) (chplan.Node, error) {
	return lower(expr, s, lowerCtx{start: start, end: end})
}

func lower(expr parser.Expr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	switch e := expr.(type) {
	case *parser.VectorSelector:
		return lowerVectorSelector(e, s, ctx)
	case *parser.Call:
		return lowerCall(e, s, ctx)
	case *parser.AggregateExpr:
		return lowerAggregate(e, s, ctx)
	case *parser.ParenExpr:
		return lower(e.Expr, s, ctx)
	case *parser.BinaryExpr:
		return lowerBinary(e, s, ctx)
	case *parser.SubqueryExpr:
		return lowerSubquery(e, s, ctx)
	default:
		return nil, fmt.Errorf("promql: unsupported expression %T", expr)
	}
}

// lowerVectorSelector turns `metric{label="val"}` into Scan + Filter.
// `@` and `offset` modifiers add a `Timestamp <= anchor` predicate so the
// instant evaluation reflects the requested shifted time.
func lowerVectorSelector(v *parser.VectorSelector, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	metricName := metricNameFromMatchers(v.LabelMatchers)
	table := s.GaugeTable
	if metricName != "" {
		table = s.TableFor(metricName)
	}

	scan := &chplan.Scan{Table: table}

	pred := buildPredicate(v.LabelMatchers, s)
	if hasModifier(v) {
		anchor, err := anchorFromSelector(v, ctx)
		if err != nil {
			return nil, err
		}
		timeBound := timeBoundExpr(s.TimestampColumn, anchor)
		if pred == nil {
			pred = timeBound
		} else {
			pred = &chplan.Binary{Op: chplan.OpAnd, Left: pred, Right: timeBound}
		}
	}
	if pred == nil {
		return scan, nil
	}
	return &chplan.Filter{Input: scan, Predicate: pred}, nil
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

// lowerCall dispatches PromQL function calls. The arg shape decides the
// path: a MatrixSelector means a range-vector function (rate, increase,
// *_over_time); the clamp family takes a vector + scalar bounds; everything
// else is treated as an instant-vector math function (abs, sqrt, ln, ...)
// if recognised. Other functions surface a clear "not yet supported"
// error pointing at the relevant milestone.
func lowerCall(c *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	if len(c.Args) >= 1 {
		if _, ok := c.Args[0].(*parser.MatrixSelector); ok {
			return lowerRangeVectorCall(c, s, ctx)
		}
		if sq, ok := c.Args[0].(*parser.SubqueryExpr); ok {
			// `<range-vector-fn>(<subquery>)` — the canonical Grafana
			// shape `max_over_time(rate(m[5m])[1h:5m])`. Lowers to a
			// chained RangeWindow: outer reducer over the inner matrix.
			return lowerOuterRangeFnOverSubquery(c, sq, s, ctx)
		}
	}
	switch c.Func.Name {
	case "clamp", "clamp_min", "clamp_max":
		return lowerClamp(c, s, ctx)
	}
	if chFn, ok := instantFnCH[c.Func.Name]; ok {
		return lowerInstantFn(c, s, chFn, ctx)
	}
	return nil, fmt.Errorf("promql: function %s is not yet supported", c.Func.Name)
}

// lowerRangeVectorCall handles range-vector functions: rate, increase,
// delta, and the `*_over_time` family. The single argument is a
// MatrixSelector wrapping a VectorSelector; we lower the VectorSelector
// and wrap the result in a RangeWindow capturing the function name +
// range duration.
func lowerRangeVectorCall(c *parser.Call, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	switch c.Func.Name {
	case "predict_linear":
		return lowerPredictLinear(c, s, ctx)
	case "holt_winters", "double_exponential_smoothing":
		return lowerHoltWinters(c, s, ctx)
	}
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

	anchor, err := anchorFromSelector(vs, ctx)
	if err != nil {
		return nil, err
	}

	// The RangeWindow already encodes the window's eval anchor; emitting a
	// duplicate time-bound predicate on the inner Filter would double-count.
	// Build the inner Scan/Filter without the modifier-derived bound here.
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
		Func:            c.Func.Name,
		Range:           ms.Range,
		End:             anchor.End,
		Offset:          anchor.Offset,
		TimestampColumn: s.TimestampColumn,
		ValueColumn:     s.ValueColumn,
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: s.AttributesColumn}},
	}, nil
}

// lowerAggregate handles `sum by (job) (...)`, `sum without (instance) (...)`,
// `count(...)`, `stddev(...)`, `stdvar(...)`, `group(...)`, and
// `quantile(0.95, ...)`. Output-shape-changing aggregates (`topk`, `bottomk`,
// `count_values`) are deferred to M1.7 since they produce K rows per group
// rather than one.
//
// The Aggregate is wrapped with a Project that re-shapes its output into
// the Sample contract (MetricName, Attributes, TimeUnix, Value) so the
// API layer can stream rows through `chclient.Sample` directly. PromQL
// aggregations drop `__name__`, so the projected MetricName is the empty
// string; the projected Attributes is built from the group-key columns.
func lowerAggregate(a *parser.AggregateExpr, s schema.Metrics, ctx lowerCtx) (chplan.Node, error) {
	input, err := lower(a.Expr, s, ctx)
	if err != nil {
		return nil, err
	}

	groupBy, err := aggregateGroupBy(a, s)
	if err != nil {
		return nil, err
	}

	aggFunc, err := buildAggFunc(a, s)
	if err != nil {
		return nil, err
	}

	aliases := groupKeyAliases(len(groupBy))
	agg := &chplan.Aggregate{
		Input:          input,
		GroupBy:        groupBy,
		GroupByAliases: aliases,
		AggFuncs:       []chplan.AggFunc{aggFunc},
	}
	return wrapAggregateForSample(agg, a, s, aliases), nil
}

// groupKeyAliases returns ["gkey_0", "gkey_1", ...] of length n. Empty
// slice for n=0 so unaggregated aggregates (`count(up)` with no `by/
// without`) still skip the aliasing path.
func groupKeyAliases(n int) []string {
	if n == 0 {
		return nil
	}
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("gkey_%d", i)
	}
	return out
}

// wrapAggregateForSample produces the Sample-shape Project on top of an
// Aggregate so downstream `chclient.Sample` decoding works for any
// PromQL aggregation.
//
//	MetricName  = ''                          (aggregations drop __name__)
//	Attributes  = map('lbl0', gkey_0, ...)    for `by (lbl0, lbl1, ...)`
//	            | gkey_0                       for `without (...)` (mapFilter output)
//	            | empty Map(String,String)     for unaggregated forms
//	TimeUnix    = now64(9)                    (eval time)
//	Value       = <aggFunc alias>             (sum / avg / quantile / ...)
func wrapAggregateForSample(agg *chplan.Aggregate, a *parser.AggregateExpr, s schema.Metrics, aliases []string) chplan.Node {
	var attrs chplan.Expr
	switch {
	case len(aliases) == 0:
		// No grouping — emit an empty Map(String, String).
		attrs = emptyAttrsMap()
	case a.Without:
		// Single mapFilter-derived attribute column; the gkey IS the map.
		attrs = &chplan.ColumnRef{Name: aliases[0]}
	default:
		args := make([]chplan.Expr, 0, len(a.Grouping)*2)
		for i, label := range a.Grouping {
			args = append(args, &chplan.LitString{V: label}, &chplan.ColumnRef{Name: aliases[i]})
		}
		attrs = &chplan.FuncCall{Name: "map", Args: args}
	}

	return &chplan.Project{
		Input: agg,
		Projections: []chplan.Projection{
			{Expr: &chplan.LitString{V: ""}, Alias: s.MetricNameColumn},
			{Expr: attrs, Alias: s.AttributesColumn},
			{Expr: &chplan.FuncCall{Name: "now64", Args: []chplan.Expr{&chplan.LitInt{V: 9}}}, Alias: s.TimestampColumn},
			{Expr: &chplan.ColumnRef{Name: s.ValueColumn}, Alias: s.ValueColumn},
		},
	}
}

// emptyAttrsMap returns a CH expression for an empty Map(String,String),
// used when an aggregation drops all labels (e.g. `count(up)` with no
// `by/without` clause).
func emptyAttrsMap() chplan.Expr {
	return &chplan.FuncCall{
		Name: "CAST",
		Args: []chplan.Expr{
			&chplan.FuncCall{Name: "map", Args: nil},
			&chplan.LitString{V: "Map(String,String)"},
		},
	}
}

// aggregateGroupBy builds the group-key list for an aggregation. For
// `by (...)` it returns one MapAccess per named label; for `without (...)`
// it returns a single MapWithoutKeys spanning the full Attributes map with
// the named labels stripped.
func aggregateGroupBy(a *parser.AggregateExpr, s schema.Metrics) ([]chplan.Expr, error) {
	if a.Without {
		return []chplan.Expr{
			&chplan.MapWithoutKeys{
				Map:  &chplan.ColumnRef{Name: s.AttributesColumn},
				Keys: append([]string(nil), a.Grouping...),
			},
		}, nil
	}
	out := make([]chplan.Expr, 0, len(a.Grouping))
	for _, label := range a.Grouping {
		out = append(out, &chplan.MapAccess{
			Map: &chplan.ColumnRef{Name: s.AttributesColumn},
			Key: &chplan.LitString{V: label},
		})
	}
	return out, nil
}

// buildAggFunc produces the single AggFunc for an aggregation. Output-shape-
// changing aggregates (`topk`, `bottomk`, `count_values`) are intentionally
// rejected here pointing at M1.7.
func buildAggFunc(a *parser.AggregateExpr, s schema.Metrics) (chplan.AggFunc, error) {
	valueArg := &chplan.ColumnRef{Name: s.ValueColumn}

	switch a.Op {
	case parser.SUM, parser.COUNT, parser.AVG, parser.MIN, parser.MAX, parser.STDDEV, parser.STDVAR:
		if a.Param != nil {
			return chplan.AggFunc{}, fmt.Errorf("promql: aggregation %s does not take a parameter", a.Op.String())
		}
		name, err := plainAggCH(a.Op)
		if err != nil {
			return chplan.AggFunc{}, err
		}
		return chplan.AggFunc{
			Name:  name,
			Args:  []chplan.Expr{valueArg},
			Alias: s.ValueColumn,
		}, nil

	case parser.GROUP:
		// PromQL `group(...)` returns 1 for every label combination; emit
		// `any(1)` which yields a constant 1 per CH group.
		if a.Param != nil {
			return chplan.AggFunc{}, fmt.Errorf("promql: group() does not take a parameter")
		}
		return chplan.AggFunc{
			Name:  "any",
			Args:  []chplan.Expr{&chplan.LitInt{V: 1}},
			Alias: s.ValueColumn,
		}, nil

	case parser.QUANTILE:
		phi, ok := tryScalarLiteral(a.Param)
		if !ok {
			return chplan.AggFunc{}, fmt.Errorf("promql: quantile(phi, ...) requires a scalar literal phi (computed phi defers to M1.7)")
		}
		return chplan.AggFunc{
			Name:   "quantile",
			Params: []chplan.Expr{&chplan.LitFloat{V: phi}},
			Args:   []chplan.Expr{valueArg},
			Alias:  s.ValueColumn,
		}, nil

	case parser.TOPK, parser.BOTTOMK, parser.COUNT_VALUES:
		return chplan.AggFunc{}, fmt.Errorf("promql: %s changes output shape and lands with M1.7 result shaping", a.Op.String())
	}

	return chplan.AggFunc{}, fmt.Errorf("promql: aggregation op %s is not yet supported", a.Op.String())
}

// plainAggCH maps a non-parameterised PromQL aggregator to its CH name.
func plainAggCH(op parser.ItemType) (string, error) {
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
	case parser.STDDEV:
		return "stddevPop", nil
	case parser.STDVAR:
		return "varPop", nil
	}
	return "", fmt.Errorf("promql: aggregation op %s is not yet supported", op.String())
}
