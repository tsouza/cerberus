package chplan

// Projection is one SELECT-list entry: an expression and an optional alias.
// The emitter renders `<expr> AS <alias>` when alias is non-empty.
type Projection struct {
	Expr  Expr
	Alias string
}

// Equal reports structural equality with another Projection.
func (p Projection) Equal(other Projection) bool {
	return p.Alias == other.Alias && p.Expr.Equal(other.Expr)
}

// Project narrows or reshapes the columns flowing through it — the SELECT
// list of the eventual SQL. An empty Projections slice means "pass through
// all columns from Input"; the emitter renders that as `*`.
type Project struct {
	Input       Node
	Projections []Projection
}

func (*Project) planNode() {}

func (p *Project) Children() []Node { return []Node{p.Input} }

func (p *Project) Equal(other Node) bool {
	o, ok := other.(*Project)
	if !ok || len(p.Projections) != len(o.Projections) {
		return false
	}
	for i := range p.Projections {
		if !p.Projections[i].Equal(o.Projections[i]) {
			return false
		}
	}
	return p.Input.Equal(o.Input)
}
