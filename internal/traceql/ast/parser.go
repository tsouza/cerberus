package ast

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// parser is a hand-written recursive-descent + precedence-climbing parser over
// the token slice produced by the lexer. It produces the native TraceQL AST
// directly, building the same node shapes the grammar's actions describe.
//
// The grammar shares one operator-precedence table across its field, spanset
// and scalar expression layers; the parser encodes that table once (see the
// *Prec helpers) and reuses it per layer, dispatching the same operator token
// to the layer-appropriate node (BinaryOperation / SpansetOperation /
// ScalarOperation) by the static role of its operands.
type parser struct {
	toks []token
}

// parseErr is thrown via panic during parsing and recovered at the top level,
// keeping the climbing routines free of error plumbing.
type parseErr struct {
	msg  string
	line int
	col  int
}

// cursor tracks the parse position. It is a thin value over the token slice so
// the climbing helpers stay allocation-free.
type cursor struct {
	p   *parser
	pos int
}

func (c *cursor) peek() token     { return c.p.toks[c.pos] }
func (c *cursor) kind() tokenKind { return c.p.toks[c.pos].kind }

func (c *cursor) advance() token {
	t := c.p.toks[c.pos]
	if c.pos < len(c.p.toks)-1 {
		c.pos++
	}
	return t
}

func (c *cursor) accept(k tokenKind) (token, bool) {
	if c.kind() == k {
		return c.advance(), true
	}
	return token{}, false
}

func (c *cursor) expect(k tokenKind) token {
	t := c.peek()
	if t.kind != k {
		c.fail("syntax error: unexpected %s, expecting %s", t.kind, k)
	}
	return c.advance()
}

func (c *cursor) fail(format string, args ...any) {
	t := c.peek()
	panic(parseErr{msg: fmt.Sprintf(format, args...), line: t.line, col: t.col})
}

// =====================================================================
// Precedence table (shared across expression layers).
//
// Mirrors the grammar's %left ordering, lowest binding first:
//   1 PIPE
//   2 AND OR
//   3 EQ NEQ LT LTE GT GTE NRE structural/union operators
//   4 ADD SUB
//   5 NOT
//   6 MUL DIV MOD
//   7 POW (right associative)
// PIPE is handled structurally (pipeline chaining), not as a climbing level.
// =====================================================================

const (
	precCmp = 3
	precAdd = 4
	precNot = 5
	precMul = 6
	precPow = 7
)

// fieldBinPrec returns the binding precedence of a field-expression binary
// operator, or 0 if the token is not one.
func fieldBinPrec(k tokenKind) int {
	switch k {
	case tokAnd, tokOr:
		return 2
	case tokEq, tokNeq, tokLt, tokLte, tokGt, tokGte, tokRe, tokNre:
		return precCmp
	case tokAdd, tokSub:
		return precAdd
	case tokMul, tokDiv, tokMod:
		return precMul
	case tokPow:
		return precPow
	default:
		return 0
	}
}

func fieldBinOp(k tokenKind) Operator {
	switch k {
	case tokAnd:
		return OpAnd
	case tokOr:
		return OpOr
	case tokEq:
		return OpEqual
	case tokNeq:
		return OpNotEqual
	case tokLt:
		return OpLess
	case tokLte:
		return OpLessEqual
	case tokGt:
		return OpGreater
	case tokGte:
		return OpGreaterEqual
	case tokRe:
		return OpRegex
	case tokNre:
		return OpNotRegex
	case tokAdd:
		return OpAdd
	case tokSub:
		return OpSub
	case tokMul:
		return OpMult
	case tokDiv:
		return OpDiv
	case tokMod:
		return OpMod
	case tokPow:
		return OpPower
	default:
		return OpNone
	}
}

func spansetBinPrec(k tokenKind) int {
	switch k {
	case tokAnd, tokOr:
		return 2
	case tokGt, tokLt, tokDesc, tokAnce, tokSibl,
		tokNotChild, tokNotParent, tokNre, tokNotAnce, tokNotDesc,
		tokUnionChild, tokUnionPar, tokUnionSibl, tokUnionAnce, tokUnionDesc:
		return precCmp
	default:
		return 0
	}
}

