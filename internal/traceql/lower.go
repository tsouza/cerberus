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
		plan, err = lowerMetricsSecondStage(plan, expr.MetricsSecondStage)
		if err != nil {
			return nil, err
		}
	}
	return plan, nil
}

// lowerMetricsSecondStage wraps the metrics-aggregate subtree with a
// chplan.MetricsSecondStage carrying the `| topk(N)` / `| bottomk(N)`
// / `| > N` / `| < N` / `| >= N` / `| <= N` / `| == N` / `| != N`
// transform. Chained second-stage (`| topk(5) | > 10`) is supported
// via traceql.ChainedSecondStage: each element wraps the previous
// result in document order, so the rightmost element ends up as the
// outermost chplan node (which matches the chsql emitter's
// inside-out subquery wrap).
//
// The IR + chsql emit foundation landed in PR #437. This lowering
// became wireable once tsouza/tempo v0.0.3-cerberus-accessors
// exposed Op() / Limit() / Value() / Elements() / Separators()
// accessors on the upstream-unexported SecondStageElement variants
// (mirrors the MetricsAggregate accessor pattern from #143).
func lowerMetricsSecondStage(inner chplan.Node, ss traceql.SecondStageElement) (chplan.Node, error) {
	switch v := ss.(type) {
	case *traceql.TopKBottomK:
		return lowerTopKBottomK(inner, v)
	case *traceql.MetricsFilter:
		return lowerMetricsFilter(inner, v)
	case traceql.ChainedSecondStage:
		return lowerChainedSecondStage(inner, v)
	case *traceql.ChainedSecondStage:
		return lowerChainedSecondStage(inner, *v)
	}
	return nil, fmt.Errorf("traceql: metrics second-stage element %T is unsupported", ss)
}

// lowerTopKBottomK turns `| topk(N)` / `| bottomk(N)` into a
// chplan.MetricsSecondStage wrap with discriminator SecondStageTopK
// or SecondStageBottomK. K is the upstream limit; the emitter
// renders `ORDER BY Value <DESC|ASC> LIMIT K` and treats PartitionBy
// (empty here — TraceQL instant-metrics path; matrix path supplied
// by the /api/metrics/query_range handler via a wrapping
// RangeWindow) as the per-anchor key.
func lowerTopKBottomK(inner chplan.Node, t *traceql.TopKBottomK) (chplan.Node, error) {
	op, err := mapSecondStageOp(t.Op())
	if err != nil {
		return nil, err
	}
	limit := t.Limit()
	if limit <= 0 {
		return nil, fmt.Errorf("traceql: %s(%d): limit must be > 0", t.Op(), limit)
	}
	return &chplan.MetricsSecondStage{
		Input:      inner,
		Op:         op,
		K:          int64(limit),
		ValueAlias: metricsValueAlias,
	}, nil
}

// lowerMetricsFilter turns `| > N` / `| < N` / `| >= N` / `| <= N`
// / `| == N` / `| != N` into a chplan.MetricsSecondStage with
// discriminator SecondStageThreshold. The chsql emitter renders the
// wrap as `WHERE Value <Op> <Value>` on the inner aggregate's row
// shape.
func lowerMetricsFilter(inner chplan.Node, f *traceql.MetricsFilter) (chplan.Node, error) {
	op, err := mapBinaryOp(f.Op())
	if err != nil {
		return nil, fmt.Errorf("traceql: metrics filter operator %s: %w", f.Op(), err)
	}
	if !isThresholdBinaryOp(op) {
		return nil, fmt.Errorf("traceql: metrics filter operator %s is not a supported threshold comparison", f.Op())
	}
	return &chplan.MetricsSecondStage{
		Input:          inner,
		Op:             chplan.SecondStageThreshold,
		ThresholdOp:    op,
		ThresholdValue: f.Value(),
		ValueAlias:     metricsValueAlias,
	}, nil
}

