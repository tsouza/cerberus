// Package traceql lowers Tempo TraceQL queries into the shared cerberus
// chplan IR. The seed (M4.1) covers the SpansetFilter form: attribute
// matchers like `{ .service.name = "x" }`, `{ duration > 100ms }`,
// `{ span.http.status_code >= 500 }`. Structural operators (`>>`/`>`),
// aggregators, time filters, and `| select(...)` land in M4.2-M4.4.
package traceql

import (
	"context"
	"fmt"

	"github.com/grafana/tempo/pkg/traceql"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/tsouza/cerberus/internal/cerbtrace"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// tracer emits the `lower` pipeline-stage span for TraceQL lowering.
var tracer = otel.Tracer("github.com/tsouza/cerberus/internal/traceql")

// Lower turns a parsed TraceQL expression into a chplan tree.
//
// When `expr.MetricsPipeline` is non-nil the query is a metrics
// aggregation (`{ ... } | rate()`, `{ ... } | sum_over_time(attr)`,
// etc.). The spanset prefix in `expr.Pipeline.Elements` (typically a
// single `{ ... }` selector) lowers to a Scan/Filter tree, then
// lowerMetricsPipeline wraps it with a chplan.Aggregate carrying the
// CH aggregate function + group-by labels. The query time range itself
// is supplied by the HTTP /api/metrics/query_range handler (which
// wraps the returned tree with a chplan.RangeWindow) — TraceQL's
// grammar doesn't carry the range argument in the AST. See
// docs/upstream-forks.md.
func Lower(ctx context.Context, expr *traceql.RootExpr, s schema.Traces) (chplan.Node, error) {
	_, span := tracer.Start(ctx, cerbtrace.SpanLower, trace.WithAttributes(cerbtrace.AttrQL.String("traceql")))
	defer span.End()
	plan, err := lowerRoot(expr, s)
	if err != nil {
		span.RecordError(err)
		return nil, err
	}
	span.SetAttributes(cerbtrace.AttrPlanNodeCount.Int(cerbtrace.CountNodes(plan)))
	return plan, nil
}

// lowerRoot is the body of Lower minus the span bookkeeping; split so
// the public entry point keeps tracing concerns separate.
func lowerRoot(expr *traceql.RootExpr, s schema.Traces) (chplan.Node, error) {
	if expr == nil {
		return nil, fmt.Errorf("traceql: nil RootExpr")
	}
	if len(expr.Pipeline.Elements) == 0 {
		return nil, fmt.Errorf("traceql: empty pipeline")
	}

	first := expr.Pipeline.Elements[0]
	plan, err := lowerPipelineElement(first, s)
	if err != nil {
		return nil, err
	}

	// Subsequent pipeline elements layer onto the previous result. M4.3
	// supports `| count()` / `| sum(...)` / `| avg(...)` / `| max(...)`
	// / `| min(...)`. Other follow-on stages (scalar filter, group /
	// coalesce / select) defer to M4.4.
	for _, el := range expr.Pipeline.Elements[1:] {
		next, err := lowerFollowingElement(plan, el, s)
		if err != nil {
			return nil, err
		}
		plan = next
	}

	if expr.MetricsPipeline != nil {
		plan, err = lowerMetricsPipeline(plan, expr.MetricsPipeline, s)
		if err != nil {
			return nil, err
		}
	}
	if expr.MetricsSecondStage != nil {
		return nil, fmt.Errorf("traceql: metrics second-stage operators (`| topk`, `| bottomk`, `| > N`) are not yet supported")
	}
	return plan, nil
}

// lowerFollowingElement layers a pipeline element onto the previous
// stage's plan. Aggregate / ScalarFilter / SelectOperation /
// GroupOperation / CoalesceOperation are supported.
func lowerFollowingElement(prev chplan.Node, elem traceql.PipelineElement, s schema.Traces) (chplan.Node, error) {
	switch v := elem.(type) {
	case traceql.Aggregate:
		return lowerAggregate(prev, v, s)
	case *traceql.Aggregate:
		return lowerAggregate(prev, *v, s)
	case traceql.ScalarFilter:
		return lowerScalarFilter(prev, v, s)
	case *traceql.ScalarFilter:
		return lowerScalarFilter(prev, *v, s)
	case traceql.SelectOperation:
		return lowerSelect(prev, v, s)
	case *traceql.SelectOperation:
		return lowerSelect(prev, *v, s)
	case traceql.GroupOperation:
		return lowerGroup(prev, v, s)
	case *traceql.GroupOperation:
		return lowerGroup(prev, *v, s)
	case traceql.CoalesceOperation:
		return lowerCoalesce(prev, s)
	case *traceql.CoalesceOperation:
		return lowerCoalesce(prev, s)
	}
	return nil, fmt.Errorf("traceql: pipeline tail element %T is not yet supported", elem)
}

// lowerScalarFilter handles `| count() > 0`, `| sum(.duration) >= 1s`,
// etc. Lowers as Aggregate (LHS) wrapped in a Filter on the output
// Value column.
func lowerScalarFilter(prev chplan.Node, sf traceql.ScalarFilter, s schema.Traces) (chplan.Node, error) {
	aggNode, err := lowerScalarExpr(prev, sf.LHS, s)
	if err != nil {
		return nil, err
	}
	rhs, err := lowerScalarExpr(prev, sf.RHS, s)
	if err != nil {
		return nil, err
	}

	op, err := mapBinaryOp(sf.Op)
	if err != nil {
		return nil, err
	}

	// rhs is expected to be a chplan.Expr from a Static literal; the
	// LHS recursed back as a chplan.Node (Aggregate). For the typical
	// `count() > 0` shape, wrap aggNode with a Filter.
	rhsExpr, ok := rhs.(chplan.Expr)
	if !ok {
		return nil, fmt.Errorf("traceql: scalar-filter RHS must be a literal, got %T", rhs)
	}

	return &chplan.Filter{
		Input:     aggNode.(chplan.Node),
		Predicate: &chplan.Binary{Op: op, Left: &chplan.ColumnRef{Name: "Value"}, Right: rhsExpr},
	}, nil
}

// lowerScalarExpr converts a TraceQL ScalarExpression into either a
// chplan.Node (when the expression aggregates / produces a series) or
// a chplan.Expr (when it's a literal). Returns `any`; callers
// type-assert based on context.
func lowerScalarExpr(prev chplan.Node, e traceql.ScalarExpression, s schema.Traces) (any, error) {
	switch v := e.(type) {
	case traceql.Aggregate:
		return lowerAggregate(prev, v, s)
	case *traceql.Aggregate:
		return lowerAggregate(prev, *v, s)
	case traceql.Static:
		return lowerStatic(v)
	case *traceql.Static:
		return lowerStatic(*v)
	}
	return nil, fmt.Errorf("traceql: scalar expression %T is not yet supported", e)
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

// lowerSpansetOperation handles structural relations (`A > B`, `A < B`,
// `A ~ B`, plus the recursive forms `A >> B` / `A << B`) and set
// operations (`A && B`, `A || B`). Multi-hop chains (`A > B > C`) fall
// out of the binary StructuralJoin shape — the Tempo grammar binds `>`
// left-associatively, so chained operators parse as nested
// SpansetOperation nodes that this function recurses into via
// lowerSpansetExpr. Negated / union-prefixed variants still defer.
func lowerSpansetOperation(op *traceql.SpansetOperation, s schema.Traces) (chplan.Node, error) {
	left, err := lowerSpansetExpr(op.LHS, s)
	if err != nil {
		return nil, err
	}
	right, err := lowerSpansetExpr(op.RHS, s)
	if err != nil {
		return nil, err
	}

	// Set operations (`&&` / `||`) lower to a chplan.SetOperation; the
	// emitter renders an INNER JOIN (intersect) or UNION DISTINCT (union)
	// keyed on (TraceID, SpanID).
	if setOp, ok := mapSetOp(op.Op); ok {
		return &chplan.SetOperation{
			Left:          left,
			Right:         right,
			Op:            setOp,
			TraceIDColumn: s.TraceIDColumn,
			SpanIDColumn:  s.SpanIDColumn,
		}, nil
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

// mapSetOp identifies the TraceQL operators that lower to a
// chplan.SetOperation. Returns ok=false for non-set operators so the
// caller falls back to structural-relation handling.
func mapSetOp(op traceql.Operator) (chplan.SetOp, bool) {
	switch op {
	case traceql.OpSpansetAnd:
		return chplan.SetIntersect, true
	case traceql.OpSpansetUnion:
		return chplan.SetUnion, true
	}
	return "", false
}

// lowerSpansetExpr lowers a TraceQL SpansetExpression (the LHS/RHS of
// a SpansetOperation). Handles SpansetFilter and nested SpansetOperation
// — the nested case is what makes multi-hop chains (`A > B > C`) and
// mixed direct/recursive chains (`A > B >> C`) work without any
// dedicated lowering pass.
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
	case traceql.OpSpansetSibling:
		return chplan.StructuralSibling, nil
	case traceql.OpSpansetNotChild, traceql.OpSpansetNotParent, traceql.OpSpansetNotSibling,
		traceql.OpSpansetNotAncestor, traceql.OpSpansetNotDescendant,
		traceql.OpSpansetUnionChild, traceql.OpSpansetUnionParent,
		traceql.OpSpansetUnionSibling, traceql.OpSpansetUnionAncestor,
		traceql.OpSpansetUnionDescendant:
		return "", fmt.Errorf("traceql: spanset op %s is not yet supported (negated / union-prefixed structural variants defer to RC3)", op)
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
		return lowerAttributeExpr(*v, s)
	case traceql.Attribute:
		return lowerAttributeExpr(v, s)
	case *traceql.Static:
		return lowerStatic(*v)
	case traceql.Static:
		return lowerStatic(v)
	}
	return nil, fmt.Errorf("traceql: field expression %T is not yet supported", e)
}

// lowerAttributeExpr wraps lowerAttribute with a guard: link- /
// event-scoped attributes can only appear inside a comparison
// (lowerBinaryOperation intercepts them and returns a
// NestedArrayExists). Reaching this path means the attribute is being
// used as a scalar value (e.g. `| select(link.span_id)`) which would
// silently dereference the wrong CH column — error out so the operator
// can surface the gap rather than ship wrong SQL.
func lowerAttributeExpr(a traceql.Attribute, s schema.Traces) (chplan.Expr, error) {
	if a.Scope == traceql.AttributeScopeLink || a.Scope == traceql.AttributeScopeEvent {
		return nil, fmt.Errorf("traceql: %s.%s used outside a comparison; only equality / inequality / regex filters on link.* and event.* are supported (see RC2 link traversal + span-event queries)", a.Scope, a.Name)
	}
	return lowerAttribute(a, s), nil
}

func lowerBinaryOperation(b *traceql.BinaryOperation, s schema.Traces) (chplan.Expr, error) {
	op, err := mapBinaryOp(b.Op)
	if err != nil {
		return nil, err
	}
	// TraceQL link / event spanset filters live on the OTel-CH `Links` /
	// `Events` Nested columns. Their predicate shape is
	//   arrayExists(x -> x[<key>] <op> <value>, <Col>.Attributes)
	// rather than a flat column comparison; capture that as a
	// NestedArrayExists before generic Binary lowering kicks in.
	if nested, ok := lowerNestedAttrBinary(b, op, s); ok {
		return nested, nil
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

// lowerNestedAttrBinary recognises the
//
//	<link|event>.<name> <op> <literal>
//
// shape and returns a chplan.NestedArrayExists. The LHS/RHS may be in
// either order in upstream TraceQL ASTs; we normalise so the attribute
// reference is always the implicit `x[?]` and the literal is the RHS
// of the comparison. Returns ok=false when neither side is a link- or
// event-scoped attribute (the caller falls back to flat Binary lowering).
func lowerNestedAttrBinary(b *traceql.BinaryOperation, op chplan.BinaryOp, s schema.Traces) (chplan.Expr, bool) {
	if lhsAttr, ok := nestedScopedAttr(b.LHS); ok {
		col, key, ok := nestedAttrTarget(lhsAttr, s)
		if !ok {
			return nil, false
		}
		val, err := lowerFieldExpr(b.RHS, s)
		if err != nil {
			return nil, false
		}
		return &chplan.NestedArrayExists{
			Column:   col,
			SubField: "Attributes",
			Key:      key,
			Op:       op,
			Value:    val,
		}, true
	}
	if rhsAttr, ok := nestedScopedAttr(b.RHS); ok {
		col, key, ok := nestedAttrTarget(rhsAttr, s)
		if !ok {
			return nil, false
		}
		val, err := lowerFieldExpr(b.LHS, s)
		if err != nil {
			return nil, false
		}
		return &chplan.NestedArrayExists{
			Column:   col,
			SubField: "Attributes",
			Key:      key,
			Op:       flipComparisonOp(op),
			Value:    val,
		}, true
	}
	return nil, false
}

// nestedScopedAttr returns the attribute if e is a link- or event-scoped
// attribute reference (pointer or value form), so callers can branch
// without re-running the same type-switch twice.
func nestedScopedAttr(e traceql.FieldExpression) (traceql.Attribute, bool) {
	switch v := e.(type) {
	case *traceql.Attribute:
		if v == nil {
			return traceql.Attribute{}, false
		}
		if v.Scope == traceql.AttributeScopeLink || v.Scope == traceql.AttributeScopeEvent {
			return *v, true
		}
	case traceql.Attribute:
		if v.Scope == traceql.AttributeScopeLink || v.Scope == traceql.AttributeScopeEvent {
			return v, true
		}
	}
	return traceql.Attribute{}, false
}

// nestedAttrTarget maps a link- / event-scoped attribute to the Nested
// parent column it lives under (LinksColumn or EventsColumn) plus the
// attribute key to look up inside each row's Attributes map. Returns
// ok=false when the configured schema has no column for that scope —
// the caller falls back to the generic lowering and the resulting SQL
// will error at emit time (better than silently writing the wrong
// column name).
func nestedAttrTarget(a traceql.Attribute, s schema.Traces) (col, key string, ok bool) {
	switch a.Scope {
	case traceql.AttributeScopeLink:
		if s.LinksColumn == "" {
			return "", "", false
		}
		return s.LinksColumn, a.Name, true
	case traceql.AttributeScopeEvent:
		if s.EventsColumn == "" {
			return "", "", false
		}
		return s.EventsColumn, a.Name, true
	}
	return "", "", false
}

// flipComparisonOp swaps the direction of an asymmetric comparison so
// `<literal> <op> <attr>` rewrites cleanly to `<attr> <flipped> <literal>`.
// = / != / AND / OR are symmetric and pass through unchanged.
func flipComparisonOp(op chplan.BinaryOp) chplan.BinaryOp {
	switch op {
	case chplan.OpLt:
		return chplan.OpGt
	case chplan.OpLe:
		return chplan.OpGe
	case chplan.OpGt:
		return chplan.OpLt
	case chplan.OpGe:
		return chplan.OpLe
	}
	return op
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
//
// TypeStatus and TypeKind map to the TitleCase string the OTel-CH
// exporter writes into StatusCode / SpanKind. Tempo's Status.String() /
// Kind.String() emits lowercase ("error", "client", …); we re-case
// here so `{ status = error }` matches CH's `'Error'` row.
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
	case traceql.TypeStatus:
		s, ok := st.Status()
		if !ok {
			return nil, fmt.Errorf("traceql: static status literal has no Status() value")
		}
		return &chplan.LitString{V: statusString(s)}, nil
	case traceql.TypeKind:
		k, ok := st.Kind()
		if !ok {
			return nil, fmt.Errorf("traceql: static kind literal has no Kind() value")
		}
		return &chplan.LitString{V: kindString(k)}, nil
	}
	return nil, fmt.Errorf("traceql: static literal type %s is not yet supported", st.Type)
}

// statusString maps Tempo's Status enum to the StatusCode string the
// OTel-CH exporter writes. Tempo's Status.String() is lowercase; CH
// rows carry TitleCase ("Unset" / "Ok" / "Error").
func statusString(s traceql.Status) string {
	switch s {
	case traceql.StatusError:
		return "Error"
	case traceql.StatusOk:
		return "Ok"
	case traceql.StatusUnset:
		return "Unset"
	}
	// Defensive: future enum values surface as-is rather than silently
	// producing an empty filter.
	return s.String()
}

// kindString maps Tempo's Kind enum to the SpanKind string the OTel-CH
// exporter writes. Tempo's Kind.String() is lowercase; CH rows carry
// TitleCase ("Internal" / "Client" / "Server" / "Producer" / "Consumer";
// "Unspecified" is the conventional unset value).
func kindString(k traceql.Kind) string {
	switch k {
	case traceql.KindUnspecified:
		return "Unspecified"
	case traceql.KindInternal:
		return "Internal"
	case traceql.KindClient:
		return "Client"
	case traceql.KindServer:
		return "Server"
	case traceql.KindProducer:
		return "Producer"
	case traceql.KindConsumer:
		return "Consumer"
	}
	return k.String()
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
