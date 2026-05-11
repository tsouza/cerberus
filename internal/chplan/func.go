package chplan

// FuncCall is a function-call expression. The emitter passes Name through to
// ClickHouse verbatim, so callers must use names CH knows (or that we wrap
// with a User-Defined Function). Args are emitted positionally.
type FuncCall struct {
	Name string
	Args []Expr
}

func (*FuncCall) exprNode() {}

func (f *FuncCall) Equal(other Expr) bool {
	o, ok := other.(*FuncCall)
	if !ok || f.Name != o.Name || len(f.Args) != len(o.Args) {
		return false
	}
	for i := range f.Args {
		if !f.Args[i].Equal(o.Args[i]) {
			return false
		}
	}
	return true
}
