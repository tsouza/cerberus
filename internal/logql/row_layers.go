package logql

import (
	"slices"
	"strconv"

	"github.com/tsouza/cerberus/internal/chplan"
)

// rowLayers builds the per-row stage of a LogQL metric query — everything
// between the stream scan and the projection a range aggregation windows
// over — as a stack of Project layers, one per value the stage computes.
//
// Each [rowLayers.define] adds one layer that computes an expression once
// per row under a column alias and hands back a reference to that alias.
// Every later expression that needs the value reads the column instead of
// embedding the expression again. A LogQL pipeline reads the same values
// over and over: a parser stage's labels map feeds every label filter,
// the unwrap lookup, the conversion-error mark and the series identity,
// and the unwrap conversion reads its source several times. Inlining a
// value at each read repeats it in the statement text once per read, and
// one value nested inside another multiplies the copies. Defining each
// value as a column keeps every value to one copy.
//
// [rowLayers.filter] AND-folds a predicate onto the current row. Before
// the first layer exists that is the stream scan's own filter, so a
// stage that runs ahead of any defined value stays a scan predicate.
//
// A layer is created with its defined value only; [rowLayers.seal]
// decides what else it passes through, once the consumer above the stack
// is known. It walks the stack from the top, gives each layer exactly the
// columns read above it, and drops a layer whose value nothing reads.
//
// With materialise unset, define returns its expression unchanged and no
// layer is built: the stage lowers to a single filtered scan whose
// expressions are fully inlined. The log-query path uses that mode, since
// its labels expression is rebuilt from the pipeline after the rows return
// ([PipelineLabelsExpr]) rather than read off a column.
type rowLayers struct {
	node        chplan.Node
	materialise bool
	layers      []*chplan.Project
	scopes      int
}

// newRowLayers starts a per-row stage over node, the lowered stream
// selector.
func newRowLayers(node chplan.Node, materialise bool) *rowLayers {
	return &rowLayers{node: node, materialise: materialise}
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
// expr itself when it is already a bare column or the stack does not
// materialise.
func (r *rowLayers) define(alias string, expr chplan.Expr) chplan.Expr {
	if !r.materialise {
		return expr
	}
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
// which already exposes every column of the logs table.
func (r *rowLayers) seal(consumer *chplan.Project, carry ...string) {
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
		case *chplan.Project:
			value := n.Projections[0]
			if !needed[value.Alias] {
				setInputOf(parent, n.Input)
				continue
			}
			delete(needed, value.Alias)
			carried = carried[:0]
			for name := range needed {
				carried = append(carried, name)
			}
			slices.Sort(carried)
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

// stackNode reports whether n is a node seal rewrites: one of the
// stack's layers, or a filter sitting above one. The stream scan and the
// filter folded into it are not.
func (r *rowLayers) stackNode(n chplan.Node) bool {
	switch v := n.(type) {
	case *chplan.Project:
		return r.owns(v)
	case *chplan.Filter:
		return r.built(v.Input)
	}
	return false
}

// inputOf returns the input of a node seal walks through: a Project or
// a Filter. setInputOf replaces it.
func inputOf(n chplan.Node) chplan.Node {
	switch v := n.(type) {
	case *chplan.Project:
		return v.Input
	case *chplan.Filter:
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
// naming its values under a prefix no other scope of r uses. A stack
// that does not materialise defines nothing.
func (r *rowLayers) scope() defineFunc {
	if !r.materialise {
		return inlineDefine
	}
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
