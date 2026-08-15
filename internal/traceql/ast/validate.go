package ast

import (
	"fmt"
	"regexp"
)

// This file implements the reference backend's static-validation pass —
// the checks that run over the parsed tree BEFORE a query ever reaches
// lowering. cerberus's in-house parser reimplements the grammar and
// `impliedType` inference, but went without this pass entirely — a gap
// that surfaced as HTTP 502s (or silently wrong answers) rather than the
// 400 the reference gives.
//
// Without these rules a query like `{ name > 3 }` (a String-typed
// intrinsic against an integer literal) parses, lowers, is emitted as
// SQL, and is only rejected by ClickHouse at execution time with
// NO_COMMON_TYPE ("there is no supertype for types String, UInt8"). That
// reaches the client as a gateway error carrying ClickHouse's own error
// code and type names — the wrong status class for a malformed query, and
// storage-engine internals on a Tempo-shaped wire. Other rules below
// (an aggregate over a String, a span filter that never resolves to a
// boolean, an unparsable regex) are wrong-ACCEPTANCE divergences: the
// reference rejects with 400 and cerberus, absent the rule, answers 200
// with whatever the query happens to lower to.
//
// The checks run inside Parse, so a violation is a parse-stage failure
// and the Tempo head answers it with 400 and the reference backend's own
// wording, exactly as the reference does.
//
// Two of the reference's rules are deliberately NOT adopted, because
// cerberus answers queries the reference declines rather than mirroring
// the rejection (see #2035):
//   - UnaryOperation's `intrinsic = nil` rejection — `{ kind != nil }` is
//     in cerberus's own corpus and lowers to a real predicate.
//   - Attribute's parent-scope unsupported-error — `{ parent = "<hex>" }`
//     lowers against ParentSpanId.

// typeMismatchFormat reproduces the reference backend's own wording for a
// mismatched binary operation, verbatim. A client (or a compatibility
// harness) that reads the message sees the same sentence from cerberus and
// from the reference.
const typeMismatchFormat = "binary operations must operate on the same type: %s"

// illegalOperationTypesFormat reproduces the reference's wording for a
// binary operation whose two sides have a statically compatible type
// (typeMismatchFormat above already passed) but a type the OPERATOR itself
// does not accept — `.foo && 3`: TypeInt matches nothing else at play, but
// `&&` only accepts boolean operands.
const illegalOperationTypesFormat = "illegal operation for the given types: %s"

// illegalOperationTypeFormat is the unary-operation twin of
// illegalOperationTypesFormat (singular "type", matching the reference
// verbatim) — `-name`: unary `-` requires a numeric operand.
const illegalOperationTypeFormat = "illegal operation for the given type: %s"

// aggregateNumericFormat reproduces the reference's wording for an
// aggregate (`max` / `min` / `sum` / `avg`) whose inner expression cannot
// resolve to a number — `max(name)`: `name` is always a String.
const aggregateNumericFormat = "aggregate field expressions must resolve to a number type: %s"

// spanFilterBooleanFormat reproduces the reference's wording for a span
// filter `{ ... }` whose expression cannot resolve to a boolean —
// `{ duration }`: a bare duration names a value, not a predicate.
const spanFilterBooleanFormat = "span filter field expressions must resolve to a boolean: %s"

// invalidRegexFormat reproduces the reference's wording for a `=~` / `!~`
// pattern that does not compile — `{ span.x =~ "[" }`.
const invalidRegexFormat = "invalid regex: %s"

// ValidationError is returned by Parse for a query that parses cleanly but
// breaks one of the language's static rules. It is distinct from
// ParseError because nothing about the TEXT was wrong — the grammar
// accepted it and the tree is well formed; the tree merely fails a rule
// the reference backend enforces over the shape or the types of that tree.
//
// See the package doc comment above for the rules enforced here and the
// two the reference enforces that cerberus deliberately does not adopt.
type ValidationError struct{ msg string }

// Error renders the message with the reference backend's `invalid TraceQL
// query: ` prefix, so the wire text a client sees matches.
func (e *ValidationError) Error() string { return "invalid TraceQL query: " + e.msg }

func newValidationError(format string, args ...any) *ValidationError {
	return &ValidationError{msg: fmt.Sprintf(format, args...)}
}