func spansetBinOp(k tokenKind) Operator {
	switch k {
	case tokAnd:
		return OpSpansetAnd
	case tokOr:
		return OpSpansetUnion
	case tokGt:
		return OpSpansetChild
	case tokLt:
		return OpSpansetParent
	case tokDesc:
		return OpSpansetDescendant
	case tokAnce:
		return OpSpansetAncestor
	case tokSibl:
		return OpSpansetSibling
	case tokNotChild:
		return OpSpansetNotChild
	case tokNotParent:
		return OpSpansetNotParent
	case tokNre:
		return OpSpansetNotSibling
	case tokNotAnce:
		return OpSpansetNotAncestor
	case tokNotDesc:
		return OpSpansetNotDescendant
	case tokUnionChild:
		return OpSpansetUnionChild
	case tokUnionPar:
		return OpSpansetUnionParent
	case tokUnionSibl:
		return OpSpansetUnionSibling
	case tokUnionAnce:
		return OpSpansetUnionAncestor
	case tokUnionDesc:
		return OpSpansetUnionDescendant
	default:
		return OpNone
	}
}

func scalarArithPrec(k tokenKind) int {
	switch k {
	case tokAdd, tokSub:
		return precAdd
	case tokMul, tokDiv, tokMod:
		return precMul
	case tokPow:
		return precPow
	default:
		return 0
	}
}

func isScalarFilterOp(k tokenKind) bool {
	switch k {
	case tokEq, tokNeq, tokLt, tokLte, tokGt, tokGte:
		return true
	default:
		return false
	}
}

func scalarFilterOp(k tokenKind) Operator {
	switch k {
	case tokEq:
		return OpEqual
	case tokNeq:
		return OpNotEqual
	case tokLt:
		return OpLess
	case tokLte:
		return OpLessEqual
	case tokGt:
		return OpGreater
	case tokGte:
		return OpGreaterEqual
	default:
		return OpNone
	}
}

// =====================================================================
// Root
// =====================================================================

func (c *cursor) parseRoot() *RootExpr {
	first := c.parsePipelineStage(true)
	elems := []PipelineElement{first}

	var m1 FirstStageElement
	var m2 SecondStageElement

	for c.kind() == tokPipe {
		if isMetricsFirstStage(c.p.toks[c.pos+1].kind) {
			c.advance() // consume PIPE
			m1 = c.parseMetricsFirstStage()
			m2 = c.parseSecondStagePipeline()
			break
		}
		c.advance() // consume PIPE
		elems = append(elems, c.parsePipelineStage(false))
	}

	var lead PipelineElement
	if len(elems) == 1 {
		lead = elems[0]
	} else {
		lead = Pipeline{Elements: elems}
	}

	var root *RootExpr
	switch {
	case m1 == nil:
		root = newRootExpr(lead)
	case m2 == nil:
		root = newRootExprWithMetrics(lead, m1)
	default:
		root = newRootExprWithMetricsTwoStage(lead, m1, m2)
	}

	if c.kind() == tokWith {
		root.Hints = c.parseHints()
	}

	if c.kind() != tokEOF {
		c.fail("syntax error: unexpected %s, expecting %s", c.kind(), tokEOF)
	}
	return root
}

// newRootExpr wraps a bare pipeline element into a single-element pipeline,
// mirroring the grammar's root reductions.
func newRootExpr(e PipelineElement) *RootExpr {
	return &RootExpr{Pipeline: asPipeline(e)}
}

func newRootExprWithMetrics(e PipelineElement, m FirstStageElement) *RootExpr {
	return &RootExpr{Pipeline: asPipeline(e), MetricsPipeline: m}
}

func newRootExprWithMetricsTwoStage(e PipelineElement, m1 FirstStageElement, m2 SecondStageElement) *RootExpr {
	return &RootExpr{Pipeline: asPipeline(e), MetricsPipeline: m1, MetricsSecondStage: m2}
}

func asPipeline(e PipelineElement) Pipeline {
	if p, ok := e.(Pipeline); ok {
		return p
	}
	return Pipeline{Elements: []PipelineElement{e}}
}

// =====================================================================
// Pipeline stages
// =====================================================================

