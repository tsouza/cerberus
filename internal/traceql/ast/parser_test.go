package ast

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, q string) *RootExpr {
	t.Helper()
	expr, err := Parse(q)
	if err != nil {
		t.Fatalf("Parse(%q): %v", q, err)
	}
	if expr == nil {
		t.Fatalf("Parse(%q) returned nil", q)
	}
	return expr
}

func firstElem(t *testing.T, q string) PipelineElement {
	t.Helper()
	expr := mustParse(t, q)
	if len(expr.Pipeline.Elements) == 0 {
		t.Fatalf("Parse(%q): empty pipeline", q)
	}
	return expr.Pipeline.Elements[0]
}

// TestParseEmptySelector pins `{}` → single *SpansetFilter whose expression is
// the boolean-true static.
func TestParseEmptySelector(t *testing.T) {
	t.Parallel()
	expr := mustParse(t, `{}`)
	if len(expr.Pipeline.Elements) != 1 {
		t.Fatalf("Elements = %d; want 1", len(expr.Pipeline.Elements))
	}
	sf, ok := expr.Pipeline.Elements[0].(*SpansetFilter)
	if !ok {
		t.Fatalf("element[0] = %T; want *SpansetFilter", expr.Pipeline.Elements[0])
	}
	st, ok := sf.Expression.(Static)
	if !ok {
		t.Fatalf("Expression = %T; want Static", sf.Expression)
	}
	if st.Type != TypeBoolean {
		t.Errorf("Static.Type = %v; want TypeBoolean", st.Type)
	}
	if b, _ := st.Bool(); !b {
		t.Errorf("Static bool = false; want true")
	}
}

// TestParseIntrinsicNameEq pins `{ name = "GET /api" }`.
func TestParseIntrinsicNameEq(t *testing.T) {
	t.Parallel()
	sf := firstElem(t, `{ name = "GET /api" }`).(*SpansetFilter)
	bin, ok := sf.Expression.(*BinaryOperation)
	if !ok {
		t.Fatalf("Expression = %T; want *BinaryOperation", sf.Expression)
	}
	if bin.Op != OpEqual {
		t.Errorf("Op = %v; want OpEqual", bin.Op)
	}
	attr, ok := bin.LHS.(Attribute)
	if !ok {
		t.Fatalf("LHS = %T; want Attribute", bin.LHS)
	}
	if attr.Intrinsic != IntrinsicName {
		t.Errorf("Intrinsic = %v; want IntrinsicName", attr.Intrinsic)
	}
	st, ok := bin.RHS.(Static)
	if !ok || st.Type != TypeString {
		t.Fatalf("RHS = %T (type %v); want string Static", bin.RHS, st.Type)
	}
	if got := st.EncodeToString(false); got != "GET /api" {
		t.Errorf("RHS = %q; want %q", got, "GET /api")
	}
}

// TestParseScopedAttributes pins resource./span./parent.span. scopes.
func TestParseScopedAttributes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		query  string
		scope  AttributeScope
		parent bool
		name   string
	}{
		{`{ resource.service.name = "frontend" }`, AttributeScopeResource, false, "service.name"},
		{`{ span.http.status_code >= 500 }`, AttributeScopeSpan, false, "http.status_code"},
		{`{ .http.method = "GET" }`, AttributeScopeNone, false, "http.method"},
		{`{ parent.span.foo = 1 }`, AttributeScopeSpan, true, "foo"},
		{`{ parent.resource.bar = 1 }`, AttributeScopeResource, true, "bar"},
		{`{ parent.baz = 1 }`, AttributeScopeNone, true, "baz"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.query, func(t *testing.T) {
			t.Parallel()
			sf := firstElem(t, tc.query).(*SpansetFilter)
			bin := sf.Expression.(*BinaryOperation)
			attr, ok := bin.LHS.(Attribute)
			if !ok {
				t.Fatalf("LHS = %T; want Attribute", bin.LHS)
			}
			if attr.Scope != tc.scope {
				t.Errorf("Scope = %v; want %v", attr.Scope, tc.scope)
			}
			if attr.Parent != tc.parent {
				t.Errorf("Parent = %v; want %v", attr.Parent, tc.parent)
			}
			if attr.Name != tc.name {
				t.Errorf("Name = %q; want %q", attr.Name, tc.name)
			}
			if attr.Intrinsic != IntrinsicNone {
				t.Errorf("Intrinsic = %v; want IntrinsicNone", attr.Intrinsic)
			}
		})
	}
}

