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
// while the expanded size of everything counted in it stays within this
// many times the size of its text. The parser and label-filter scopes of
// the shapes the stage lowers to expand to 1.8–2.9 times their text; the
// unwrap conversion chain above them expands past 3.4, and moved into the
// scan's scope it adds seconds of analysis on ClickHouse 24.8.
//
// Three kinds of filter conjunct join past the bound, each when it reads
// only values already in the scope; none adds a layer, only its own reads
// of the scope's aliases. The first is the first conjunct of an ordered
// prefix ([scanGather.first]). A detected_level regex matcher as that one
// reads the level source three times, through two lower()s and its regex,
// which puts some patterns past the bound; left above, the scan reads the
// body for every row the level drops — 27,874,186 bytes more on 24.8 for
// `| detected_level=~"err.*" |= "error"` — while in the scope the query
// runs in 0.9 s against 6.0 s for the same stack inlined into the scan's
// WHERE. The second is a conjunct behind the first one: left above the
// scan, it leaves the scan reading more of the table than pipeline order
// needs — 120,838 bytes more on 24.8 for a `| json | status >= 400` filter
// between a level filter and a line filter — while the scope it joins
// analyses and runs in 2.6 s, against 19.3 s for the same stack inlined
// into the scan's WHERE. The third is a conjunct a moved one overtakes
// ([scanGather.admitted]), which is not counted, so the values and
// conjuncts after it move as they would without it: left above, the
// statement for `| detected_level=~"error|fatal" | logfmt | status="500"`
// parses the body for every row the level drops and peaks at 85 MiB in
// 33–37 s on 24.8, against 46 MiB in 7–11 s with the level in the scope.
// The emitted-SQL byte bound still applies to the whole scope, past which
// the chain renders nested.
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
//     stage, or of consumer, reads as a whole in its own text, unless it
//     follows the moved first conjunct of an ordered prefix; behind a
//     moved conjunct otherwise, it joins the moved filter;
//   - a layer whose value reads only the table's columns and the values
//     already moved, and a filter conjunct that reads only those values,
//     moves into the scan's scope while the scope stays within
//     [maxScanScopeExpansion];
//   - none of this happens to a filter conjunct whose own text reads a
//     map column as a whole, except, without inWhereOrder, one that
//     [wholeMapFiltersAhead] returns, which folds or joins the moved filter
//     as a conjunct that reads no layer does; and a value or a conjunct
//     that reads a layer does not move when its own text reads a key of a
//     map column the stage reads as a whole;
//   - with inWhereOrder, which reports that the servers run a multi-step
//     PREWHERE in the order of the WHERE conjuncts, the rule above is lifted
//     up to the filter [orderedScanPrefix] finds, when that keeps the first
//     filter it names in the scan's scope, and that first one, and a filter
//     conjunct after it and up to that filter, joins the scope whatever the
//     scope's expansion, as long as it reads only values in the scope;
//   - with inWhereOrder too, a filter conjunct left above that a later
//     moved conjunct overtakes ([scanGather.overtaken]) joins the scope
//     whatever the scope's expansion, as long as it reads only values in
//     the scope, and counts toward none of it, so every value and conjunct
//     that moved without it still moves; the rule for map columns read as
//     a whole holds for it as for the rest;
//   - without inWhereOrder, a filter conjunct that reads the detected_level
//     source joins the scope whatever the scope's expansion, as long as it
//     reads only values in the scope, and so does the normalised level
//     when it reads only values in the scope and a moved filter reads its
//     source; otherwise the level stays in its place;
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
// the scan's scope only reads it a second time, and on a server that keeps
// WHERE order a predicate on the whole map in the scan's scope reads it
// ahead of the rows the conjuncts after it filter.
//
// The conjuncts in the scan's scope keep the order the stage gives them,
// which is the pipeline's: a server that keeps WHERE order
// (chopt.PrewhereKeepsWhereOrder) runs a multi-step PREWHERE in that
// order, and the order the query was written in is the one a reader of it
// can tune. A server that reorders the conditions chooses their order
// itself.
//
// A detected_level filter reads the level source, so in the scan's scope
// it runs ahead of a later line filter. A server that reorders the
// conditions moves it to PREWHERE, and a scan whose output carries the
// source without the normalised level hands on an empty string for a line
// with no level: on 26.8 the reader then sizes its blocks for a narrower
// row, and an aggregation whose filter keeps every row peaks at about
// twice the memory. Computed in the scan's scope beside the source, the
// level is what the scan hands on.
//
// Moving a value below the layers it does not read changes nothing it
// computes: every layer is a pure per-row value, and an AND of predicates
// over the same row keeps the same rows wherever each of them runs.
func (r *rowLayers) gatherAtScan(maps []string, inWhereOrder bool, consumer ...chplan.Expr) {
	var items []chplan.Node
	base := r.node
	for r.stackNode(base) {
		items = append(items, base)
		base = inputOf(base)
	}
	slices.Reverse(items)

	reads := stageMapReads(maps, consumer, items)
	values := r.values(maps)
	if !inWhereOrder {
		r.node = r.rebuild(r.gather(base, items, reads, -1, nil, wholeMapFiltersAhead(items, values, reads), nil, filtersLevelSource(items)))
		return
	}
	g := r.gather(base, items, reads, -1, nil, nil, nil, false)
	through, first := orderedScanPrefix(base, items, values, reads)
	if first != nil {
		if ordered := r.gather(base, items, reads, through, first, nil, nil, false); slices.Contains(ordered.filtered, first) {
			g = ordered
		}
	}
	if admitted := g.overtaken(items, values); admitted != nil {
		g = r.gather(base, items, reads, g.keyedThrough, g.first, nil, admitted, false)
	}
	r.node = r.rebuild(g)
}