// parsePipelineStage parses one stage of a spanset pipeline. The first stage
// permits the same forms as later ones (the grammar's only difference is that
// coalesce may not lead; accepting it here is harmless over-acceptance).
func (c *cursor) parsePipelineStage(_ bool) PipelineElement {
	switch c.kind() {
	case tokBy:
		return c.parseGroupOperation()
	case tokCoalesce:
		return c.parseCoalesceOperation()
	case tokSelect:
		return c.parseSelectOperation()
	case tokCount, tokMax, tokMin, tokAvg, tokSum:
		return c.parseScalarStage()
	case tokInteger, tokFloat, tokDuration, tokString, tokTrue, tokFalse,
		tokStatusOk, tokStatusError, tokStatusUnset,
		tokKindUnspecified, tokKindInternal, tokKindServer, tokKindClient,
		tokKindProducer, tokKindConsumer, tokSub:
		// A leading literal can only begin a scalar filter (e.g. `2 > count()`).
		return c.parseScalarStage()
	default:
		return c.parseSpansetExpr(2)
	}
}

// parseScalarStage parses an aggregate-or-literal-led stage that is either a
// bare aggregate (valid as a scalar pipeline tail) or a scalar filter.
func (c *cursor) parseScalarStage() PipelineElement {
	lhs := c.parseScalarExpr(0)
	if isScalarFilterOp(c.kind()) {
		op := scalarFilterOp(c.advance().kind)
		rhs := c.parseScalarExpr(0)
		return ScalarFilter{Op: op, LHS: lhs, RHS: rhs}
	}
	if agg, ok := lhs.(Aggregate); ok {
		return agg
	}
	c.fail("syntax error: expected comparison after scalar expression")
	return nil
}

func (c *cursor) parseGroupOperation() PipelineElement {
	c.expect(tokBy)
	c.expect(tokOpenParen)
	fe := c.parseFieldExpr(0)
	c.expect(tokCloseParen)
	return GroupOperation{Expression: fe}
}

func (c *cursor) parseCoalesceOperation() PipelineElement {
	c.expect(tokCoalesce)
	c.expect(tokOpenParen)
	c.expect(tokCloseParen)
	return CoalesceOperation{}
}

func (c *cursor) parseSelectOperation() PipelineElement {
	c.expect(tokSelect)
	c.expect(tokOpenParen)
	attrs := c.parseAttributeList()
	c.expect(tokCloseParen)
	return newSelectOperation(attrs)
}

// =====================================================================
// Spanset expressions (structural + set operators over spanset operands)
// =====================================================================

func (c *cursor) parseSpansetExpr(minPrec int) SpansetExpression {
	left := c.parseSpansetPrimary()
	for {
		k := c.kind()
		prec := spansetBinPrec(k)
		if prec == 0 || prec < minPrec {
			break
		}
		c.advance()
		right := c.parseSpansetExpr(prec + 1)
		left = SpansetOperation{Op: spansetBinOp(k), LHS: left, RHS: right}
	}
	return left
}

func (c *cursor) parseSpansetPrimary() SpansetExpression {
	switch c.kind() {
	case tokOpenBrace:
		return c.parseSpansetFilter()
	case tokOpenParen:
		c.advance()
		inner := c.parseParenPipeline()
		c.expect(tokCloseParen)
		return inner
	default:
		c.fail("syntax error: unexpected %s, expecting a spanset", c.kind())
		return nil
	}
}

// parseParenPipeline parses the contents of a parenthesised spanset, which may
// itself be a `|`-chained pipeline or a spanset operator expression.
func (c *cursor) parseParenPipeline() SpansetExpression {
	first := c.parsePipelineStage(true)
	if c.kind() != tokPipe {
		if se, ok := first.(SpansetExpression); ok {
			return se
		}
		return asPipeline(first)
	}
	elems := []PipelineElement{first}
	for c.kind() == tokPipe {
		c.advance()
		elems = append(elems, c.parsePipelineStage(false))
	}
	return Pipeline{Elements: elems}
}

func (c *cursor) parseSpansetFilter() *SpansetFilter {
	c.expect(tokOpenBrace)
	if _, ok := c.accept(tokCloseBrace); ok {
		return &SpansetFilter{Expression: NewStaticBool(true)}
	}
	fe := c.parseFieldExpr(0)
	c.expect(tokCloseBrace)
	return &SpansetFilter{Expression: fe}
}

