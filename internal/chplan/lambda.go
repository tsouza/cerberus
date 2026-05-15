package chplan

// Lambda is a ClickHouse-style lambda expression — `(p1, p2, ...) -> <body>`
// — used inside higher-order array functions (`arrayMap`, `arrayFilter`,
// `arrayFold`, …). Params is the parameter-name list; Body is the
// expression evaluated for each input tuple.
//
// Lambdas are rendered as bare lambdas (no `function` keyword); the
// emitter writes single-parameter shapes as `p -> body` (no parens)
// and multi-parameter shapes as `(p1, p2) -> body` (with parens) to
// match CH's conventional rendering for the two cases.
//
// Parameter names follow the BareIdent trust contract — must be a
// CH-safe bare identifier `[a-zA-Z_][a-zA-Z0-9_]*`; the caller is
// responsible for ensuring it. Use BareIdent inside the body to
// reference parameter names without going through Ident's backtick
// quoting (which would render `s` as “ `s` “ and bind to a
// non-existent column).
type Lambda struct {
	Params []string
	Body   Expr
}

func (*Lambda) exprNode() {}

func (l *Lambda) Equal(other Expr) bool {
	o, ok := other.(*Lambda)
	if !ok || len(l.Params) != len(o.Params) {
		return false
	}
	for i := range l.Params {
		if l.Params[i] != o.Params[i] {
			return false
		}
	}
	if l.Body == nil || o.Body == nil {
		return l.Body == o.Body
	}
	return l.Body.Equal(o.Body)
}

// BareIdent is an unquoted identifier reference — emitted verbatim by
// the chsql expression renderer. Used inside Lambda bodies to refer to
// the lambda's parameter names without going through ColumnRef's
// backtick quoting (which would treat the param as a column lookup).
//
// The trust contract matches chsql.BareIdent: Name MUST be a CH-safe
// bare identifier `[a-zA-Z_][a-zA-Z0-9_]*`. The caller is responsible
// for ensuring it.
type BareIdent struct {
	Name string
}

func (*BareIdent) exprNode() {}

func (b *BareIdent) Equal(other Expr) bool {
	o, ok := other.(*BareIdent)
	return ok && b.Name == o.Name
}

// Subscript is the array / map element-access shape — `<container>[<key>]`.
// Both Container and Key are arbitrary expressions; the emitter renders
// `<container>[<key>]` with no spaces. Used by the exp-histogram merge
// to read individual elements out of a groupArray-of-arrays (e.g.
// `_hq_pos_buckets[i]` for the i-th per-row bucket array).
type Subscript struct {
	Container Expr
	Key       Expr
}

func (*Subscript) exprNode() {}

func (s *Subscript) Equal(other Expr) bool {
	o, ok := other.(*Subscript)
	if !ok {
		return false
	}
	if s.Container == nil || o.Container == nil {
		if s.Container != o.Container {
			return false
		}
	} else if !s.Container.Equal(o.Container) {
		return false
	}
	if s.Key == nil || o.Key == nil {
		return s.Key == o.Key
	}
	return s.Key.Equal(o.Key)
}