// overtaken returns the filter conjuncts of items that g leaves above the
// scan's scope while a conjunct the pipeline runs after them moves into
// it and reads a table column that neither the scan's filter nor any
// conjunct ahead of that one reads, whole or by any key, through the
// layers they read; in pipeline order, and nil when there are none. On a
// server that keeps WHERE order the moved conjunct reads that column for
// every row the ones left above would drop. A moved conjunct that reads
// only columns already read spares no column read by running after them,
// and the ones left above stay where the expansion bound puts them.
func (g *scanGather) overtaken(items []chplan.Node, values layerValues) []chplan.Expr {
	early := tableReads{}
	if f, ok := g.base.(*chplan.Filter); ok {
		values.addTableReads(early, f.Predicate)
	}
	var overtaken, pending []chplan.Expr
	for _, item := range items {
		f, ok := item.(*chplan.Filter)
		if !ok {
			continue
		}
		for _, conjunct := range conjuncts(f.Predicate) {
			switch {
			case !slices.Contains(g.filtered, conjunct):
				if g.leftAbove(conjunct) {
					pending = append(pending, conjunct)
				}
			case early.readsAnyColumnOutside(values.tableReads(conjunct)):
				overtaken, pending = append(overtaken, pending...), nil
			}
			values.addTableReads(early, conjunct)
		}
	}
	return overtaken
}

// leftAbove reports whether g leaves the filter conjunct e above the scan's
// scope.
func (g *scanGather) leftAbove(e chplan.Expr) bool {
	for _, rest := range g.above {
		if slices.Contains(rest.conjuncts, e) {
			return true
		}
	}
	return false
}

// rebuild stacks what g moved over the scan, then what it left above.
func (r *rowLayers) rebuild(g scanGather) chplan.Node {
	return rebuildAtScan(g.base, g.layers, g.filtered, g.above, &r.atScan)
}

// gather walks items, the stack over base, and returns what it moves into
// the scan's scope, folds into the scan's filter and leaves above, with
// keyedThrough, first, wholeAtScan, admitted and levelBesideSource as
// [scanGather]'s.
func (r *rowLayers) gather(base chplan.Node, items []chplan.Node, reads stageMaps, keyedThrough int, first chplan.Expr, wholeAtScan, admitted []chplan.Expr, levelBesideSource bool) scanGather {
	aliases := map[string]bool{}
	for _, layer := range r.layers {
		aliases[layer.Projections[0].Alias] = true
	}
	scope := scanScope{aliases: aliases, defs: map[string]chplan.Expr{}, expanded: map[string]int{}}
	g := scanGather{reads: reads, base: base, keyedThrough: keyedThrough, first: first, wholeAtScan: wholeAtScan, admitted: admitted, levelBesideSource: levelBesideSource}
	for i, item := range items {
		switch n := item.(type) {
		case *chplan.Project:
			g.takeLayer(&scope, i, n)
		case *chplan.Filter:
			g.takeFilter(&scope, i, n)
		}
	}
	g.demote(&scope)
	return g
}

