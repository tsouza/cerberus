//go:build agpl_oracle

// Package agpl_oracle holds the differential A/B check that pins the in-house
// clean-room TraceQL parser (internal/traceql/ast) against the AGPL reference
// parser (grafana/tempo/pkg/traceql). It is guarded by the `agpl_oracle` build
// tag so the reference parser is NEVER linked into cmd/cerberus or any normal
// `go test` run — it exists purely as a fidelity oracle you opt into with
//
//	go test -tags agpl_oracle ./test/agpl_oracle/...
//
// Both parsers are run over the same corpus, each AST is serialized to one
// neutral S-expression, and the two strings are asserted equal. The neutral
// form deliberately casts every enum (Operator, StaticType, AttributeScope,
// Intrinsic, …) to its integer iota value rather than calling String(): the
// two packages have independently authored printers whose formatting differs
// (pipe spacing, `1m` vs `1m0s`, parenthesization), so String() equality would
// conflate printer cosmetics with AST structure. Comparing the mirrored iota
// values instead checks structure — and incidentally validates the Phase-1
// claim that the enum orderings match.
package agpl_oracle

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	tempo "github.com/grafana/tempo/pkg/traceql"

	ast "github.com/tsouza/cerberus/internal/traceql/ast"
)

// corpus is kept within the equivalence envelope the two parsers share. It
// excludes the two transforms the in-house parser intentionally defers
// (general two-static binary constant folding such as `{ 1 + 1 = 2 }`, and
// parenthesized top-level scalar pipelines) — those are documented gaps, not
// fidelity bugs, and would diverge by design.
var corpus = []string{
	// Spanset filters: attribute matchers, scopes, intrinsics.
	`{ resource.service.name = "frontend" }`,
	`{ .http.status_code = 200 }`,
	`{ span.http.method != "GET" }`,
	`{ name =~ "foo.*" }`,
	`{ name !~ "bar.*" }`,
	`{ duration > 100ms }`,
	`{ duration >= 1s && duration < 2s }`,
	`{ status = error }`,
	`{ status = ok }`,
	`{ kind = server }`,
	`{ kind = client }`,
	`{ true }`,
	`{ }`,
	`{ .a = -100 }`,
	`{ .x > 1 && .y < 2 }`,
	`{ .x = 1 || .y = 2 }`,
	`{ parent.span.http.method = "GET" }`,
	`{ parent.resource.service.name = "x" }`,
	`{ span:duration > 1s }`,
	`{ trace:duration > 1s }`,
	`{ span:name = "GET" }`,
	`{ .ok = true }`,
	`{ .ratio = 1.5 }`,

	// Array-fold rewrites (or_to_in family).
	`{ .x = "a" || .x = "b" }`,
	`{ .x = "a" || .x = "b" || .x = "c" }`,
	`{ .n = 1 || .n = 2 }`,
	`{ .a != "x" && .a != "y" }`,
	`{ name =~ "a.*" || name =~ "b.*" }`,
	`{ name !~ "a.*" && name !~ "b.*" }`,

	// Pipelines.
	`{ .a = 1 } | count() > 2`,
	`{ .a = 1 } | avg(duration) > 1s`,
	`{ .a = 1 } | by(.b)`,
	`{ .a = 1 } | select(.b, .c)`,
	`{ .a = 1 } | coalesce()`,
	`{ .a = 1 } | max(duration) > 500ms`,
	`{ .a = 1 } | min(duration) < 1s`,
	`{ .a = 1 } | sum(duration) > 1s`,

	// Structural / set spanset operations.
	`{ .a = 1 } >> { .b = 2 }`,
	`{ .a = 1 } << { .b = 2 }`,
	`{ .a = 1 } > { .b = 2 }`,
	`{ .a = 1 } && { .b = 2 }`,
	`{ .a = 1 } || { .b = 2 }`,
	`{ .a = 1 } ~ { .b = 2 }`,
	`{ .a = 1 } !> { .b = 2 }`,

	// Metrics first stage.
	`{ .a = 1 } | rate()`,
	`{ .a = 1 } | count_over_time()`,
	`{ .a = 1 } | rate() by (resource.service.name)`,
	`{ .a = 1 } | sum_over_time(duration)`,
	`{ .a = 1 } | quantile_over_time(duration, 0.9, 0.99)`,
	`{ .a = 1 } | histogram_over_time(duration)`,
	`{ .a = 1 } | avg_over_time(duration)`,
	`{ .a = 1 } | min_over_time(duration) by (.b)`,

	// Metrics second stage.
	`{ .a = 1 } | rate() | topk(5)`,
	`{ .a = 1 } | rate() | bottomk(3)`,
}