// lowerChainedSecondStage walks ChainedSecondStage.Elements() in
// source order, wrapping the previous result in each successive
// second-stage node. The first element wraps the upstream metrics
// aggregate (`inner`); each subsequent element wraps the previous
// chplan.MetricsSecondStage. The rightmost element in the TraceQL
// source ends up as the outermost chplan node, matching the
// inside-out subquery wrap the chsql emitter renders (see
// test/spec/chsql/metrics_second_stage_chained_topk_threshold.txtar).
//
// Separators() carries the pipeline punctuation upstream uses for
// String() round-trip fidelity. The lowering does not care about
// the punctuation per se (it's a chained pipe stream — the wrapping
// order is what matters), but the accessor existence keeps the
// upstream contract explicit for future regression cases.
func lowerChainedSecondStage(inner chplan.Node, c traceql.ChainedSecondStage) (chplan.Node, error) {
	elements := c.Elements()
	if len(elements) == 0 {
		return nil, fmt.Errorf("traceql: chained second-stage has no elements")
	}
	// Validate Separators() length matches Elements() so a future
	// upstream change (e.g. dropping a separator slot) trips this
	// check rather than silently dropping a wrap.
	if seps := c.Separators(); len(seps) != len(elements) {
		return nil, fmt.Errorf("traceql: chained second-stage element/separator length mismatch (%d vs %d)", len(elements), len(seps))
	}
	current := inner
	for _, el := range elements {
		next, err := lowerMetricsSecondStage(current, el)
		if err != nil {
			return nil, err
		}
		current = next
	}
	return current, nil
}

// mapSecondStageOp translates Tempo's SecondStageOp (OpTopK /
// OpBottomK) to the chplan discriminator. Reserved-for-future
// SecondStageOp values surface as a clean unsupported error rather
// than collapse to SecondStageInvalid (which the emitter would
// reject anyway).
func mapSecondStageOp(op traceql.SecondStageOp) (chplan.SecondStageOp, error) {
	switch op {
	case traceql.OpTopK:
		return chplan.SecondStageTopK, nil
	case traceql.OpBottomK:
		return chplan.SecondStageBottomK, nil
	}
	return chplan.SecondStageInvalid, fmt.Errorf("traceql: second-stage op %s is not supported", op)
}

// isThresholdBinaryOp reports whether op is one of the six
// comparison operators Tempo's `MetricsFilter.validate()` accepts
// (>, >=, <, <=, =, !=). Mirrors chsql.isThresholdOp; duplicated
// here because the chsql helper is unexported and importing
// chsql from a lowering package would create the wrong dep
// direction (chsql consumes chplan; chsql consuming lowering would
// invert the layering).
func isThresholdBinaryOp(op chplan.BinaryOp) bool {
	switch op {
	case chplan.OpGt, chplan.OpGe, chplan.OpLt, chplan.OpLe, chplan.OpEq, chplan.OpNe:
		return true
	}
	return false
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
	return nil, fmt.Errorf("traceql: pipeline tail element %T is unsupported", elem)
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
	// `count() > 0` shape, wrap aggNode with a Filter. The Tempo parser
	// happily accepts pathological forms like `{} | 0 > 0` (no aggregate
	// on either side) — type-assert before dereferencing so we return a
	// structured error instead of panicking.
	aggPlan, ok := aggNode.(chplan.Node)
	if !ok {
		return nil, fmt.Errorf("traceql: scalar-filter LHS must aggregate to a series (count() / sum(...) / avg(...) / min(...) / max(...)), got %T", aggNode)
	}
	rhsExpr, ok := rhs.(chplan.Expr)
	if !ok {
		return nil, fmt.Errorf("traceql: scalar-filter RHS must be a literal, got %T", rhs)
	}

	return &chplan.Filter{
		Input:     aggPlan,
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
	return nil, fmt.Errorf("traceql: scalar expression %T is unsupported", e)
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
	return nil, fmt.Errorf("traceql: pipeline element %T is unsupported", elem)
}

// lowerSpansetOperation handles structural relations (`A > B`, `A < B`,
// `A ~ B`, the recursive forms `A >> B` / `A << B`, plus their negated
// (`A !> B`, `A !< B`, `A !~ B`, `A !>> B`, `A !<< B`) and union-prefixed
// (`A &> B`, `A &< B`, `A &~ B`, `A &>> B`, `A &<< B`) variants) and set
// operations (`A && B`, `A || B`). Multi-hop chains (`A > B > C`) fall
// out of the binary StructuralJoin shape — the Tempo grammar binds `>`
// left-associatively, so chained operators parse as nested
// SpansetOperation nodes that this function recurses into via
// lowerSpansetExpr.
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
		Left:                   left,
		Right:                  right,
		Op:                     relation,
		TraceIDColumn:          s.TraceIDColumn,
		SpanIDColumn:           s.SpanIDColumn,
		ParentSpanIDColumn:     s.ParentSpanIDColumn,
		ExtraProjectionColumns: structuralExtraProjectionColumns(s),
	}, nil
}

