package traceql

import (
	"strings"
	"testing"
	"time"

	"github.com/grafana/tempo/pkg/traceql"
)

// TestParserShape_* tests pin specific properties of the parsed Tempo
// AST so we catch silent breakage when the upstream parser bumps. Each
// test mirrors what cerberus's lowering layer reads in
// internal/traceql/{lower,metrics_pipeline,aggregate,set_ops,...}.go.
//
// Pure acceptance / rejection is already covered by TestParserSmoke /
// TestParserSmoke_Rejected.

func mustParseTraceQL(t *testing.T, q string) *traceql.RootExpr {
	t.Helper()
	expr, err := traceql.Parse(q)
	if err != nil {
		t.Fatalf("Parse(%q): %v", q, err)
	}
	if expr == nil {
		t.Fatalf("Parse(%q) returned nil", q)
	}
	return expr
}

// TestParserShape_EmptySelector pins the `{}` shape: a single
// *SpansetFilter with Expression == Static{Type: TypeBoolean, Bool: true}.
// cerberus relies on at least one Pipeline element for any traceql query.
func TestParserShape_EmptySelector(t *testing.T) {
	t.Parallel()
	expr := mustParseTraceQL(t, `{}`)
	if len(expr.Pipeline.Elements) != 1 {
		t.Fatalf("Pipeline.Elements length = %d; want 1", len(expr.Pipeline.Elements))
	}
	sf, ok := expr.Pipeline.Elements[0].(*traceql.SpansetFilter)
	if !ok {
		t.Fatalf("element[0] = %T; want *SpansetFilter", expr.Pipeline.Elements[0])
	}
	// Empty `{}` lowers as a `true` Static literal expression.
	st, ok := sf.Expression.(traceql.Static)
	if !ok {
		t.Fatalf("Expression = %T; want traceql.Static for empty {}", sf.Expression)
	}
	if st.Type != traceql.TypeBoolean {
		t.Errorf("Static.Type = %v; want TypeBoolean", st.Type)
	}
	b, bOK := st.Bool()
	if !bOK || !b {
		t.Errorf("Static.Bool() = (%v, %v); want (true, true)", b, bOK)
	}
}

// TestParserShape_IntrinsicName pins the intrinsic-name shape:
// `{ name = "GET /api" }` lowers to BinaryOperation(=, Attribute{Intrinsic:
// IntrinsicName}, Static{TypeString}).
func TestParserShape_IntrinsicName(t *testing.T) {
	t.Parallel()
	expr := mustParseTraceQL(t, `{ name = "GET /api" }`)
	sf := expr.Pipeline.Elements[0].(*traceql.SpansetFilter)
	bin, ok := sf.Expression.(*traceql.BinaryOperation)
	if !ok {
		t.Fatalf("Expression = %T; want *BinaryOperation", sf.Expression)
	}
	if bin.Op != traceql.OpEqual {
		t.Errorf("Op = %v; want OpEqual", bin.Op)
	}
	attr, ok := bin.LHS.(traceql.Attribute)
	if !ok {
		t.Fatalf("LHS = %T; want traceql.Attribute", bin.LHS)
	}
	if attr.Intrinsic != traceql.IntrinsicName {
		t.Errorf("Attribute.Intrinsic = %v; want IntrinsicName", attr.Intrinsic)
	}
	st, ok := bin.RHS.(traceql.Static)
	if !ok {
		t.Fatalf("RHS = %T; want traceql.Static", bin.RHS)
	}
	if st.Type != traceql.TypeString {
		t.Errorf("RHS.Type = %v; want TypeString", st.Type)
	}
	if got := st.EncodeToString(false); got != "GET /api" {
		t.Errorf("RHS string = %q; want %q", got, "GET /api")
	}
}

// TestParserShape_ResourceAttribute pins
// `{ resource.service.name = "frontend" }`. LHS Attribute must have Scope
// == AttributeScopeResource and Name == "service.name".
func TestParserShape_ResourceAttribute(t *testing.T) {
	t.Parallel()
	expr := mustParseTraceQL(t, `{ resource.service.name = "frontend" }`)
	sf := expr.Pipeline.Elements[0].(*traceql.SpansetFilter)
	bin := sf.Expression.(*traceql.BinaryOperation)
	attr, ok := bin.LHS.(traceql.Attribute)
	if !ok {
		t.Fatalf("LHS = %T; want traceql.Attribute", bin.LHS)
	}
	if attr.Scope != traceql.AttributeScopeResource {
		t.Errorf("Scope = %v; want AttributeScopeResource", attr.Scope)
	}
	if attr.Name != "service.name" {
		t.Errorf("Name = %q; want %q", attr.Name, "service.name")
	}
	if attr.Intrinsic != traceql.IntrinsicNone {
		t.Errorf("Intrinsic = %v; want IntrinsicNone", attr.Intrinsic)
	}
}