// =====================================================================
// Field expressions (inside `{ ... }`, aggregate args, group-by)
// =====================================================================

func (c *cursor) parseFieldExpr(minPrec int) FieldExpression {
	left := c.parseFieldUnary()
	for {
		k := c.kind()
		prec := fieldBinPrec(k)
		if prec == 0 || prec < minPrec {
			break
		}
		c.advance()
		nextMin := prec + 1
		if k == tokPow {
			nextMin = prec // right associative
		}
		right := c.parseFieldExpr(nextMin)
		left = makeFieldBinary(fieldBinOp(k), left, right)
	}
	return left
}

func (c *cursor) parseFieldUnary() FieldExpression {
	switch c.kind() {
	case tokSub:
		c.advance()
		operand := c.parseFieldExpr(precNot)
		return makeFieldUnary(OpSub, operand)
	case tokNot:
		c.advance()
		operand := c.parseFieldExpr(precMul)
		return makeFieldUnary(OpNot, operand)
	default:
		return c.parseFieldPrimary()
	}
}

func (c *cursor) parseFieldPrimary() FieldExpression {
	switch c.kind() {
	case tokOpenParen:
		c.advance()
		inner := c.parseFieldExpr(0)
		c.expect(tokCloseParen)
		return inner
	case tokNil:
		c.advance()
		return NewStaticNil()
	}
	if s, ok := c.tryParseStatic(); ok {
		return s
	}
	return c.parseAttributeNode()
}

// makeFieldBinary builds a binary field node, applying the grammar's nil
// handling (existence checks) and constant folding for trace-id operands.
func makeFieldBinary(op Operator, l, r FieldExpression) FieldExpression {
	lNil := isNilStatic(l)
	rNil := isNilStatic(r)
	if op == OpEqual || op == OpNotEqual {
		switch {
		case lNil && rNil:
			return NewStaticBool(false)
		case lNil || rNil:
			operand := l
			if lNil {
				operand = r
			}
			if op == OpNotEqual {
				return UnaryOperation{Op: OpExists, Expression: operand}
			}
			return UnaryOperation{Op: OpNotExists, Expression: operand}
		}
	}

	bin := &BinaryOperation{Op: op, LHS: l, RHS: r}
	if a, ok := l.(Attribute); ok && a.Intrinsic == IntrinsicTraceID {
		if s, ok := r.(Static); ok {
			bin.RHS = normalizeTraceIDOperand(s)
		}
	}
	if a, ok := r.(Attribute); ok && a.Intrinsic == IntrinsicTraceID {
		if s, ok := l.(Static); ok {
			bin.LHS = normalizeTraceIDOperand(s)
		}
	}
	return bin
}

// makeFieldUnary builds a unary field node, constant-folding negation and
// logical-not over a literal operand (matching the reference parser, which
// evaluates span-free unary expressions at parse time).
func makeFieldUnary(op Operator, e FieldExpression) FieldExpression {
	if s, ok := e.(Static); ok {
		switch op {
		case OpSub:
			if neg, ok := negateStatic(s); ok {
				return neg
			}
		case OpNot:
			if b, ok := s.Bool(); ok {
				return NewStaticBool(!b)
			}
		}
	}
	return UnaryOperation{Op: op, Expression: e}
}

func negateStatic(s Static) (Static, bool) {
	switch s.Type {
	case TypeInt:
		if i, ok := s.Int(); ok {
			return NewStaticInt(-i), true
		}
	case TypeFloat:
		return NewStaticFloat(-s.Float()), true
	case TypeDuration:
		if d, ok := s.Duration(); ok {
			return NewStaticDuration(-d), true
		}
	}
	return s, false
}

func normalizeTraceIDOperand(s Static) Static {
	if s.Type != TypeString {
		return s
	}
	id := strings.TrimLeft(s.EncodeToString(false), "0")
	return NewStaticString(id)
}

func isNilStatic(e FieldExpression) bool {
	s, ok := e.(Static)
	return ok && s.Type == TypeNil
}

// =====================================================================
// Scalar expressions (aggregate / static arithmetic)
// =====================================================================

