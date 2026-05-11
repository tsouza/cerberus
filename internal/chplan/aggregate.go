package chplan

// AggFunc is one aggregate-function call in an Aggregate node's projection
// list. The emitter renders `<Name>(<args>) AS <alias>` (alias optional).
type AggFunc struct {
	Name  string // ClickHouse function name: "sum", "count", "avg", "max", "min", ...
	Args  []Expr
	Alias string
}

// Equal reports structural equality with another AggFunc.
func (a AggFunc) Equal(other AggFunc) bool {
	if a.Name != other.Name || a.Alias != other.Alias || len(a.Args) != len(other.Args) {
		return false
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
type Aggregate struct {
	Input    Node
	GroupBy  []Expr
	AggFuncs []AggFunc
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