// TestParserShape_SpanAttribute pins
// `{ span.http.status_code = 500 }`. LHS Attribute must have Scope ==
// AttributeScopeSpan; RHS must be Static{TypeInt, 500}.
func TestParserShape_SpanAttribute(t *testing.T) {
	t.Parallel()
	expr := mustParseTraceQL(t, `{ span.http.status_code = 500 }`)
	sf := expr.Pipeline.Elements[0].(*traceql.SpansetFilter)
	bin := sf.Expression.(*traceql.BinaryOperation)
	attr, ok := bin.LHS.(traceql.Attribute)
	if !ok {
		t.Fatalf("LHS = %T; want traceql.Attribute", bin.LHS)
	}
	if attr.Scope != traceql.AttributeScopeSpan {
		t.Errorf("Scope = %v; want AttributeScopeSpan", attr.Scope)
	}
	if attr.Name != "http.status_code" {
		t.Errorf("Name = %q; want %q", attr.Name, "http.status_code")
	}
	st, ok := bin.RHS.(traceql.Static)
	if !ok {
		t.Fatalf("RHS = %T; want traceql.Static", bin.RHS)
	}
	if st.Type != traceql.TypeInt {
		t.Errorf("RHS Type = %v; want TypeInt", st.Type)
	}
	i, ok := st.Int()
	if !ok || i != 500 {
		t.Errorf("RHS Int = (%d, %v); want (500, true)", i, ok)
	}
}

// TestParserShape_StructuralChild pins SpansetOperation with
// OpSpansetChild for `A > B`.
func TestParserShape_StructuralChild(t *testing.T) {
	t.Parallel()
	expr := mustParseTraceQL(t, `{ name = "A" } > { name = "B" }`)
	op, ok := expr.Pipeline.Elements[0].(traceql.SpansetOperation)
	if !ok {
		t.Fatalf("element[0] = %T; want traceql.SpansetOperation", expr.Pipeline.Elements[0])
	}
	if op.Op != traceql.OpSpansetChild {
		t.Errorf("Op = %v; want OpSpansetChild", op.Op)
	}
	if _, ok := op.LHS.(*traceql.SpansetFilter); !ok {
		t.Errorf("LHS = %T; want *SpansetFilter", op.LHS)
	}
	if _, ok := op.RHS.(*traceql.SpansetFilter); !ok {
		t.Errorf("RHS = %T; want *SpansetFilter", op.RHS)
	}
}

// TestParserShape_StructuralDescendant pins SpansetOperation with
// OpSpansetDescendant for `A >> B`.
func TestParserShape_StructuralDescendant(t *testing.T) {
	t.Parallel()
	expr := mustParseTraceQL(t, `{ name = "A" } >> { name = "B" }`)
	op, ok := expr.Pipeline.Elements[0].(traceql.SpansetOperation)
	if !ok {
		t.Fatalf("element[0] = %T; want traceql.SpansetOperation", expr.Pipeline.Elements[0])
	}
	if op.Op != traceql.OpSpansetDescendant {
		t.Errorf("Op = %v; want OpSpansetDescendant", op.Op)
	}
}

// TestParserShape_SetIntersection pins SpansetOperation with
// OpSpansetAnd for `A && B`.
func TestParserShape_SetIntersection(t *testing.T) {
	t.Parallel()
	expr := mustParseTraceQL(t, `{ name = "A" } && { name = "B" }`)
	op, ok := expr.Pipeline.Elements[0].(traceql.SpansetOperation)
	if !ok {
		t.Fatalf("element[0] = %T; want traceql.SpansetOperation", expr.Pipeline.Elements[0])
	}
	if op.Op != traceql.OpSpansetAnd {
		t.Errorf("Op = %v; want OpSpansetAnd", op.Op)
	}
}

