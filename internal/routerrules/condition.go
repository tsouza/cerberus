package routerrules

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chsql"
)

// Condition is the strongly-typed, validated condition AST. It is the heart of
// the no-numbers invariant: the node set below has NO number-literal node, so a
// bare number in a comparison operand position is unrepresentable, not merely
// rejected. A numeric operand can only be a ParamCond (a reference to a resolved
// named parameter). The same AST lowers to a chsql Frag (the CH path) and to an
// in-Go row matcher (the JSONL path), so both backends share identical
// semantics.
type Condition interface {
	// frag lowers the condition to a typed chsql expression, binding every
	// parameter reference to its resolved value from env. Returns an error if a
	// referenced parameter is missing or is partition-keyed (partition-keyed
	// params are resolved per-group during evaluation, not embedded in the
	// WHERE clause).
	frag(env Env) (chsql.Frag, error)
	// match evaluates the condition against a decoded corpus row, binding
	// parameters from env. The same missing/partition-keyed contract as frag
	// applies.
	match(row corpusRow, env Env) (bool, error)
	// paramRefs appends every parameter name this subtree references.
	paramRefs(acc map[string]struct{})
}

// CmpOp is the closed set of leaf comparison operators.
type CmpOp uint8

const (
	OpEq CmpOp = iota
	OpGt
	OpGte
	OpLt
	OpLte
	OpIn
)

func parseCmpOp(s string) (CmpOp, bool) {
	switch s {
	case "eq":
		return OpEq, true
	case "gt":
		return OpGt, true
	case "gte":
		return OpGte, true
	case "lt":
		return OpLt, true
	case "lte":
		return OpLte, true
	case "in":
		return OpIn, true
	default:
		return 0, false
	}
}

// AndCond is the conjunction of its children.
type AndCond struct{ Children []Condition }

// OrCond is the disjunction of its children.
type OrCond struct{ Children []Condition }

// NotCond is the negation of its child.
type NotCond struct{ Child Condition }

// EnumCmp compares an enum column against one or more category tokens. The
// tokens are domain categories (route='A', exit_status IN ('oom')), never
// tunable numbers — so this node is invariant-safe by construction.
type EnumCmp struct {
	Column string
	Op     CmpOp // OpEq or OpIn
	Values []string
}

// ParamCmp compares a numeric corpus column against a RESOLVED named parameter.
// This is the ONLY node that yields a numeric comparison, and the number is
// never present in the AST — only the parameter Name is. The value is bound
// from env at lowering/matching time.
type ParamCmp struct {
	Column string
	Op     CmpOp // OpGt/OpGte/OpLt/OpLte/OpEq
	Param  string
}

// --- paramRefs -------------------------------------------------------------

func (c *AndCond) paramRefs(acc map[string]struct{}) {
	for _, ch := range c.Children {
		ch.paramRefs(acc)
	}
}

func (c *OrCond) paramRefs(acc map[string]struct{}) {
	for _, ch := range c.Children {
		ch.paramRefs(acc)
	}
}

func (c *NotCond) paramRefs(acc map[string]struct{}) { c.Child.paramRefs(acc) }

func (c *EnumCmp) paramRefs(map[string]struct{}) {}

func (c *ParamCmp) paramRefs(acc map[string]struct{}) { acc[c.Param] = struct{}{} }

// --- frag (lowering to chsql) ----------------------------------------------

func (c *AndCond) frag(env Env) (chsql.Frag, error) {
	parts, err := childFrags(c.Children, env)
	if err != nil {
		return nil, err
	}
	return chsql.And(parts...), nil
}

func (c *OrCond) frag(env Env) (chsql.Frag, error) {
	parts, err := childFrags(c.Children, env)
	if err != nil {
		return nil, err
	}
	return chsql.Or(parts...), nil
}

func (c *NotCond) frag(env Env) (chsql.Frag, error) {
	inner, err := c.Child.frag(env)
	if err != nil {
		return nil, err
	}
	return chsql.Not(inner), nil
}

func (c *EnumCmp) frag(Env) (chsql.Frag, error) {
	col := chsql.BareIdent(c.Column)
	switch c.Op {
	case OpEq:
		return chsql.Eq(col, chsql.InlineLit(c.Values[0])), nil
	case OpIn:
		lits := make([]chsql.Frag, len(c.Values))
		for i, v := range c.Values {
			lits[i] = chsql.InlineLit(v)
		}
		return chsql.In(col, lits...), nil
	default:
		return nil, fmt.Errorf("routerrules: enum column %q only supports eq/in", c.Column)
	}
}

func (c *ParamCmp) frag(env Env) (chsql.Frag, error) {
	val, ok := env[c.Param]
	if !ok {
		return nil, fmt.Errorf("routerrules: condition references unresolved param %q", c.Param)
	}
	if val.IsPartitioned() {
		return nil, fmt.Errorf("routerrules: param %q is partition-keyed and cannot be used in a flat WHERE clause", c.Param)
	}
	col := chsql.BareIdent(c.Column)
	lit := chsql.InlineLit(val.Scalar)
	return cmpFrag(c.Op, col, lit)
}

func cmpFrag(op CmpOp, l, r chsql.Frag) (chsql.Frag, error) {
	switch op {
	case OpEq:
		return chsql.Eq(l, r), nil
	case OpGt:
		return chsql.Gt(l, r), nil
	case OpGte:
		return chsql.Gte(l, r), nil
	case OpLt:
		return chsql.Lt(l, r), nil
	case OpLte:
		return chsql.Lte(l, r), nil
	default:
		return nil, fmt.Errorf("routerrules: operator %d not valid for a numeric param comparison", op)
	}
}

