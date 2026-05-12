package chplan

import "math"

// Expr is a value-producing sub-expression inside a Node (predicates,
// projections, aggregate arguments, etc.).
type Expr interface {
	exprNode()
	// Equal reports structural equality with another Expr.
	Equal(Expr) bool
}

// ColumnRef references a ClickHouse column by name. Quoting and escaping
// happen at emit time.
//
// Qualifier is optional. When non-empty, the emitter renders
// `<Qualifier>.<Name>` (both identifiers backtick-quoted) — useful for
// referencing columns out of a named join side (e.g. `R.SpanName` for
// the right half of a chplan.StructuralJoin). Empty Qualifier emits
// just the bare column name.
type ColumnRef struct {
	Name      string
	Qualifier string
}

func (*ColumnRef) exprNode() {}

func (c *ColumnRef) Equal(other Expr) bool {
	o, ok := other.(*ColumnRef)
	return ok && c.Name == o.Name && c.Qualifier == o.Qualifier
}

// LitString is a string literal. Emitted as a `?` placeholder with the value
// bound as a parameter.
type LitString struct {
	V string
}

func (*LitString) exprNode() {}

func (l *LitString) Equal(other Expr) bool {
	o, ok := other.(*LitString)
	return ok && l.V == o.V
}

// LitInt is a signed integer literal.
type LitInt struct {
	V int64
}

func (*LitInt) exprNode() {}

func (l *LitInt) Equal(other Expr) bool {
	o, ok := other.(*LitInt)
	return ok && l.V == o.V
}

// LitFloat is a 64-bit floating point literal.
type LitFloat struct {
	V float64
}

func (*LitFloat) exprNode() {}

func (l *LitFloat) Equal(other Expr) bool {
	o, ok := other.(*LitFloat)
	if !ok {
		return false
	}
	if math.IsNaN(l.V) && math.IsNaN(o.V) {
		return true
	}
	return l.V == o.V
}

// LitBool is a boolean literal.
type LitBool struct {
	V bool
}

func (*LitBool) exprNode() {}

func (l *LitBool) Equal(other Expr) bool {
	o, ok := other.(*LitBool)
	return ok && l.V == o.V
}
