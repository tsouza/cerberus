package chplan

// AggFunc is one aggregate-function call in an Aggregate node's projection
// list. The emitter renders `<Name>(<Args>) AS <Alias>` for plain aggregates
// and `<Name>(<Params>)(<Args>) AS <Alias>` for parameterised aggregates
// (e.g. CH `quantile(0.95)(value)`).
type AggFunc struct {
	Name string // ClickHouse function name: "sum", "count", "avg", "max", "min", ...
	// Params is the parameter list for parameterised aggregates (CH-style).
	// Nil/empty for plain aggregates.
	Params []Expr
	Args   []Expr
	Alias  string
}

// Equal reports structural equality with another AggFunc.
func (a AggFunc) Equal(other AggFunc) bool {
	if a.Name != other.Name || a.Alias != other.Alias ||
		len(a.Args) != len(other.Args) || len(a.Params) != len(other.Params) {
		return false
	}
	for i := range a.Params {
		if !a.Params[i].Equal(other.Params[i]) {
			return false
		}
	}
	for i := range a.Args {
		if !a.Args[i].Equal(other.Args[i]) {
			return false
		}
	}
	return true
}

// Aggregate groups rows by the GroupBy expressions and computes the
// AggFuncs. SQL form: `SELECT <GroupBy>, <AggFuncs> FROM <Input> GROUP BY
// <GroupBy>`.
//
// GroupByAliases is an optional parallel slice (same length as GroupBy
// when non-empty); each entry aliases the matching group-key column in
// the SELECT list so a wrapping Project can reference it by name. Empty
// means "no aliases" — emit the raw expression.
type Aggregate struct {
	Input          Node
	GroupBy        []Expr
	GroupByAliases []string
	AggFuncs       []AggFunc
}

func (*Aggregate) planNode() {}

func (a *Aggregate) Children() []Node { return []Node{a.Input} }

func (a *Aggregate) Equal(other Node) bool {
	o, ok := other.(*Aggregate)
	if !ok || len(a.GroupBy) != len(o.GroupBy) || len(a.AggFuncs) != len(o.AggFuncs) {
		return false
	}
	for i := range a.GroupBy {
		if !a.GroupBy[i].Equal(o.GroupBy[i]) {
			return false
		}
	}
	for i := range a.AggFuncs {
		if !a.AggFuncs[i].Equal(o.AggFuncs[i]) {
			return false
		}
	}
	return a.Input.Equal(o.Input)
}