// TestParseStructuralOps pins structural / set operators → SpansetOperation
// (value) with the right Op.
func TestParseStructuralOps(t *testing.T) {
	t.Parallel()
	cases := []struct {
		query string
		op    Operator
	}{
		{`{ name = "A" } > { name = "B" }`, OpSpansetChild},
		{`{ name = "A" } < { name = "B" }`, OpSpansetParent},
		{`{ name = "A" } >> { name = "B" }`, OpSpansetDescendant},
		{`{ name = "A" } << { name = "B" }`, OpSpansetAncestor},
		{`{ name = "A" } ~ { name = "B" }`, OpSpansetSibling},
		{`{ .a } && { .b }`, OpSpansetAnd},
		{`{ .a } || { .b }`, OpSpansetUnion},
		{`{ .a } !> { .b }`, OpSpansetNotChild},
		{`{ .a } !>> { .b }`, OpSpansetNotDescendant},
		{`{ .a } &> { .b }`, OpSpansetUnionChild},
		{`{ .a } &>> { .b }`, OpSpansetUnionDescendant},
		{`{ .a } &< { .b }`, OpSpansetUnionParent},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.query, func(t *testing.T) {
			t.Parallel()
			op, ok := firstElem(t, tc.query).(SpansetOperation)
			if !ok {
				t.Fatalf("element[0] = %T; want SpansetOperation", firstElem(t, tc.query))
			}
			if op.Op != tc.op {
				t.Errorf("Op = %v; want %v", op.Op, tc.op)
			}
			if _, ok := op.LHS.(*SpansetFilter); !ok {
				t.Errorf("LHS = %T; want *SpansetFilter", op.LHS)
			}
			if _, ok := op.RHS.(*SpansetFilter); !ok {
				t.Errorf("RHS = %T; want *SpansetFilter", op.RHS)
			}
		})
	}
}

// TestParseStatusKindStatics pins the status / kind literal value types.
func TestParseStatusKindStatics(t *testing.T) {
	t.Parallel()
	sf := firstElem(t, `{ status = error }`).(*SpansetFilter)
	bin := sf.Expression.(*BinaryOperation)
	st := bin.RHS.(Static)
	if st.Type != TypeStatus {
		t.Fatalf("Type = %v; want TypeStatus", st.Type)
	}
	if s, _ := st.Status(); s != StatusError {
		t.Errorf("Status = %v; want StatusError", s)
	}

	sf = firstElem(t, `{ kind = client }`).(*SpansetFilter)
	bin = sf.Expression.(*BinaryOperation)
	st = bin.RHS.(Static)
	if st.Type != TypeKind {
		t.Fatalf("Type = %v; want TypeKind", st.Type)
	}
	if k, _ := st.Kind(); k != KindClient {
		t.Errorf("Kind = %v; want KindClient", k)
	}
}

// TestParseScalarFilterPipeline pins `{ duration > 100ms } | count() > 0`.
func TestParseScalarFilterPipeline(t *testing.T) {
	t.Parallel()
	expr := mustParse(t, `{ duration > 100ms } | count() > 0`)
	if len(expr.Pipeline.Elements) != 2 {
		t.Fatalf("Elements = %d; want 2", len(expr.Pipeline.Elements))
	}
	if _, ok := expr.Pipeline.Elements[0].(*SpansetFilter); !ok {
		t.Errorf("element[0] = %T; want *SpansetFilter", expr.Pipeline.Elements[0])
	}
	sf, ok := expr.Pipeline.Elements[1].(ScalarFilter)
	if !ok {
		t.Fatalf("element[1] = %T; want ScalarFilter", expr.Pipeline.Elements[1])
	}
	if sf.Op != OpGreater {
		t.Errorf("Op = %v; want OpGreater", sf.Op)
	}
	if _, ok := sf.LHS.(Aggregate); !ok {
		t.Errorf("LHS = %T; want Aggregate", sf.LHS)
	}
	if _, ok := sf.RHS.(Static); !ok {
		t.Errorf("RHS = %T; want Static", sf.RHS)
	}
}

