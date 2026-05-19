// This file (and select.go) read parser AST nodes exclusively via the
// upstream-fork-exposed accessors on github.com/tsouza/tempo:cerberus-accessors
// — no reflection, no pointer aliasing tricks. See docs/upstream-forks.md.

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
// docs/upstream-forks.md.
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

	// Map(String, String) coercion: when the aggregate input is a
	// FieldAccess against SpanAttributes / ResourceAttributes the value
	// is a String. ClickHouse refuses `max(String) > 100` with
	// NO_COMMON_TYPE; wrap in `toFloat64OrZero(...)` at lowering time so
	// the aggregate sees a Float64 and the downstream numeric
	// comparison resolves. Intrinsic ColumnRefs (Duration etc.) lower
	// to a bare ColumnRef and pass through unchanged.
	arg = coerceMapNumericAggInput(arg)

	return &chplan.Aggregate{
		Input: prev,
		AggFuncs: []chplan.AggFunc{{
			Name:  chFunc,
			Args:  []chplan.Expr{arg},
			Alias: valueAlias,
		}},
	}, nil
}

// coerceMapNumericAggInput wraps Map-subscript expressions
// (`SpanAttributes['foo']`, `ResourceAttributes['foo']`) with
// `toFloat64OrZero(...)` so they can flow into a numeric CH aggregate
// (`max`/`min`/`sum`/`avg`/`quantiles`). The OTel-CH attribute carriers
// are typed `Map(String, String)`, so a bare subscript returns String —
// CH then refuses to compare the aggregate against a numeric literal
// with NO_COMMON_TYPE.
//
// The `OrZero` variant silently coerces strings that don't parse as
// numbers (matches Loki's silent-fallback for typed label filters via
// PR #479).
//
// Pass-through for everything else: intrinsic ColumnRefs (Duration,
// already Int64) need no cast; pre-wrapped FuncCalls (e.g. an
// arithmetic Binary that was already coerced) keep their existing
// shape.
func coerceMapNumericAggInput(expr chplan.Expr) chplan.Expr {
	if _, ok := expr.(*chplan.FieldAccess); ok {
		return &chplan.FuncCall{
			Name: "toFloat64OrZero",
			Args: []chplan.Expr{expr},
		}
	}
	return expr
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
	return "", fmt.Errorf("traceql: aggregate op %q is unsupported", op)
}
