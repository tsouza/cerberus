package traceql

import (
	"fmt"
	"reflect"
	"unsafe"

	"github.com/grafana/tempo/pkg/traceql"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// lowerAggregate handles `| count()`, `| sum(...)`, `| avg(...)`,
// `| max(...)`, `| min(...)`. count() has no inner expression — we
// aggregate the constant 1 per row. The other four read the inner
// FieldExpression from Tempo's unexported `e` field via the
// readAggregateExpr unsafe shim (see comment below for the why).
func lowerAggregate(prev chplan.Node, agg traceql.Aggregate, s schema.Traces) (chplan.Node, error) {
	op := readAggregateFields(agg)
	if op == "" {
		return nil, fmt.Errorf("traceql: aggregate operator not extractable from parser AST")
	}

	chFunc, err := mapAggregateOp(op)
	if err != nil {
		return nil, err
	}

	const valueAlias = "Value"

	// count() takes no inner expression — aggregate a constant.
	if op == "count" {
		return &chplan.Aggregate{
			Input: prev,
			AggFuncs: []chplan.AggFunc{{
				Name:  chFunc,
				Args:  []chplan.Expr{&chplan.LitInt{V: 1}},
				Alias: valueAlias,
			}},
		}, nil
	}

	// sum/avg/max/min — extract the inner FieldExpression and lower it.
	inner, err := readAggregateExpr(agg)
	if err != nil {
		return nil, err
	}
	if inner == nil {
		return nil, fmt.Errorf("traceql: aggregate `%s` has nil inner expression", op)
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

// readAggregateExpr reads Tempo's unexported `e` field on a
// traceql.Aggregate. Tempo doesn't ship an accessor (no `Expr()`
// method, no public field) and reflect.Value.Interface() panics on
// unexported fields, so we take the value's address and reconstruct a
// typed pointer through unsafe.Pointer.
//
// Safe because: agg is passed by value, so &agg is a valid pointer to
// a local for this function's lifetime; the field offset is fixed by
// the pinned Tempo dependency in go.mod; the read is type-safe
// (FieldExpression is an interface, same width as the underlying
// itab+data tuple).
func readAggregateExpr(agg traceql.Aggregate) (traceql.FieldExpression, error) {
	v := reflect.ValueOf(&agg).Elem()
	field := v.FieldByName("e")
	if !field.IsValid() {
		return nil, fmt.Errorf("traceql: Aggregate has no `e` field (Tempo internal layout changed?)")
	}
	// #nosec G103 — intentional unsafe.Pointer to read Tempo's unexported
	// FieldExpression field. Safety justification is documented above.
	ptr := unsafe.Pointer(field.UnsafeAddr())
	expr := *(*traceql.FieldExpression)(ptr)
	return expr, nil
}

// readAggregateFields reflects Aggregate's unexported `op` field.
// Tempo's parser doesn't expose an accessor (no Op() method, no public
// field), so we map the enum's int value back to its name via the
// fixed iota order in aggregateOpName.
//
// The companion `e` field (inner FieldExpression) is read via
// readAggregateExpr below — same unexported-field problem, harder shim.
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
