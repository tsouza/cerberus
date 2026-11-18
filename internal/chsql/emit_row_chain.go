package chsql

import (
	"slices"

	"github.com/tsouza/cerberus/internal/chplan"
)

// rowChain is a LogQL per-row stage, or the part of one that reads the
// table, over one filtered table scan, as the Input of a Project marked
// [chplan.Project.OneScopeRows]:
// each layer passes columns through by name and defines values under
// aliases, and each filter reads those aliases. fusedRowChain renders it
// as ONE `SELECT … FROM <table> WHERE …` whose select list defines every
// alias and whose WHERE reads them by name, where the nested form would
// put each filter in an outer scope over a subquery.
//
// Reading the table's columns in the scope that also filters on them is
// what lets ClickHouse resolve a map key read to its subcolumn and defer
// reading a column until the filter has passed a row; behind a subquery
// both are lost.
type rowChain struct {
	scanFilter *chplan.Filter
	scan       *chplan.Scan
	// layers are the chain's Projects, bottom first.
	layers []*chplan.Project
	// filters are the chain's Filters above the scan's, bottom first.
	filters []rowChainFilter
}

// rowChainFilter is one Filter of a [rowChain] and the number of the
// chain's layers below it.
type rowChainFilter struct {
	predicate chplan.Expr
	above     int
}

// rowChainOf returns the chain n heads, or false when n is not one: a
// Filter or a Project of the chain's shape with a Filter over a Scan at
// the bottom, which fusing changes without reading an alias where it is
// defined at no gain. That is a chain with at least one Filter above a
// layer, or one with layers whose values all read the table's columns
// alone: fused, those values are computed in the scope that reads the
// table, and none of them reads another's alias.
func rowChainOf(n chplan.Node) (rowChain, bool) {
	var c rowChain
	// Walking top-down, each filter first records how many layers sit
	// above it; once the bottom is reached that becomes how many sit below.
	for {
		switch v := n.(type) {
		case *chplan.Filter:
			if scan, ok := v.Input.(*chplan.Scan); ok {
				c.scanFilter, c.scan = v, scan
				filterAboveLayer := len(c.filters) > 0 && c.filters[0].above < len(c.layers)
				if !filterAboveLayer && (len(c.layers) == 0 || c.layersReadAlias()) {
					return rowChain{}, false
				}
				for i := range c.filters {
					c.filters[i].above = len(c.layers) - c.filters[i].above
				}
				slices.Reverse(c.layers)
				slices.Reverse(c.filters)
				return c, c.namesAreUnambiguous()
			}
			c.filters = append(c.filters, rowChainFilter{predicate: v.Predicate, above: len(c.layers)})
			n = v.Input
		case *chplan.Project:
			if !isRowLayer(v) {
				return rowChain{}, false
			}
			c.layers = append(c.layers, v)
			n = v.Input
		default:
			return rowChain{}, false
		}
	}
}

// isRowLayer reports whether p has a layer's shape: an explicit select
// list of bare columns passed through by name and aliased values. That
// each value reads its own row alone is what the
// [chplan.Project.OneScopeRows] mark above the chain vouches for.
func isRowLayer(p *chplan.Project) bool {
	if len(p.Projections) == 0 || len(p.Replacements) > 0 || p.SubqueryGrid {
		return false
	}
	for _, pr := range p.Projections {
		if _, ok := pr.Expr.(*chplan.ColumnRef); pr.Alias == "" && !ok {
			return false
		}
	}
	return true
}

// layersReadAlias reports whether a value one of the chain's layers
// defines reads an alias the chain defines.
func (c rowChain) layersReadAlias() bool {
	aliases := map[string]bool{}
	for _, layer := range c.layers {
		for _, pr := range layer.Projections {
			if pr.Alias != "" {
				aliases[pr.Alias] = true
			}
		}
	}
	for _, layer := range c.layers {
		for _, pr := range layer.Projections {
			if pr.Alias == "" {
				continue
			}
			reads := false
			chplan.InspectExpr(pr.Expr, func(e chplan.Expr) bool {
				if ref, ok := e.(*chplan.ColumnRef); ok && aliases[ref.Name] {
					reads = true
				}
				return !reads
			})
			if reads {
				return true
			}
		}
	}
	return false
}

// namesAreUnambiguous reports whether every name the chain reads means
// in one SELECT what it meant where it was read: each alias is defined
// once, and no node reads under an alias's name a table column — which
// it does when the alias is defined above that node, or not at all
// below it. Each layer passing a column through by name reads it too.
func (c rowChain) namesAreUnambiguous() bool {
	definedAt := map[string]int{}
	for i, layer := range c.layers {
		for _, pr := range layer.Projections {
			if pr.Alias == "" {
				continue
			}
			if _, dup := definedAt[pr.Alias]; dup {
				return false
			}
			definedAt[pr.Alias] = i
		}
	}
	// readsTable reports whether expr, read by a node with below layers
	// under it, reads a table column some layer's alias names.
	readsTable := func(expr chplan.Expr, below int) bool {
		clash := false
		chplan.InspectExpr(expr, func(e chplan.Expr) bool {
			if ref, ok := e.(*chplan.ColumnRef); ok {
				if at, defined := definedAt[ref.Name]; defined && at >= below {
					clash = true
				}
			}
			return !clash
		})
		return clash
	}
	if readsTable(c.scanFilter.Predicate, 0) {
		return false
	}
	for i, layer := range c.layers {
		for _, pr := range layer.Projections {
			if readsTable(pr.Expr, i) {
				return false
			}
		}
	}
	for _, f := range c.filters {
		if readsTable(f.predicate, f.above) {
			return false
		}
	}
	return true
}

