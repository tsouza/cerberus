package logql

import (
	"slices"
	"strconv"

	"github.com/tsouza/cerberus/internal/chplan"
)

// rowLayers builds the per-row stage of a LogQL query — everything
// between the stream scan and the consumer that reads the row: the
// projection a range aggregation windows over, or the projection a log
// query returns its lines through — as a stack of Project layers, one per
// value the stage computes.
//
// Each [rowLayers.define] adds one layer that computes an expression once
// per row under a column alias and hands back a reference to that alias.
// Every later expression that needs the value reads the column instead of
// embedding the expression again. A LogQL pipeline reads the same values
// over and over: a parser stage's labels map feeds every label filter,
// the unwrap lookup, the conversion-error mark, the series identity and a
// log line's labels, and the unwrap conversion reads its source several
// times. Inlining a value at each read repeats it in the statement text
// once per read, and one value nested inside another multiplies the
// copies. Defining each value as a column keeps every value to one copy.
//
// [rowLayers.filter] AND-folds a predicate onto the current row. Before
// the first layer exists that is the stream scan's own filter, so a
// stage that runs ahead of any defined value stays a scan predicate.
//
// [rowLayers.hoist] splits the stack at a log query's ORDER BY … LIMIT.
// Below it sit only the values a filter reads. A wide one among them, such
// as a labels map, is computed again above the limit for the rows that
// survive it, rather than carried through the sort; the sort carries only
// the narrow values the consumer also reads. Every value no filter reads is
// computed above the limit alone.
//
// [rowLayers.gatherAtScan] is a range aggregation's counterpart: it moves
// the values and filters the table is best read with into the stream
// scan's own scope, and leaves the rest of the stack in order above it.
//
// A layer is created with its defined value only; [rowLayers.seal]
// decides what else it passes through, once the consumer above the stack
// is known. It walks the stack from the top, gives each layer exactly the
// columns read above it, and drops a layer whose value nothing reads.
type rowLayers struct {
	node   chplan.Node
	layers []*chplan.Project
	// passes are the nodes [rowLayers.hoist] wrapped the filtered rows in;
	// seal walks through them, and gives a Project among them exactly the
	// columns read above it.
	passes []chplan.Node
	// wide are the layers [rowLayers.defineWide] added.
	wide []*chplan.Project
	// atScan are the nodes [rowLayers.gatherAtScan] moved into the stream
	// scan's scope; seal marks the Project that reads them.
	atScan []chplan.Node
	scopes int
}

// newRowLayers starts a per-row stage over node, the lowered stream
// selector.
func newRowLayers(node chplan.Node) *rowLayers {
	return &rowLayers{node: node}
}

// filter AND-folds pred onto the current row. A nil pred is a no-op.
func (r *rowLayers) filter(pred chplan.Expr) {
	if pred == nil {
		return
	}
	r.node = andFilter(r.node, pred)
}

// define computes expr once per row under alias and returns the
// expression later reads use in its place: a reference to the alias, or
// expr itself when it is already a bare column.
func (r *rowLayers) define(alias string, expr chplan.Expr) chplan.Expr {
	if _, ok := expr.(*chplan.ColumnRef); ok {
		return expr
	}
	// A layer declares no roles: every column it passes through is a
	// ColumnRef that inherits its role from the input, and its own value
	// has none.
	layer := &chplan.Project{
		Input:       r.node,
		Projections: []chplan.Projection{{Expr: expr, Alias: alias}},
	}
	r.layers = append(r.layers, layer)
	r.node = layer
	return &chplan.ColumnRef{Name: alias}
}

// defineWide is [rowLayers.define] for a value a log query's sort does
// not carry: one too wide to, such as a parser stage's labels map, or one
// whose table inputs ClickHouse reads for every row the sort sees when the
// sort carries it, such as the detected level ([defineDetectedLevel]).
// When a filter reads it, [rowLayers.hoist] defines it once more above the
// limit rather than passing it through the ORDER BY.
func (r *rowLayers) defineWide(alias string, expr chplan.Expr) chplan.Expr {
	n := len(r.layers)
	ref := r.define(alias, expr)
	if len(r.layers) > n {
		r.wide = append(r.wide, r.layers[n])
	}
	return ref
}