// isMatchingOperand reports whether two statically-known operand types may
// meet under a binary operator. It is deliberately permissive: the rule
// exists to reject the provably impossible, not to demand exact types.
//
//   - TypeAttribute is the "resolved at query time" type a scoped
//     attribute carries. Its real type varies span by span, so no static
//     verdict is possible and the pair always matches; a span whose value
//     turns out to be incomparable simply does not match the filter (see
//     coerceNumericFieldAccess in the lowering for how that per-span
//     "no match" is produced in SQL).
//   - The numeric family (int / float / duration) is inter-comparable.
//   - nil compares against anything — `{ span.foo != nil }` is the
//     existence idiom.
//
// The array types need no case of their own. The grammar has no array
// literal — the only producer is the OR-to-IN fold in rewrite.go, which
// runs AFTER this rule (see Parse) and only ever merges literals of one
// family. A chain that would fold into a mismatched array comparison
// therefore always contains a mismatched SCALAR comparison, and is
// rejected on that one first.
func (t StaticType) isMatchingOperand(otherT StaticType) bool {
	if t == TypeAttribute || otherT == TypeAttribute {
		return true
	}
	if t == otherT {
		return true
	}
	if t.isNumeric() && otherT.isNumeric() {
		return true
	}
	return t == TypeNil || otherT == TypeNil
}

// binaryTypesValid reports whether op accepts both lhsT and rhsT as
// operands — the reference's second static-typing rule, checked only
// after isMatchingOperand has already confirmed the two sides are
// statically compatible with EACH OTHER. The two rules catch different
// mistakes: `{ name > 3 }` fails isMatchingOperand (String vs Int, no
// sensible comparison exists at all); `{ .foo && 3 }` passes it (Int
// matches nothing else at play except itself, but TypeAttribute matches
// anything) and fails this rule instead, because `&&` never accepts an
// Int operand regardless of what it is compared against.
func (op Operator) binaryTypesValid(lhsT, rhsT StaticType) bool {
	return binaryTypeValid(op, lhsT) && binaryTypeValid(op, rhsT)
}

// binaryTypeValid reports whether op accepts a single operand of type t,
// independent of the other side. TypeAttribute is query-time typed, so it
// is always valid — a per-span mismatch is a per-span "no match", not a
// static error (see isMatchingOperand).
func binaryTypeValid(op Operator, t StaticType) bool {
	if t == TypeAttribute {
		return true
	}
	switch t {
	case TypeBoolean, TypeBooleanArray:
		return operatorIn(op, booleanBinaryOperators)
	case TypeInt, TypeFloat, TypeDuration, TypeIntArray, TypeFloatArray:
		return operatorIn(op, numericBinaryOperators)
	case TypeString, TypeStringArray:
		return operatorIn(op, stringBinaryOperators)
	case TypeNil, TypeStatus, TypeKind:
		return operatorIn(op, equalityOnlyBinaryOperators)
	default:
		return false
	}
}

// The four operator sets binaryTypeValid dispatches on, one per operand
// family — pulled out to package level so the function itself is a plain
// switch over operatorIn calls rather than a wall of `||` chains, which is
// what drove its cyclomatic complexity over golangci-lint's cyclop budget.
var (
	booleanBinaryOperators = []Operator{
		OpAnd, OpOr, OpEqual, OpNotEqual, OpIn, OpNotIn,
	}
	numericBinaryOperators = []Operator{
		OpAdd, OpSub, OpMult, OpDiv, OpMod, OpPower,
		OpEqual, OpNotEqual, OpGreater, OpGreaterEqual, OpLess, OpLessEqual,
		OpIn, OpNotIn,
	}
	stringBinaryOperators = []Operator{
		OpEqual, OpNotEqual, OpRegex, OpNotRegex,
		OpGreater, OpGreaterEqual, OpLess, OpLessEqual,
		OpIn, OpNotIn, OpRegexMatchAny, OpRegexMatchNone,
	}
	equalityOnlyBinaryOperators = []Operator{OpEqual, OpNotEqual}
)

// unaryTypesValid reports whether op accepts an operand of type t —
// `{ -name = "a" }`: unary `-` requires a numeric operand, and `name` is
// always a String. TypeAttribute is query-time typed and always valid, and
// the two existence operators (produced only by the nil-comparison fold in
// makeFieldBinary, never by surface syntax) accept anything, mirroring the
// reference.
func (op Operator) unaryTypesValid(t StaticType) bool {
	if t == TypeAttribute {
		return true
	}
	switch op {
	case OpSub:
		return t.isNumeric()
	case OpNot:
		return t == TypeBoolean
	case OpExists, OpNotExists:
		return true
	default:
		return false
	}
}

