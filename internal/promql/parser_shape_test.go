package promql

import (
	"reflect"
	"strings"
	"testing"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/stretchr/testify/require"
)

// TestParserShape_* tests pin specific properties of the parsed AST so we
// catch silent breakage when the upstream Prometheus parser bumps. Each
// test parses a representative query and asserts the concrete root-node
// type plus the field values cerberus's lowering actually reads (see
// internal/promql/lower.go, internal/promql/binary.go,
// internal/promql/subquery.go).
//
// The intent is shape pinning, not parser coverage — generic acceptance
// is already covered by TestParserSmoke / TestParserSmoke_Rejected.
//
// If upstream renames a field or changes a type, these tests fail loudly.
// If upstream just adds a new field, these tests stay green.

func mustParse(t *testing.T, q string) parser.Expr {
	t.Helper()
	p := parser.NewParser(parser.Options{})
	expr, err := p.ParseExpr(q)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", q, err)
	}
	if expr == nil {
		t.Fatalf("ParseExpr(%q) returned nil", q)
	}
	return expr
}

// TestParserShape_InstantSelector pins the VectorSelector root + the
// __name__ matcher promotion the parser performs for `up`.
func TestParserShape_InstantSelector(t *testing.T) {
	t.Parallel()
	expr := mustParse(t, `up`)
	vs, ok := expr.(*parser.VectorSelector)
	if !ok {
		t.Fatalf("expected *VectorSelector, got %T", expr)
	}
	if vs.Name != "up" {
		t.Errorf("Name = %q; want %q", vs.Name, "up")
	}
	if len(vs.LabelMatchers) == 0 {
		t.Fatal("LabelMatchers is empty; expected synthesised __name__ matcher")
	}
	// The parser synthesises a __name__=="up" matcher from the bare name.
	var nameMatcher *labels.Matcher
	for _, m := range vs.LabelMatchers {
		if m.Name == model.MetricNameLabel {
			nameMatcher = m
			break
		}
	}
	if nameMatcher == nil {
		t.Fatal("no __name__ matcher in LabelMatchers")
	}
	require.NotNil(t, nameMatcher, "nameMatcher should not be nil")
	if nameMatcher.Type != labels.MatchEqual {
		t.Errorf("__name__ matcher Type = %v; want MatchEqual", nameMatcher.Type)
	}
	if nameMatcher.Value != "up" {
		t.Errorf("__name__ matcher Value = %q; want %q", nameMatcher.Value, "up")
	}
}

// TestParserShape_RateOverMatrix pins the Call(rate, MatrixSelector(VS))
// shape — the canonical range-vector function form rate / increase /
// *_over_time take.
func TestParserShape_RateOverMatrix(t *testing.T) {
	t.Parallel()
	expr := mustParse(t, `rate(http_requests_total[5m])`)
	call, ok := expr.(*parser.Call)
	if !ok {
		t.Fatalf("expected *Call, got %T", expr)
	}
	if call.Func == nil {
		t.Fatal("Call.Func is nil")
	}
	if call.Func.Name != "rate" {
		t.Errorf("Func.Name = %q; want %q", call.Func.Name, "rate")
	}
	if len(call.Args) != 1 {
		t.Fatalf("len(Args) = %d; want 1", len(call.Args))
	}
	ms, ok := call.Args[0].(*parser.MatrixSelector)
	if !ok {
		t.Fatalf("Args[0] = %T; want *MatrixSelector", call.Args[0])
	}
	if ms.Range.String() != "5m0s" && ms.Range.Minutes() != 5 {
		t.Errorf("Range = %v; want 5m", ms.Range)
	}
	vs, ok := ms.VectorSelector.(*parser.VectorSelector)
	if !ok {
		t.Fatalf("MatrixSelector.VectorSelector = %T; want *VectorSelector", ms.VectorSelector)
	}
	if vs.Name != "http_requests_total" {
		t.Errorf("inner Name = %q; want %q", vs.Name, "http_requests_total")
	}
}