func TestParserDifferential_AB(t *testing.T) {
	for _, q := range corpus {
		q := q
		t.Run(q, func(t *testing.T) {
			ref, refErr := tempo.Parse(q)
			got, gotErr := ast.Parse(q)

			if (refErr == nil) != (gotErr == nil) {
				t.Fatalf("accept/reject divergence: tempo err=%v, in-house err=%v", refErr, gotErr)
			}
			if refErr != nil {
				return // both rejected — agreement is enough.
			}

			want := tqRoot(ref)
			have := astRoot(got)
			if want != have {
				t.Fatalf("structural divergence for %q\n tempo: %s\n  ours: %s", q, want, have)
			}
		})
	}
}

// ---- in-house (ast) serializer ----

func astRoot(r *ast.RootExpr) string {
	var b strings.Builder
	b.WriteString("(root ")
	b.WriteString(astPipeline(r.Pipeline))
	if r.MetricsPipeline != nil {
		b.WriteString(" ")
		b.WriteString(astFirstStage(r.MetricsPipeline))
	}
	if r.MetricsSecondStage != nil {
		b.WriteString(" ")
		b.WriteString(astSecondStage(r.MetricsSecondStage))
	}
	b.WriteString(")")
	return b.String()
}

func astPipeline(p ast.Pipeline) string {
	parts := make([]string, len(p.Elements))
	for i, e := range p.Elements {
		parts[i] = astElement(e)
	}
	return "(pipe " + strings.Join(parts, " ") + ")"
}

func astElement(e ast.PipelineElement) string {
	switch v := e.(type) {
	case *ast.SpansetFilter:
		return "(filter " + astField(v.Expression) + ")"
	case ast.SpansetFilter:
		return "(filter " + astField(v.Expression) + ")"
	case ast.SpansetOperation:
		return fmt.Sprintf("(sop %d %s %s)", int(v.Op), astSpanset(v.LHS), astSpanset(v.RHS))
	case ast.ScalarFilter:
		return fmt.Sprintf("(scf %d %s %s)", int(v.Op), astScalar(v.LHS), astScalar(v.RHS))
	case ast.GroupOperation:
		return "(by " + astField(v.Expression) + ")"
	case ast.CoalesceOperation:
		return "(coalesce)"
	case ast.SelectOperation:
		return "(select " + astAttrs(v.Attrs()) + ")"
	case ast.Aggregate:
		return astAggregate(v)
	case ast.Pipeline:
		return astPipeline(v)
	default:
		panic(fmt.Sprintf("ast: unhandled pipeline element %T", e))
	}
}

func astAggregate(a ast.Aggregate) string {
	if a.InnerExpr() == nil {
		return fmt.Sprintf("(agg %d)", int(a.Op()))
	}
	return fmt.Sprintf("(agg %d %s)", int(a.Op()), astField(a.InnerExpr()))
}

func astSpanset(se ast.SpansetExpression) string {
	switch v := se.(type) {
	case *ast.SpansetFilter:
		return "(filter " + astField(v.Expression) + ")"
	case ast.SpansetFilter:
		return "(filter " + astField(v.Expression) + ")"
	case ast.SpansetOperation:
		return fmt.Sprintf("(sop %d %s %s)", int(v.Op), astSpanset(v.LHS), astSpanset(v.RHS))
	case ast.ScalarFilter:
		return fmt.Sprintf("(scf %d %s %s)", int(v.Op), astScalar(v.LHS), astScalar(v.RHS))
	case ast.Pipeline:
		return astPipeline(v)
	default:
		panic(fmt.Sprintf("ast: unhandled spanset expr %T", se))
	}
}