func (c *cursor) parseScalarExpr(minPrec int) ScalarExpression {
	left := c.parseScalarPrimary()
	for {
		k := c.kind()
		prec := scalarArithPrec(k)
		if prec == 0 || prec < minPrec {
			break
		}
		c.advance()
		nextMin := prec + 1
		if k == tokPow {
			nextMin = prec
		}
		right := c.parseScalarExpr(nextMin)
		left = ScalarOperation{Op: fieldBinOp(k), LHS: left, RHS: right}
	}
	return left
}

func (c *cursor) parseScalarPrimary() ScalarExpression {
	switch c.kind() {
	case tokCount, tokMax, tokMin, tokAvg, tokSum:
		return c.parseAggregate()
	case tokOpenParen:
		c.advance()
		inner := c.parseScalarExpr(0)
		c.expect(tokCloseParen)
		return inner
	}
	if s, ok := c.tryParseStatic(); ok {
		return s
	}
	c.fail("syntax error: unexpected %s in scalar expression", c.kind())
	return nil
}

func (c *cursor) parseAggregate() Aggregate {
	switch c.advance().kind {
	case tokCount:
		c.expect(tokOpenParen)
		c.expect(tokCloseParen)
		return newAggregate(AggregateCount, nil)
	case tokMax:
		return newAggregate(AggregateMax, c.parseAggregateArg())
	case tokMin:
		return newAggregate(AggregateMin, c.parseAggregateArg())
	case tokAvg:
		return newAggregate(AggregateAvg, c.parseAggregateArg())
	case tokSum:
		return newAggregate(AggregateSum, c.parseAggregateArg())
	default:
		c.fail("syntax error: expected aggregate")
		return Aggregate{}
	}
}

func (c *cursor) parseAggregateArg() FieldExpression {
	c.expect(tokOpenParen)
	fe := c.parseFieldExpr(0)
	c.expect(tokCloseParen)
	return fe
}

// =====================================================================
// Statics
// =====================================================================

// tryParseStatic parses a literal value if the current token begins one. It
// also handles the `- <number>` literal form used in scalar contexts.
func (c *cursor) tryParseStatic() (Static, bool) {
	switch c.kind() {
	case tokString:
		return NewStaticString(c.advance().str), true
	case tokInteger:
		return NewStaticInt(c.advance().i), true
	case tokFloat:
		return NewStaticFloat(c.advance().f), true
	case tokDuration:
		return NewStaticDuration(c.advance().d), true
	case tokTrue:
		c.advance()
		return NewStaticBool(true), true
	case tokFalse:
		c.advance()
		return NewStaticBool(false), true
	case tokStatusOk:
		c.advance()
		return NewStaticStatus(StatusOk), true
	case tokStatusError:
		c.advance()
		return NewStaticStatus(StatusError), true
	case tokStatusUnset:
		c.advance()
		return NewStaticStatus(StatusUnset), true
	case tokKindUnspecified:
		c.advance()
		return NewStaticKind(KindUnspecified), true
	case tokKindInternal:
		c.advance()
		return NewStaticKind(KindInternal), true
	case tokKindServer:
		c.advance()
		return NewStaticKind(KindServer), true
	case tokKindClient:
		c.advance()
		return NewStaticKind(KindClient), true
	case tokKindProducer:
		c.advance()
		return NewStaticKind(KindProducer), true
	case tokKindConsumer:
		c.advance()
		return NewStaticKind(KindConsumer), true
	case tokSub:
		// negative numeric literal
		switch c.p.toks[c.pos+1].kind {
		case tokInteger:
			c.advance()
			return NewStaticInt(-c.advance().i), true
		case tokFloat:
			c.advance()
			return NewStaticFloat(-c.advance().f), true
		case tokDuration:
			c.advance()
			return NewStaticDuration(-c.advance().d), true
		}
		return Static{}, false
	case tokIdentifier:
		name := c.peek().str
		switch name {
		case "minInt":
			c.advance()
			return NewStaticInt(math.MinInt), true
		case "maxInt":
			c.advance()
			return NewStaticInt(math.MaxInt), true
		default:
			c.fail("unknown identifier: %s", name)
			return Static{}, false
		}
	default:
		return Static{}, false
	}
}

// =====================================================================
// Attributes and intrinsics
// =====================================================================