// fusedRowChain renders c as one SELECT: the top layer's table columns
// and every alias the chain defines, bottom first, over the scan, with
// the chain's filters joined to the scan's own predicates. Its columns
// are a superset of the top layer's, so only a consumer that selects its
// columns by name reads it.
func (e *emitter) fusedRowChain(c rowChain) (*QueryBuilder, error) {
	sb, err := filterScanQuery(c.scanFilter, c.scan, e.strategies)
	if err != nil {
		return nil, err
	}
	sb.selectList = nil
	defined := map[string]bool{}
	for _, layer := range c.layers {
		for _, pr := range layer.Projections {
			if pr.Alias != "" {
				defined[pr.Alias] = true
			}
		}
	}
	for _, pr := range c.layers[len(c.layers)-1].Projections {
		if ref, ok := pr.Expr.(*chplan.ColumnRef); ok && pr.Alias == "" && !defined[ref.Name] {
			sb.Select(Col(ref.Name))
		}
	}
	for _, layer := range c.layers {
		for _, pr := range layer.Projections {
			if pr.Alias == "" {
				continue
			}
			expr := pr.Expr
			sb.SelectAs(func(b *Builder) { _ = b.Expr(projectedExpr(b, expr)) }, pr.Alias)
		}
	}
	if len(c.filters) == 0 {
		return sb, nil
	}
	predicates := make([]chplan.Expr, len(c.filters))
	for i, f := range c.filters {
		predicates[i] = f.predicate
	}
	sb.Where(conjunctionFrag(predicates))
	return sb, nil
}

// rowChainFrag returns the FROM source a Project marked
// [chplan.Project.OneScopeRows], with an explicit select list, reads its
// Input through: the fused SELECT when the Input heads a [rowChain], and
// false otherwise.
func (e *emitter) rowChainFrag(input chplan.Node) (Frag, bool, error) {
	c, ok := rowChainOf(input)
	if !ok || e.fusionExpandsPastBound(c) {
		return nil, false, nil
	}
	sb, err := e.fusedRowChain(c)
	if err != nil {
		return nil, false, err
	}
	sb.WithSchemaStrategies(e.strategies)
	sb.env = e
	sql, args, err := sb.subquerySQL()
	if err != nil {
		return nil, false, err
	}
	if err := e.requireEmittedSQLBounded(input, sql); err != nil {
		return nil, false, err
	}
	return Subquery(PreRenderedSQL{SQL: sql, Args: args, PhysicalScans: sb.physicalScans()}), true, nil
}

// fusionExpandsPastBound reports whether c, rendered as one SELECT, would
// be past the emitted-SQL byte bound ([emitter.emittedSQLByteBound]) once
// every alias read in it is replaced by the expression it names. That
// replacement is what ClickHouse's analyzer does with an alias read in
// the scope that defines it, and it is the statement an inlining lowering
// would have rendered: a labels map that reads the previous one several
// times, stage after stage, multiplies the copies. Past the bound the
// chain is rendered nested, where each alias is a column of the subquery
// below and is analyzed once.
func (e *emitter) fusionExpandsPastBound(c rowChain) bool {
	bound := e.emittedSQLByteBound()
	defs := map[string]chplan.Expr{}
	for _, layer := range c.layers {
		for _, pr := range layer.Projections {
			if pr.Alias != "" {
				defs[pr.Alias] = pr.Expr
			}
		}
	}
	expanded := map[string]int64{}
	// size returns the rendered length of expr with every alias read in
	// it replaced by its expanded definition, capped just past bound.
	var size func(expr chplan.Expr, project bool) int64
	size = func(expr chplan.Expr, project bool) int64 {
		b := &Builder{strategies: e.strategies, env: e}
		if project {
			_ = b.Expr(projectedExpr(b, expr))
		} else {
			_ = b.Expr(expr)
		}
		n := int64(b.sb.Len())
		chplan.InspectExpr(expr, func(sub chplan.Expr) bool {
			ref, isRef := sub.(*chplan.ColumnRef)
			if !isRef {
				return true
			}
			def, isAlias := defs[ref.Name]
			if !isAlias {
				return true
			}
			if _, done := expanded[ref.Name]; !done {
				expanded[ref.Name] = size(def, false)
			}
			n += expanded[ref.Name]
			return n <= bound
		})
		return min(n, bound+1)
	}
	total := int64(0)
	for _, def := range defs {
		total += size(def, true)
	}
	for _, f := range c.filters {
		total += size(f.predicate, false)
	}
	return total > bound
}
