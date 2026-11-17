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
//   - a filter conjunct that reads no layer, and that no conjunct moved
//     ahead of it, is folded into the stream scan's own filter;
//   - a layer whose value reads only the table's columns and the values
//     already moved, and a filter conjunct that reads only those values
//     or no layer at all, moves into the scan's scope while the scope
//     stays within [maxScanScopeExpansion];
//   - neither happens to a filter conjunct whose own text reads a map
//     column as a whole, or to a value or conjunct whose own text reads a
//     key of a map column that some expression of the stage, or of
//     consumer, reads as a whole in its own text;
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

	// whole holds the map columns some expression of the stage reads as a
	// whole.
	var whole []string
	note := func(e chplan.Expr) {
		forEachMapRead(e, maps, func(name string, keyed bool) {
			if !keyed && !slices.Contains(whole, name) {
				whole = append(whole, name)
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
	// readsWhole reports whether e reads a map column as a whole, and
	// readsWholeKey whether it reads a key of one the stage reads whole.
	readsWhole := func(e chplan.Expr) bool {
		found := false
		forEachMapRead(e, maps, func(_ string, keyed bool) { found = found || !keyed })
		return found
	}
	readsWholeKey := func(e chplan.Expr) bool {
		found := false
		if len(whole) > 0 {
			forEachMapRead(e, maps, func(name string, keyed bool) {
				found = found || keyed && slices.Contains(whole, name)
			})
		}
		return found
	}
	movable := func(e chplan.Expr) bool {
		return !readsWholeKey(e) && scope.admits(e)
	}

	// rest is a layer, or a filter's remaining conjuncts, left above the
	// scope, at its place in the stack.
	type rest struct {
		at        int
		layer     *chplan.Project
		conjuncts []chplan.Expr
	}
	var above []rest
	var layers []*chplan.Project
	var layerAt []int
	var filtered []chplan.Expr
	for i, item := range items {
		switch n := item.(type) {
		case *chplan.Project:
			value := n.Projections[0]
			if movable(value.Expr) {
				scope.add(value.Expr)
				scope.defs[value.Alias] = value.Expr
				layers = append(layers, n)
				layerAt = append(layerAt, i)
				continue
			}
			above = append(above, rest{at: i, layer: n})
		case *chplan.Filter:
			var kept []chplan.Expr
			for _, conjunct := range conjuncts(n.Predicate) {
				switch {
				case readsWhole(conjunct):
					kept = append(kept, conjunct)
				case !scope.readsLayer(conjunct) && len(filtered) == 0:
					base = andFilter(base, conjunct)
				case movable(conjunct):
					scope.add(conjunct)
					filtered = append(filtered, conjunct)
				default:
					kept = append(kept, conjunct)
				}
			}
			if len(kept) > 0 {
				above = append(above, rest{at: i, conjuncts: kept})
			}
		}
	}

	// A moved layer that reads another value stays only when a moved
	// filter reads it: without one, the scope would read an alias where it
	// is defined at no gain, and the layer goes back to its place.
	needed := map[string]bool{}
	for _, conjunct := range filtered {
		addColumnsRead(needed, conjunct)
	}
	kept := layers[:0:0]
	for i := len(layers) - 1; i >= 0; i-- {
		value := layers[i].Projections[0]
		if needed[value.Alias] || !scope.readsLayer(value.Expr) {
			addColumnsRead(needed, value.Expr)
			kept = append(kept, layers[i])
			continue
		}
		above = append(above, rest{at: layerAt[i], layer: layers[i]})
	}
	slices.Reverse(kept)
	slices.SortFunc(above, func(a, b rest) int { return a.at - b.at })

	node := base
	r.atScan = r.atScan[:0]
	for _, layer := range kept {
		layer.Input = node
		node = layer
		r.atScan = append(r.atScan, layer)
	}
	if len(filtered) > 0 {
		node = &chplan.Filter{Input: node, Predicate: conjunction(filtered)}
		r.atScan = append(r.atScan, node)
	}
	for _, item := range above {
		if item.layer != nil {
			item.layer.Input = node
			node = item.layer
			continue
		}
		node = &chplan.Filter{Input: node, Predicate: conjunction(item.conjuncts)}
	}
	r.node = node
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
	found := false
	chplan.InspectExpr(e, func(sub chplan.Expr) bool {
		if ref, ok := sub.(*chplan.ColumnRef); ok && s.aliases[ref.Name] {
			found = true
		}
		return !found
	})
	return found
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
// keyed for a read through a constant key, and not keyed for a read of the
// map as a whole.
func forEachMapRead(e chplan.Expr, maps []string, visit func(name string, keyed bool)) {
	chplan.InspectExpr(e, func(sub chplan.Expr) bool {
		switch v := sub.(type) {
		case *chplan.MapAccess:
			ref, isColumn := v.Map.(*chplan.ColumnRef)
			if _, constant := v.Key.(*chplan.LitString); isColumn && constant && slices.Contains(maps, ref.Name) {
				visit(ref.Name, true)
				return false
			}
		case *chplan.ColumnRef:
			if slices.Contains(maps, v.Name) {
				visit(v.Name, false)
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