func astScalar(se ast.ScalarExpression) string {
	switch v := se.(type) {
	case ast.Aggregate:
		return astAggregate(v)
	case ast.Static:
		return astStatic(v)
	case ast.Pipeline:
		return astPipeline(v)
	default:
		panic(fmt.Sprintf("ast: unhandled scalar expr %T", se))
	}
}

func astField(fe ast.FieldExpression) string {
	switch v := fe.(type) {
	case *ast.BinaryOperation:
		return fmt.Sprintf("(b %d %s %s)", int(v.Op), astField(v.LHS), astField(v.RHS))
	case *ast.UnaryOperation:
		return fmt.Sprintf("(u %d %s)", int(v.Op), astField(v.Expression))
	case ast.UnaryOperation:
		return fmt.Sprintf("(u %d %s)", int(v.Op), astField(v.Expression))
	case ast.Attribute:
		return astAttr(v)
	case ast.Static:
		return astStatic(v)
	default:
		panic(fmt.Sprintf("ast: unhandled field expr %T", fe))
	}
}

func astAttr(a ast.Attribute) string {
	return fmt.Sprintf("(attr %d %t %d %q)", int(a.Scope), a.Parent, int(a.Intrinsic), a.Name)
}

func astAttrs(as []ast.Attribute) string {
	parts := make([]string, len(as))
	for i, a := range as {
		parts[i] = astAttr(a)
	}
	return strings.Join(parts, " ")
}

func astStatic(s ast.Static) string {
	return "(s " + strconv.Itoa(int(s.Type)) + ":" + astStaticValue(s) + ")"
}

func astStaticValue(s ast.Static) string {
	switch s.Type {
	case ast.TypeNil:
		return "nil"
	case ast.TypeInt:
		n, _ := s.Int()
		return strconv.Itoa(n)
	case ast.TypeFloat:
		return strconv.FormatFloat(s.Float(), 'g', -1, 64)
	case ast.TypeString:
		return strconv.Quote(s.EncodeToString(false))
	case ast.TypeBoolean:
		b, _ := s.Bool()
		return strconv.FormatBool(b)
	case ast.TypeDuration:
		d, _ := s.Duration()
		return strconv.FormatInt(int64(d), 10)
	case ast.TypeStatus:
		st, _ := s.Status()
		return strconv.Itoa(int(st))
	case ast.TypeKind:
		k, _ := s.Kind()
		return strconv.Itoa(int(k))
	case ast.TypeIntArray, ast.TypeFloatArray, ast.TypeStringArray, ast.TypeBooleanArray:
		var parts []string
		for _, el := range s.Elements() {
			parts = append(parts, astStaticValue(el))
		}
		return "[" + strings.Join(parts, ",") + "]"
	default:
		panic(fmt.Sprintf("ast: unhandled static type %d", int(s.Type)))
	}
}

func astFirstStage(fs ast.FirstStageElement) string {
	switch v := fs.(type) {
	case *ast.MetricsAggregate:
		return fmt.Sprintf("(magg %d %s by[%s] q[%s])",
			int(v.Op()), astAttr(v.Attribute()), astAttrs(v.GroupBy()), floatsStr(v.Quantiles()))
	case *ast.AverageOverTimeAggregator:
		return fmt.Sprintf("(avgot %s by[%s])", astAttr(v.Attribute()), astAttrs(v.GroupBy()))
	case *ast.MetricsCompare:
		return fmt.Sprintf("(compare %s %d %d %d)",
			astElement(*v.Filter()), v.TopN(), v.Start(), v.End())
	default:
		panic(fmt.Sprintf("ast: unhandled first stage %T", fs))
	}
}

func astSecondStage(ss ast.SecondStageElement) string {
	switch v := ss.(type) {
	case *ast.TopKBottomK:
		return fmt.Sprintf("(tkbk %d %d)", int(v.Op()), v.Limit())
	case *ast.MetricsFilter:
		return fmt.Sprintf("(mfilter %d %s)", int(v.Op()), strconv.FormatFloat(v.Value(), 'g', -1, 64))
	case ast.ChainedSecondStage:
		return astChain(v.Elements())
	case *ast.ChainedSecondStage:
		return astChain(v.Elements())
	default:
		panic(fmt.Sprintf("ast: unhandled second stage %T", ss))
	}
}

