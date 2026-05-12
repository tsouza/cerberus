// Package traceql lowers Tempo TraceQL queries into the shared cerberus
// chplan IR. The seed (M4.1) covers the SpansetFilter form: attribute
// matchers like `{ .service.name = "x" }`, `{ duration > 100ms }`,
// `{ span.http.status_code >= 500 }`. Structural operators (`>>`/`>`),
// aggregators, time filters, and `| select(...)` land in M4.2-M4.4.
package traceql

import (
	"fmt"

	"github.com/grafana/tempo/pkg/traceql"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// Lower turns a parsed TraceQL expression into a chplan tree.
func Lower(expr *traceql.RootExpr, s schema.Traces) (chplan.Node, error) {
	if expr == nil {
		return nil, fmt.Errorf("traceql: nil RootExpr")
	}
	if expr.MetricsPipeline != nil {
		return nil, fmt.Errorf("traceql: metrics pipelines (`| count()`, `| rate()`) are not yet supported (lands with M4.3)")
	}
	if len(expr.Pipeline.Elements) == 0 {
		return nil, fmt.Errorf("traceql: empty pipeline")
	}

	// First element must be a SpansetFilter for the M4.1 slice. Subsequent
	// elements (structural ops, scalar filters, group / coalesce / select)
	// defer to M4.2-M4.4.
	if len(expr.Pipeline.Elements) > 1 {
		return nil, fmt.Errorf("traceql: multi-stage pipelines are not yet supported (structural ops + filters land in M4.2)")
	}

	first := expr.Pipeline.Elements[0]
	return lowerPipelineElement(first, s)
}

// lowerPipelineElement dispatches a single TraceQL pipeline element to
// its corresponding lowering routine. Currently SpansetFilter and
// SpansetOperation; aggregates / select / scalar filters defer.
func lowerPipelineElement(elem traceql.PipelineElement, s schema.Traces) (chplan.Node, error) {
	switch v := elem.(type) {
	case *traceql.SpansetFilter:
		return lowerSpansetFilter(v, s)
	case traceql.SpansetOperation:
		return lowerSpansetOperation(&v, s)
	case *traceql.SpansetOperation:
		return lowerSpansetOperation(v, s)
	}
	return nil, fmt.Errorf("traceql: pipeline element %T is not yet supported", elem)
}

// lowerSpansetOperation handles structural relations (`A > B`, `A < B`)
// and set operations (`A && B`, `A || B`). The seed (M4.2) covers
// direct-parent / direct-child structural ops; recursive forms (`>>`,
// `<<`) and set ops defer.
func lowerSpansetOperation(op *traceql.SpansetOperation, s schema.Traces) (chplan.Node, error) {
	left, err := lowerSpansetExpr(op.LHS, s)
	if err != nil {
		return nil, err
	}
	right, err := lowerSpansetExpr(op.RHS, s)
	if err != nil {
		return nil, err
	}

	relation, err := mapStructuralOp(op.Op)
	if err != nil {
		return nil, err
	}
	return &chplan.StructuralJoin{
		Left:               left,
		Right:              right,
		Op:                 relation,
		TraceIDColumn:      s.TraceIDColumn,
		SpanIDColumn:       s.SpanIDColumn,
		ParentSpanIDColumn: s.ParentSpanIDColumn,
	}, nil
}

// lowerSpansetExpr lowers a TraceQL SpansetExpression (the LHS/RHS of
// a SpansetOperation). Currently SpansetFilter; nested operations land
// once `>>` / `<<` recursive support arrives.
func lowerSpansetExpr(e traceql.SpansetExpression, s schema.Traces) (chplan.Node, error) {
	switch v := e.(type) {
	case *traceql.SpansetFilter:
		return lowerSpansetFilter(v, s)
	case *traceql.SpansetOperation:
		return lowerSpansetOperation(v, s)
	case traceql.SpansetOperation:
		return lowerSpansetOperation(&v, s)
	}
	return nil, fmt.Errorf("traceql: spanset expression %T is not yet supported", e)
}

// mapStructuralOp translates Tempo's structural Operator enum to the
// chplan.StructuralOp this emitter understands.
func mapStructuralOp(op traceql.Operator) (chplan.StructuralOp, error) {
	switch op {
	case traceql.OpSpansetChild:
		return chplan.StructuralChild, nil
	case traceql.OpSpansetParent:
		return chplan.StructuralParent, nil
	case traceql.OpSpansetDescendant:
		return chplan.StructuralDescendant, nil
	case traceql.OpSpansetAncestor:
		return chplan.StructuralAncestor, nil
	case traceql.OpSpansetAnd, traceql.OpSpansetUnion, traceql.OpSpansetSibling,
		traceql.OpSpansetNotChild, traceql.OpSpansetNotParent, traceql.OpSpansetNotSibling,
		traceql.OpSpansetNotAncestor, traceql.OpSpansetNotDescendant:
		return "", fmt.Errorf("traceql: spanset op %s is not yet supported (set / sibling ops land in M4.2 follow-ups)", op)
	}
	return "", fmt.Errorf("traceql: spanset op %s is not a structural relation", op)
}

// lowerSpansetFilter turns `{ <field-expr> }` into Scan + Filter on
// otel_traces. The field expression is recursively lowered into a
// chplan.Expr predicate.
func lowerSpansetFilter(f *traceql.SpansetFilter, s schema.Traces) (chplan.Node, error) {
	pred, err := lowerFieldExpr(f.Expression, s)
	if err != nil {
		return nil, err
	}
	scan := &chplan.Scan{Table: s.SpansTable}
	if pred == nil {
		return scan, nil
	}
	return &chplan.Filter{Input: scan, Predicate: pred}, nil
}

// lowerFieldExpr recursively translates a TraceQL FieldExpression into
// a chplan.Expr. Handles BinaryOperation (= / != / </ <= / > / >= /
// =~ / !~ / + / - / etc.), Attribute (dotted paths), Static (typed
// literal).
func lowerFieldExpr(e traceql.FieldExpression, s schema.Traces) (chplan.Expr, error) {
	switch v := e.(type) {
	case *traceql.BinaryOperation:
		return lowerBinaryOperation(v, s)
	case *traceql.Attribute:
		return lowerAttribute(*v, s), nil
	case traceql.Attribute:
		return lowerAttribute(v, s), nil
	case *traceql.Static:
		return lowerStatic(*v)
	case traceql.Static:
		return lowerStatic(v)
	}
	return nil, fmt.Errorf("traceql: field expression %T is not yet supported", e)
}

func lowerBinaryOperation(b *traceql.BinaryOperation, s schema.Traces) (chplan.Expr, error) {
	op, err := mapBinaryOp(b.Op)
	if err != nil {
		return nil, err
	}
	lhs, err := lowerFieldExpr(b.LHS, s)
	if err != nil {
		return nil, err
	}
	rhs, err := lowerFieldExpr(b.RHS, s)
	if err != nil {
		return nil, err
	}
	return &chplan.Binary{Op: op, Left: lhs, Right: rhs}, nil
}

// lowerAttribute resolves a TraceQL attribute reference to a chplan
// expression against the appropriate carrier column.
//
// Scope mapping:
//   - .name (no prefix), span.name → SpanAttributes['name']
//   - resource.name        → ResourceAttributes['name']
//   - intrinsic duration   → Duration
//   - intrinsic name       → SpanName
//   - intrinsic kind       → SpanKind
//   - intrinsic status     → StatusCode
//   - intrinsic statusMessage → StatusMessage
//   - intrinsic traceID    → TraceId
//   - intrinsic spanID     → SpanId
//   - intrinsic parent     → ParentSpanId
func lowerAttribute(a traceql.Attribute, s schema.Traces) chplan.Expr {
	if a.Intrinsic != traceql.IntrinsicNone {
		if col := intrinsicColumn(a.Intrinsic, s); col != "" {
			return &chplan.ColumnRef{Name: col}
		}
	}
	carrier := s.AttributesColumn
	switch a.Scope {
	case traceql.AttributeScopeResource:
		carrier = s.ResourceAttributesColumn
	case traceql.AttributeScopeSpan:
		carrier = s.AttributesColumn
	}
	return &chplan.FieldAccess{
		Source: &chplan.ColumnRef{Name: carrier},
		Path:   a.Name,
	}
}

func intrinsicColumn(i traceql.Intrinsic, s schema.Traces) string {
	switch i {
	case traceql.IntrinsicDuration:
		return s.DurationColumn
	case traceql.IntrinsicName:
		return s.SpanNameColumn
	case traceql.IntrinsicKind:
		return s.SpanKindColumn
	case traceql.IntrinsicStatus:
		return s.StatusCodeColumn
	case traceql.IntrinsicStatusMessage:
		return s.StatusMessageColumn
	case traceql.IntrinsicTraceID:
		return s.TraceIDColumn
	case traceql.IntrinsicSpanID:
		return s.SpanIDColumn
	case traceql.IntrinsicParent:
		return s.ParentSpanIDColumn
	}
	return ""
}

// lowerStatic turns a TraceQL Static literal into a chplan literal.
func lowerStatic(st traceql.Static) (chplan.Expr, error) {
	switch st.Type {
	case traceql.TypeString:
		return &chplan.LitString{V: st.EncodeToString(false)}, nil
	case traceql.TypeInt:
		i, _ := st.Int()
		return &chplan.LitInt{V: int64(i)}, nil
	case traceql.TypeFloat:
		return &chplan.LitFloat{V: st.Float()}, nil
	case traceql.TypeBoolean:
		b, _ := st.Bool()
		return &chplan.LitBool{V: b}, nil
	case traceql.TypeDuration:
		// Durations encode as nanoseconds; emit as int64 since the
		// Duration column in OTel-CH is Int64 ns.
		d, _ := st.Duration()
		return &chplan.LitInt{V: d.Nanoseconds()}, nil
	}
	return nil, fmt.Errorf("traceql: static literal type %s is not yet supported", st.Type)
}

func mapBinaryOp(op traceql.Operator) (chplan.BinaryOp, error) {
	switch op {
	case traceql.OpEqual:
		return chplan.OpEq, nil
	case traceql.OpNotEqual:
		return chplan.OpNe, nil
	case traceql.OpLess:
		return chplan.OpLt, nil
	case traceql.OpLessEqual:
		return chplan.OpLe, nil
	case traceql.OpGreater:
		return chplan.OpGt, nil
	case traceql.OpGreaterEqual:
		return chplan.OpGe, nil
	case traceql.OpRegex:
		return chplan.OpMatch, nil
	case traceql.OpNotRegex:
		return chplan.OpNotMatch, nil
	case traceql.OpAnd:
		return chplan.OpAnd, nil
	case traceql.OpOr:
		return chplan.OpOr, nil
	case traceql.OpAdd:
		return chplan.OpAdd, nil
	case traceql.OpSub:
		return chplan.OpSub, nil
	case traceql.OpMult:
		return chplan.OpMul, nil
	case traceql.OpDiv:
		return chplan.OpDiv, nil
	case traceql.OpMod:
		return chplan.OpMod, nil
	case traceql.OpPower:
		return chplan.OpPow, nil
	}
	return "", fmt.Errorf("traceql: operator %s is not yet supported", op)
}