// TestParseAggregateInner pins `| max(duration)` carries its inner expr + op.
func TestParseAggregateInner(t *testing.T) {
	t.Parallel()
	expr := mustParse(t, `{ } | max(duration) > 1s`)
	sf := expr.Pipeline.Elements[1].(ScalarFilter)
	agg := sf.LHS.(Aggregate)
	if agg.Op() != AggregateMax {
		t.Errorf("Op = %v; want AggregateMax", agg.Op())
	}
	inner, ok := agg.InnerExpr().(Attribute)
	if !ok || inner.Intrinsic != IntrinsicDuration {
		t.Errorf("InnerExpr = %#v; want duration intrinsic", agg.InnerExpr())
	}
}

// TestParseGroupSelectCoalesce pins by()/select()/coalesce() stages.
func TestParseGroupSelectCoalesce(t *testing.T) {
	t.Parallel()
	g := mustParse(t, `{ } | by(resource.service.name)`).Pipeline.Elements[1]
	grp, ok := g.(GroupOperation)
	if !ok {
		t.Fatalf("group = %T; want GroupOperation", g)
	}
	if _, ok := grp.Expression.(Attribute); !ok {
		t.Errorf("group expr = %T; want Attribute", grp.Expression)
	}

	s := mustParse(t, `{ } | select(span.http.method, span.http.status_code)`).Pipeline.Elements[1]
	sel, ok := s.(SelectOperation)
	if !ok {
		t.Fatalf("select = %T; want SelectOperation", s)
	}
	if len(sel.Attrs()) != 2 {
		t.Errorf("select attrs = %d; want 2", len(sel.Attrs()))
	}

	co := mustParse(t, `{ } | coalesce()`).Pipeline.Elements[1]
	if _, ok := co.(CoalesceOperation); !ok {
		t.Fatalf("coalesce = %T; want CoalesceOperation", co)
	}
}

// TestParseNilExistence pins the nil-comparison → existence-check rewrites.
func TestParseNilExistence(t *testing.T) {
	t.Parallel()
	sf := firstElem(t, `{ .foo != nil }`).(*SpansetFilter)
	u, ok := sf.Expression.(UnaryOperation)
	if !ok {
		t.Fatalf("expr = %T; want UnaryOperation", sf.Expression)
	}
	if u.Op != OpExists {
		t.Errorf("Op = %v; want OpExists", u.Op)
	}

	sf = firstElem(t, `{ .foo = nil }`).(*SpansetFilter)
	u = sf.Expression.(UnaryOperation)
	if u.Op != OpNotExists {
		t.Errorf("Op = %v; want OpNotExists", u.Op)
	}
}

// TestParseUnaryMinusFold pins that `-<literal>` folds to a static while
// `-<span-attr>` stays a UnaryOperation.
func TestParseUnaryMinusFold(t *testing.T) {
	t.Parallel()
	// -100 folds to a static int on the RHS.
	sf := firstElem(t, `{ .x > -100 }`).(*SpansetFilter)
	bin := sf.Expression.(*BinaryOperation)
	rs, ok := bin.RHS.(Static)
	if !ok {
		t.Fatalf("RHS = %T; want folded Static", bin.RHS)
	}
	if i, _ := rs.Int(); i != -100 {
		t.Errorf("RHS int = %d; want -100", i)
	}

	// -.x references the span and must remain a UnaryOperation.
	sf = firstElem(t, `{ -.payload > 5 }`).(*SpansetFilter)
	bin = sf.Expression.(*BinaryOperation)
	if _, ok := bin.LHS.(UnaryOperation); !ok {
		t.Errorf("LHS = %T; want UnaryOperation", bin.LHS)
	}
}