// structuralExtraProjectionColumns returns the non-key column list the
// structural-join wrap subquery must expose as bare-name aliases so the
// Tempo API-layer wrap projection (rQualifiedSampleProjections in
// internal/api/tempo/handler.go) can reference them without the
// `Unknown identifier 'SpanName' in scope` CH 25.8 analyzer rejection
// exposed by tempo compat run 26098988786.
//
// The list mirrors the schema columns the canonical/sample wrap
// projections read: SpanName, Duration, Timestamp, ResourceAttributes.
// Adding a column the wrap projection reads goes through this helper
// so the Tempo handler stays the source of truth for "what the search
// envelope needs".
func structuralExtraProjectionColumns(s schema.Traces) []string {
	cols := make([]string, 0, 4)
	for _, col := range []string{
		s.SpanNameColumn,
		s.DurationColumn,
		s.TimestampColumn,
		s.ResourceAttributesColumn,
	} {
		if col != "" {
			cols = append(cols, col)
		}
	}
	return cols
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
	return nil, fmt.Errorf("traceql: spanset expression %T is unsupported", e)
}

// mapStructuralOp translates Tempo's structural Operator enum to the
// chplan.StructuralOp this emitter understands. Covers the positive
// relations (`>` / `<` / `>>` / `<<` / `~`), their negated variants
// (`!>` / `!<` / `!>>` / `!<<` / `!~`), and the union-prefixed
// variants (`&>` / `&<` / `&>>` / `&<<` / `&~`). The negated forms
// lower to the same StructuralJoin shape with a `Not…` Op constant;
// the emitter swaps the outer INNER JOIN for a LEFT ANTI JOIN. The
// union forms lower to a `Union…` Op constant; the emitter emits the
// positive relation twice (once projecting each side) glued with
// UNION DISTINCT.
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
	case traceql.OpSpansetNotChild:
		return chplan.StructuralNotChild, nil
	case traceql.OpSpansetNotParent:
		return chplan.StructuralNotParent, nil
	case traceql.OpSpansetNotDescendant:
		return chplan.StructuralNotDescendant, nil
	case traceql.OpSpansetNotAncestor:
		return chplan.StructuralNotAncestor, nil
	case traceql.OpSpansetNotSibling:
		return chplan.StructuralNotSibling, nil
	case traceql.OpSpansetUnionChild:
		return chplan.StructuralUnionChild, nil
	case traceql.OpSpansetUnionParent:
		return chplan.StructuralUnionParent, nil
	case traceql.OpSpansetUnionDescendant:
		return chplan.StructuralUnionDescendant, nil
	case traceql.OpSpansetUnionAncestor:
		return chplan.StructuralUnionAncestor, nil
	case traceql.OpSpansetUnionSibling:
		return chplan.StructuralUnionSibling, nil
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
	return nil, fmt.Errorf("traceql: field expression %T is unsupported", e)
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
		return nil, fmt.Errorf("traceql: %s.%s used outside a comparison; only equality / inequality / regex filters on link.* and event.* are supported", a.Scope, a.Name)
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
	// Map(String, String) coercion: SpanAttributes / ResourceAttributes
	// are typed Map(String, String) in OTel-CH, so a bare
	// `SpanAttributes['http.status_code'] >= 500` comparison fails in
	// ClickHouse with NO_COMMON_TYPE ("there is no supertype for types
	// String, UInt8"). When the lowered Binary has numeric semantics
	// (arithmetic op, or comparison whose peer is a numeric expression)
	// we wrap any FieldAccess child in `toFloat64(...)` so the cast
	// happens server-side. Float64 widens both int and float literals
	// without precision loss for the magnitudes typical of attribute
	// values (HTTP status codes, percentages, sizes).
	lhs, rhs = coerceNumericFieldAccess(op, lhs, rhs)
	return &chplan.Binary{Op: op, Left: lhs, Right: rhs}, nil
}