// validate reports the first static rule violation in r, or nil when the
// whole tree validates.
//
// The pipeline traversal deliberately mirrors ReferencesIntrinsic's
// (references.go) node for node, down to reusing spansetFilterExpression:
// both walks answer a question about the WHOLE query, so a node one of
// them reaches and the other does not is a bug in whichever is the
// shorter. The metrics second stage is absent from both for the same
// reason — MetricsFilter, TopKBottomK and ChainedSecondStage carry no
// expression to check.
func (r *RootExpr) validate() error {
	if r == nil {
		return nil
	}
	if err := validatePipeline(r.Pipeline); err != nil {
		return err
	}
	// compare() is the one metrics first stage carrying a filter, and its
	// filter is a SpansetFilter like any other — same boolean-result rule.
	if cmp, ok := r.MetricsPipeline.(*MetricsCompare); ok && cmp.Filter() != nil {
		return validateSpansetFilterResult(cmp.Filter(), cmp.Filter().Expression)
	}
	return nil
}

func validatePipeline(p Pipeline) error {
	for _, elem := range p.Elements {
		if err := validatePipelineElement(elem); err != nil {
			return err
		}
	}
	return nil
}

func validatePipelineElement(elem PipelineElement) error {
	if expr, ok := spansetFilterExpression(elem); ok {
		return validateSpansetFilterResult(elem, expr)
	}
	switch e := elem.(type) {
	case SpansetOperation:
		return validateSpansetExpr(e)
	case ScalarFilter:
		return validateScalarFilter(e.Op, e.LHS, e.RHS, e)
	case GroupOperation:
		return validateFieldExpr(e.Expression)
	case Aggregate:
		return validateAggregate(e)
	case Pipeline:
		return validatePipeline(e)
	}
	// SelectOperation and CoalesceOperation name attributes and nothing
	// else: an attribute is a leaf, so there is no binary node to check.
	return nil
}

func validateSpansetExpr(se SpansetExpression) error {
	if expr, ok := spansetFilterExpression(se); ok {
		return validateSpansetFilterResult(se, expr)
	}
	switch s := se.(type) {
	case SpansetOperation:
		if err := validateSpansetExpr(s.LHS); err != nil {
			return err
		}
		return validateSpansetExpr(s.RHS)
	case ScalarFilter:
		return validateScalarFilter(s.Op, s.LHS, s.RHS, s)
	case Pipeline:
		return validatePipeline(s)
	}
	return nil
}

// validateSpansetFilterResult type-checks a SpansetFilter's expression and
// then applies the reference's span-filter result-type rule: the
// expression must resolve to a boolean (or stay query-time typed) —
// `{ duration }` and `{ 3 }` both name a value, not a predicate, and the
// reference rejects both. elem carries the original node so the message
// names the whole `{ ... }` filter, matching the reference's own wording.
func validateSpansetFilterResult(elem Element, expr FieldExpression) error {
	if err := validateFieldExpr(expr); err != nil {
		return err
	}
	t := expr.impliedType()
	if t != TypeAttribute && t != TypeBoolean {
		return newValidationError(spanFilterBooleanFormat, elem.String())
	}
	return nil
}

// validateAggregate type-checks an aggregate's inner expression and then
// applies the reference's number-type rule: `max` / `min` / `sum` / `avg`
// must resolve to a number (or stay query-time typed) — `max(name)`:
// `name` is always a String. `count()` carries no inner expression and is
// exempt, matching the reference.
func validateAggregate(a Aggregate) error {
	inner := a.InnerExpr()
	if inner == nil {
		return nil
	}
	if err := validateFieldExpr(inner); err != nil {
		return err
	}
	t := inner.impliedType()
	if t != TypeAttribute && !t.isNumeric() {
		return newValidationError(aggregateNumericFormat, a.String())
	}
	return nil
}

// validateScalarFilter is the shared body of the two positions carrying an
// Op plus a scalar LHS/RHS pair — a pipeline-stage or spanset-operand
// ScalarFilter (`| count() > 2`) and a ScalarOperation (`max(duration) + 1`)
// — which is also why references.go handles both through one function.
func validateScalarFilter(op Operator, lhs, rhs ScalarExpression, node Element) error {
	if err := validateScalarExpr(lhs); err != nil {
		return err
	}
	if err := validateScalarExpr(rhs); err != nil {
		return err
	}
	return matchOperands(op, lhs, rhs, node)
}