func (c *cursor) parseAttributeList() []Attribute {
	attrs := []Attribute{c.parseAttributeNode()}
	for c.kind() == tokComma {
		c.advance()
		attrs = append(attrs, c.parseAttributeNode())
	}
	return attrs
}

// parseAttributeNode parses one attribute reference: a bare intrinsic, a
// scope-colon intrinsic, or a scope-dot attribute.
func (c *cursor) parseAttributeNode() Attribute {
	k := c.kind()
	if in, ok := bareIntrinsic(k); ok {
		c.advance()
		return NewIntrinsic(in)
	}
	switch k {
	case tokTraceColon, tokSpanColon, tokEventColon, tokLinkColon, tokInstrColon:
		return c.parseScopedIntrinsic()
	case tokDot, tokResourceDot, tokSpanDot, tokParentDot,
		tokEventDot, tokLinkDot, tokInstrDot:
		return c.parseAttributeField()
	default:
		c.fail("syntax error: unexpected %s, expecting an attribute", k)
		return Attribute{}
	}
}

func bareIntrinsic(k tokenKind) (Intrinsic, bool) {
	switch k {
	case tokIDuration:
		return IntrinsicDuration, true
	case tokName:
		return IntrinsicName, true
	case tokStatus:
		return IntrinsicStatus, true
	case tokStatusMessage:
		return IntrinsicStatusMessage, true
	case tokKind:
		return IntrinsicKind, true
	case tokParent:
		return IntrinsicParent, true
	case tokRootName:
		return IntrinsicTraceRootSpan, true
	case tokRootServiceName:
		return IntrinsicTraceRootService, true
	case tokTraceDuration:
		return IntrinsicTraceDuration, true
	case tokNestedSetLeft:
		return IntrinsicNestedSetLeft, true
	case tokNestedSetRight:
		return IntrinsicNestedSetRight, true
	case tokNestedSetParent:
		return IntrinsicNestedSetParent, true
	default:
		return IntrinsicNone, false
	}
}

func (c *cursor) parseScopedIntrinsic() Attribute {
	scope := c.advance().kind
	field := c.advance().kind
	in, ok := scopedIntrinsic(scope, field)
	if !ok {
		c.fail("syntax error: %s is not a valid scoped intrinsic", field)
	}
	return NewIntrinsic(in)
}

func scopedIntrinsic(scope, field tokenKind) (Intrinsic, bool) {
	switch scope {
	case tokTraceColon:
		switch field {
		case tokIDuration:
			return IntrinsicTraceDuration, true
		case tokRootName:
			return IntrinsicTraceRootSpan, true
		case tokRootService:
			return IntrinsicTraceRootService, true
		case tokID:
			return IntrinsicTraceID, true
		}
	case tokSpanColon:
		switch field {
		case tokIDuration:
			return IntrinsicDuration, true
		case tokName:
			return IntrinsicName, true
		case tokKind:
			return IntrinsicKind, true
		case tokStatus:
			return IntrinsicStatus, true
		case tokStatusMessage:
			return IntrinsicStatusMessage, true
		case tokID:
			return IntrinsicSpanID, true
		case tokParentID:
			return IntrinsicParentID, true
		case tokChildCount:
			return IntrinsicChildCount, true
		}
	case tokEventColon:
		switch field {
		case tokName:
			return IntrinsicEventName, true
		case tokTimeSinceStart:
			return IntrinsicEventTimeSinceStart, true
		}
	case tokLinkColon:
		switch field {
		case tokTraceID:
			return IntrinsicLinkTraceID, true
		case tokSpanID:
			return IntrinsicLinkSpanID, true
		}
	case tokInstrColon:
		switch field {
		case tokName:
			return IntrinsicInstrumentationName, true
		case tokVersion:
			return IntrinsicInstrumentationVersion, true
		}
	}
	return IntrinsicNone, false
}

