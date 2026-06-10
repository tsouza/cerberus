package inventory

import (
	"fmt"
	"sort"

	"github.com/grafana/tempo/pkg/traceql"
)

// traceQLSource documents where the TraceQL inventory comes from. The
// pinned tsouza/tempo fork exports no enumerable feature table
// (operators, intrinsics, and metrics ops are iota enums with no
// public registry slice), so the inventory is fully hand-curated with
// parser-pinned existence checks: every row's Pin must parse via
// traceql.Parse — the exact parser configuration cerberus's Tempo head
// uses (internal/api/tempo/handler.go: parseExpr) — AND be matched
// back to its own row ID by [CollectTraceQLFeatureIDs].
const traceQLSource = "github.com/grafana/tempo/pkg/traceql " +
	"(hand-curated, parser-pinned existence checks; tsouza fork pin in go.mod)"

// GenerateTraceQL builds the TraceQL feature inventory. It returns an
// error when any pin fails to parse or fails to round-trip through the
// AST matcher — both indicate the pinned parser drifted out from under
// the inventory's assumptions.
func GenerateTraceQL() (*Inventory, error) {
	rows := curatedTraceQLRows()

	for _, r := range rows {
		expr, err := traceql.Parse(r.Pin)
		if err != nil {
			return nil, fmt.Errorf("inventory row %s: pin %q does not parse: %w", r.ID, r.Pin, err)
		}
		got := CollectTraceQLFeatureIDs(expr)
		if !got[r.ID] {
			return nil, fmt.Errorf(
				"inventory row %s: pin %q is not matched back to its own ID (matcher saw %v)",
				r.ID, r.Pin, sortedKeys(got),
			)
		}
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return &Inventory{QL: "traceql", Source: traceQLSource, Rows: rows}, nil
}

// curatedTraceQLRows returns the hand-curated TraceQL rows. Each Pin is
// the parser-pinned existence check: GenerateTraceQL fails if the
// pinned parser stops accepting it.
//
// Pins are authored with two extra constraints beyond parseability:
//
//   - Field-level `||` over the SAME attribute is avoided where the
//     row isn't specifically about it: traceql.Parse runs the upstream
//     AST optimiser, which rewrites `a = "x" || a = "y"` into the
//     non-parseable internal OpIn — that rewrite IS the op:in row.
//   - Arithmetic pins put the attribute on the operator's left so the
//     constant-folding optimisation can't collapse the operator away.
func curatedTraceQLRows() []Row {
	mk := func(id, class, token, pin string) Row {
		return Row{ID: id, Class: class, Token: token, Pin: pin}
	}
	rows := []Row{
		// --- Attribute selection (scopes) ---
		mk("attr:unscoped", "attribute", ".attr", `{ .service.name = "shop" }`),
		mk("attr:span", "attribute", "span.attr", `{ span.http.method = "GET" }`),
		mk("attr:resource", "attribute", "resource.attr", `{ resource.service.name = "shop" }`),
		mk("attr:event", "attribute", "event.attr", `{ event.exception.message =~ ".*timeout.*" }`),
		mk("attr:link", "attribute", "link.attr", `{ link.opentracing.ref_type = "child_of" }`),
		mk("attr:instrumentation", "attribute", "instrumentation.attr", `{ instrumentation.deployment = "blue" }`),

		// --- Column-backed intrinsics ---
		mk("intrinsic:name", "intrinsic", "name", `{ name = "GET /home" }`),
		mk("intrinsic:duration", "intrinsic", "duration", `{ duration > 100ms }`),
		mk("intrinsic:status", "intrinsic", "status", `{ status = error }`),
		mk("intrinsic:statusMessage", "intrinsic", "statusMessage", `{ statusMessage =~ ".*deadline.*" }`),
		mk("intrinsic:kind", "intrinsic", "kind", `{ kind = server }`),
		mk("intrinsic:trace-id", "intrinsic", "trace:id", `{ trace:id = "a0000000000000000000000000000001" }`),
		mk("intrinsic:span-id", "intrinsic", "span:id", `{ span:id = "0000000000000001" }`),
		mk("intrinsic:parent", "intrinsic", "parent", `{ parent = "0000000000000001" }`),
		mk("intrinsic:event-name", "intrinsic", "event:name", `{ event:name = "exception" }`),
		mk("intrinsic:link-traceID", "intrinsic", "link:traceID", `{ link:traceID = "a0000000000000000000000000000001" }`),
		mk("intrinsic:link-spanID", "intrinsic", "link:spanID", `{ link:spanID = "0000000000000001" }`),
		mk("intrinsic:instrumentation-name", "intrinsic", "instrumentation:name", `{ instrumentation:name = "showcase-instrumentation" }`),
		mk("intrinsic:instrumentation-version", "intrinsic", "instrumentation:version", `{ instrumentation:version = "1.2.3" }`),

		// --- Unbacked intrinsics (parse, but cerberus 422s — the
		// OTel-CH span-row schema cannot answer them; the showcase pins
		// the rejection bidirectionally) ---
		mk("intrinsic:rootName", "intrinsic-unbacked", "rootName", `{ rootName = "GET /home" }`),
		mk("intrinsic:rootServiceName", "intrinsic-unbacked", "rootServiceName", `{ rootServiceName = "frontend" }`),
		mk("intrinsic:traceDuration", "intrinsic-unbacked", "traceDuration", `{ traceDuration > 100ms }`),
		mk("intrinsic:childCount", "intrinsic-unbacked", "span:childCount", `{ span:childCount > 0 }`),
		mk("intrinsic:event-timeSinceStart", "intrinsic-unbacked", "event:timeSinceStart", `{ event:timeSinceStart > 1ms }`),

		// --- Comparison operators ---
		mk("op:eq", "comparison", "=", `{ name = "GET /home" }`),
		mk("op:neq", "comparison", "!=", `{ name != "cron.refresh" }`),
		mk("op:gt", "comparison", ">", `{ duration > 100ms }`),
		mk("op:gte", "comparison", ">=", `{ duration >= 100ms }`),
		mk("op:lt", "comparison", "<", `{ duration < 10s }`),
		mk("op:lte", "comparison", "<=", `{ duration <= 10s }`),
		mk("op:regex", "comparison", "=~", `{ name =~ "GET.*" }`),
		mk("op:not-regex", "comparison", "!~", `{ name !~ "^cron\\." }`),
		mk("op:exists", "comparison", "!= nil", `{ resource.service.name != nil }`),
		mk("op:not-exists", "comparison", "= nil", `{ .never.set.attr = nil }`),

		// --- Field-expression logic ---
		mk("op:and", "field-logic", "&&", `{ name = "GET /home" && duration > 1ms }`),
		mk("op:or", "field-logic", "||", `{ kind = server || status = error }`),
		// `a = "x" || a = "y"` over the same attribute is rewritten to
		// the internal IN operator by the parser's optimiser; cerberus
		// rejects OpIn today (showcase pins the 422).
		mk("op:in", "field-logic", "in (|| same-attr rewrite)", `{ name = "GET /home" || name = "POST /checkout" }`),
		mk("op:not", "field-logic", "!", `{ !(name = "GET /home") }`),

		// --- Field-expression arithmetic ---
		mk("op:add", "arithmetic", "+", `{ .payload_bytes + 1 > 100 }`),
		mk("op:sub", "arithmetic", "-", `{ .payload_bytes - 1 > 100 }`),
		mk("op:mult", "arithmetic", "*", `{ .payload_bytes * 2 > 100 }`),
		mk("op:div", "arithmetic", "/", `{ .payload_bytes / 2 > 100 }`),
		mk("op:mod", "arithmetic", "%", `{ .payload_bytes % 10 >= 0 }`),
		mk("op:pow", "arithmetic", "^", `{ .payload_bytes ^ 2 > 100 }`),

		// --- Typed literals ---
		mk("static:string", "literal", "string", `{ name = "GET /home" }`),
		mk("static:int", "literal", "int", `{ span.http.status_code >= 500 }`),
		mk("static:float", "literal", "float", `{ .checkout.amount > 1.5 }`),
		mk("static:bool", "literal", "bool", `{ .cache.hit = true }`),
		mk("static:duration", "literal", "duration", `{ duration > 100ms }`),
		mk("static:status", "literal", "status enum", `{ status = error }`),
		mk("static:kind", "literal", "kind enum", `{ kind = client }`),

		// --- Spanset pipeline (aggregates appear only in the scalar-filter form) ---
		mk("pipe:count", "spanset-aggregate", "count()", `{ } | count() > 1`),
		mk("pipe:sum", "spanset-aggregate", "sum()", `{ } | sum(.payload_bytes) > 1`),
		mk("pipe:avg", "spanset-aggregate", "avg()", `{ } | avg(duration) > 1ms`),
		mk("pipe:min", "spanset-aggregate", "min()", `{ } | min(duration) > 1ms`),
		mk("pipe:max", "spanset-aggregate", "max()", `{ } | max(duration) > 1ms`),
		mk("pipe:select", "spanset-pipeline", "select()", `{ } | select(span.http.method, resource.service.name)`),
		mk("pipe:by", "spanset-pipeline", "by()", `{ } | by(resource.service.name)`),
		mk("pipe:coalesce", "spanset-pipeline", "coalesce()", `({ kind = server } || { kind = client }) | coalesce()`),

		// --- Metrics pipeline (first stage) ---
		mk("metric:rate", "metrics", "rate", `{ } | rate()`),
		mk("metric:count_over_time", "metrics", "count_over_time", `{ } | count_over_time()`),
		mk("metric:min_over_time", "metrics", "min_over_time", `{ } | min_over_time(duration)`),
		mk("metric:max_over_time", "metrics", "max_over_time", `{ } | max_over_time(duration)`),
		mk("metric:avg_over_time", "metrics", "avg_over_time", `{ } | avg_over_time(duration)`),
		mk("metric:sum_over_time", "metrics", "sum_over_time", `{ } | sum_over_time(.payload_bytes)`),
		mk("metric:quantile_over_time", "metrics", "quantile_over_time", `{ } | quantile_over_time(duration, .5, .9, .99)`),
		mk("metric:histogram_over_time", "metrics", "histogram_over_time", `{ } | histogram_over_time(duration)`),
		mk("metric:compare", "metrics", "compare", `{ } | compare({ status = error })`),
		mk("metric-mod:by", "metrics-modifier", "by", `{ } | rate() by (resource.service.name)`),
		mk("feature:hints", "feature", "with()", `{ } | rate() with (exemplars = true)`),

		// --- Metrics second stage ---
		mk("second:topk", "metrics-second-stage", "topk", `{ } | rate() by (name) | topk(3)`),
		mk("second:bottomk", "metrics-second-stage", "bottomk", `{ } | rate() by (name) | bottomk(3)`),
		mk("second:threshold", "metrics-second-stage", "> N", `{ } | rate() by (name) > 0.0001`),

		// --- Nested-set intrinsics (Grafana Traces Drilldown's
		// root-span idiom; only root-ness shapes are lowerable) ---
		mk("nestedset:root", "nested-set", "nestedSetParent < 0", `{ nestedSetParent < 0 }`),
		mk("nestedset:non-root", "nested-set", "nestedSetParent >= 0", `{ nestedSetParent >= 0 }`),
		mk("nestedset:position", "nested-set", "nestedSetParent = N", `{ nestedSetParent = 5 }`),
		mk("nestedset:left", "nested-set", "nestedSetLeft", `{ nestedSetLeft > 0 }`),
		mk("nestedset:right", "nested-set", "nestedSetRight", `{ nestedSetRight > 0 }`),
	}

	// Structural + set operators between spansets.
	for _, o := range []struct{ id, token string }{
		{"struct:child", ">"},
		{"struct:parent", "<"},
		{"struct:descendant", ">>"},
		{"struct:ancestor", "<<"},
		{"struct:sibling", "~"},
		{"struct:not-child", "!>"},
		{"struct:not-parent", "!<"},
		{"struct:not-descendant", "!>>"},
		{"struct:not-ancestor", "!<<"},
		{"struct:not-sibling", "!~"},
		{"struct:union-child", "&>"},
		{"struct:union-parent", "&<"},
		{"struct:union-descendant", "&>>"},
		{"struct:union-ancestor", "&<<"},
		{"struct:union-sibling", "&~"},
	} {
		rows = append(rows, mk(o.id, "structural", o.token,
			fmt.Sprintf(`{ resource.service.name = "gateway" } %s { resource.service.name = "shop" }`, o.token)))
	}
	rows = append(
		rows,
		mk("setop:and", "set-operation", "&&", `{ kind = server } && { status = error }`),
		mk("setop:union", "set-operation", "||", `{ kind = server } || { kind = client }`),
	)
	return rows
}

// CollectTraceQLFeatureIDs walks a parsed TraceQL expression and
// returns the set of inventory row IDs it exercises. AST-level so an
// operator token inside a string literal can never count as coverage.
// The pinned parser exports no generic visitor, so the walk is a local
// type-switch over the exported AST surface (pipeline elements, field
// expressions, metrics stages).
func CollectTraceQLFeatureIDs(root *traceql.RootExpr) map[string]bool {
	ids := map[string]bool{}
	if root == nil {
		return ids
	}
	for _, el := range root.Pipeline.Elements {
		collectTraceQLPipelineElement(ids, el)
	}
	if root.MetricsPipeline != nil {
		collectTraceQLFirstStage(ids, root.MetricsPipeline)
	}
	if root.MetricsSecondStage != nil {
		collectTraceQLSecondStage(ids, root.MetricsSecondStage)
	}
	if root.Hints != nil && len(root.Hints.Hints) > 0 {
		ids["feature:hints"] = true
	}
	return ids
}

func collectTraceQLPipelineElement(ids map[string]bool, el traceql.PipelineElement) {
	switch v := el.(type) {
	case *traceql.SpansetFilter:
		if v != nil {
			collectTraceQLFieldExpr(ids, v.Expression)
		}
	case *traceql.SpansetOperation:
		collectTraceQLSpansetOperation(ids, v)
	case traceql.SpansetOperation:
		collectTraceQLSpansetOperation(ids, &v)
	case traceql.ScalarFilter:
		collectTraceQLScalarExpr(ids, v.LHS)
		collectTraceQLScalarExpr(ids, v.RHS)
	case *traceql.ScalarFilter:
		if v != nil {
			collectTraceQLScalarExpr(ids, v.LHS)
			collectTraceQLScalarExpr(ids, v.RHS)
		}
	case traceql.Aggregate:
		collectTraceQLAggregate(ids, v)
	case *traceql.Aggregate:
		if v != nil {
			collectTraceQLAggregate(ids, *v)
		}
	case traceql.SelectOperation:
		ids["pipe:select"] = true
		for _, a := range v.Attrs() {
			collectTraceQLAttribute(ids, a)
		}
	case *traceql.SelectOperation:
		if v != nil {
			collectTraceQLPipelineElement(ids, *v)
		}
	case traceql.GroupOperation:
		ids["pipe:by"] = true
		collectTraceQLFieldExpr(ids, v.Expression)
	case *traceql.GroupOperation:
		if v != nil {
			collectTraceQLPipelineElement(ids, *v)
		}
	case traceql.CoalesceOperation, *traceql.CoalesceOperation:
		ids["pipe:coalesce"] = true
	}
}

func collectTraceQLSpansetOperation(ids map[string]bool, op *traceql.SpansetOperation) {
	if op == nil {
		return
	}
	if id, ok := spansetOpID(op.Op); ok {
		ids[id] = true
	}
	collectTraceQLSpansetExpr(ids, op.LHS)
	collectTraceQLSpansetExpr(ids, op.RHS)
}

func collectTraceQLSpansetExpr(ids map[string]bool, e traceql.SpansetExpression) {
	switch v := e.(type) {
	case *traceql.SpansetFilter:
		if v != nil {
			collectTraceQLFieldExpr(ids, v.Expression)
		}
	case *traceql.SpansetOperation:
		collectTraceQLSpansetOperation(ids, v)
	case traceql.SpansetOperation:
		collectTraceQLSpansetOperation(ids, &v)
	}
}

// spansetOpID maps the spanset-level operators (structural relations +
// set operations) onto row IDs.
func spansetOpID(op traceql.Operator) (string, bool) {
	switch op {
	case traceql.OpSpansetChild:
		return "struct:child", true
	case traceql.OpSpansetParent:
		return "struct:parent", true
	case traceql.OpSpansetDescendant:
		return "struct:descendant", true
	case traceql.OpSpansetAncestor:
		return "struct:ancestor", true
	case traceql.OpSpansetSibling:
		return "struct:sibling", true
	case traceql.OpSpansetNotChild:
		return "struct:not-child", true
	case traceql.OpSpansetNotParent:
		return "struct:not-parent", true
	case traceql.OpSpansetNotDescendant:
		return "struct:not-descendant", true
	case traceql.OpSpansetNotAncestor:
		return "struct:not-ancestor", true
	case traceql.OpSpansetNotSibling:
		return "struct:not-sibling", true
	case traceql.OpSpansetUnionChild:
		return "struct:union-child", true
	case traceql.OpSpansetUnionParent:
		return "struct:union-parent", true
	case traceql.OpSpansetUnionDescendant:
		return "struct:union-descendant", true
	case traceql.OpSpansetUnionAncestor:
		return "struct:union-ancestor", true
	case traceql.OpSpansetUnionSibling:
		return "struct:union-sibling", true
	case traceql.OpSpansetAnd:
		return "setop:and", true
	case traceql.OpSpansetUnion:
		return "setop:union", true
	}
	return "", false
}

func collectTraceQLScalarExpr(ids map[string]bool, e traceql.ScalarExpression) {
	switch v := e.(type) {
	case traceql.Aggregate:
		collectTraceQLAggregate(ids, v)
	case *traceql.Aggregate:
		if v != nil {
			collectTraceQLAggregate(ids, *v)
		}
	case traceql.Static:
		collectTraceQLStatic(ids, v)
	case *traceql.Static:
		if v != nil {
			collectTraceQLStatic(ids, *v)
		}
	}
}

func collectTraceQLAggregate(ids map[string]bool, agg traceql.Aggregate) {
	ids["pipe:"+agg.Op().String()] = true
	if inner := agg.InnerExpr(); inner != nil {
		collectTraceQLFieldExpr(ids, inner)
	}
}

func collectTraceQLFieldExpr(ids map[string]bool, e traceql.FieldExpression) {
	switch v := e.(type) {
	case *traceql.BinaryOperation:
		if v == nil {
			return
		}
		collectTraceQLBinary(ids, v)
	case *traceql.UnaryOperation:
		if v != nil {
			collectTraceQLUnary(ids, *v)
		}
	case traceql.UnaryOperation:
		collectTraceQLUnary(ids, v)
	case *traceql.Attribute:
		if v != nil {
			collectTraceQLAttribute(ids, *v)
		}
	case traceql.Attribute:
		collectTraceQLAttribute(ids, v)
	case *traceql.Static:
		if v != nil {
			collectTraceQLStatic(ids, *v)
		}
	case traceql.Static:
		collectTraceQLStatic(ids, v)
	}
}

func collectTraceQLBinary(ids map[string]bool, b *traceql.BinaryOperation) {
	// Nested-set comparisons get their own row family (root-ness vs
	// position-dependence) instead of the generic op rows, mirroring
	// internal/traceql's lowerNestedSetBinary classification.
	if nestedSetRowID(ids, b) {
		return
	}
	if id, ok := fieldOpID(b.Op); ok {
		ids[id] = true
	}
	collectTraceQLFieldExpr(ids, b.LHS)
	collectTraceQLFieldExpr(ids, b.RHS)
}

// fieldOpID maps field-expression operators onto row IDs.
func fieldOpID(op traceql.Operator) (string, bool) {
	switch op {
	case traceql.OpEqual:
		return "op:eq", true
	case traceql.OpNotEqual:
		return "op:neq", true
	case traceql.OpGreater:
		return "op:gt", true
	case traceql.OpGreaterEqual:
		return "op:gte", true
	case traceql.OpLess:
		return "op:lt", true
	case traceql.OpLessEqual:
		return "op:lte", true
	case traceql.OpRegex, traceql.OpRegexMatchAny:
		return "op:regex", true
	case traceql.OpNotRegex, traceql.OpRegexMatchNone:
		return "op:not-regex", true
	case traceql.OpAnd:
		return "op:and", true
	case traceql.OpOr:
		return "op:or", true
	case traceql.OpIn, traceql.OpNotIn:
		return "op:in", true
	case traceql.OpAdd:
		return "op:add", true
	case traceql.OpSub:
		return "op:sub", true
	case traceql.OpMult:
		return "op:mult", true
	case traceql.OpDiv:
		return "op:div", true
	case traceql.OpMod:
		return "op:mod", true
	case traceql.OpPower:
		return "op:pow", true
	}
	return "", false
}

func collectTraceQLUnary(ids map[string]bool, u traceql.UnaryOperation) {
	switch u.Op {
	case traceql.OpExists:
		ids["op:exists"] = true
	case traceql.OpNotExists:
		ids["op:not-exists"] = true
	case traceql.OpNot:
		ids["op:not"] = true
	}
	collectTraceQLFieldExpr(ids, u.Expression)
}

func collectTraceQLAttribute(ids map[string]bool, a traceql.Attribute) {
	if a.Intrinsic != traceql.IntrinsicNone {
		if id, ok := intrinsicRowID(a.Intrinsic); ok {
			ids[id] = true
		}
		return
	}
	switch a.Scope {
	case traceql.AttributeScopeSpan:
		ids["attr:span"] = true
	case traceql.AttributeScopeResource:
		ids["attr:resource"] = true
	case traceql.AttributeScopeEvent:
		ids["attr:event"] = true
	case traceql.AttributeScopeLink:
		ids["attr:link"] = true
	case traceql.AttributeScopeInstrumentation:
		ids["attr:instrumentation"] = true
	default:
		ids["attr:unscoped"] = true
	}
}

// intrinsicRowID maps the intrinsic enum onto row IDs. Scoped spellings
// the parser does NOT normalise (trace:rootName vs rootName) collapse
// onto the same row — the feature is the intrinsic, not the spelling.
// Nested-set intrinsics return no row here: their comparisons classify
// through nestedSetRowID and a bare reference matches no row.
func intrinsicRowID(i traceql.Intrinsic) (string, bool) {
	switch i {
	case traceql.IntrinsicName, traceql.ScopedIntrinsicSpanName:
		return "intrinsic:name", true
	case traceql.IntrinsicDuration, traceql.ScopedIntrinsicSpanDuration:
		return "intrinsic:duration", true
	case traceql.IntrinsicStatus, traceql.ScopedIntrinsicSpanStatus:
		return "intrinsic:status", true
	case traceql.IntrinsicStatusMessage, traceql.ScopedIntrinsicSpanStatusMessage:
		return "intrinsic:statusMessage", true
	case traceql.IntrinsicKind, traceql.ScopedIntrinsicSpanKind:
		return "intrinsic:kind", true
	case traceql.IntrinsicTraceID:
		return "intrinsic:trace-id", true
	case traceql.IntrinsicSpanID:
		return "intrinsic:span-id", true
	case traceql.IntrinsicParent, traceql.IntrinsicParentID:
		return "intrinsic:parent", true
	case traceql.IntrinsicEventName:
		return "intrinsic:event-name", true
	case traceql.IntrinsicLinkTraceID:
		return "intrinsic:link-traceID", true
	case traceql.IntrinsicLinkSpanID:
		return "intrinsic:link-spanID", true
	case traceql.IntrinsicInstrumentationName:
		return "intrinsic:instrumentation-name", true
	case traceql.IntrinsicInstrumentationVersion:
		return "intrinsic:instrumentation-version", true
	case traceql.IntrinsicTraceRootSpan, traceql.ScopedIntrinsicTraceRootName:
		return "intrinsic:rootName", true
	case traceql.IntrinsicTraceRootService, traceql.ScopedIntrinsicTraceRootService:
		return "intrinsic:rootServiceName", true
	case traceql.IntrinsicTraceDuration, traceql.ScopedIntrinsicTraceDuration:
		return "intrinsic:traceDuration", true
	case traceql.IntrinsicChildCount:
		return "intrinsic:childCount", true
	case traceql.IntrinsicEventTimeSinceStart:
		return "intrinsic:event-timeSinceStart", true
	}
	return "", false
}

// nestedSetRowID classifies a comparison touching the nested-set
// intrinsics into the nestedset:* row family and reports whether it
// did. Mirrors internal/traceql's lowerNestedSetBinary semantics:
// nestedSetParent comparisons whose truth value depends only on
// root-ness map to root / non-root; everything else is
// position-dependent.
func nestedSetRowID(ids map[string]bool, b *traceql.BinaryOperation) bool {
	attr, lit, flipped, ok := nestedSetOperands(b)
	if !ok {
		return false
	}
	switch attr.Intrinsic {
	case traceql.IntrinsicNestedSetLeft:
		ids["nestedset:left"] = true
		return true
	case traceql.IntrinsicNestedSetRight:
		ids["nestedset:right"] = true
		return true
	}
	if lit.Type != traceql.TypeInt {
		ids["nestedset:position"] = true
		return true
	}
	v64, _ := lit.Int()
	v := int64(v64)
	op := b.Op
	if flipped {
		op = flipTraceQLCmp(op)
	}
	root, rootOK := evalNestedSetCmp(-1, op, v)
	nonRoot, constant := nestedSetNonRootConstant(op, v)
	switch {
	case !rootOK || !constant:
		ids["nestedset:position"] = true
	case root && !nonRoot:
		ids["nestedset:root"] = true
	case !root && nonRoot:
		ids["nestedset:non-root"] = true
	default:
		ids["nestedset:position"] = true
	}
	return true
}

// nestedSetOperands extracts the (attribute, literal) pair from a
// comparison where one side is a nested-set intrinsic. flipped reports
// the literal was on the left.
func nestedSetOperands(b *traceql.BinaryOperation) (attr traceql.Attribute, lit traceql.Static, flipped, ok bool) {
	isNested := func(e traceql.FieldExpression) (traceql.Attribute, bool) {
		a, aok := exprAttribute(e)
		if !aok {
			return traceql.Attribute{}, false
		}
		switch a.Intrinsic {
		case traceql.IntrinsicNestedSetParent, traceql.IntrinsicNestedSetLeft, traceql.IntrinsicNestedSetRight:
			return a, true
		}
		return traceql.Attribute{}, false
	}
	if a, aok := isNested(b.LHS); aok {
		st, _ := exprStatic(b.RHS)
		return a, st, false, true
	}
	if a, aok := isNested(b.RHS); aok {
		st, _ := exprStatic(b.LHS)
		return a, st, true, true
	}
	return traceql.Attribute{}, traceql.Static{}, false, false
}

func exprAttribute(e traceql.FieldExpression) (traceql.Attribute, bool) {
	switch v := e.(type) {
	case *traceql.Attribute:
		if v == nil {
			return traceql.Attribute{}, false
		}
		return *v, true
	case traceql.Attribute:
		return v, true
	}
	return traceql.Attribute{}, false
}

func exprStatic(e traceql.FieldExpression) (traceql.Static, bool) {
	switch v := e.(type) {
	case *traceql.Static:
		if v == nil {
			return traceql.Static{}, false
		}
		return *v, true
	case traceql.Static:
		return v, true
	}
	return traceql.Static{}, false
}

func flipTraceQLCmp(op traceql.Operator) traceql.Operator {
	switch op {
	case traceql.OpLess:
		return traceql.OpGreater
	case traceql.OpLessEqual:
		return traceql.OpGreaterEqual
	case traceql.OpGreater:
		return traceql.OpLess
	case traceql.OpGreaterEqual:
		return traceql.OpLessEqual
	}
	return op
}

func evalNestedSetCmp(a int64, op traceql.Operator, v int64) (bool, bool) {
	switch op {
	case traceql.OpEqual:
		return a == v, true
	case traceql.OpNotEqual:
		return a != v, true
	case traceql.OpLess:
		return a < v, true
	case traceql.OpLessEqual:
		return a <= v, true
	case traceql.OpGreater:
		return a > v, true
	case traceql.OpGreaterEqual:
		return a >= v, true
	}
	return false, false
}

// nestedSetNonRootConstant mirrors internal/traceql's
// nonRootCmpConstant: whether `p op v` has the same truth value for
// every non-root position p >= 1, and what that value is.
func nestedSetNonRootConstant(op traceql.Operator, v int64) (value, constant bool) {
	switch op {
	case traceql.OpEqual:
		if v < 1 {
			return false, true
		}
	case traceql.OpNotEqual:
		if v < 1 {
			return true, true
		}
	case traceql.OpLess:
		if v <= 1 {
			return false, true
		}
	case traceql.OpLessEqual:
		if v < 1 {
			return false, true
		}
	case traceql.OpGreater:
		if v < 1 {
			return true, true
		}
	case traceql.OpGreaterEqual:
		if v <= 1 {
			return true, true
		}
	}
	return false, false
}

func collectTraceQLStatic(ids map[string]bool, st traceql.Static) {
	switch st.Type {
	case traceql.TypeString:
		ids["static:string"] = true
	case traceql.TypeInt:
		ids["static:int"] = true
	case traceql.TypeFloat:
		ids["static:float"] = true
	case traceql.TypeBoolean:
		ids["static:bool"] = true
	case traceql.TypeDuration:
		ids["static:duration"] = true
	case traceql.TypeStatus:
		ids["static:status"] = true
	case traceql.TypeKind:
		ids["static:kind"] = true
	}
}

func collectTraceQLFirstStage(ids map[string]bool, fs traceql.FirstStageElement) {
	switch v := fs.(type) {
	case *traceql.MetricsAggregate:
		if v == nil {
			return
		}
		ids["metric:"+v.Op().String()] = true
		if attr := v.Attribute(); attr != (traceql.Attribute{}) {
			collectTraceQLAttribute(ids, attr)
		}
		collectTraceQLGroupBy(ids, v.GroupBy())
	case *traceql.AverageOverTimeAggregator:
		if v == nil {
			return
		}
		ids["metric:avg_over_time"] = true
		if attr := v.Attribute(); attr != (traceql.Attribute{}) {
			collectTraceQLAttribute(ids, attr)
		}
		collectTraceQLGroupBy(ids, v.GroupBy())
	case *traceql.MetricsCompare:
		ids["metric:compare"] = true
	}
}

func collectTraceQLGroupBy(ids map[string]bool, attrs []traceql.Attribute) {
	if len(attrs) == 0 {
		return
	}
	ids["metric-mod:by"] = true
	for _, a := range attrs {
		collectTraceQLAttribute(ids, a)
	}
}

func collectTraceQLSecondStage(ids map[string]bool, ss traceql.SecondStageElement) {
	switch v := ss.(type) {
	case *traceql.TopKBottomK:
		if v == nil {
			return
		}
		switch v.Op() {
		case traceql.OpTopK:
			ids["second:topk"] = true
		case traceql.OpBottomK:
			ids["second:bottomk"] = true
		}
	case *traceql.MetricsFilter:
		ids["second:threshold"] = true
	case traceql.ChainedSecondStage:
		for _, el := range v.Elements() {
			collectTraceQLSecondStage(ids, el)
		}
	case *traceql.ChainedSecondStage:
		if v != nil {
			collectTraceQLSecondStage(ids, *v)
		}
	}
}
