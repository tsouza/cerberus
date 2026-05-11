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
// Multi-arg forms (e.g. PromQL `round(v, to_nearest)`) are deferred — only
// the unary form lands in M1.3.
func lowerInstantFn(c *parser.Call, s schema.Metrics, chFn string) (chplan.Node, error) {
	if len(c.Args) != 1 {
		return nil, fmt.Errorf("promql: %s with %d arguments is not yet supported (M1.3 supports the unary form only)",
			c.Func.Name, len(c.Args))
	}

	inner, err := lower(c.Args[0], s)
	if err != nil {
		return nil, err
	}

	newValue := &chplan.FuncCall{
		Name: chFn,
		Args: []chplan.Expr{&chplan.ColumnRef{Name: s.ValueColumn}},
	}
	return &chplan.Project{
		Input: inner,
		Projections: []chplan.Projection{
			{Expr: &chplan.ColumnRef{Name: s.MetricNameColumn}},
			{Expr: &chplan.ColumnRef{Name: s.AttributesColumn}},
			{Expr: &chplan.ColumnRef{Name: s.TimestampColumn}},
			{Expr: newValue, Alias: s.ValueColumn},
		},
	}, nil
}