func (c *cursor) parseAttributeField() Attribute {
	scope := c.advance().kind
	switch scope {
	case tokDot:
		name := c.expect(tokIdentifier).str
		c.expect(tokEndAttribute)
		return NewAttribute(name)
	case tokResourceDot:
		name := c.expect(tokIdentifier).str
		c.expect(tokEndAttribute)
		return NewScopedAttribute(AttributeScopeResource, false, name)
	case tokSpanDot:
		name := c.expect(tokIdentifier).str
		c.expect(tokEndAttribute)
		return NewScopedAttribute(AttributeScopeSpan, false, name)
	case tokParentDot:
		switch c.kind() {
		case tokResourceDot:
			c.advance()
			name := c.expect(tokIdentifier).str
			c.expect(tokEndAttribute)
			return NewScopedAttribute(AttributeScopeResource, true, name)
		case tokSpanDot:
			c.advance()
			name := c.expect(tokIdentifier).str
			c.expect(tokEndAttribute)
			return NewScopedAttribute(AttributeScopeSpan, true, name)
		default:
			name := c.expect(tokIdentifier).str
			c.expect(tokEndAttribute)
			return NewScopedAttribute(AttributeScopeNone, true, name)
		}
	case tokEventDot:
		name := c.expect(tokIdentifier).str
		c.expect(tokEndAttribute)
		return NewScopedAttribute(AttributeScopeEvent, false, name)
	case tokLinkDot:
		name := c.expect(tokIdentifier).str
		c.expect(tokEndAttribute)
		return NewScopedAttribute(AttributeScopeLink, false, name)
	case tokInstrDot:
		name := c.expect(tokIdentifier).str
		c.expect(tokEndAttribute)
		return NewScopedAttribute(AttributeScopeInstrumentation, false, name)
	default:
		c.fail("syntax error: unexpected %s, expecting an attribute scope", scope)
		return Attribute{}
	}
}

// =====================================================================
// Metrics first stage
// =====================================================================

func isMetricsFirstStage(k tokenKind) bool {
	switch k {
	case tokRate, tokCountOverTime, tokMinOverTime, tokMaxOverTime,
		tokAvgOverTime, tokSumOverTime, tokQuantileOverTime,
		tokHistogramOverTime, tokCompare:
		return true
	default:
		return false
	}
}

func (c *cursor) parseMetricsFirstStage() FirstStageElement {
	switch c.advance().kind {
	case tokRate:
		c.expect(tokOpenParen)
		c.expect(tokCloseParen)
		return newMetricsAggregate(MetricsAggregateRate, Attribute{}, c.parseOptionalBy(), nil)
	case tokCountOverTime:
		c.expect(tokOpenParen)
		c.expect(tokCloseParen)
		return newMetricsAggregate(MetricsAggregateCountOverTime, Attribute{}, c.parseOptionalBy(), nil)
	case tokMinOverTime:
		attr := c.parseParenAttribute()
		return newMetricsAggregate(MetricsAggregateMinOverTime, attr, c.parseOptionalBy(), nil)
	case tokMaxOverTime:
		attr := c.parseParenAttribute()
		return newMetricsAggregate(MetricsAggregateMaxOverTime, attr, c.parseOptionalBy(), nil)
	case tokSumOverTime:
		attr := c.parseParenAttribute()
		return newMetricsAggregate(MetricsAggregateSumOverTime, attr, c.parseOptionalBy(), nil)
	case tokAvgOverTime:
		attr := c.parseParenAttribute()
		return newAverageOverTimeAggregator(attr, c.parseOptionalBy())
	case tokHistogramOverTime:
		attr := c.parseParenAttribute()
		return newMetricsAggregate(MetricsAggregateHistogramOverTime, attr, c.parseOptionalBy(), nil)
	case tokQuantileOverTime:
		c.expect(tokOpenParen)
		attr := c.parseAttributeNode()
		c.expect(tokComma)
		qs := c.parseNumericList()
		c.expect(tokCloseParen)
		return newMetricsAggregate(MetricsAggregateQuantileOverTime, attr, c.parseOptionalBy(), qs)
	case tokCompare:
		return c.parseCompare()
	default:
		c.fail("syntax error: expected a metrics aggregation")
		return nil
	}
}

func (c *cursor) parseParenAttribute() Attribute {
	c.expect(tokOpenParen)
	attr := c.parseAttributeNode()
	c.expect(tokCloseParen)
	return attr
}

// parseOptionalBy parses a trailing `by(attr, ...)` grouping if present.
func (c *cursor) parseOptionalBy() []Attribute {
	if c.kind() != tokBy {
		return nil
	}
	c.advance()
	c.expect(tokOpenParen)
	attrs := c.parseAttributeList()
	c.expect(tokCloseParen)
	return attrs
}

