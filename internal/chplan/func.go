package chplan

// FuncCall is a function-call expression. Args are emitted positionally.
//
// Fn is the sealed, head-agnostic symbol. The SQL emitter resolves it
// through its dialect-specific table; callers cannot inject a raw backend
// function name into the shared plan.
type FuncCall struct {
	Fn   Fn
	Args []Expr
}

func (*FuncCall) exprNode() {}

func (f *FuncCall) Equal(other Expr) bool {
	o, ok := other.(*FuncCall)
	if !ok || f.Fn != o.Fn || len(f.Args) != len(o.Args) {
		return false
	}
	for i := range f.Args {
		if !f.Args[i].Equal(o.Args[i]) {
			return false
		}
	}
	return true
}
