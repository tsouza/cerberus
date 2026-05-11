package promql

import (
	"fmt"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// lowerBinary handles PromQL BinaryExpr — arithmetic for now. Comparison
// and logical ops, plus vector-vector matching, land in M1.x follow-ups.
//
// Supported shapes:
//   - scalar OP scalar           → folded to chplan.LitFloat at lowering
//   - scalar OP vector / vec OP scalar → Project that maps Value through
//     the binary op
//
// Vector OP vector is rejected with a clear error pointing at M1.6
// (vector matching), since the join semantics need first-class support.
func lowerBinary(b *parser.BinaryExpr, s schema.Metrics) (chplan.Node, error) {
	op, err := promBinaryOp(b.Op, b.ReturnBool)
	if err != nil {
		return nil, err
	}

	lhsScalar, lhsIsScalar := tryScalarLiteral(b.LHS)
	rhsScalar, rhsIsScalar := tryScalarLiteral(b.RHS)

	switch {
	case lhsIsScalar && rhsIsScalar:
		return nil, fmt.Errorf("promql: scalar-only binary expressions not yet lowered (constant fold lands when scalars are first-class chplan nodes)")
	case lhsIsScalar:
		return projectWithScalar(b.RHS, s, op, lhsScalar, true /*scalarOnLeft*/)
	case rhsIsScalar:
		return projectWithScalar(b.LHS, s, op, rhsScalar, false)
	default:
		return nil, fmt.Errorf("promql: vector OP vector binary expressions require vector matching (lands in M1.6); op=%s", b.Op.String())
	}
}

// promBinaryOp maps a PromQL parser op to the chplan op. Comparison ops
// and the bool modifier defer to a follow-up.
func promBinaryOp(op parser.ItemType, returnBool bool) (chplan.BinaryOp, error) {
	if returnBool {
		return "", fmt.Errorf("promql: 'bool' modifier on binary ops is not yet supported")
	}
	switch op {
	case parser.ADD:
		return chplan.OpAdd, nil
	case parser.SUB:
		return chplan.OpSub, nil
	case parser.MUL:
		return chplan.OpMul, nil
	case parser.DIV:
		return chplan.OpDiv, nil
	case parser.MOD:
		return chplan.OpMod, nil
	case parser.POW:
		return chplan.OpPow, nil
	}
	return "", fmt.Errorf("promql: binary op %s not yet supported (comparison + logical ops land in M1.x follow-ups)", op.String())
}

// tryScalarLiteral unwraps NumberLiteral, ParenExpr around a literal, and
// UnaryExpr(-N) at lowering time. Returns the literal value and true on a
// match, or 0+false otherwise.
func tryScalarLiteral(e parser.Expr) (float64, bool) {
	switch v := e.(type) {
	case *parser.NumberLiteral:
		return v.Val, true
	case *parser.ParenExpr:
		return tryScalarLiteral(v.Expr)
	case *parser.UnaryExpr:
		if v.Op == parser.SUB {
			if inner, ok := tryScalarLiteral(v.Expr); ok {
				return -inner, true
			}
		}
		if v.Op == parser.ADD {
			return tryScalarLiteral(v.Expr)
		}
	}
	return 0, false
}

// projectWithScalar lowers the vector side and wraps it with a Project
// that replaces ValueColumn with (scalar OP Value) or (Value OP scalar).
//
// The projection list is explicit (MetricName, Attributes, TimestampColumn,
// transformed Value) so the downstream emitter knows the column shape.
// scalarOnLeft flips the operand order — important for non-commutative
// ops like SUB and DIV.
func projectWithScalar(vec parser.Expr, s schema.Metrics, op chplan.BinaryOp, scalar float64, scalarOnLeft bool) (chplan.Node, error) {
	inner, err := lower(vec, s)
	if err != nil {
		return nil, err
	}
	valueRef := &chplan.ColumnRef{Name: s.ValueColumn}
	scalarLit := &chplan.LitFloat{V: scalar}
	var newValue chplan.Expr
	if scalarOnLeft {
		newValue = &chplan.Binary{Op: op, Left: scalarLit, Right: valueRef}
	} else {
		newValue = &chplan.Binary{Op: op, Left: valueRef, Right: scalarLit}
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
