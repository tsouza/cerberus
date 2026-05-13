// This file (and select.go) read parser AST nodes exclusively via the
// upstream-fork-exposed accessors on github.com/tsouza/tempo:cerberus-accessors
// — no reflection, no pointer aliasing tricks. See docs/fork-tempo-plan.md.

package traceql

import (
	"fmt"

	"github.com/grafana/tempo/pkg/traceql"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// lowerAggregate handles `| count()`, `| sum(...)`, `| avg(...)`,
// `| max(...)`, `| min(...)`. count() has no inner expression — we
// aggregate the constant 1 per row. The other four read the inner
// FieldExpression via the upstream-fork-exposed Aggregate.InnerExpr()
// accessor (github.com/tsouza/tempo:cerberus-accessors) — see
// docs/fork-tempo-plan.md.
func lowerAggregate(prev chplan.Node, agg traceql.Aggregate, s schema.Traces) (chplan.Node, error) {
	chFunc, err := mapAggregateOp(agg.Op())
	if err != nil {
		return nil, err
	}

	const valueAlias = "Value"

	// count() takes no inner expression — aggregate a constant.
	if agg.Op() == traceql.AggregateCount {
		return &chplan.Aggregate{
			Input: prev,
			AggFuncs: []chplan.AggFunc{{
				Name:  chFunc,
				Args:  []chplan.Expr{&chplan.LitInt{V: 1}},
				Alias: valueAlias,
			}},
		}, nil
	}

	// sum/avg/max/min — read the inner FieldExpression via the fork
	// accessor and lower it.
	inner := agg.InnerExpr()
	if inner == nil {
		return nil, fmt.Errorf("traceql: aggregate `%s` has nil inner expression", agg.Op())
	}
	arg, err := lowerFieldExpr(inner, s)
	if err != nil {
		return nil, err
	}

	return &chplan.Aggregate{
		Input: prev,
		AggFuncs: []chplan.AggFunc{{
			Name:  chFunc,
			Args:  []chplan.Expr{arg},
			Alias: valueAlias,
		}},
	}, nil
}

// mapAggregateOp turns a TraceQL AggregateOp into the CH agg function
// name. count / max / min / sum / avg map 1:1.
func mapAggregateOp(op traceql.AggregateOp) (string, error) {
	switch op {
	case traceql.AggregateCount:
		return "count", nil
	case traceql.AggregateMax:
		return "max", nil
	case traceql.AggregateMin:
		return "min", nil
	case traceql.AggregateSum:
		return "sum", nil
	case traceql.AggregateAvg:
		return "avg", nil
	}
	return "", fmt.Errorf("traceql: aggregate op %q is not yet supported", op)
}
