package chplan

// InList is a flat membership test: `<Left> IN (<List[0]>, <List[1]>, ...)`.
//
// The flat shape is load-bearing, not cosmetic. An equivalent nested
// OR-chain of equality Binary nodes grows ClickHouse's parser AST
// depth linearly with the element count, and CH rejects queries past
// `max_parser_depth` (default 1000) with code 306 ("Maximum parse
// depth exceeded"). The /api/search root-span lookup hit exactly that
// when a search returned >1000 traces needing root enrichment. An IN
// tuple parses at constant depth regardless of List length, so plans
// built from caller-controlled collections (trace-ID sets, label
// allow-sets) must use InList instead of an OR-chain.
//
// The emitter renders each List element positionally inside a single
// parenthesised tuple; literal elements ride the usual `?` bound-arg
// path. List must be non-empty — CH rejects an empty IN list with
// "Function 'in' is supported only if the second argument is non-empty"
// — and the emitter surfaces ErrUnsupported for it; callers expressing
// "match nothing" should emit a constant-false predicate instead.
type InList struct {
	Left Expr
	List []Expr
	// Negated flips the membership test to `<Left> NOT IN (…)`. It is the
	// flat, constant-depth equivalent of an AND-chain of `<Left> != Li`
	// inequalities — the negated counterpart to the OR-chain InList
	// replaces. Used by the `__name__!=` matcher lowering, where the
	// dotted-candidate fan-out would otherwise build an unbounded AND-chain
	// (the same parser-depth / query-size blowup as the equality case).
	Negated bool
}

func (*InList) exprNode() {}

func (i *InList) Equal(other Expr) bool {
	o, ok := other.(*InList)
	if !ok || len(i.List) != len(o.List) || i.Negated != o.Negated {
		return false
	}
	if (i.Left == nil) != (o.Left == nil) {
		return false
	}
	if i.Left != nil && !i.Left.Equal(o.Left) {
		return false
	}
	for k := range i.List {
		if !i.List[k].Equal(o.List[k]) {
			return false
		}
	}
	return true
}