func astChain(elems []ast.SecondStageElement) string {
	parts := make([]string, len(elems))
	for i, e := range elems {
		parts[i] = astSecondStage(e)
	}
	return "(chain " + strings.Join(parts, " ") + ")"
}

// ---- reference (tempo) serializer — structurally identical to the above ----

func tqRoot(r *tempo.RootExpr) string {
	var b strings.Builder
	b.WriteString("(root ")
	b.WriteString(tqPipeline(r.Pipeline))
	if r.MetricsPipeline != nil {
		b.WriteString(" ")
		b.WriteString(tqFirstStage(r.MetricsPipeline))
	}
	if r.MetricsSecondStage != nil {
		b.WriteString(" ")
		b.WriteString(tqSecondStage(r.MetricsSecondStage))
	}
	b.WriteString(")")
	return b.String()
}

func tqPipeline(p tempo.Pipeline) string {
	parts := make([]string, len(p.Elements))
	for i, e := range p.Elements {
		parts[i] = tqElement(e)
	}
	return "(pipe " + strings.Join(parts, " ") + ")"
}

func tqElement(e tempo.Element) string {
	switch v := e.(type) {
	case *tempo.SpansetFilter:
		return "(filter " + tqField(v.Expression) + ")"
	case tempo.SpansetFilter:
		return "(filter " + tqField(v.Expression) + ")"
	case tempo.SpansetOperation:
		return fmt.Sprintf("(sop %d %s %s)", int(v.Op), tqSpanset(v.LHS), tqSpanset(v.RHS))
	case tempo.ScalarFilter:
		return fmt.Sprintf("(scf %d %s %s)", int(v.Op), tqScalar(v.LHS), tqScalar(v.RHS))
	case tempo.GroupOperation:
		return "(by " + tqField(v.Expression) + ")"
	case tempo.CoalesceOperation:
		return "(coalesce)"
	case tempo.SelectOperation:
		return "(select " + tqAttrs(v.Attrs()) + ")"
	case tempo.Aggregate:
		return tqAggregate(v)
	case tempo.Pipeline:
		return tqPipeline(v)
	default:
		panic(fmt.Sprintf("tempo: unhandled pipeline element %T", e))
	}
}

func tqAggregate(a tempo.Aggregate) string {
	if a.InnerExpr() == nil {
		return fmt.Sprintf("(agg %d)", int(a.Op()))
	}
	return fmt.Sprintf("(agg %d %s)", int(a.Op()), tqField(a.InnerExpr()))
}

func tqSpanset(se tempo.SpansetExpression) string {
	switch v := se.(type) {
	case *tempo.SpansetFilter:
		return "(filter " + tqField(v.Expression) + ")"
	case tempo.SpansetOperation:
		return fmt.Sprintf("(sop %d %s %s)", int(v.Op), tqSpanset(v.LHS), tqSpanset(v.RHS))
	case tempo.ScalarFilter:
		return fmt.Sprintf("(scf %d %s %s)", int(v.Op), tqScalar(v.LHS), tqScalar(v.RHS))
	case tempo.Pipeline:
		return tqPipeline(v)
	default:
		panic(fmt.Sprintf("tempo: unhandled spanset expr %T", se))
	}
}

func tqScalar(se tempo.ScalarExpression) string {
	switch v := se.(type) {
	case tempo.Aggregate:
		return tqAggregate(v)
	case tempo.Static:
		return tqStatic(v)
	case tempo.Pipeline:
		return tqPipeline(v)
	default:
		panic(fmt.Sprintf("tempo: unhandled scalar expr %T", se))
	}
}

func tqField(fe tempo.FieldExpression) string {
	switch v := fe.(type) {
	case *tempo.BinaryOperation:
		return fmt.Sprintf("(b %d %s %s)", int(v.Op), tqField(v.LHS), tqField(v.RHS))
	case *tempo.UnaryOperation:
		return fmt.Sprintf("(u %d %s)", int(v.Op), tqField(v.Expression))
	case tempo.UnaryOperation:
		return fmt.Sprintf("(u %d %s)", int(v.Op), tqField(v.Expression))
	case tempo.Attribute:
		return tqAttr(v)
	case tempo.Static:
		return tqStatic(v)
	default:
		panic(fmt.Sprintf("tempo: unhandled field expr %T", fe))
	}
}

