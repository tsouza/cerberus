package ast

import "testing"

// Mutation-coverage tests for parser.go: operator precedence and
// associativity, trace-ID operand normalization, and topk/bottomk dispatch.
// These pin parser *correctness*, so they break the precedence/associativity
// arithmetic and the branch conditions a mutation would flip.

// TestFieldExprPrecedenceAssociativity pins arithmetic precedence and
// associativity in a span filter. The rendered form parenthesises the tighter
// sub-expression, so a wrong `nextMin := prec + 1`, a flipped `k == tokPow`
// right-associativity branch, or a `prec < minPrec` boundary shift all change
// the bracketing.
func TestFieldExprPrecedenceAssociativity(t *testing.T) {
	t.Parallel()
	cases := []struct {
		query string
		want  string
	}{
		// `*` binds tighter than `+`.
		{`{ .a + .b * .c }`, "{ .a + (.b * .c) }"},
		// `-` is left associative.
		{`{ .a - .b - .c }`, "{ (.a - .b) - .c }"},
		// `/` left associative, tighter than `+`.
		{`{ .a + .b / .c + .d }`, "{ (.a + (.b / .c)) + .d }"},
		// `^` is right associative.
		{`{ .a ^ .b ^ .c }`, "{ .a ^ (.b ^ .c) }"},
		// `^` binds tighter than `*`.
		{`{ .a * .b ^ .c }`, "{ .a * (.b ^ .c) }"},
	}
	for _, c := range cases {
		if got := mustParse(t, c.query).String(); got != c.want {
			t.Errorf("Parse(%q).String() = %q; want %q", c.query, got, c.want)
		}
	}
}

// TestSpansetOperatorAssociativity pins that a chain of equal-precedence
// spanset operators is parsed left-associatively. `a >> b >> c` must nest as
// `(a >> b) >> c`: the left operand is itself a SpansetOperation and the right
// operand is a leaf filter. Mutating the recursion's `prec + 1` or the
// `prec == 0 || prec < minPrec` break guard re-associates it to the right.
func TestSpansetOperatorAssociativity(t *testing.T) {
	t.Parallel()
	elem := firstElem(t, `{ .x = 1 } >> { .y = 2 } >> { .z = 3 }`)
	top, ok := elem.(SpansetOperation)
	if !ok {
		t.Fatalf("element = %T; want SpansetOperation", elem)
	}
	if top.Op != OpSpansetDescendant {
		t.Fatalf("top.Op = %v; want OpSpansetDescendant", top.Op)
	}
	if _, ok := top.LHS.(SpansetOperation); !ok {
		t.Errorf("left associativity broken: top.LHS = %T; want SpansetOperation", top.LHS)
	}
	if _, ok := top.RHS.(SpansetOperation); ok {
		t.Errorf("left associativity broken: top.RHS = %T; want a leaf, not SpansetOperation", top.RHS)
	}
}

// findScalarFilter returns the ScalarFilter stage of a parsed query.
func findScalarFilter(t *testing.T, q string) ScalarFilter {
	t.Helper()
	expr := mustParse(t, q)
	for _, e := range expr.Pipeline.Elements {
		if sf, ok := e.(ScalarFilter); ok {
			return sf
		}
	}
	t.Fatalf("Parse(%q): no ScalarFilter stage; elements=%v", q, expr.Pipeline.Elements)
	return ScalarFilter{}
}

// TestScalarExprPrecedenceAssociativity pins precedence/associativity inside a
// scalar comparison's right-hand expression (parseScalarExpr). `^` is right
// associative and `*` binds tighter than `+`; the recursion arithmetic and the
// `prec == 0 || prec < minPrec` break guard control both.
func TestScalarExprPrecedenceAssociativity(t *testing.T) {
	t.Parallel()

	// `2 ^ 3 ^ 2` → right associative: RHS = 2 ^ (3 ^ 2).
	sf := findScalarFilter(t, `{} | count() > 2 ^ 3 ^ 2`)
	pow, ok := sf.RHS.(ScalarOperation)
	if !ok || pow.Op != OpPower {
		t.Fatalf("RHS = %T (op %v); want ScalarOperation ^", sf.RHS, pow.Op)
	}
	if _, ok := pow.RHS.(ScalarOperation); !ok {
		t.Errorf("`^` not right-associative: RHS.RHS = %T; want ScalarOperation", pow.RHS)
	}
	if _, ok := pow.LHS.(ScalarOperation); ok {
		t.Errorf("`^` not right-associative: RHS.LHS = %T; want a leaf static", pow.LHS)
	}

	// `1 + 2 * 3` → `*` tighter: RHS = 1 + (2 * 3).
	sf2 := findScalarFilter(t, `{} | count() > 1 + 2 * 3`)
	add, ok := sf2.RHS.(ScalarOperation)
	if !ok || add.Op != OpAdd {
		t.Fatalf("RHS = %T (op %v); want ScalarOperation +", sf2.RHS, add.Op)
	}
	mul, ok := add.RHS.(ScalarOperation)
	if !ok || mul.Op != OpMult {
		t.Errorf("`*` not tighter than `+`: add.RHS = %T (op %v); want ScalarOperation *", add.RHS, mul.Op)
	}
}

