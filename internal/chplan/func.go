package chplan

// FuncCall is a function-call expression. Args are emitted positionally.
//
// Exactly one of Fn or Name identifies the function: Fn is the sealed,
// engine-agnostic symbol (internal/chsql resolves it through a per-dialect
// table); Name is the legacy raw ClickHouse spelling, passed to the
// emitter verbatim. Setting both is a construction error the emitter
// rejects rather than silently preferring one — see chsql.exprFunc.
// Name survives alongside Fn only until #2060 PR 6 deletes it once every
// construction site has migrated.
type FuncCall struct {
	Fn   Fn
	Name string
	Args []Expr
}

func (*FuncCall) exprNode() {}

func (f *FuncCall) Equal(other Expr) bool {
	o, ok := other.(*FuncCall)
	if !ok || f.Fn != o.Fn || f.Name != o.Name || len(f.Args) != len(o.Args) {
		return false
	}
	for i := range f.Args {
		if !f.Args[i].Equal(o.Args[i]) {
			return false
		}
	}
	return true
}