// seal fills in the pass-through columns of every layer below consumer,
// a Project whose Input is the stack's current node, and removes the
// layers whose value nothing above reads. Each name in carry is passed
// through every kept layer as though consumer read it.
//
// The walk runs top-down. needed holds the columns read above the
// current node. A filter adds the columns its predicate reads. A layer
// whose alias is in needed keeps its value, passes through the rest of
// needed by name, and passes down the columns its value reads; a layer
// whose alias is not in needed is spliced out. The walk stops at the
// first node the stack did not build — the stream scan and its filter —
// which already exposes every column of the logs table. It walks through
// the nodes [rowLayers.hoist] added, adding the columns an ORDER BY
// reads. Once the layers are sealed, the Project that reads the part
// [rowLayers.gatherAtScan] moved into the stream scan's scope is marked.
func (r *rowLayers) seal(consumer *chplan.Project, carry ...string) {
	r.sealLayers(consumer, carry)
	r.markAtScan(consumer)
}

// sealLayers is [rowLayers.seal] without the mark [rowLayers.markAtScan]
// adds.
func (r *rowLayers) sealLayers(consumer *chplan.Project, carry []string) {
	needed := map[string]bool{}
	for _, name := range carry {
		needed[name] = true
	}
	for _, p := range consumer.Projections {
		addColumnsRead(needed, p.Expr)
	}
	var carried []string
	var parent chplan.Node = consumer
	for {
		child := inputOf(parent)
		if !r.stackNode(child) {
			return
		}
		switch n := child.(type) {
		case *chplan.Filter:
			addColumnsRead(needed, n.Predicate)
		case *chplan.OrderBy:
			for _, key := range n.Keys {
				addColumnsRead(needed, key.Expr)
			}
		case *chplan.Project:
			if slices.Contains(r.passes, child) {
				carried = sortedNames(carried[:0], needed)
				n.Projections = make([]chplan.Projection, 0, len(carried))
				for _, name := range carried {
					n.Projections = append(n.Projections, chplan.Projection{Expr: &chplan.ColumnRef{Name: name}})
				}
				parent = child
				continue
			}
			value := n.Projections[0]
			if !needed[value.Alias] {
				setInputOf(parent, n.Input)
				continue
			}
			delete(needed, value.Alias)
			carried = sortedNames(carried[:0], needed)
			projections := make([]chplan.Projection, 0, len(carried)+1)
			for _, name := range carried {
				projections = append(projections, chplan.Projection{Expr: &chplan.ColumnRef{Name: name}})
			}
			n.Projections = append(projections, value)
			addColumnsRead(needed, value.Expr)
		}
		parent = child
	}
}

// sortedNames appends the names in set to dst in sorted order.
func sortedNames(dst []string, set map[string]bool) []string {
	for name := range set {
		dst = append(dst, name)
	}
	slices.Sort(dst)
	return dst
}

// hoist rebuilds the stack around limit, a log query's ORDER BY … LIMIT:
// limit's Input is set to order and order's to the filtered rows. A nil
// limit wraps nothing. It must be called before [rowLayers.seal], on a
// stack whose layers still hold their value alone.
//
// A layer stays below the limit when a filter reads it, directly or
// through another layer that stays; every other layer moves above it, in
// its original order, so a value only the consumer reads is computed for
// the rows the limit lets through and not for every row the filters keep.
// Between the filtered rows and the sort sits a Project that passes on
// only the columns read above it. A wide value ([rowLayers.defineWide])
// that stays is defined once more above the limit, in its place in that
// order, so the sort never carries it: it is computed once for every row
// the filters see and once for the rows returned. Any other value that
// stays and that the consumer reads is passed through the sort.
//
// A filter conjunct that reads no layer is folded into the stream scan's
// own filter when [foldScanConjuncts] finds it reads no column for more
// rows there; any other keeps its place in the pipeline's order. The
// layers are pure per-row values, so where an AND of predicates over the
// same row runs does not change which rows survive, only which columns
// ClickHouse reads for them.
func (r *rowLayers) hoist(maps []string, order *chplan.OrderBy, limit *chplan.Limit) {
	var items []chplan.Node
	base := r.node
	for r.stackNode(base) {
		items = append(items, base)
		base = inputOf(base)
	}
	slices.Reverse(items)

	needed := map[string]bool{}
	stays := map[*chplan.Project]bool{}
	for i := len(items) - 1; i >= 0; i-- {
		switch n := items[i].(type) {
		case *chplan.Filter:
			addColumnsRead(needed, n.Predicate)
		case *chplan.Project:
			if value := n.Projections[0]; needed[value.Alias] {
				stays[n] = true
				addColumnsRead(needed, value.Expr)
			}
		}
	}

	base, kept := foldScanConjuncts(base, items, r.values(maps))
	node := base
	var above []*chplan.Project
	for _, item := range items {
		switch n := item.(type) {
		case *chplan.Filter:
			for _, conjunct := range kept[n] {
				node = andFilter(node, conjunct)
			}
		case *chplan.Project:
			if !stays[n] {
				above = append(above, n)
				continue
			}
			n.Input = node
			node = n
			if limit != nil && slices.Contains(r.wide, n) {
				again := chplan.CloneNode(&chplan.Project{Projections: n.Projections}).(*chplan.Project)
				r.layers = append(r.layers, again)
				r.wide = append(r.wide, again)
				above = append(above, again)
			}
		}
	}

	top := node
	if limit != nil {
		if len(stays) > 0 {
			narrow := &chplan.Project{Input: node, OneScopeRows: true}
			r.passes = append(r.passes, narrow)
			node = narrow
		}
		order.Input = node
		limit.Input = order
		top = limit
		r.passes = append(r.passes, limit, order)
	}
	for _, layer := range above {
		layer.Input = top
		top = layer
	}
	r.node = top
}