// coerceNumericFieldAccess wraps FieldAccess children in toFloat64(...)
// when the parent Binary needs numeric semantics:
//
//   - Arithmetic ops (+ / - / * / / / % / ^) always coerce both sides,
//     recursing into nested arithmetic so a chain like `.a + .b + .c`
//     yields `toFloat64(.a) + toFloat64(.b) + toFloat64(.c)`.
//
//   - Comparison ops (= / != / < / <= / > / >=) coerce both sides only
//     when at least one side is a numeric expression (literal int /
//     float, an arithmetic Binary, or an already-coerced FuncCall).
//     The "both sides" rule covers commutative comparisons where the
//     literal appears on the left (`500 <= span.http.status_code`).
//
//   - Regex / logical ops (=~ / !~ / AND / OR) leave both sides alone
//     because their operands are strings or booleans.
//
// FieldAccess that resolves to an intrinsic column (e.g. Duration,
// already Int64) doesn't reach this path — intrinsics lower to a
// ColumnRef, not a FieldAccess. So the wrap is restricted to the
// Map(String, String) carriers by construction.
func coerceNumericFieldAccess(op chplan.BinaryOp, lhs, rhs chplan.Expr) (chplan.Expr, chplan.Expr) {
	if isArithmeticOp(op) {
		return coerceFieldAccess(lhs), coerceFieldAccess(rhs)
	}
	if isComparisonOp(op) && (isNumericExpr(lhs) || isNumericExpr(rhs)) {
		return coerceFieldAccess(lhs), coerceFieldAccess(rhs)
	}
	return lhs, rhs
}

// coerceFieldAccess wraps every FieldAccess inside expr in
// toFloat64(...), recursing into arithmetic Binary nodes so a nested
// `.a + .b` becomes `toFloat64(.a) + toFloat64(.b)`. Non-arithmetic
// sub-expressions (literals, ColumnRefs, FuncCalls already produced by
// a deeper coercion) pass through unchanged.
func coerceFieldAccess(expr chplan.Expr) chplan.Expr {
	switch v := expr.(type) {
	case *chplan.FieldAccess:
		return &chplan.FuncCall{Name: "toFloat64", Args: []chplan.Expr{v}}
	case *chplan.Binary:
		if isArithmeticOp(v.Op) {
			return &chplan.Binary{
				Op:    v.Op,
				Left:  coerceFieldAccess(v.Left),
				Right: coerceFieldAccess(v.Right),
			}
		}
	}
	return expr
}

// isArithmeticOp reports whether op is one of the numeric arithmetic
// operators where both operands must compute as numbers.
func isArithmeticOp(op chplan.BinaryOp) bool {
	switch op {
	case chplan.OpAdd, chplan.OpSub, chplan.OpMul, chplan.OpDiv, chplan.OpMod, chplan.OpPow:
		return true
	}
	return false
}

// isComparisonOp reports whether op is one of the value-comparison
// operators eligible for numeric-attribute coercion. Excludes regex
// (=~ / !~) which operate on strings, and AND / OR which compose
// booleans.
func isComparisonOp(op chplan.BinaryOp) bool {
	switch op {
	case chplan.OpEq, chplan.OpNe, chplan.OpLt, chplan.OpLe, chplan.OpGt, chplan.OpGe:
		return true
	}
	return false
}

// isNumericExpr reports whether expr has numeric semantics on the CH
// side — a numeric literal, an arithmetic Binary, or a FuncCall
// (which in this lowering only comes from a prior toFloat64 wrap).
// Used to decide whether a comparison's "other side" needs a numeric
// peer, which is what triggers FieldAccess coercion.
//
// ColumnRef deliberately does NOT count as numeric here: the only
// intrinsic ColumnRef that's numeric in OTel-CH is Duration, and a
// `Duration > 100ms` comparison doesn't need attribute coercion (both
// sides are already typed Int64). Treating ColumnRef as non-numeric
// keeps `{ name = "checkout" }` (string intrinsic) from incorrectly
// triggering toFloat64 wraps on the literal side.
func isNumericExpr(expr chplan.Expr) bool {
	if b, ok := expr.(*chplan.Binary); ok {
		return isArithmeticOp(b.Op)
	}
	switch expr.(type) {
	case *chplan.LitInt, *chplan.LitFloat, *chplan.FuncCall:
		return true
	}
	return false
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
	return nil, fmt.Errorf("traceql: static literal type %s is unsupported", st.Type)
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
	return "", fmt.Errorf("traceql: operator %s is unsupported", op)
}