// TestParserShape_StatusEnum pins `{ status = error }`. LHS is the
// intrinsic status; RHS is Static{TypeStatus, StatusError}.
func TestParserShape_StatusEnum(t *testing.T) {
	t.Parallel()
	expr := mustParseTraceQL(t, `{ status = error }`)
	sf := expr.Pipeline.Elements[0].(*traceql.SpansetFilter)
	bin := sf.Expression.(*traceql.BinaryOperation)
	attr, ok := bin.LHS.(traceql.Attribute)
	if !ok {
		t.Fatalf("LHS = %T; want traceql.Attribute", bin.LHS)
	}
	if attr.Intrinsic != traceql.IntrinsicStatus {
		t.Errorf("Attribute.Intrinsic = %v; want IntrinsicStatus", attr.Intrinsic)
	}
	st, ok := bin.RHS.(traceql.Static)
	if !ok {
		t.Fatalf("RHS = %T; want traceql.Static", bin.RHS)
	}
	if st.Type != traceql.TypeStatus {
		t.Errorf("RHS.Type = %v; want TypeStatus", st.Type)
	}
	s, ok := st.Status()
	if !ok {
		t.Fatalf("Status() returned ok=false")
	}
	if s != traceql.StatusError {
		t.Errorf("Status() = %v; want StatusError", s)
	}
}

// TestParserShape_ScalarFilterCount pins
// `{ duration > 100ms } | count() > 0` — a two-element Pipeline whose
// tail element is a ScalarFilter{LHS: Aggregate, RHS: Static}.
func TestParserShape_ScalarFilterCount(t *testing.T) {
	t.Parallel()
	expr := mustParseTraceQL(t, `{ duration > 100ms } | count() > 0`)
	if len(expr.Pipeline.Elements) != 2 {
		t.Fatalf("Pipeline.Elements length = %d; want 2", len(expr.Pipeline.Elements))
	}
	if _, ok := expr.Pipeline.Elements[0].(*traceql.SpansetFilter); !ok {
		t.Errorf("element[0] = %T; want *SpansetFilter", expr.Pipeline.Elements[0])
	}
	sf, ok := expr.Pipeline.Elements[1].(traceql.ScalarFilter)
	if !ok {
		t.Fatalf("element[1] = %T; want traceql.ScalarFilter", expr.Pipeline.Elements[1])
	}
	if sf.Op != traceql.OpGreater {
		t.Errorf("ScalarFilter.Op = %v; want OpGreater", sf.Op)
	}
	if _, ok := sf.LHS.(traceql.Aggregate); !ok {
		t.Errorf("LHS = %T; want traceql.Aggregate", sf.LHS)
	}
	if _, ok := sf.RHS.(traceql.Static); !ok {
		t.Errorf("RHS = %T; want traceql.Static", sf.RHS)
	}
	// Also confirm the spanset's duration RHS is a TypeDuration static.
	specSf := expr.Pipeline.Elements[0].(*traceql.SpansetFilter)
	bin := specSf.Expression.(*traceql.BinaryOperation)
	durSt, ok := bin.RHS.(traceql.Static)
	if !ok {
		t.Fatalf("SpansetFilter RHS = %T; want traceql.Static", bin.RHS)
	}
	if durSt.Type != traceql.TypeDuration {
		t.Errorf("duration Static.Type = %v; want TypeDuration", durSt.Type)
	}
	d, ok := durSt.Duration()
	if !ok || d != 100*time.Millisecond {
		t.Errorf("Duration() = (%v, %v); want (100ms, true)", d, ok)
	}
}

// TestParserShape_MetricsPipelineRate pins `{} | rate()`. expr.MetricsPipeline
// must be a *MetricsAggregate whose Op() == MetricsAggregateRate.
func TestParserShape_MetricsPipelineRate(t *testing.T) {
	t.Parallel()
	expr := mustParseTraceQL(t, `{} | rate()`)
	if expr.MetricsPipeline == nil {
		t.Fatal("expr.MetricsPipeline = nil; want *MetricsAggregate")
	}
	ma, ok := expr.MetricsPipeline.(*traceql.MetricsAggregate)
	if !ok {
		t.Fatalf("MetricsPipeline = %T; want *traceql.MetricsAggregate", expr.MetricsPipeline)
	}
	if ma.Op() != traceql.MetricsAggregateRate {
		t.Errorf("Op() = %v; want MetricsAggregateRate", ma.Op())
	}
	// `rate()` has no attribute operand — Attribute() should be the zero
	// value (Tempo's "no-attribute" sentinel).
	if ma.Attribute() != (traceql.Attribute{}) {
		t.Errorf("Attribute() = %v; want zero-value Attribute{}", ma.Attribute())
	}
}