// foldScanConjuncts AND-folds onto base, the stream scan's filter, the
// conjuncts of the filters among items that can run there, and returns the
// new base with each filter's remaining conjuncts, in order.
//
// A server that keeps WHERE order (chopt.PrewhereKeepsWhereOrder) runs a
// multi-step PREWHERE in the order of the WHERE conjuncts and reads each
// column at the first step that needs it, for the rows the steps ahead of
// that one keep; a server that reorders the conditions chooses their order
// itself. Every conjunct the pipeline runs ahead of
// the first one that reads a layer folds. After that first one, a conjunct
// that reads no layer folds only when base and the first one, through the
// layers it reads, already read every table column it reads: run ahead of
// the first one it then reads no column for more rows, and the first one
// reads its other columns for fewer. Any other conjunct keeps its place;
// ahead of the first one it would read its own columns for rows that one
// drops.
func foldScanConjuncts(base chplan.Node, items []chplan.Node, values layerValues) (chplan.Node, map[*chplan.Filter][]chplan.Expr) {
	early := tableReads{}
	if f, ok := base.(*chplan.Filter); ok {
		values.addTableReads(early, f.Predicate)
	}
	kept := map[*chplan.Filter][]chplan.Expr{}
	behind := false
	for _, item := range items {
		f, ok := item.(*chplan.Filter)
		if !ok {
			continue
		}
		for _, conjunct := range conjuncts(f.Predicate) {
			switch {
			case values.reads(conjunct):
				if !behind {
					values.addTableReads(early, conjunct)
					behind = true
				}
				kept[f] = append(kept[f], conjunct)
			case !behind || early.covers(values.tableReads(conjunct)):
				values.addTableReads(early, conjunct)
				base = andFilter(base, conjunct)
			default:
				kept[f] = append(kept[f], conjunct)
			}
		}
	}
	return base, kept
}

// layerValues is what [foldScanConjuncts] reads of a stack: the value each
// layer's alias defines, and the table's map columns.
type layerValues struct {
	defs map[string]chplan.Expr
	maps []string
}

// values returns the values r's layers define over a table whose map
// columns are maps.
func (r *rowLayers) values(maps []string) layerValues {
	defs := map[string]chplan.Expr{}
	for _, layer := range r.layers {
		value := layer.Projections[0]
		defs[value.Alias] = value.Expr
	}
	return layerValues{defs: defs, maps: maps}
}

// reads reports whether expr reads a layer's value.
func (v layerValues) reads(expr chplan.Expr) bool {
	return readsAlias(expr, v.defs)
}

// tableRead is one read of a table column: the whole column, or the value
// of one constant key of a map column, which ClickHouse can read apart from
// the rest of the map (a JSON-typed attribute's subcolumn, a bucketed map's
// bucket).
type tableRead struct {
	column string
	key    string
	keyed  bool
}

// tableReads is a set of table reads.
type tableReads map[tableRead]bool

// covers reports whether t already reads everything in reads: each read is
// in t, or is a key of a column t reads whole.
func (t tableReads) covers(reads tableReads) bool {
	for read := range reads {
		if !t[read] && (!read.keyed || !t[tableRead{column: read.column}]) {
			return false
		}
	}
	return true
}

// readsAnyColumnOutside reports whether reads reads a table column t reads
// neither whole nor by any key.
func (t tableReads) readsAnyColumnOutside(reads tableReads) bool {
	columns := map[string]bool{}
	for read := range t {
		columns[read.column] = true
	}
	for read := range reads {
		if !columns[read.column] {
			return true
		}
	}
	return false
}

// tableReads returns the table reads of expr.
func (v layerValues) tableReads(expr chplan.Expr) tableReads {
	reads := tableReads{}
	v.addTableReads(reads, expr)
	return reads
}