// binOf extracts the BinaryOperation expression of a single span-filter query.
func binOf(t *testing.T, q string) *BinaryOperation {
	t.Helper()
	sf, ok := firstElem(t, q).(*SpansetFilter)
	if !ok {
		t.Fatalf("Parse(%q): element is not *SpansetFilter", q)
	}
	bin, ok := sf.Expression.(*BinaryOperation)
	if !ok {
		t.Fatalf("Parse(%q): expression = %T; want *BinaryOperation", q, sf.Expression)
	}
	return bin
}

// TestTraceIDOperandNormalization pins that a string operand compared against
// the trace:id intrinsic has its leading zeros stripped, while an ordinary
// attribute's string operand is left untouched. This guards three coupled
// conditions: the `ok && a.Intrinsic == IntrinsicTraceID` gate on each side of
// the comparison and the `s.Type != TypeString` guard inside
// normalizeTraceIDOperand.
func TestTraceIDOperandNormalization(t *testing.T) {
	t.Parallel()

	// trace:id on the LHS — RHS string normalized.
	bin := binOf(t, `{ trace:id = "000abc" }`)
	rhs, ok := bin.RHS.(Static)
	if !ok || rhs.EncodeToString(false) != "abc" {
		t.Errorf("trace:id LHS: RHS = %v; want `abc` (leading zeros stripped)", bin.RHS)
	}

	// trace:id on the RHS — LHS string normalized.
	bin = binOf(t, `{ "000abc" = trace:id }`)
	lhs, ok := bin.LHS.(Static)
	if !ok || lhs.EncodeToString(false) != "abc" {
		t.Errorf("trace:id RHS: LHS = %v; want `abc`", bin.LHS)
	}

	// Ordinary attribute — operand must NOT be normalized, on either side.
	// These pin the `ok && a.Intrinsic == IntrinsicTraceID` gate: turning the
	// `&&` into `||` (the attribute-is-present clause alone) would normalize a
	// non-trace-id attribute's string operand.
	bin = binOf(t, `{ .x = "007" }`)
	rhs, ok = bin.RHS.(Static)
	if !ok || rhs.EncodeToString(false) != "007" {
		t.Errorf("ordinary attr LHS: RHS = %v; want `007` (unchanged)", bin.RHS)
	}
	bin = binOf(t, `{ "007" = .x }`)
	lhs, ok = bin.LHS.(Static)
	if !ok || lhs.EncodeToString(false) != "007" {
		t.Errorf("ordinary attr RHS: LHS = %v; want `007` (unchanged)", bin.LHS)
	}
}

// TestScalarSubtractionLeftAssociative pins that a chain of equal-precedence
// scalar operators (`-`) is left associative: `1 - 2 - 3` nests as
// `(1 - 2) - 3`. The recursion's `nextMin := prec + 1` arithmetic controls
// this; a `prec - 1` mutant re-associates it to the right.
func TestScalarSubtractionLeftAssociative(t *testing.T) {
	t.Parallel()
	sf := findScalarFilter(t, `{} | count() > 1 - 2 - 3`)
	sub, ok := sf.RHS.(ScalarOperation)
	if !ok || sub.Op != OpSub {
		t.Fatalf("RHS = %T (op %v); want ScalarOperation -", sf.RHS, sub.Op)
	}
	if _, ok := sub.LHS.(ScalarOperation); !ok {
		t.Errorf("`-` not left-associative: RHS.LHS = %T; want ScalarOperation", sub.LHS)
	}
	if _, ok := sub.RHS.(ScalarOperation); ok {
		t.Errorf("`-` not left-associative: RHS.RHS = %T; want a leaf static", sub.RHS)
	}
}

// TestTopKBottomKDispatch pins that bottomk is recognised as a second-stage
// keyword and dispatched to OpBottomK while topk maps to OpTopK. Negating the
// `k == tokBottomK` clause of isSecondStageKeyword makes the `| bottomk(N)`
// stage unparseable.
func TestTopKBottomKDispatch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		query string
		want  SecondStageOp
	}{
		{`{} | rate() | topk(3)`, OpTopK},
		{`{} | rate() | bottomk(5)`, OpBottomK},
	}
	for _, c := range cases {
		expr := mustParse(t, c.query)
		ts := findTopKBottomK(t, expr)
		if ts.Op() != c.want {
			t.Errorf("Parse(%q): second-stage op = %v; want %v", c.query, ts.Op(), c.want)
		}
	}
}

