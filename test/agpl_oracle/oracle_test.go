//go:build agpl_oracle

// Package agpl_oracle holds the differential A/B check that pins the in-house
// clean-room TraceQL parser (internal/traceql/ast) against the AGPL reference
// parser (grafana/tempo/pkg/traceql). It is guarded by the `agpl_oracle` build
// tag so the reference parser is NEVER linked into cmd/cerberus or any normal
// `go test` run — it exists purely as a fidelity oracle you opt into with
//
//	go test -tags agpl_oracle ./test/agpl_oracle/...
//
// Two tiers run over the corpus:
//
//   - TestParserAcceptReject_AB asserts both parsers accept/reject every query
//     identically, across the full grammar (spanset filters, field
//     expressions, pipelines, and the metrics first/second stages).
//   - TestParserStructural_AB additionally asserts a neutral S-expression of the
//     parsed AST is byte-equal. Each AST is serialized over the *exported*
//     surface of upstream tempo only — every enum (Operator, StaticType,
//     AttributeScope, Intrinsic, …) is cast to its integer iota value rather
//     than calling String(), so the comparison checks structure, not the two
//     independently authored printers' cosmetics (pipe spacing, `IN` vs `in`,
//     `1m` vs `1m0s`, parenthesization). It incidentally validates that the two
//     packages' enum orderings match.
//
// Upstream tempo keeps the metrics/aggregate scalars (op, attr, quantiles,
// limit, …) unexported with no accessors, so the structural tier covers the
// spanset-filter / field-expression / scalar-filter / set-operation universe —
// where the parser's real ambiguity (operator precedence, or→IN folding, scope
// and intrinsic resolution, static typing) lives. The two nodes whose internals
// upstream hides yet appear in that universe — Aggregate and SelectOperation —
// are pinned via each parser's own local String(); the metrics first/second
// stages are exercised by the accept/reject tier only.
package agpl_oracle

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	tempo "github.com/grafana/tempo/pkg/traceql"

	ast "github.com/tsouza/cerberus/internal/traceql/ast"
)

// structuralCorpus is the subset both parsers expose structurally: spanset
// filters, field expressions, scalar filters, set/structural spanset
// operations, group/select/coalesce pipeline elements, and aggregates compared
// over scalars. It excludes the two transforms the in-house parser
// intentionally defers (general two-static binary constant folding such as
// `{ 1 + 1 = 2 }`, and parenthesized top-level scalar pipelines) — documented
// gaps, not fidelity bugs, that diverge by design.
var structuralCorpus = []string{
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

	// Pipelines (aggregates pinned via local String()).
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
}

// metricsCorpus exercises the metrics first/second stages, whose scalars
// upstream tempo keeps unexported. These run through the accept/reject tier
// only — there is no exported-surface structural view of them.
var metricsCorpus = []string{
	`{ .a = 1 } | rate()`,
	`{ .a = 1 } | count_over_time()`,
	`{ .a = 1 } | rate() by (resource.service.name)`,
	`{ .a = 1 } | sum_over_time(duration)`,
	`{ .a = 1 } | quantile_over_time(duration, 0.9, 0.99)`,
	`{ .a = 1 } | histogram_over_time(duration)`,
	`{ .a = 1 } | avg_over_time(duration)`,
	`{ .a = 1 } | min_over_time(duration) by (.b)`,
	`{ .a = 1 } | rate() | topk(5)`,
	`{ .a = 1 } | rate() | bottomk(3)`,
}

// TestParserAcceptReject_AB pins accept/reject parity across the full grammar.
func TestParserAcceptReject_AB(t *testing.T) {
	for _, q := range append(append([]string{}, structuralCorpus...), metricsCorpus...) {
		q := q
		t.Run(q, func(t *testing.T) {
			_, refErr := tempo.Parse(q)
			_, gotErr := ast.Parse(q)
			if (refErr == nil) != (gotErr == nil) {
				t.Fatalf("accept/reject divergence: tempo err=%v, in-house err=%v", refErr, gotErr)
			}
		})
	}
}

// TestParserStructural_AB pins structural equality over the exported-surface
// subset.
func TestParserStructural_AB(t *testing.T) {
	for _, q := range structuralCorpus {
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

// normStr canonicalizes the only cross-parser cosmetic divergence that reaches
// the structural tier: the in-house printer lowercases the `in` / `not in`
// set-membership tokens, upstream uppercases them. (Set-membership inside an
// Aggregate's inner field expression is the sole place a node compared via
// String() can carry one.)
func normStr(s string) string {
	s = strings.ReplaceAll(s, " NOT IN ", " not in ")
	s = strings.ReplaceAll(s, " IN ", " in ")
	return s
}

// ---- in-house (ast) serializer ----

func astRoot(r *ast.RootExpr) string {
	var b strings.Builder
	b.WriteString("(root ")
	b.WriteString(astPipeline(r.Pipeline))
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
		return "(select-str " + strconv.Quote(normStr(v.String())) + ")"
	case ast.Aggregate:
		return "(agg-str " + strconv.Quote(normStr(v.String())) + ")"
	case ast.Pipeline:
		return astPipeline(v)
	default:
		panic(fmt.Sprintf("ast: unhandled pipeline element %T", e))
	}
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
		return "(agg-str " + strconv.Quote(normStr(v.String())) + ")"
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

// ---- reference (tempo) serializer — structurally identical to the above,
// over upstream's exported surface only ----

func tqRoot(r *tempo.RootExpr) string {
	var b strings.Builder
	b.WriteString("(root ")
	b.WriteString(tqPipeline(r.Pipeline))
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
		return "(select-str " + strconv.Quote(normStr(v.String())) + ")"
	case tempo.Aggregate:
		return "(agg-str " + strconv.Quote(normStr(v.String())) + ")"
	case tempo.Pipeline:
		return tqPipeline(v)
	default:
		panic(fmt.Sprintf("tempo: unhandled pipeline element %T", e))
	}
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
		return "(agg-str " + strconv.Quote(normStr(v.String())) + ")"
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