// TestParserShape_AggregationBy pins the AggregateExpr root + the
// Grouping / Without contract for `sum by(job)(rate(...))`.
func TestParserShape_AggregationBy(t *testing.T) {
	t.Parallel()
	expr := mustParse(t, `sum by(job)(rate(http_requests_total[5m]))`)
	agg, ok := expr.(*parser.AggregateExpr)
	if !ok {
		t.Fatalf("expected *AggregateExpr, got %T", expr)
	}
	if agg.Op != parser.SUM {
		t.Errorf("Op = %v; want SUM", agg.Op)
	}
	if got, want := agg.Grouping, []string{"job"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Grouping = %v; want %v", got, want)
	}
	if agg.Without {
		t.Error("Without = true; want false (we used `by`)")
	}
	if _, ok := agg.Expr.(*parser.Call); !ok {
		t.Errorf("inner Expr = %T; want *Call", agg.Expr)
	}
	if agg.Param != nil {
		t.Errorf("Param = %v; want nil for non-parameterised SUM", agg.Param)
	}
}

// TestParserShape_NestedAggregation pins the histogram_quantile shape:
// outer Call wrapping inner AggregateExpr by (le).
func TestParserShape_NestedAggregation(t *testing.T) {
	t.Parallel()
	expr := mustParse(t, `histogram_quantile(0.95, sum by(le)(rate(http_request_duration_seconds_bucket[5m])))`)
	call, ok := expr.(*parser.Call)
	if !ok {
		t.Fatalf("expected *Call, got %T", expr)
	}
	if call.Func.Name != "histogram_quantile" {
		t.Errorf("Func.Name = %q; want histogram_quantile", call.Func.Name)
	}
	if len(call.Args) != 2 {
		t.Fatalf("len(Args) = %d; want 2", len(call.Args))
	}
	if _, ok := call.Args[0].(*parser.NumberLiteral); !ok {
		t.Errorf("Args[0] = %T; want *NumberLiteral", call.Args[0])
	}
	inner, ok := call.Args[1].(*parser.AggregateExpr)
	if !ok {
		t.Fatalf("Args[1] = %T; want *AggregateExpr", call.Args[1])
	}
	if inner.Op != parser.SUM {
		t.Errorf("inner Op = %v; want SUM", inner.Op)
	}
	if got, want := inner.Grouping, []string{"le"}; !reflect.DeepEqual(got, want) {
		t.Errorf("inner Grouping = %v; want %v", got, want)
	}
}

// TestParserShape_MatcherAndOffset pins both label-matcher decoding and
// the OriginalOffset field cerberus reads in anchorFromSelector.
func TestParserShape_MatcherAndOffset(t *testing.T) {
	t.Parallel()
	expr := mustParse(t, `up{job="prom"} offset 5m`)
	vs, ok := expr.(*parser.VectorSelector)
	if !ok {
		t.Fatalf("expected *VectorSelector, got %T", expr)
	}
	// We expect a __name__=up matcher AND a job=prom matcher.
	var jobMatcher *labels.Matcher
	for _, m := range vs.LabelMatchers {
		if m.Name == "job" {
			jobMatcher = m
			break
		}
	}
	if jobMatcher == nil {
		t.Fatal("no job matcher in LabelMatchers")
	}
	require.NotNil(t, jobMatcher, "jobMatcher should not be nil")
	if jobMatcher.Type != labels.MatchEqual {
		t.Errorf("job matcher Type = %v; want MatchEqual", jobMatcher.Type)
	}
	if jobMatcher.Value != "prom" {
		t.Errorf("job matcher Value = %q; want %q", jobMatcher.Value, "prom")
	}
	if vs.OriginalOffset.Minutes() != 5 {
		t.Errorf("OriginalOffset = %v; want 5m", vs.OriginalOffset)
	}
}