// TestParseMetricsRate pins `| rate()` and its by() grouping.
func TestParseMetricsRate(t *testing.T) {
	t.Parallel()
	expr := mustParse(t, `{ } | rate()`)
	ma, ok := expr.MetricsPipeline.(*MetricsAggregate)
	if !ok {
		t.Fatalf("MetricsPipeline = %T; want *MetricsAggregate", expr.MetricsPipeline)
	}
	if ma.Op() != MetricsAggregateRate {
		t.Errorf("Op = %v; want MetricsAggregateRate", ma.Op())
	}
	if ma.Attribute() != (Attribute{}) {
		t.Errorf("Attribute = %v; want zero", ma.Attribute())
	}

	expr = mustParse(t, `{ } | rate() by(resource.service.name)`)
	ma = expr.MetricsPipeline.(*MetricsAggregate)
	if len(ma.GroupBy()) != 1 {
		t.Errorf("GroupBy = %d; want 1", len(ma.GroupBy()))
	}
}

// TestParseMetricsQuantileAndOverTime pins quantile_over_time + *_over_time.
func TestParseMetricsQuantileAndOverTime(t *testing.T) {
	t.Parallel()
	expr := mustParse(t, `{ } | quantile_over_time(duration, 0.5, 0.9)`)
	ma := expr.MetricsPipeline.(*MetricsAggregate)
	if ma.Op() != MetricsAggregateQuantileOverTime {
		t.Errorf("Op = %v; want quantile_over_time", ma.Op())
	}
	if got := ma.Quantiles(); len(got) != 2 || got[0] != 0.5 || got[1] != 0.9 {
		t.Errorf("Quantiles = %v; want [0.5 0.9]", got)
	}
	if ma.Attribute().Intrinsic != IntrinsicDuration {
		t.Errorf("attr = %v; want duration", ma.Attribute())
	}

	expr = mustParse(t, `{ } | avg_over_time(duration)`)
	if _, ok := expr.MetricsPipeline.(*AverageOverTimeAggregator); !ok {
		t.Fatalf("avg_over_time = %T; want *AverageOverTimeAggregator", expr.MetricsPipeline)
	}
}

// TestParseMetricsCompare pins compare() defaults and explicit args.
func TestParseMetricsCompare(t *testing.T) {
	t.Parallel()
	expr := mustParse(t, `{ status = error } | compare({ .x = 1 })`)
	mc, ok := expr.MetricsPipeline.(*MetricsCompare)
	if !ok {
		t.Fatalf("MetricsPipeline = %T; want *MetricsCompare", expr.MetricsPipeline)
	}
	if mc.TopN() != 10 {
		t.Errorf("TopN = %d; want 10 (default)", mc.TopN())
	}
	if mc.Filter() == nil {
		t.Errorf("Filter = nil; want a *SpansetFilter")
	}

	expr = mustParse(t, `{ } | compare({ .x = 1 }, 5, 100, 200)`)
	mc = expr.MetricsPipeline.(*MetricsCompare)
	if mc.TopN() != 5 || mc.Start() != 100 || mc.End() != 200 {
		t.Errorf("compare args = (%d,%d,%d); want (5,100,200)", mc.TopN(), mc.Start(), mc.End())
	}
}

// TestParseMetricsSecondStage pins topk/bottomk and bare metrics filters.
func TestParseMetricsSecondStage(t *testing.T) {
	t.Parallel()
	expr := mustParse(t, `{ } | rate() | topk(5)`)
	chain, ok := expr.MetricsSecondStage.(*ChainedSecondStage)
	if !ok {
		t.Fatalf("SecondStage = %T; want *ChainedSecondStage", expr.MetricsSecondStage)
	}
	if len(chain.Elements()) != 1 {
		t.Fatalf("chain elems = %d; want 1", len(chain.Elements()))
	}
	tk, ok := chain.Elements()[0].(*TopKBottomK)
	if !ok {
		t.Fatalf("elem = %T; want *TopKBottomK", chain.Elements()[0])
	}
	if tk.Op() != OpTopK || tk.Limit() != 5 {
		t.Errorf("topk = (%v,%d); want (OpTopK,5)", tk.Op(), tk.Limit())
	}

	expr = mustParse(t, `{ } | rate() > 0.5`)
	chain = expr.MetricsSecondStage.(*ChainedSecondStage)
	mf, ok := chain.Elements()[0].(*MetricsFilter)
	if !ok {
		t.Fatalf("elem = %T; want *MetricsFilter", chain.Elements()[0])
	}
	if mf.Op() != OpGreater || mf.Value() != 0.5 {
		t.Errorf("filter = (%v,%v); want (OpGreater,0.5)", mf.Op(), mf.Value())
	}
}