func validateScalarExpr(se ScalarExpression) error {
	switch s := se.(type) {
	case ScalarOperation:
		return validateScalarFilter(s.Op, s.LHS, s.RHS, s)
	case Aggregate:
		return validateAggregate(s)
	case Pipeline:
		return validatePipeline(s)
	}
	return nil
}

// validateFieldExpr type-checks a field expression bottom-up: the deepest
// mismatch is the one reported, so the message names the sub-expression
// that is actually wrong rather than the whole filter.
func validateFieldExpr(fe FieldExpression) error {
	switch e := fe.(type) {
	case *BinaryOperation:
		if err := validateFieldExpr(e.LHS); err != nil {
			return err
		}
		if err := validateFieldExpr(e.RHS); err != nil {
			return err
		}
		if err := matchOperands(e.Op, e.LHS, e.RHS, e); err != nil {
			return err
		}
		return validateRegexPattern(e)
	case UnaryOperation:
		if err := validateFieldExpr(e.Expression); err != nil {
			return err
		}
		return validateUnaryOperand(e)
	}
	// Attribute and Static are leaves.
	return nil
}

// matchOperands applies the two operand-type rules to one binary node,
// naming node in the message so the client sees which sub-expression is
// ill-typed. isMatchingOperand runs first — a mismatch there means no
// operator could ever relate the two operands, which is the more
// fundamental complaint; binaryTypesValid then asks whether THIS operator
// accepts operands of this (now mutually compatible) type.
func matchOperands(op Operator, lhs, rhs typedExpression, node Element) error {
	lhsT := lhs.impliedType()
	rhsT := rhs.impliedType()
	if !lhsT.isMatchingOperand(rhsT) {
		return newValidationError(typeMismatchFormat, node.String())
	}
	if isParentMarker(lhs) || isParentMarker(rhs) {
		// The bare `parent` intrinsic (issue #2035's exclusion list) is a
		// cerberus-only structural marker with no reference equivalent —
		// `{ parent && ... }` lowers to a ParentSpanId existence check
		// (edge_intr_parent_with_attrs.txtar). Its TypeNil implied type
		// exists only so it compares against anything (`parent = "<hex>"`
		// in isMatchingOperand above); reference's Nil-restricted-to-
		// Equal/NotEqual operator rule was never designed with this
		// extension in mind and must not constrain which operators it
		// may compose under.
		return nil
	}
	if !op.binaryTypesValid(lhsT, rhsT) {
		return newValidationError(illegalOperationTypesFormat, node.String())
	}
	return nil
}

// isParentMarker reports whether e is the bare `parent` intrinsic
// (Attribute{Intrinsic: IntrinsicParent}), the cerberus-only structural
// marker matchOperands exempts from the operator-validity rule above.
func isParentMarker(e typedExpression) bool {
	a, ok := e.(Attribute)
	return ok && a.Intrinsic == IntrinsicParent
}

// validateUnaryOperand applies the reference's unary operand-type rule —
// `{ -name = "a" }`: unary `-` requires a numeric operand.
func validateUnaryOperand(o UnaryOperation) error {
	t := o.Expression.impliedType()
	if !o.Op.unaryTypesValid(t) {
		return newValidationError(illegalOperationTypeFormat, o.String())
	}
	return nil
}

// validateRegexPattern applies the reference's compile-time regex check —
// `{ span.x =~ "[" }`: an unparsable pattern cannot match anything on any
// span, so the reference rejects it as a mistake in the query text rather
// than accepting it and matching nothing forever. Only a literal (Static)
// String pattern can be checked here; a dynamic attribute pattern
// (`x =~ y`) is resolved per span at query time and has no text to
// validate until then.
func validateRegexPattern(o *BinaryOperation) error {
	if o.Op != OpRegex && o.Op != OpNotRegex {
		return nil
	}
	static, ok := o.RHS.(Static)
	if !ok || static.Type != TypeString {
		return nil
	}
	if _, err := regexp.Compile(static.EncodeToString(false)); err != nil {
		return newValidationError(invalidRegexFormat, o.RHS.String())
	}
	return nil
}