func tqAttr(a tempo.Attribute) string {
	return fmt.Sprintf("(attr %d %t %d %q)", int(a.Scope), a.Parent, int(a.Intrinsic), a.Name)
}

func tqAttrs(as []tempo.Attribute) string {
	parts := make([]string, len(as))
	for i, a := range as {
		parts[i] = tqAttr(a)
	}
	return strings.Join(parts, " ")
}

func tqStatic(s tempo.Static) string {
	return "(s " + strconv.Itoa(int(s.Type)) + ":" + tqStaticValue(s) + ")"
}

func tqStaticValue(s tempo.Static) string {
	switch s.Type {
	case tempo.TypeNil:
		return "nil"
	case tempo.TypeInt:
		n, _ := s.Int()
		return strconv.Itoa(n)
	case tempo.TypeFloat:
		return strconv.FormatFloat(s.Float(), 'g', -1, 64)
	case tempo.TypeString:
		return strconv.Quote(s.EncodeToString(false))
	case tempo.TypeBoolean:
		b, _ := s.Bool()
		return strconv.FormatBool(b)
	case tempo.TypeDuration:
		d, _ := s.Duration()
		return strconv.FormatInt(int64(d), 10)
	case tempo.TypeStatus:
		st, _ := s.Status()
		return strconv.Itoa(int(st))
	case tempo.TypeKind:
		k, _ := s.Kind()
		return strconv.Itoa(int(k))
	case tempo.TypeIntArray, tempo.TypeFloatArray, tempo.TypeStringArray, tempo.TypeBooleanArray:
		var parts []string
		for _, el := range s.Elements() {
			parts = append(parts, tqStaticValue(el))
		}
		return "[" + strings.Join(parts, ",") + "]"
	default:
		panic(fmt.Sprintf("tempo: unhandled static type %d", int(s.Type)))
	}
}

func tqFirstStage(fs tempo.FirstStageElement) string {
	switch v := fs.(type) {
	case *tempo.MetricsAggregate:
		return fmt.Sprintf("(magg %d %s by[%s] q[%s])",
			int(v.Op()), tqAttr(v.Attribute()), tqAttrs(v.GroupBy()), floatsStr(v.Quantiles()))
	case *tempo.AverageOverTimeAggregator:
		return fmt.Sprintf("(avgot %s by[%s])", tqAttr(v.Attribute()), tqAttrs(v.GroupBy()))
	case *tempo.MetricsCompare:
		return fmt.Sprintf("(compare %s %d %d %d)",
			tqElement(*v.Filter()), v.TopN(), v.Start(), v.End())
	default:
		panic(fmt.Sprintf("tempo: unhandled first stage %T", fs))
	}
}

func tqSecondStage(ss tempo.SecondStageElement) string {
	switch v := ss.(type) {
	case *tempo.TopKBottomK:
		return fmt.Sprintf("(tkbk %d %d)", int(v.Op()), v.Limit())
	case *tempo.MetricsFilter:
		return fmt.Sprintf("(mfilter %d %s)", int(v.Op()), strconv.FormatFloat(v.Value(), 'g', -1, 64))
	case tempo.ChainedSecondStage:
		return tqChain(v.Elements())
	case *tempo.ChainedSecondStage:
		return tqChain(v.Elements())
	default:
		panic(fmt.Sprintf("tempo: unhandled second stage %T", ss))
	}
}

func tqChain(elems []tempo.SecondStageElement) string {
	parts := make([]string, len(elems))
	for i, e := range elems {
		parts[i] = tqSecondStage(e)
	}
	return "(chain " + strings.Join(parts, " ") + ")"
}

func floatsStr(fs []float64) string {
	parts := make([]string, len(fs))
	for i, f := range fs {
		parts[i] = strconv.FormatFloat(f, 'g', -1, 64)
	}
	return strings.Join(parts, ",")
}