func (c *cursor) parseNumericList() []float64 {
	var out []float64
	out = append(out, c.parseNumeric())
	for c.kind() == tokComma {
		c.advance()
		out = append(out, c.parseNumeric())
	}
	return out
}

func (c *cursor) parseNumeric() float64 {
	switch c.kind() {
	case tokInteger:
		return float64(c.advance().i)
	case tokFloat:
		return c.advance().f
	default:
		c.fail("syntax error: unexpected %s, expecting a number", c.kind())
		return 0
	}
}

// compareDefaultTopN is the default number of distinguishing attributes the
// `compare()` first stage reports when no explicit count is given.
const compareDefaultTopN = 10

func (c *cursor) parseCompare() FirstStageElement {
	c.expect(tokOpenParen)
	filter := c.parseSpansetFilter()
	topN := compareDefaultTopN
	start, end := 0, 0
	if c.kind() == tokComma {
		c.advance()
		topN = c.expectInteger()
		if c.kind() == tokComma {
			c.advance()
			start = c.expectInteger()
			c.expect(tokComma)
			end = c.expectInteger()
		}
	}
	c.expect(tokCloseParen)
	return newMetricsCompare(filter, topN, start, end)
}

func (c *cursor) expectInteger() int {
	return c.expect(tokInteger).i
}

// =====================================================================
// Metrics second stage
// =====================================================================

// parseSecondStagePipeline parses zero or more second-stage elements (topk /
// bottomk via `| ...`, or a bare metrics filter). Returns nil when none are
// present; otherwise a *ChainedSecondStage recording the textual separators.
func (c *cursor) parseSecondStagePipeline() SecondStageElement {
	var chain *ChainedSecondStage
	for {
		switch {
		case isScalarFilterOp(c.kind()):
			mf := c.parseMetricsFilter()
			if chain == nil {
				chain = newChainedSecondStage()
			}
			chain.Append(mf, " ")
		case c.kind() == tokPipe && isSecondStageKeyword(c.p.toks[c.pos+1].kind):
			c.advance() // PIPE
			ts := c.parseTopKBottomK()
			if chain == nil {
				chain = newChainedSecondStage()
			}
			chain.Append(ts, " | ")
		default:
			if chain == nil {
				return nil
			}
			return chain
		}
	}
}

func isSecondStageKeyword(k tokenKind) bool {
	return k == tokTopK || k == tokBottomK
}

func (c *cursor) parseTopKBottomK() SecondStageElement {
	op := OpTopK
	if c.advance().kind == tokBottomK {
		op = OpBottomK
	}
	c.expect(tokOpenParen)
	n := c.expectInteger()
	c.expect(tokCloseParen)
	return newTopKBottomK(op, n)
}

func (c *cursor) parseMetricsFilter() SecondStageElement {
	op := scalarFilterOp(c.advance().kind)
	value := c.parseMetricsFilterValue()
	return newMetricsFilter(op, value)
}

func (c *cursor) parseMetricsFilterValue() float64 {
	neg := false
	if c.kind() == tokSub {
		c.advance()
		neg = true
	}
	var v float64
	switch c.kind() {
	case tokInteger:
		v = float64(c.advance().i)
	case tokFloat:
		v = c.advance().f
	case tokDuration:
		v = float64(c.advance().d) / float64(time.Second)
	default:
		c.fail("syntax error: unexpected %s, expecting a metrics filter value", c.kind())
	}
	if neg {
		return -v
	}
	return v
}

// =====================================================================
// Hints
// =====================================================================

func (c *cursor) parseHints() *Hints {
	c.expect(tokWith)
	c.expect(tokOpenParen)
	hints := []*Hint{c.parseHint()}
	for c.kind() == tokComma {
		c.advance()
		hints = append(hints, c.parseHint())
	}
	c.expect(tokCloseParen)
	return &Hints{Hints: hints}
}

func (c *cursor) parseHint() *Hint {
	name := c.expect(tokIdentifier).str
	c.expect(tokEq)
	val, ok := c.tryParseStatic()
	if !ok {
		c.fail("syntax error: expected a static hint value")
	}
	return &Hint{Name: name, Value: val}
}