// TestParserError_* tests pin the error message substrings cerberus's
// /tempo HTTP handlers translate into TraceQL error responses.

func errContainsAny(err error, wants ...string) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, w := range wants {
		if strings.Contains(msg, strings.ToLower(w)) {
			return true
		}
	}
	return false
}

func parseShouldFailTraceQL(t *testing.T, q string) error {
	t.Helper()
	_, err := traceql.Parse(q)
	if err == nil {
		t.Fatalf("Parse(%q): expected error, got nil", q)
	}
	return err
}

func TestParserError_MissingMatcherRHS(t *testing.T) {
	t.Parallel()
	err := parseShouldFailTraceQL(t, `{ name = }`)
	if !errContainsAny(err, "parse", "syntax", "unexpected", "}", "expected") {
		t.Errorf("err = %q; want substring rejecting missing RHS", err)
	}
}

func TestParserError_TrailingGreater(t *testing.T) {
	t.Parallel()
	err := parseShouldFailTraceQL(t, `{} > `)
	if !errContainsAny(err, "parse", "syntax", "unexpected", "expected", "$end") {
		t.Errorf("err = %q; want substring rejecting trailing operator", err)
	}
}

func TestParserError_DurationMissingRHS(t *testing.T) {
	t.Parallel()
	err := parseShouldFailTraceQL(t, `{ duration > }`)
	if !errContainsAny(err, "parse", "syntax", "unexpected", "}", "expected") {
		t.Errorf("err = %q; want substring rejecting missing duration RHS", err)
	}
}

func TestParserError_AndMissingRHS(t *testing.T) {
	t.Parallel()
	// `a` is not a typed identifier in TraceQL — upstream rejects this
	// at parse time with "unknown identifier: a". We pin that signal so
	// the handler's error-class translation stays observable.
	err := parseShouldFailTraceQL(t, `{ a && }`)
	if !errContainsAny(err, "unknown identifier", "parse", "syntax", "unexpected", "}", "expected") {
		t.Errorf("err = %q; want substring rejecting unknown identifier", err)
	}
}

func TestParserError_BadOperator(t *testing.T) {
	t.Parallel()
	// `<>` is not a valid TraceQL operator.
	err := parseShouldFailTraceQL(t, `{ 2 <> 3 }`)
	if !errContainsAny(err, "parse", "syntax", "unexpected", "expected") {
		t.Errorf("err = %q; want substring rejecting bad operator", err)
	}
}

func TestParserError_UnterminatedBrace(t *testing.T) {
	t.Parallel()
	err := parseShouldFailTraceQL(t, `{ .a = .b `)
	if !errContainsAny(err, "parse", "syntax", "unexpected", "EOF", "$end", "expected") {
		t.Errorf("err = %q; want substring rejecting unterminated brace", err)
	}
}

func TestParserError_MalformedExpression(t *testing.T) {
	t.Parallel()
	err := parseShouldFailTraceQL(t, `{ + }`)
	if !errContainsAny(err, "parse", "syntax", "unexpected", "expected") {
		t.Errorf("err = %q; want substring rejecting malformed expression", err)
	}
}

func TestParserError_TrailingPipe(t *testing.T) {
	t.Parallel()
	err := parseShouldFailTraceQL(t, `{ .a } | `)
	if !errContainsAny(err, "parse", "syntax", "unexpected", "EOF", "$end", "expected") {
		t.Errorf("err = %q; want substring rejecting trailing pipe", err)
	}
}

func TestParserError_IncompleteAggregate(t *testing.T) {
	t.Parallel()
	err := parseShouldFailTraceQL(t, `{ .a } | count(`)
	if !errContainsAny(err, "parse", "syntax", "unexpected", "EOF", "$end", "expected") {
		t.Errorf("err = %q; want substring rejecting incomplete aggregate", err)
	}
}

func TestParserError_UnknownTopLevelIdentifier(t *testing.T) {
	t.Parallel()
	err := parseShouldFailTraceQL(t, `wharblgarbl`)
	if !errContainsAny(err, "parse", "syntax", "unexpected", "wharblgarbl", "expected") {
		t.Errorf("err = %q; want substring rejecting top-level identifier", err)
	}
}