func findTopKBottomK(t *testing.T, expr *RootExpr) *TopKBottomK {
	t.Helper()
	var walk func(SecondStageElement) *TopKBottomK
	walk = func(e SecondStageElement) *TopKBottomK {
		switch v := e.(type) {
		case *TopKBottomK:
			return v
		case *ChainedSecondStage:
			for _, el := range v.Elements() {
				if r := walk(el); r != nil {
					return r
				}
			}
		}
		return nil
	}
	if expr.MetricsSecondStage != nil {
		if r := walk(expr.MetricsSecondStage); r != nil {
			return r
		}
	}
	t.Fatalf("no TopKBottomK found in %s", expr.String())
	return nil
}

// TestBottomKLimit pins the limit value carried by the second stage.
func TestBottomKLimit(t *testing.T) {
	t.Parallel()
	ts := findTopKBottomK(t, mustParse(t, `{} | rate() | bottomk(7)`))
	if ts.Limit() != 7 {
		t.Errorf("bottomk limit = %d; want 7", ts.Limit())
	}
}

// TestAdvanceClampsAtEOF pins the exact boundary of cursor.advance()'s
// end-of-input clamp: `if c.pos < len(c.p.toks)-1 { c.pos++ }`. The clamp keeps
// pos pinned at the final index (the trailing EOF token) once reached, so a
// call to advance() while already at EOF returns EOF and does not run off the
// slice.
//
// Each input below is a scoped-intrinsic scope colon (`trace:`, `span:`, …)
// that is the LAST token before EOF. parseScopedIntrinsic advances twice
// unconditionally (`scope := c.advance().kind; field := c.advance().kind`): the
// first advance consumes the colon and lands on EOF, the second advance is the
// one that fires the clamp. The scope+EOF pair is not a valid intrinsic, so the
// parser calls c.fail, which peek()s the current token to build the error.
//
// On the correct clamp, pos stays on EOF and c.fail surfaces a clean
// ParseError. If the boundary is loosened (`<`→`<=`, or `-1`→`+1`/dropped so the
// bound becomes len or len+1), the second advance pushes pos past the slice and
// the subsequent peek panics with an index-out-of-range that escapes Parse's
// parseErr-only recover — turning a clean error into a crash. Asserting a clean
// error here therefore kills the CONDITIONALS_BOUNDARY / ARITHMETIC_BASE /
// INVERT_NEGATIVES mutants on that clamp condition.
func TestAdvanceClampsAtEOF(t *testing.T) {
	t.Parallel()
	// Scope colons whose lexer token is emitted even with no field following.
	cases := []string{
		`{ trace:`,
		`{ span:`,
		`{ event:`,
		`{ link:`,
	}
	for _, q := range cases {
		expr, err := Parse(q)
		if err == nil {
			t.Errorf("Parse(%q) = %v, nil; want a clean parse error (advance() must clamp at EOF)", q, expr)
		}
		if expr != nil {
			t.Errorf("Parse(%q): expr = %v; want nil on error", q, expr)
		}
	}
}

// TestMetricsFirstStageBreaksAtTrailingPipe pins the `break` at the end of the
// metrics-first-stage branch in parseRoot. After a `| rate()` (or any metrics
// first stage) is parsed, the loop must STOP consuming pipeline stages: a
// second metrics first stage chained by a bare pipe (`| rate()`) is not a legal
// continuation, so the leftover pipe must reach the trailing tokEOF check and
// be rejected.
//
// If the loop-control mutates break→continue (INVERT_LOOPCTRL), the loop
// re-enters, sees the trailing `| rate()`, treats it as another metrics first
// stage, overwrites the first, and parses successfully. Asserting that the
// chained form is REJECTED (while the single-stage form is ACCEPTED) kills that
// mutant.
func TestMetricsFirstStageBreaksAtTrailingPipe(t *testing.T) {
	t.Parallel()

	// Baseline: a single metrics first stage parses cleanly.
	if _, err := Parse(`{} | rate()`); err != nil {
		t.Fatalf("Parse(`{} | rate()`) errored: %v; want success", err)
	}

	// Two metrics first stages chained by a pipe is not a valid pipeline. With
	// break, the trailing `| rate()` is left unconsumed and rejected at EOF.
	// With continue, the loop swallows it and the parse wrongly succeeds.
	if expr, err := Parse(`{} | rate() | rate()`); err == nil {
		t.Errorf("Parse(`{} | rate() | rate()`) = %v, nil; want a parse error (loop must break after the metrics first stage)", expr)
	}
}