func childFrags(children []Condition, env Env) ([]chsql.Frag, error) {
	parts := make([]chsql.Frag, len(children))
	for i, ch := range children {
		f, err := ch.frag(env)
		if err != nil {
			return nil, err
		}
		parts[i] = f
	}
	return parts, nil
}

// --- match (in-Go evaluation for the JSONL path) ---------------------------

func (c *AndCond) match(row corpusRow, env Env) (bool, error) {
	for _, ch := range c.Children {
		ok, err := ch.match(row, env)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

func (c *OrCond) match(row corpusRow, env Env) (bool, error) {
	for _, ch := range c.Children {
		ok, err := ch.match(row, env)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

func (c *NotCond) match(row corpusRow, env Env) (bool, error) {
	ok, err := c.Child.match(row, env)
	if err != nil {
		return false, err
	}
	return !ok, nil
}

func (c *EnumCmp) match(row corpusRow, _ Env) (bool, error) {
	got := row.enumValue(c.Column)
	for _, v := range c.Values {
		if got == v {
			return true, nil
		}
	}
	return false, nil
}

func (c *ParamCmp) match(row corpusRow, env Env) (bool, error) {
	val, ok := env[c.Param]
	if !ok {
		return false, fmt.Errorf("routerrules: condition references unresolved param %q", c.Param)
	}
	if val.IsPartitioned() {
		return false, fmt.Errorf("routerrules: param %q is partition-keyed and cannot be used as a flat row predicate", c.Param)
	}
	lhs := row.numericValue(c.Column)
	rhs := val.Scalar
	switch c.Op {
	case OpEq:
		return lhs == rhs, nil
	case OpGt:
		return lhs > rhs, nil
	case OpGte:
		return lhs >= rhs, nil
	case OpLt:
		return lhs < rhs, nil
	case OpLte:
		return lhs <= rhs, nil
	default:
		return false, fmt.Errorf("routerrules: operator %d not valid for a numeric param comparison", c.Op)
	}
}

// lowerPredicate converts a decoded Predicate (from YAML) into a validated
// Condition AST. It is the single chokepoint that REFUSES to construct a
// number-literal operand: a leaf comparison's operand is either an enum
// category list or a param reference — there is no third option, so a number
// can never enter the AST. Structural errors (a leaf that sets both a combinator
// and a column, an unknown operator) are returned here.
func lowerPredicate(p Predicate) (Condition, error) {
	combinators := 0
	if len(p.All) > 0 {
		combinators++
	}
	if len(p.Any) > 0 {
		combinators++
	}
	if p.Not != nil {
		combinators++
	}
	isLeaf := p.Col != ""

	if combinators > 1 {
		return nil, fmt.Errorf("routerrules: predicate sets more than one of all/any/not")
	}
	if combinators == 1 && isLeaf {
		return nil, fmt.Errorf("routerrules: predicate mixes a combinator with a leaf comparison")
	}
	if combinators == 0 && !isLeaf {
		return nil, fmt.Errorf("routerrules: empty predicate (no all/any/not and no col)")
	}

	switch {
	case len(p.All) > 0:
		children, err := lowerPredicates(p.All)
		if err != nil {
			return nil, err
		}
		return &AndCond{Children: children}, nil
	case len(p.Any) > 0:
		children, err := lowerPredicates(p.Any)
		if err != nil {
			return nil, err
		}
		return &OrCond{Children: children}, nil
	case p.Not != nil:
		child, err := lowerPredicate(*p.Not)
		if err != nil {
			return nil, err
		}
		return &NotCond{Child: child}, nil
	default:
		return lowerLeaf(p)
	}
}

func lowerPredicates(ps []Predicate) ([]Condition, error) {
	out := make([]Condition, len(ps))
	for i, p := range ps {
		c, err := lowerPredicate(p)
		if err != nil {
			return nil, err
		}
		out[i] = c
	}
	return out, nil
}

func lowerLeaf(p Predicate) (Condition, error) {
	op, ok := parseCmpOp(p.Op)
	if !ok {
		return nil, fmt.Errorf("routerrules: leaf on column %q has unknown op %q", p.Col, p.Op)
	}
	hasEnum := p.Enum != nil
	hasParam := p.Param != ""
	if hasEnum == hasParam {
		return nil, fmt.Errorf("routerrules: leaf on column %q must set exactly one of 'enum' or 'param'", p.Col)
	}
	if hasEnum {
		vals, err := enumValues(p.Enum)
		if err != nil {
			return nil, fmt.Errorf("routerrules: column %q: %w", p.Col, err)
		}
		if op != OpEq && op != OpIn {
			return nil, fmt.Errorf("routerrules: column %q enum comparison must use eq or in, got %q", p.Col, p.Op)
		}
		return &EnumCmp{Column: p.Col, Op: op, Values: vals}, nil
	}
	if op == OpIn {
		return nil, fmt.Errorf("routerrules: column %q param comparison cannot use in", p.Col)
	}
	return &ParamCmp{Column: p.Col, Op: op, Param: p.Param}, nil
}

// enumValues coerces the YAML enum operand into a token list. It accepts a
// single string or a sequence of strings. A numeric scalar (int/float) is
// REFUSED here — this is the leaf-level guard that keeps numbers out of enum
// slots; combined with the absence of a number-literal AST node, no comparison
// operand path admits a number.
func enumValues(v any) ([]string, error) {
	switch x := v.(type) {
	case string:
		return []string{x}, nil
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("enum list element is not a string: %v (%T)", e, e)
			}
			out = append(out, s)
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("enum list is empty")
		}
		return out, nil
	default:
		return nil, fmt.Errorf("enum operand must be a string or string list, got %T", v)
	}
}
