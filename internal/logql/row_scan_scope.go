package logql

import (
	"slices"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// maxScanScopeExpansion bounds how much of a range aggregation's stack
// [rowLayers.gatherAtScan] moves into the stream scan's scope. In one
// SELECT, ClickHouse's analyzer replaces each read of an alias defined in
// that SELECT with the alias's expression, so the scope is analysed at
// its expanded size — every alias read replaced by what it names — rather
// than at the size of its text. A value or filter joins the scope only
// while the expanded size of everything in it stays within this many
// times the size of its text. The parser and label-filter scopes of the
// shapes the stage lowers to expand to 1.8–2.9 times their text; the
// unwrap conversion chain above them expands past 3.4, and moved into the
// scan's scope it adds seconds of analysis on ClickHouse 24.8.
const maxScanScopeExpansion = 3

// gatherAtScan moves into the stream scan's own scope the part of a range
// aggregation's stack the table is best read with, and leaves every other
// layer and filter above it in its order. It must be called before
// [rowLayers.seal], on a stack whose layers still hold their value alone;
// seal marks the Project that reads the moved part
// [chplan.Project.OneScopeRows], which the emitter renders as one SELECT
// over the table. maps names the table's map columns, and consumer the
// expressions the Project sealed over the stack reads.
//
// Walking the stack from the bottom:
//
//   - a filter conjunct that reads no layer is folded into the stream
//     scan's own filter when no conjunct moved ahead of it, or when its
//     own text reads a key of a map column that some expression of the
//     stage, or of consumer, reads as a whole in its own text; behind a
//     moved conjunct otherwise, it joins the moved filter;
//   - a layer whose value reads only the table's columns and the values
//     already moved, and a filter conjunct that reads only those values,
//     moves into the scan's scope while the scope stays within
//     [maxScanScopeExpansion];
//   - none of this happens to a filter conjunct whose own text reads a
//     map column as a whole, and a value or a conjunct that reads a layer
//     does not move when its own text reads a key of a map column the
//     stage reads as a whole, unless it sits at or below the filter
//     [orderedScanPrefix] finds;
//   - a moved layer that reads another value goes back to its place
//     unless a moved filter reads it.
//
// Read in the scope that reads the table, a map key read resolves to its
// subcolumn and a filter on the table's columns can run in PREWHERE;
// behind a subquery both are lost. The bound keeps a value whose reads of
// another value multiply its expression — the detected_level
// normalisation reads its source once per level it compares, and an
// unwrap conversion reads its parts many times — in its own SELECT, where
// it reads the value below it as a column. A map the stage reads as a
// whole is read whole wherever it is read; a key subcolumn read of it in
// the scan's scope only reads it a second time, and a predicate on the
// whole map in the scan's scope reads it ahead of the rows it filters.
//
// The conjuncts in the scan's scope keep the order the stage gives them,
// which is the pipeline's: ClickHouse runs a multi-step PREWHERE in the
// order of the WHERE conjuncts, and the order the query was written in
// is the one a reader of it can tune.
//
// Moving a value below the layers it does not read changes nothing it
// computes: every layer is a pure per-row value, and an AND of predicates
// over the same row keeps the same rows wherever each of them runs.
func (r *rowLayers) gatherAtScan(maps []string, consumer ...chplan.Expr) {
	var items []chplan.Node
	base := r.node
	for r.stackNode(base) {
		items = append(items, base)
		base = inputOf(base)
	}
	slices.Reverse(items)

	aliases := map[string]bool{}
	for _, layer := range r.layers {
		aliases[layer.Projections[0].Alias] = true
	}
	scope := scanScope{aliases: aliases, defs: map[string]chplan.Expr{}, expanded: map[string]int{}}
	g := scanGather{reads: stageMapReads(maps, consumer, items), base: base}
	g.keyedThrough = orderedScanPrefix(base, items, r.values(maps), g.reads)
	for i, item := range items {
		switch n := item.(type) {
		case *chplan.Project:
			g.takeLayer(&scope, i, n)
		case *chplan.Filter:
			g.takeFilter(&scope, i, n)
		}
	}
	g.demote(&scope)
	r.node = rebuildAtScan(g.base, g.layers, g.filtered, g.above, &r.atScan)
}

// scanGather is the state of one [rowLayers.gatherAtScan] walk: what it
// has moved into the scan's scope and folded into its filter so far, and
// what it leaves above. The scope's sizes are a [scanScope] each method
// is passed.
type scanGather struct {
	reads stageMaps
	// keyedThrough is the place in the stack up to which a value or a
	// conjunct may move although it reads a key of a map the stage reads
	// as a whole ([orderedScanPrefix]); -1 when none may.
	keyedThrough int
	// base is the stream scan with the conjuncts folded into its filter.
	base chplan.Node
	// layers are the moved layers, bottom first, and layerAt each one's
	// place in the stack; filtered are the moved filter conjuncts.
	layers   []*chplan.Project
	layerAt  []int
	filtered []chplan.Expr
	above    []scanRest
}

// scanRest is a layer, or a filter's remaining conjuncts, left above the
// scan's scope, at its place in the stack.
type scanRest struct {
	at        int
	layer     *chplan.Project
	conjuncts []chplan.Expr
}

// movable reports whether e, at place i in the stack, may join scope.
func (g *scanGather) movable(scope *scanScope, i int, e chplan.Expr) bool {
	return (i <= g.keyedThrough || !g.reads.readsWholeKey(e)) && scope.admits(e)
}

// takeLayer moves the layer n, at place i, into the scope or leaves it
// above.
func (g *scanGather) takeLayer(scope *scanScope, i int, n *chplan.Project) {
	value := n.Projections[0]
	if !g.movable(scope, i, value.Expr) {
		g.above = append(g.above, scanRest{at: i, layer: n})
		return
	}
	scope.add(value.Expr)
	scope.defs[value.Alias] = value.Expr
	g.layers = append(g.layers, n)
	g.layerAt = append(g.layerAt, i)
}

// takeFilter folds, moves or leaves above each conjunct of the filter n,
// at place i.
func (g *scanGather) takeFilter(scope *scanScope, i int, n *chplan.Filter) {
	var kept []chplan.Expr
	for _, conjunct := range conjuncts(n.Predicate) {
		switch {
		case g.reads.readsWhole(conjunct):
			kept = append(kept, conjunct)
		case !scope.readsLayer(conjunct) && (len(g.filtered) == 0 || g.reads.readsWholeKey(conjunct)):
			g.base = andFilter(g.base, conjunct)
		case !scope.readsLayer(conjunct):
			// Behind a moved filter it joins the moved ones in order; it
			// reads no value, so it adds nothing to expand.
			g.filtered = append(g.filtered, conjunct)
		case g.movable(scope, i, conjunct):
			scope.add(conjunct)
			g.filtered = append(g.filtered, conjunct)
		default:
			kept = append(kept, conjunct)
		}
	}
	if len(kept) > 0 {
		g.above = append(g.above, scanRest{at: i, conjuncts: kept})
	}
}

// orderedScanPrefix returns the place in items of the filter holding the
// first conjunct that reads a layer and no map column as a whole, when a
// conjunct the pipeline runs after it reads no layer, no map column as a
// whole, and a table column that base's filter, the conjuncts ahead of the
// first one and the first one through the layers it reads do not already
// read. It returns -1 otherwise.
//
// Such a conjunct belongs behind the first one in the scan's scope:
// ClickHouse runs a multi-step PREWHERE in the order of the WHERE
// conjuncts and reads each column at the first step that needs it, so
// folded ahead of the first one it reads its column for the rows the first
// one drops, and left above the scan it is read for every row the scan
// passes up. The first one, and the values it reads, then move into the
// scan's scope even where they read a key of a map the stage reads as a
// whole; the second read of that map costs less than the column read for
// those rows.
func orderedScanPrefix(base chplan.Node, items []chplan.Node, values layerValues, reads stageMaps) int {
	early := tableReads{}
	if f, ok := base.(*chplan.Filter); ok {
		values.addTableReads(early, f.Predicate)
	}
	first := -1
	for i, item := range items {
		f, ok := item.(*chplan.Filter)
		if !ok {
			continue
		}
		for _, conjunct := range conjuncts(f.Predicate) {
			switch {
			case reads.readsWhole(conjunct):
			case values.reads(conjunct):
				if first < 0 {
					values.addTableReads(early, conjunct)
					first = i
				}
			case first < 0:
				values.addTableReads(early, conjunct)
			case !early.covers(values.tableReads(conjunct)):
				return first
			}
		}
	}
	return -1
}

// demote sends back to its place a moved layer that reads another value
// when no moved filter reads it: without one, the scope would read an
// alias where it is defined at no gain.
func (g *scanGather) demote(scope *scanScope) {
	needed := map[string]bool{}
	for _, conjunct := range g.filtered {
		addColumnsRead(needed, conjunct)
	}
	kept := g.layers[:0:0]
	for i := len(g.layers) - 1; i >= 0; i-- {
		value := g.layers[i].Projections[0]
		if needed[value.Alias] || !scope.readsLayer(value.Expr) {
			addColumnsRead(needed, value.Expr)
			kept = append(kept, g.layers[i])
			continue
		}
		g.above = append(g.above, scanRest{at: g.layerAt[i], layer: g.layers[i]})
	}
	slices.Reverse(kept)
	g.layers = kept
	slices.SortFunc(g.above, func(a, b scanRest) int { return a.at - b.at })
}

// rebuildAtScan stacks layers and the filter of filtered over base, then
// above in its order, records the nodes of the part over base in atScan,
// and returns the top of the stack.
func rebuildAtScan(base chplan.Node, layers []*chplan.Project, filtered []chplan.Expr, above []scanRest, atScan *[]chplan.Node) chplan.Node {
	node := base
	*atScan = (*atScan)[:0]
	for _, layer := range layers {
		layer.Input = node
		node = layer
		*atScan = append(*atScan, layer)
	}
	if len(filtered) > 0 {
		node = &chplan.Filter{Input: node, Predicate: conjunction(filtered)}
		*atScan = append(*atScan, node)
	}
	for _, item := range above {
		if item.layer != nil {
			item.layer.Input = node
			node = item.layer
			continue
		}
		node = &chplan.Filter{Input: node, Predicate: conjunction(item.conjuncts)}
	}
	return node
}

// stageMaps is what a stack reads of the table's map columns: their
// names, and those some expression of the stage, or of its consumer,
// reads as a whole in its own text.
type stageMaps struct {
	maps, whole []string
}

// stageMapReads collects the map columns of maps that consumer or an item
// of the stack reads as a whole.
func stageMapReads(maps []string, consumer []chplan.Expr, items []chplan.Node) stageMaps {
	m := stageMaps{maps: maps}
	note := func(e chplan.Expr) {
		forEachMapRead(e, maps, func(name, _ string, keyed bool) {
			if !keyed && !slices.Contains(m.whole, name) {
				m.whole = append(m.whole, name)
			}
		})
	}
	for _, e := range consumer {
		note(e)
	}
	for _, item := range items {
		switch n := item.(type) {
		case *chplan.Project:
			note(n.Projections[0].Expr)
		case *chplan.Filter:
			note(n.Predicate)
		}
	}
	return m
}

// readsWhole reports whether e reads a map column as a whole.
func (m stageMaps) readsWhole(e chplan.Expr) bool {
	found := false
	forEachMapRead(e, m.maps, func(_, _ string, keyed bool) { found = found || !keyed })
	return found
}

// readsWholeKey reports whether e reads a key of a map column the stage
// reads as a whole.
func (m stageMaps) readsWholeKey(e chplan.Expr) bool {
	found := false
	if len(m.whole) > 0 {
		forEachMapRead(e, m.maps, func(name, _ string, keyed bool) {
			found = found || keyed && slices.Contains(m.whole, name)
		})
	}
	return found
}

// markAtScan marks [chplan.Project.OneScopeRows] on the Project directly
// above the part of the sealed stack [rowLayers.gatherAtScan] moved into
// the stream scan's scope, starting the walk at consumer. A part seal
// removed entirely, or one a filter sits directly above, marks nothing.
func (r *rowLayers) markAtScan(consumer *chplan.Project) {
	if len(r.atScan) == 0 {
		return
	}
	var parent chplan.Node = consumer
	for child := inputOf(parent); r.stackNode(child); child = inputOf(child) {
		if slices.Contains(r.atScan, child) {
			if p, ok := parent.(*chplan.Project); ok {
				p.OneScopeRows = true
			}
			return
		}
		parent = child
	}
}

// scanScope is the part of a stack [rowLayers.gatherAtScan] has moved into
// the stream scan's scope so far, and its size in expression nodes: the
// size of its text and its expanded size.
type scanScope struct {
	// aliases are every alias the stack defines; defs those of the
	// values in the scope.
	aliases map[string]bool
	defs    map[string]chplan.Expr
	// expanded memoises each scope alias's expanded size.
	expanded            map[string]int
	text, expandedTotal int
}

// readsLayer reports whether e reads an alias the stack defines.
func (s *scanScope) readsLayer(e chplan.Expr) bool {
	return readsAlias(e, s.aliases)
}

// admits reports whether e — a layer's value or a filter conjunct — reads
// no alias outside the scope and keeps the scope within
// [maxScanScopeExpansion] once added.
func (s *scanScope) admits(e chplan.Expr) bool {
	outside := false
	chplan.InspectExpr(e, func(sub chplan.Expr) bool {
		if ref, ok := sub.(*chplan.ColumnRef); ok && s.aliases[ref.Name] {
			if _, in := s.defs[ref.Name]; !in {
				outside = true
			}
		}
		return !outside
	})
	return !outside && s.expandedTotal+s.expandedSize(e) <= maxScanScopeExpansion*(s.text+exprSize(e))
}

// add counts e into the scope's sizes.
func (s *scanScope) add(e chplan.Expr) {
	s.text += exprSize(e)
	s.expandedTotal += s.expandedSize(e)
}

// expandedSize is e's size with every read of a scope alias replaced by
// the alias's own expanded size.
func (s *scanScope) expandedSize(e chplan.Expr) int {
	n := 0
	chplan.InspectExpr(e, func(sub chplan.Expr) bool {
		ref, ok := sub.(*chplan.ColumnRef)
		if !ok {
			n++
			return true
		}
		def, isScope := s.defs[ref.Name]
		if !isScope {
			n++
			return true
		}
		if _, done := s.expanded[ref.Name]; !done {
			s.expanded[ref.Name] = s.expandedSize(def)
		}
		n += s.expanded[ref.Name]
		return true
	})
	return n
}

// exprSize is the number of nodes of e.
func exprSize(e chplan.Expr) int {
	n := 0
	chplan.InspectExpr(e, func(chplan.Expr) bool {
		n++
		return true
	})
	return n
}

// conjunction ANDs preds left to right.
func conjunction(preds []chplan.Expr) chplan.Expr {
	out := preds[0]
	for _, p := range preds[1:] {
		out = &chplan.Binary{Op: chplan.OpAnd, Left: out, Right: p}
	}
	return out
}

// forEachMapRead calls visit for each read e makes of a column of maps:
// keyed, with the key, for a read through a constant key, and not keyed
// for a read of the map as a whole.
func forEachMapRead(e chplan.Expr, maps []string, visit func(name, key string, keyed bool)) {
	chplan.InspectExpr(e, func(sub chplan.Expr) bool {
		switch v := sub.(type) {
		case *chplan.MapAccess:
			ref, isColumn := v.Map.(*chplan.ColumnRef)
			if key, constant := v.Key.(*chplan.LitString); isColumn && constant && slices.Contains(maps, ref.Name) {
				visit(ref.Name, key.V, true)
				return false
			}
		case *chplan.ColumnRef:
			if slices.Contains(maps, v.Name) {
				visit(v.Name, "", false)
			}
		}
		return true
	})
}

// logMapColumns names the map columns of the logs table s.
func logMapColumns(s schema.Logs) []string {
	var maps []string
	for _, name := range []string{s.ResourceAttributesColumn, s.AttributesColumn, s.ScopeAttributesColumn} {
		if name != "" {
			maps = append(maps, name)
		}
	}
	return maps
}

// projectionExprs returns the expression of each projection.
func projectionExprs(projections []chplan.Projection) []chplan.Expr {
	exprs := make([]chplan.Expr, len(projections))
	for i, p := range projections {
		exprs[i] = p.Expr
	}
	return exprs
}