// addTableReads records into set every table read of expr, directly or
// through the values of the layers it reads.
func (v layerValues) addTableReads(set tableReads, expr chplan.Expr) {
	expanded := map[string]bool{}
	for pending := []chplan.Expr{expr}; len(pending) > 0; {
		e := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		forEachMapRead(e, v.maps, func(name, key string, keyed bool) {
			set[tableRead{column: name, key: key, keyed: keyed}] = true
		})
		read := map[string]bool{}
		addColumnsRead(read, e)
		for name := range read {
			value, isLayer := v.defs[name]
			switch {
			case slices.Contains(v.maps, name):
			case !isLayer:
				set[tableRead{column: name}] = true
			case !expanded[name]:
				expanded[name] = true
				pending = append(pending, value)
			}
		}
	}
}

// readsAlias reports whether e reads a column named by a key of aliases.
func readsAlias[V any](e chplan.Expr, aliases map[string]V) bool {
	found := false
	chplan.InspectExpr(e, func(sub chplan.Expr) bool {
		if ref, ok := sub.(*chplan.ColumnRef); ok {
			if _, alias := aliases[ref.Name]; alias {
				found = true
			}
		}
		return !found
	})
	return found
}

// conjuncts splits pred into the operands of its top-level ANDs, in order.
func conjuncts(pred chplan.Expr) []chplan.Expr {
	if b, ok := pred.(*chplan.Binary); ok && b.Op == chplan.OpAnd {
		return append(conjuncts(b.Left), conjuncts(b.Right)...)
	}
	return []chplan.Expr{pred}
}

// stackNode reports whether n is a node seal rewrites: one of the
// stack's layers, a filter sitting above one, or a node hoist added. The
// stream scan and the filter folded into it are not.
func (r *rowLayers) stackNode(n chplan.Node) bool {
	switch v := n.(type) {
	case *chplan.Project:
		return r.owns(v) || slices.Contains(r.passes, n)
	case *chplan.Filter:
		return r.built(v.Input)
	case *chplan.OrderBy, *chplan.Limit:
		return slices.Contains(r.passes, n)
	}
	return false
}

// inputOf returns the input of a node seal walks through: a Project, a
// Filter, an OrderBy or a Limit. setInputOf replaces it.
func inputOf(n chplan.Node) chplan.Node {
	switch v := n.(type) {
	case *chplan.Project:
		return v.Input
	case *chplan.Filter:
		return v.Input
	case *chplan.OrderBy:
		return v.Input
	case *chplan.Limit:
		return v.Input
	}
	return nil
}

func setInputOf(n, input chplan.Node) {
	switch v := n.(type) {
	case *chplan.Project:
		v.Input = input
	case *chplan.Filter:
		v.Input = input
	case *chplan.OrderBy:
		v.Input = input
	case *chplan.Limit:
		v.Input = input
	}
}

// owns reports whether p is one of the stack's layers.
func (r *rowLayers) owns(n chplan.Node) bool {
	p, ok := n.(*chplan.Project)
	return ok && slices.Contains(r.layers, p)
}

// built reports whether n is a layer or a filter sitting on one — a node
// the stack produced, as opposed to the stream scan and its filter below
// the first layer.
func (r *rowLayers) built(n chplan.Node) bool {
	for {
		switch v := n.(type) {
		case *chplan.Filter:
			n = v.Input
			continue
		case *chplan.Project:
			return r.owns(v)
		}
		return false
	}
}

// addColumnsRead records into set every column name expr reads.
func addColumnsRead(set map[string]bool, expr chplan.Expr) {
	chplan.InspectExpr(expr, func(sub chplan.Expr) bool {
		if ref, ok := sub.(*chplan.ColumnRef); ok {
			set[ref.Name] = true
		}
		return true
	})
}

// filterAliasPrefix starts the aliases of the values one label filter
// defines on the stack; [rowLayers.scope] numbers each filter.
const filterAliasPrefix = "_logql_filter_"

// scope returns the [defineFunc] for one value set that can occur more
// than once in a stack — one duration filter's parse among several —
// naming its values under a prefix no other scope of r uses.
func (r *rowLayers) scope() defineFunc {
	r.scopes++
	return r.definer(filterAliasPrefix + strconv.Itoa(r.scopes) + "_")
}

// definer returns the [defineFunc] that defines each value on r under
// prefix followed by the value's role.
func (r *rowLayers) definer(prefix string) defineFunc {
	return func(role string, expr chplan.Expr) chplan.Expr {
		return r.define(prefix+role, expr)
	}
}