// scanGather is the state of one [rowLayers.gatherAtScan] walk: what it
// has moved into the scan's scope and folded into its filter so far, and
// what it leaves above. The scope's sizes are a [scanScope] each method
// is passed.
type scanGather struct {
	reads stageMaps
	// keyedThrough is the place in the stack up to which a value or a
	// conjunct may move although it reads a map column the stage reads as
	// a whole, by key or whole ([orderedScanPrefix]); -1 when none may.
	keyedThrough int
	// first is the conjunct [orderedScanPrefix] names first. It joins the
	// scope whatever the scope's expansion when it reads only values in
	// the scope, and once it has moved, so does a conjunct up to
	// keyedThrough that does, so the scan reads the stage up to the line
	// filter in pipeline order.
	first chplan.Expr
	// wholeAtScan are the filter conjuncts reading a map column as a whole
	// that [wholeMapFiltersAhead] returns, which run where the table is read.
	wholeAtScan []chplan.Expr
	// admitted are the filter conjuncts [scanGather.overtaken] returned for
	// the gather without them. Each joins the scope whatever the scope's
	// expansion when it reads only values in the scope, and is left out of
	// the scope's sizes, so the gather moves every value and conjunct the
	// one without them moved.
	admitted []chplan.Expr
	// levelBesideSource reports that a filter conjunct on the
	// detected_level source joins the scope whatever its expansion, and
	// the normalised level with it, when each reads only values in the
	// scope.
	levelBesideSource bool
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

// joinsFirst reports whether a conjunct at place i in the stack follows
// first, moved, up to keyedThrough.
func (g *scanGather) joinsFirst(i int) bool {
	return i <= g.keyedThrough && g.first != nil && slices.Contains(g.filtered, g.first)
}

// movable reports whether e, at place i in the stack, may join scope.
func (g *scanGather) movable(scope *scanScope, i int, e chplan.Expr) bool {
	return g.mapsAllow(i, e) && scope.admits(e)
}

// mapsAllow reports whether e, at place i in the stack, may move as far as
// the map columns the stage reads as a whole go: it reads no key of one,
// or it sits up to keyedThrough.
func (g *scanGather) mapsAllow(i int, e chplan.Expr) bool {
	return i <= g.keyedThrough || !g.reads.readsWholeKey(e)
}

// takeLayer moves the layer n, at place i, into the scope or leaves it
// above.
func (g *scanGather) takeLayer(scope *scanScope, i int, n *chplan.Project) {
	value := n.Projections[0]
	if g.levelBesideSource && value.Alias == detectedLevelAlias && scope.readsOnlyScope(value.Expr) {
		// Counted out of the scope's sizes: it stays only beside a moved
		// filter on its source ([scanGather.demote]), and a filter on the
		// level itself still pays for its expansion.
		g.place(scope, i, n)
		return
	}
	if !g.movable(scope, i, value.Expr) {
		g.above = append(g.above, scanRest{at: i, layer: n})
		return
	}
	scope.add(value.Expr)
	g.place(scope, i, n)
}

// place moves the layer n, at place i, into the scope.
func (g *scanGather) place(scope *scanScope, i int, n *chplan.Project) {
	value := n.Projections[0]
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
		case g.reads.readsWhole(conjunct) && i > g.keyedThrough && !slices.Contains(g.wholeAtScan, conjunct):
			kept = append(kept, conjunct)
		case !scope.readsLayer(conjunct) && (len(g.filtered) == 0 || g.reads.readsWholeKey(conjunct) && !g.joinsFirst(i)):
			g.base = andFilter(g.base, conjunct)
		case !scope.readsLayer(conjunct):
			// Behind a moved filter it joins the moved ones in order; it
			// reads no value, so it adds nothing to expand.
			g.filtered = append(g.filtered, conjunct)
		case slices.Contains(g.admitted, conjunct) && g.mapsAllow(i, conjunct) && scope.readsOnlyScope(conjunct):
			// Left out of the scope's sizes, it leaves every value and
			// conjunct after it to move as it would without it.
			g.filtered = append(g.filtered, conjunct)
		case (g.joinsFirst(i) || conjunct == g.first) && scope.readsOnlyScope(conjunct):
			scope.add(conjunct)
			g.filtered = append(g.filtered, conjunct)
		case g.levelBesideSource && chplan.ReadsAnyColumn(conjunct, levelSourceAliases) && scope.readsOnlyScope(conjunct):
			scope.add(conjunct)
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

// orderedScanPrefix finds, in items, the first filter conjunct that reads
// a layer and no map column as a whole, when a conjunct the pipeline runs
// after it reads no layer, no map column as a whole, and a table column that
// base's filter, the conjuncts ahead of the first one and the first one
// through the layers it reads do not already read. It returns the place of
// the filter holding that later conjunct and the first one, or a nil
// conjunct when there is none.
//
// On a server that runs a multi-step PREWHERE in the order of the WHERE
// conjuncts, ClickHouse reads each column at the first step that needs it,
// for the rows the steps ahead of it keep. Folded ahead of the first one,
// the later conjunct reads its column for the rows the first one drops;
// left above the scan, that column is read for every row the scan passes
// up. Up to the later conjunct, values and conjuncts then move into the
// scan's scope even when they read a map column the stage reads as a whole,
// so the stage reads as one scope in pipeline order there, as long as the
// first one moves.
func orderedScanPrefix(base chplan.Node, items []chplan.Node, values layerValues, reads stageMaps) (int, chplan.Expr) {
	early := tableReads{}
	if f, ok := base.(*chplan.Filter); ok {
		values.addTableReads(early, f.Predicate)
	}
	var first chplan.Expr
	for i, item := range items {
		f, ok := item.(*chplan.Filter)
		if !ok {
			continue
		}
		for _, conjunct := range conjuncts(f.Predicate) {
			switch {
			case reads.readsWhole(conjunct):
			case values.reads(conjunct):
				if first == nil {
					values.addTableReads(early, conjunct)
					first = conjunct
				}
			case first == nil:
				values.addTableReads(early, conjunct)
			case !early.covers(values.tableReads(conjunct)):
				return i, first
			}
		}
	}
	return -1, nil
}

// wholeMapFiltersAhead returns, when some filter conjunct of items reads
// no layer and no map column as a whole, each filter conjunct that reads a
// map column as a whole and no layer: a label filter on a stored label
// beside a line filter, for one, on either side of it. The conjunct that
// reads no whole map runs in the scan's filter, and on a server that orders
// a multi-step PREWHERE itself the conjunct found then runs in PREWHERE with
// it, so the columns the stage reads past the scan's filter — the series
// key's whole ResourceAttributes, for one — are read only for the granules
// both keep; left above the scan, the conjunct runs after those columns are
// read for every granule the scan's filter keeps. The server chooses the
// order of the two by its own measure, so it may read the whole map for
// rows the other conjunct drops, as the scan's WHERE of every release up to
// the per-row stage did. With no conjunct that reads no whole map, the
// scan's filter holds the stream selector alone, the server runs a filter
// left directly above the scan in PREWHERE itself, and the conjunct stays
// there.
func wholeMapFiltersAhead(items []chplan.Node, values layerValues, reads stageMaps) []chplan.Expr {
	var whole []chplan.Expr
	trigger := false
	for _, item := range items {
		f, ok := item.(*chplan.Filter)
		if !ok {
			continue
		}
		for _, conjunct := range conjuncts(f.Predicate) {
			switch {
			case values.reads(conjunct):
			case reads.readsWhole(conjunct):
				whole = append(whole, conjunct)
			default:
				trigger = true
			}
		}
	}
	if !trigger {
		return nil
	}
	return whole
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
		if g.keeps(scope, value, needed) {
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

// levelSourceAliases names the detected_level source, for
// [scanGather.levelBesideSource].
var levelSourceAliases = map[string]bool{levelSourceAlias: true}

// filtersLevelSource reports whether a filter conjunct of items reads the
// detected_level source.
func filtersLevelSource(items []chplan.Node) bool {
	for _, item := range items {
		if f, ok := item.(*chplan.Filter); ok && chplan.ReadsAnyColumn(f.Predicate, levelSourceAliases) {
			return true
		}
	}
	return false
}

// keeps reports whether the moved layer value stays in the scope, given
// needed, the columns the moved filters and the layers kept above it
// read: a value something there reads, one that reads no other value, and
// with levelBesideSource the normalised detected_level when a moved filter
// reads its source.
func (g *scanGather) keeps(scope *scanScope, value chplan.Projection, needed map[string]bool) bool {
	if needed[value.Alias] {
		return true
	}
	if g.levelBesideSource && value.Alias == detectedLevelAlias {
		return slices.ContainsFunc(g.filtered, func(conjunct chplan.Expr) bool {
			return chplan.ReadsAnyColumn(conjunct, levelSourceAliases)
		})
	}
	return !scope.readsLayer(value.Expr)
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
	return chplan.ReadsAnyColumn(e, s.aliases)
}

// admits reports whether e — a layer's value or a filter conjunct — reads
// no alias outside the scope and keeps the scope within
// [maxScanScopeExpansion] once added.
func (s *scanScope) admits(e chplan.Expr) bool {
	return s.readsOnlyScope(e) && s.expandedTotal+s.expandedSize(e) <= maxScanScopeExpansion*(s.text+exprSize(e))
}

// readsOnlyScope reports whether e reads no alias outside the scope.
func (s *scanScope) readsOnlyScope(e chplan.Expr) bool {
	outside := false
	chplan.InspectExpr(e, func(sub chplan.Expr) bool {
		if ref, ok := sub.(*chplan.ColumnRef); ok && s.aliases[ref.Name] {
			if _, in := s.defs[ref.Name]; !in {
				outside = true
			}
		}
		return !outside
	})
	return !outside
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