// TestParseScopedIntrinsics pins trace:/span:/event:/link:/instrumentation:.
func TestParseScopedIntrinsics(t *testing.T) {
	t.Parallel()
	cases := []struct {
		query string
		in    Intrinsic
	}{
		{`{ trace:id = "abc" }`, IntrinsicTraceID},
		{`{ trace:duration > 1s }`, IntrinsicTraceDuration},
		{`{ span:id = "abc" }`, IntrinsicSpanID},
		{`{ span:name = "x" }`, IntrinsicName},
		{`{ event:name = "x" }`, IntrinsicEventName},
		{`{ link:traceID = "abc" }`, IntrinsicLinkTraceID},
		{`{ instrumentation:version = "1" }`, IntrinsicInstrumentationVersion},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.query, func(t *testing.T) {
			t.Parallel()
			sf := firstElem(t, tc.query).(*SpansetFilter)
			bin := sf.Expression.(*BinaryOperation)
			attr := bin.LHS.(Attribute)
			if attr.Intrinsic != tc.in {
				t.Errorf("Intrinsic = %v; want %v", attr.Intrinsic, tc.in)
			}
		})
	}
}

// TestParseDurationUnits pins duration literal lexing.
func TestParseDurationUnits(t *testing.T) {
	t.Parallel()
	cases := []struct {
		query string
		want  time.Duration
	}{
		{`{ duration > 100ms }`, 100 * time.Millisecond},
		{`{ duration >= 1s }`, time.Second},
		{`{ duration < 30s }`, 30 * time.Second},
		{`{ duration = 1h }`, time.Hour},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.query, func(t *testing.T) {
			t.Parallel()
			sf := firstElem(t, tc.query).(*SpansetFilter)
			bin := sf.Expression.(*BinaryOperation)
			st := bin.RHS.(Static)
			d, ok := st.Duration()
			if !ok || d != tc.want {
				t.Errorf("duration = (%v,%v); want %v", d, ok, tc.want)
			}
		})
	}
}

// TestParseHints pins `with(...)` query hints.
func TestParseHints(t *testing.T) {
	t.Parallel()
	expr := mustParse(t, `{ } with(dedupe=true)`)
	if expr.Hints == nil || len(expr.Hints.Hints) != 1 {
		t.Fatalf("Hints = %#v; want one hint", expr.Hints)
	}
	if expr.Hints.Hints[0].Name != "dedupe" {
		t.Errorf("hint name = %q; want dedupe", expr.Hints.Hints[0].Name)
	}
}

// TestParseRejections pins that malformed queries return an error.
func TestParseRejections(t *testing.T) {
	t.Parallel()
	bad := []string{
		`{ name = }`,
		`{} > `,
		`{ .a = .b `,
		`{ + }`,
		`{ .a } | `,
		`{ .a } | count(`,
		`{ 2 <> 3 }`,
		`notAKeyword`,
	}
	for _, q := range bad {
		q := q
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			if _, err := Parse(q); err == nil {
				t.Errorf("Parse(%q): expected error, got nil", q)
			}
		})
	}
}

// TestParseIdentifier pins the single-attribute parse entry point.
func TestParseIdentifier(t *testing.T) {
	t.Parallel()
	a, err := ParseIdentifier(".service.name")
	if err != nil {
		t.Fatalf("ParseIdentifier: %v", err)
	}
	if a.Name != "service.name" || a.Scope != AttributeScopeNone {
		t.Errorf("attr = %#v; want name=service.name scope=none", a)
	}

	a, err = ParseIdentifier("duration")
	if err != nil {
		t.Fatalf("ParseIdentifier(duration): %v", err)
	}
	if a.Intrinsic != IntrinsicDuration {
		t.Errorf("intrinsic = %v; want duration", a.Intrinsic)
	}

	if _, err := ParseIdentifier("name = nil"); err == nil {
		t.Errorf("ParseIdentifier(non-attribute): expected error")
	}
}