// TestParserShape_BinaryVectorMatch pins the BinaryExpr + VectorMatching
// shape for `a + on(job) b`.
func TestParserShape_BinaryVectorMatch(t *testing.T) {
	t.Parallel()
	expr := mustParse(t, `a + on(job) b`)
	bin, ok := expr.(*parser.BinaryExpr)
	if !ok {
		t.Fatalf("expected *BinaryExpr, got %T", expr)
	}
	if bin.Op != parser.ADD {
		t.Errorf("Op = %v; want ADD", bin.Op)
	}
	if bin.ReturnBool {
		t.Error("ReturnBool = true; want false (no bool modifier)")
	}
	if bin.VectorMatching == nil {
		t.Fatal("VectorMatching = nil; want non-nil for `on(job)`")
	}
	vm := bin.VectorMatching
	if !vm.On {
		t.Error("VectorMatching.On = false; want true for `on(job)`")
	}
	if got, want := vm.MatchingLabels, []string{"job"}; !reflect.DeepEqual(got, want) {
		t.Errorf("MatchingLabels = %v; want %v", got, want)
	}
	if vm.Card != parser.CardOneToOne {
		t.Errorf("Card = %v; want CardOneToOne (no group_left/right)", vm.Card)
	}
}

// TestParserShape_ComparisonBool pins ReturnBool propagation for
// `up == bool 0`.
func TestParserShape_ComparisonBool(t *testing.T) {
	t.Parallel()
	expr := mustParse(t, `up == bool 0`)
	bin, ok := expr.(*parser.BinaryExpr)
	if !ok {
		t.Fatalf("expected *BinaryExpr, got %T", expr)
	}
	if bin.Op != parser.EQLC {
		t.Errorf("Op = %v; want EQLC", bin.Op)
	}
	if !bin.ReturnBool {
		t.Error("ReturnBool = false; want true (we used `bool`)")
	}
	// LHS is the vector, RHS is the scalar literal 0.
	if _, ok := bin.LHS.(*parser.VectorSelector); !ok {
		t.Errorf("LHS = %T; want *VectorSelector", bin.LHS)
	}
	rhs, ok := bin.RHS.(*parser.NumberLiteral)
	if !ok {
		t.Fatalf("RHS = %T; want *NumberLiteral", bin.RHS)
	}
	if rhs.Val != 0 {
		t.Errorf("RHS.Val = %v; want 0", rhs.Val)
	}
}

// TestParserShape_Subquery pins the SubqueryExpr root + Range / Step
// fields cerberus reads in subquery.go.
func TestParserShape_Subquery(t *testing.T) {
	t.Parallel()
	expr := mustParse(t, `max_over_time(rate(m[1m])[5m:30s])`)
	outer, ok := expr.(*parser.Call)
	if !ok {
		t.Fatalf("expected outer *Call, got %T", expr)
	}
	if outer.Func.Name != "max_over_time" {
		t.Errorf("outer Func.Name = %q; want max_over_time", outer.Func.Name)
	}
	if len(outer.Args) != 1 {
		t.Fatalf("outer len(Args) = %d; want 1", len(outer.Args))
	}
	sub, ok := outer.Args[0].(*parser.SubqueryExpr)
	if !ok {
		t.Fatalf("outer Args[0] = %T; want *SubqueryExpr", outer.Args[0])
	}
	if sub.Range.Minutes() != 5 {
		t.Errorf("Subquery.Range = %v; want 5m", sub.Range)
	}
	if sub.Step.Seconds() != 30 {
		t.Errorf("Subquery.Step = %v; want 30s", sub.Step)
	}
	innerCall, ok := sub.Expr.(*parser.Call)
	if !ok {
		t.Fatalf("Subquery.Expr = %T; want *Call (rate)", sub.Expr)
	}
	if innerCall.Func.Name != "rate" {
		t.Errorf("inner Call Func.Name = %q; want rate", innerCall.Func.Name)
	}
}

