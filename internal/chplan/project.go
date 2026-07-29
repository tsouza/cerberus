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

	// Replacements rewrites named columns IN PLACE on the pass-through
	// path, rendered as ClickHouse's `* REPLACE (<expr> AS <alias>)`
	// asterisk modifier. Each entry's Alias names an existing column of
	// Input and its Expr is the replacement value; the column keeps its
	// name, so nothing downstream has to be rewritten to see the new
	// value. That is what distinguishes it from Projections, which
	// REPLACE the whole SELECT list and therefore force every consumer
	// to be aware of the new shape.
	//
	// Only meaningful when Projections is empty — an explicit SELECT
	// list already states every column, so it has nothing to modify.
	Replacements []Projection
}

func (*Project) planNode() {}

func (p *Project) Children() []Node { return []Node{p.Input} }

func (p *Project) Equal(other Node) bool {
	o, ok := other.(*Project)
	if !ok || len(p.Projections) != len(o.Projections) || len(p.Replacements) != len(o.Replacements) {
		return false
	}
	for i := range p.Projections {
		if !p.Projections[i].Equal(o.Projections[i]) {
			return false
		}
	}
	for i := range p.Replacements {
		if !p.Replacements[i].Equal(o.Replacements[i]) {
			return false
		}
	}
	return p.Input.Equal(o.Input)
}
