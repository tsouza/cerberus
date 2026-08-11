package ast

import "fmt"

// This file implements the language's operand-type rule: the two sides of
// a binary operation must have STATICALLY COMPATIBLE types. It is the one
// static type check the language performs, and cerberus's in-house parser
// went without it — a gap that surfaced as HTTP 502 rather than 400.
//
// Without the rule a query like `{ name > 3 }` (a String-typed intrinsic
// against an integer literal) parses, lowers, is emitted as SQL, and is
// only rejected by ClickHouse at execution time with NO_COMMON_TYPE
// ("there is no supertype for types String, UInt8"). That reaches the
// client as a gateway error carrying ClickHouse's own error code and type
// names — the wrong status class for a malformed query, and storage-engine
// internals on a Tempo-shaped wire.
//
// The check runs inside Parse, so a mismatch is a parse-stage failure and
// the Tempo head answers it with 400 and the reference backend's own
// wording, exactly as the reference does.
//
// What the rule is NOT: a check that the operand types make sense for the
// OPERATOR (`{ .ok && 3 }`), nor the reference's result-type rules for
// span filters and aggregates. Those are separate rules over the same
// AST; see the doc comment on ValidationError.

// typeMismatchFormat reproduces the reference backend's own wording for a
// mismatched binary operation, verbatim. A client (or a compatibility
// harness) that reads the message sees the same sentence from cerberus and
// from the reference.
const typeMismatchFormat = "binary operations must operate on the same type: %s"

// ValidationError is returned by Parse for a query that parses cleanly but
// breaks one of the language's static typing rules. It is distinct from
// ParseError because nothing about the TEXT was wrong — the grammar
// accepted it and the tree is well formed; the tree is merely
// ill-typed.
//
// Only the binary-operand rule is enforced today. The reference performs
// several further static checks over the same tree (operator/operand
// validity, span-filter and aggregate result types, regex compilability,
// intrinsic-nil); cerberus deliberately accepts some of those shapes
// because it answers queries the reference declines, so adopting them is
// its own piece of work rather than a side effect of this one.
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
//   - An array matches whatever its element type matches, which is how the
//     `in` / `not in` and regex-set forms the array-fold rewrites produce
//     type-check. `{ name = 1 || name = 2 }` folds to `name in [1, 2]`
//     before the rule runs and is still rejected, on the element type.
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
	if t == TypeNil || otherT == TypeNil {
		return true
	}
	return t.isMatchingArrayElement(otherT)
}

// isMatchingArrayElement is isMatchingOperand for the array types: an
// array operand matches whatever its ELEMENT type would match. It is
// checked in both directions so the rule stays symmetric regardless of
// which side the array literal was written on.
func (t StaticType) isMatchingArrayElement(otherT StaticType) bool {
	if elem, ok := t.arrayElement(); ok {
		return elem.isMatchingOperand(otherT)
	}
	if elem, ok := otherT.arrayElement(); ok {
		return elem.isMatchingOperand(t)
	}
	return false
}

// arrayElement returns the element type of an array StaticType; ok is
// false for every non-array type.
func (t StaticType) arrayElement() (StaticType, bool) {
	switch t {
	case TypeIntArray:
		return TypeInt, true
	case TypeFloatArray:
		return TypeFloat, true
	case TypeStringArray:
		return TypeString, true
	case TypeBooleanArray:
		return TypeBoolean, true
	}
	return 0, false
}

// validate reports the first static typing error in r, or nil when the
// whole tree type-checks.
//
// The traversal deliberately mirrors ReferencesIntrinsic's (references.go)
// node for node, down to reusing spansetFilterExpression: both walks answer
// a question about the WHOLE query, so a node one of them reaches and the
// other does not is a bug in whichever is the shorter. The metrics second
// stage is absent from both for the same reason — MetricsFilter,
// TopKBottomK and ChainedSecondStage carry no expression to check.
func (r *RootExpr) validate() error {
	if r == nil {
		return nil
	}
	if err := validatePipeline(r.Pipeline); err != nil {
		return err
	}
	// compare() is the one metrics first stage carrying a field
	// expression; the aggregates carry bare Attributes, which are leaves.
	if cmp, ok := r.MetricsPipeline.(*MetricsCompare); ok && cmp.Filter() != nil {
		return validateFieldExpr(cmp.Filter().Expression)
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
		return validateFieldExpr(expr)
	}
	switch e := elem.(type) {
	case SpansetOperation:
		return validateSpansetExpr(e)
	case ScalarFilter:
		return validateScalarFilter(e.LHS, e.RHS, e)
	case GroupOperation:
		return validateFieldExpr(e.Expression)
	case Aggregate:
		return validateFieldExpr(e.InnerExpr())
	case Pipeline:
		return validatePipeline(e)
	}
	// SelectOperation and CoalesceOperation name attributes and nothing
	// else: an attribute is a leaf, so there is no binary node to check.
	return nil
}

func validateSpansetExpr(se SpansetExpression) error {
	if expr, ok := spansetFilterExpression(se); ok {
		return validateFieldExpr(expr)
	}
	switch s := se.(type) {
	case SpansetOperation:
		if err := validateSpansetExpr(s.LHS); err != nil {
			return err
		}
		return validateSpansetExpr(s.RHS)
	case ScalarFilter:
		return validateScalarFilter(s.LHS, s.RHS, s)
	case Pipeline:
		return validatePipeline(s)
	}
	return nil
}

// validateScalarFilter is the shared body of the two positions a
// ScalarFilter occupies — a pipeline stage and a spanset operand — which
// is also why references.go handles it in both walks.
func validateScalarFilter(lhs, rhs ScalarExpression, node Element) error {
	if err := validateScalarExpr(lhs); err != nil {
		return err
	}
	if err := validateScalarExpr(rhs); err != nil {
		return err
	}
	return matchOperands(lhs, rhs, node)
}

func validateScalarExpr(se ScalarExpression) error {
	switch s := se.(type) {
	case ScalarOperation:
		return validateScalarFilter(s.LHS, s.RHS, s)
	case Aggregate:
		return validateFieldExpr(s.InnerExpr())
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
		return matchOperands(e.LHS, e.RHS, e)
	case UnaryOperation:
		return validateFieldExpr(e.Expression)
	}
	// Attribute and Static are leaves.
	return nil
}

// matchOperands applies the rule itself to one binary node, naming node in
// the message so the client sees which sub-expression is ill-typed.
func matchOperands(lhs, rhs typedExpression, node Element) error {
	if lhs.impliedType().isMatchingOperand(rhs.impliedType()) {
		return nil
	}
	return newValidationError(typeMismatchFormat, node.String())
}
