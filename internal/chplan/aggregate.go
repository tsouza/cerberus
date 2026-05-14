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
//
// DropEmptyOnNoGroup controls the PromQL/LogQL "aggregation over empty
// input produces no result" semantics. When true AND GroupBy is empty,
// the emitter wraps the aggregate with a `count() > 0` guard so an
// empty Input projects 0 outer rows instead of CH's default 1-row-of-
// zeros for aggregate-only queries. PromQL / LogQL set it; TraceQL
// (whose `| count() = 0` idiom requires a 0 row for empty input) does
// not. Has no effect when GroupBy is non-empty.
type Aggregate struct {
	Input              Node
	GroupBy            []Expr
	GroupByAliases     []string
	AggFuncs           []AggFunc
	DropEmptyOnNoGroup bool
}

func (*Aggregate) planNode() {}

func (a *Aggregate) Children() []Node { return []Node{a.Input} }

func (a *Aggregate) Equal(other Node) bool {
	o, ok := other.(*Aggregate)
	if !ok || len(a.GroupBy) != len(o.GroupBy) || len(a.AggFuncs) != len(o.AggFuncs) ||
		a.DropEmptyOnNoGroup != o.DropEmptyOnNoGroup {
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