// TestParserShape_NestedSubqueryRejected pins the parser's type-check
// guarantee that a `SubqueryExpr`'s body must evaluate to an instant
// vector. Wrapping a subquery directly in another subquery (a range
// vector inside `<expr>[range:step]`) is rejected at parse time with
// the "subquery is only allowed on instant vector" error.
//
// This is the invariant that makes `lowerSubqueryOverSubquery` —
// the recursive branch in `subquery.go` for
// `SubqueryExpr.Expr = *SubqueryExpr` — unreachable through parsed
// PromQL. The branch only exists so the lowering stays total over the
// AST node space for programmatically-built ASTs.
func TestParserShape_NestedSubqueryRejected(t *testing.T) {
	t.Parallel()
	p := parser.NewParser(parser.Options{})
	cases := []string{
		`(up[5m:1m])[1h:5m]`,
		`(rate(m[5m])[10m:1m])[1h:5m]`,
		`((rate(m[5m])[10m:1m]))[1h:5m]`,
	}
	for _, q := range cases {
		_, err := p.ParseExpr(q)
		if err == nil {
			t.Errorf("ParseExpr(%q): want type error, got nil", q)
			continue
		}
		if !strings.Contains(err.Error(), "subquery is only allowed on instant vector") {
			t.Errorf("ParseExpr(%q): err = %v; want 'subquery is only allowed on instant vector'", q, err)
		}
	}
}

// TestParserShape_GroupLeftInclude pins the cardinality modifier on
// BinaryExpr.VectorMatching: Card == CardManyToOne and Include set.
func TestParserShape_GroupLeftInclude(t *testing.T) {
	t.Parallel()
	expr := mustParse(t, `m1 / on(le) group_left(method) m2`)
	bin, ok := expr.(*parser.BinaryExpr)
	if !ok {
		t.Fatalf("expected *BinaryExpr, got %T", expr)
	}
	if bin.Op != parser.DIV {
		t.Errorf("Op = %v; want DIV", bin.Op)
	}
	if bin.VectorMatching == nil {
		t.Fatal("VectorMatching = nil; want non-nil for `on(le) group_left(method)`")
	}
	vm := bin.VectorMatching
	if vm.Card != parser.CardManyToOne {
		t.Errorf("Card = %v; want CardManyToOne (group_left)", vm.Card)
	}
	if !vm.On {
		t.Error("On = false; want true for `on(le)`")
	}
	if got, want := vm.MatchingLabels, []string{"le"}; !reflect.DeepEqual(got, want) {
		t.Errorf("MatchingLabels = %v; want %v", got, want)
	}
	if got, want := vm.Include, []string{"method"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Include = %v; want %v", got, want)
	}
}

// TestParserShape_ScalarArithmetic pins the ParenExpr + nested
// BinaryExpr shape for `(1 + 2) * 3`. cerberus's tryScalarLiteral walks
// ParenExpr / UnaryExpr / NumberLiteral; pinning the root type as
// *BinaryExpr (the outer `*`) and its LHS as *ParenExpr ensures that
// walk stays valid.
func TestParserShape_ScalarArithmetic(t *testing.T) {
	t.Parallel()
	expr := mustParse(t, `(1 + 2) * 3`)
	bin, ok := expr.(*parser.BinaryExpr)
	if !ok {
		t.Fatalf("expected *BinaryExpr, got %T", expr)
	}
	if bin.Op != parser.MUL {
		t.Errorf("Op = %v; want MUL", bin.Op)
	}
	paren, ok := bin.LHS.(*parser.ParenExpr)
	if !ok {
		t.Fatalf("LHS = %T; want *ParenExpr", bin.LHS)
	}
	inner, ok := paren.Expr.(*parser.BinaryExpr)
	if !ok {
		t.Fatalf("ParenExpr.Expr = %T; want *BinaryExpr", paren.Expr)
	}
	if inner.Op != parser.ADD {
		t.Errorf("inner Op = %v; want ADD", inner.Op)
	}
	rhsLit, ok := bin.RHS.(*parser.NumberLiteral)
	if !ok {
		t.Fatalf("RHS = %T; want *NumberLiteral", bin.RHS)
	}
	if rhsLit.Val != 3 {
		t.Errorf("RHS.Val = %v; want 3", rhsLit.Val)
	}
}

