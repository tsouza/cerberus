package traceql

import (
	"fmt"
	"reflect"

	"github.com/grafana/tempo/pkg/traceql"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// lowerAggregate handles `| count()` for the M4.3 slice. `sum(...)`,
// `avg(...)`, `max(...)`, `min(...)` parse but their inner attribute
// expression sits on Tempo's unexported `e` field; reflect can't read
// it without panicking. They surface as "not yet supported" until
// Tempo exposes an accessor (or we adopt an unsafe shim).
//
// The lowering wraps the previous pipeline element's plan with a
// chplan.Aggregate. count() has no inner expression — we aggregate
// the constant 1 per row.
func lowerAggregate(prev chplan.Node, agg traceql.Aggregate, _ schema.Traces) (chplan.Node, error) {
	op := readAggregateFields(agg)
	if op == "" {
		return nil, fmt.Errorf("traceql: aggregate operator not extractable from parser AST")
	}
	if op != "count" {
		return nil, fmt.Errorf("traceql: aggregate `%s` requires reading the inner attribute expression, which Tempo's parser keeps on an unexported field. Lands when upstream adds an accessor or cerberus adopts an `unsafe` shim", op)
	}

	const valueAlias = "Value"
	chFunc, err := mapAggregateOp(op)
	if err != nil {
		return nil, err
	}

	return &chplan.Aggregate{
		Input: prev,
		AggFuncs: []chplan.AggFunc{{
			Name:  chFunc,
			Args:  []chplan.Expr{&chplan.LitInt{V: 1}},
			Alias: valueAlias,
		}},
	}, nil
}

// readAggregateFields reflects Aggregate's unexported `op` field.
// Tempo's parser doesn't expose an accessor — no Op() method, no
// public field. The `e` (inner expression) field is also unexported,
// but reflect.Value.Interface() panics on unexported fields, so we
// can't safely read it here. Aggregates over an inner attribute
// (`sum(.duration)`, `avg(.x)`, `max/min`) currently surface as
// "not yet supported"; only `count()` (which has no inner expr) lowers
// cleanly via this M4.3 slice. The rest land when Tempo upstream adds
// accessors or we adopt a guarded `unsafe.Pointer` shim.
func readAggregateFields(agg traceql.Aggregate) string {
	v := reflect.ValueOf(agg)
	if v.Kind() != reflect.Struct {
		return ""
	}
	opField := v.FieldByName("op")
	if !opField.IsValid() {
		return ""
	}
	return aggregateOpName(opField.Int())
}

// aggregateOpName mirrors Tempo's enum_aggregates.go iota order:
//
//	count(0), max(1), min(2), sum(3), avg(4)
//
// It's the only safe way to map back since the constants are
// unexported.
func aggregateOpName(i int64) string {
	switch i {
	case 0:
		return "count"
	case 1:
		return "max"
	case 2:
		return "min"
	case 3:
		return "sum"
	case 4:
		return "avg"
	}
	return ""
}

// mapAggregateOp turns a TraceQL aggregate op name into the CH agg
// function name. count / max / min / sum / avg map 1:1.
func mapAggregateOp(op string) (string, error) {
	switch op {
	case "count", "max", "min", "sum", "avg":
		return op, nil
	}
	return "", fmt.Errorf("traceql: aggregate op %q is not yet supported", op)
}