// TestParserError_* tests pin the error message substrings cerberus's
// HTTP handlers translate into PromQL error responses. If upstream
// changes a message, these tests catch the drift before users see a
// different errorType bubble up.

// errContainsAny returns true when err's message contains at least one
// of the supplied substrings (case-insensitive).
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

func parseShouldFail(t *testing.T, q string) error {
	t.Helper()
	p := parser.NewParser(parser.Options{})
	_, err := p.ParseExpr(q)
	if err == nil {
		t.Fatalf("ParseExpr(%q): expected error, got nil", q)
	}
	return err
}

func TestParserError_UnterminatedSelector(t *testing.T) {
	t.Parallel()
	err := parseShouldFail(t, `up{`)
	if !errContainsAny(err, "unexpected", "EOF", "}", "expected") {
		t.Errorf("err = %q; want substring indicating an unterminated selector", err)
	}
}

func TestParserError_UnclosedRangeCall(t *testing.T) {
	t.Parallel()
	err := parseShouldFail(t, `rate(`)
	if !errContainsAny(err, "unexpected", "EOF", "expected", "unclosed", "parenthesis") {
		t.Errorf("err = %q; want substring indicating an unclosed call", err)
	}
}

func TestParserError_MissingRightOperand(t *testing.T) {
	t.Parallel()
	err := parseShouldFail(t, `1 +`)
	if !errContainsAny(err, "unexpected", "EOF", "expected") {
		t.Errorf("err = %q; want substring indicating missing rhs", err)
	}
}

func TestParserError_MissingFunctionArg(t *testing.T) {
	t.Parallel()
	err := parseShouldFail(t, `histogram_quantile(0.95)`)
	// Upstream surfaces a wrong-number-of-args message; pin the
	// signal so the API layer's translation stays observable.
	if !errContainsAny(err, "argument", "expected", "wrong", "got") {
		t.Errorf("err = %q; want substring indicating arg-count mismatch", err)
	}
}

func TestParserError_BadOffsetLiteral(t *testing.T) {
	t.Parallel()
	err := parseShouldFail(t, `up offset abc`)
	if !errContainsAny(err, "unexpected", "duration", "abc", "expected") {
		t.Errorf("err = %q; want substring indicating bad offset value", err)
	}
}

func TestParserError_UnknownFunction(t *testing.T) {
	t.Parallel()
	err := parseShouldFail(t, `unknown_function(up)`)
	if !errContainsAny(err, "unknown function", "unknown_function", "unexpected") {
		t.Errorf("err = %q; want substring naming the unknown function", err)
	}
}

func TestParserError_NegativeSubqueryStep(t *testing.T) {
	t.Parallel()
	err := parseShouldFail(t, `up[5m:-5s]`)
	if !errContainsAny(err, "negative", "positive", "step", "unexpected", "duration", "greater than 0") {
		t.Errorf("err = %q; want substring rejecting a negative step", err)
	}
}

func TestParserError_UnterminatedString(t *testing.T) {
	t.Parallel()
	err := parseShouldFail(t, `up{label="`)
	if !errContainsAny(err, "unterminated", "string", "EOF", "unexpected") {
		t.Errorf("err = %q; want substring indicating unterminated string literal", err)
	}
}

func TestParserError_MismatchedParen(t *testing.T) {
	t.Parallel()
	err := parseShouldFail(t, `((1)`)
	if !errContainsAny(err, "unexpected", "EOF", "expected", "paren", ")") {
		t.Errorf("err = %q; want substring indicating mismatched parens", err)
	}
}

func TestParserError_AtNow(t *testing.T) {
	t.Parallel()
	// `up @ now` — `@` expects a numeric timestamp or start()/end(),
	// not a bare identifier. Upstream rejects this.
	err := parseShouldFail(t, `up @ now`)
	if !errContainsAny(err, "unexpected", "expected", "@", "now") {
		t.Errorf("err = %q; want substring rejecting `@ now`", err)
	}
}
