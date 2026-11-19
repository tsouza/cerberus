package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// The canonical series key and its type cast ([CanonicalMap]).
//
// On ClickHouse 24.8–24.12, mapSort (like every Map function that derives
// its result type from the underlying Array(Tuple(key, value))) returns a
// Map whose inner Tuple has no element names, while the Map parsed from
// the name `Map(K, V)` has its Tuple named (keys, values). The two print
// the same name but are unequal types. An aggregation or sort that spills
// writes its blocks under the type name and reads them back as the parsed
// type; the reader finds that type unequal to the in-memory header and has
// no conversion between two Maps, so the read fails with
// `Cannot convert: Map(String, String) to Map(String, String)`
// (TYPE_MISMATCH). Cast to the type parsed from its operand's type name,
// the key already carries the type a spilled block reads back as, so the
// header and the block agree. From 25.1 mapSort returns the parsed type
// itself, and chopt.FeatureCanonicalMapTypeCast leaves the cast off there.
//
// Every Map function that derives its result type that way returns the
// same unequal Map on those servers: mapConcat and mapApply among the ones
// cerberus emits, as well as mapSort. mapFilter keeps its operand's type,
// and mapUpdate, mapFromArrays and map() return the parsed type. Under the
// same strategy, a mapConcat or mapApply result is cast back to its parsed
// type wherever it is rendered ([ParsedMapType]): a bucket fan-out's
// Attributes, merged with mapConcat, is the key of the range lookback's
// GROUP BY, and a spilled block of that GROUP BY fails the same way. The
// direct operand of a canonical key renders without that cast, because
// the key's own cast already gives the column it feeds the parsed type,
// and casting both would render the operand four times.
//
// The cast copies the Map column, so it is rendered only where the value
// can reach an operator that writes blocks to disk. [mapTypeCastFree]
// walks the plan an emission renders and returns the Map calls whose value
// never does: one a Filter's predicate consumes, or one a Project computes
// into a column that no operator above reads except through Filters,
// Limits and Projects up to the statement's result. Those render bare,
// under the strategy too. Every other Map call keeps its cast, so any
// operator other than a Filter, a Limit or a Project counts as one that
// spills, and a Map call the walk does not reach keeps its cast.
//
// The type comes from the operand rather than a literal, so a
// LowCardinality key type, or any value type, passes through unchanged. The
// operand appears twice rather than being named once by an inline alias:
// the projection a key feeds is routinely aliased back to the operand's own
// column name (`... AS Attributes`), and under enable_analyzer=0 an inline
// alias inside that projection whose expression reads the column resolves
// the column to the projection's alias, which ClickHouse rejects as
// CYCLIC_ALIASES.

// exprCanonicalMap renders chplan.FnMapSort through [CanonicalMap].
// exprFunc dispatches here ahead of the fnResolutions lookup for the same
// package-init-cycle reason exprMapContainsKey is special-cased: a
// fnRender hook in that table that calls b.Expr would make the table's
// initializer depend on itself.
func (b *Builder) exprCanonicalMap(key *chplan.FuncCall) error {
	args := key.Args
	if len(args) != 1 {
		return fmt.Errorf("%w: chplan.FnMapSort takes exactly one Map argument, got %d", ErrUnsupported, len(args))
	}
	var err error
	canonicalMap(b.strategies.CanonicalMapTypeCast && !b.mapTypeCastFree(key), func(b *Builder) {
		if f, ok := args[0].(*chplan.FuncCall); ok && parsedMapTypeFns[f.Fn] {
			err = b.exprFuncCall(f)
			return
		}
		_ = b.Expr(args[0])
	})(b)
	if err != nil {
		return err
	}
	return b.err
}

// parsedMapTypeFns are the Map functions, among the ones cerberus emits,
// that derive their result type from the underlying Array(Tuple(key,
// value)) the way mapSort does; exprFunc renders them through
// [ParsedMapType].
var parsedMapTypeFns = map[chplan.Fn]bool{
	chplan.FnMapMerge: true,
	chplan.FnMapApply: true,
}

// mapTypeCastFree reports whether f, a Map call of the emission b renders
// inside, renders without the parsed-Map-type cast: its value reaches no
// operator that can spill (see [mapTypeCastFree]). A Builder outside an
// emission renders every Map call cast.
func (b *Builder) mapTypeCastFree(f *chplan.FuncCall) bool {
	return b.env != nil && b.env.mapCastFree[f]
}

// isMapTypeCastCall reports whether e is a Map call the parsed-Map-type
// cast applies to: a canonical key or a call in parsedMapTypeFns.
func isMapTypeCastCall(e chplan.Expr) (*chplan.FuncCall, bool) {
	f, ok := e.(*chplan.FuncCall)
	if !ok || (f.Fn != chplan.FnMapSort && !parsedMapTypeFns[f.Fn]) {
		return nil, false
	}
	return f, true
}

// mapTypeCastFree returns the Map calls of the plan n whose value no
// operator that can write blocks to disk ever reads. It walks n from the
// statement's result down, tracking which of each node's output columns
// the operators above read on their way to one that can spill:
//
//   - A Filter or a Limit forwards its rows, so its input owes what its
//     output owes. A Filter's predicate is consumed where it runs.
//   - A Project's input owes the columns its owed projections read. A
//     projection no operator above reads on that way is not owed. A
//     Project that computes a window function sorts its rows, so it counts
//     as a node that can spill.
//   - Any other node can spill: its input owes every column, and its own
//     expressions are read where it runs.
//   - A plan embedded in an expression (a scalar or IN subquery) owes every
//     column.
//
// A node reached along several paths owes the union. A Map call is free
// when every expression it appears in is a predicate or a projection that
// is not owed; one inside an owed projection, or in a spilling node's own
// expressions, is not.
func mapTypeCastFree(n chplan.Node) map[*chplan.FuncCall]bool {
	w := castWalk{owed: map[chplan.Node]castDemand{}}
	w.node(n, castDemand{})
	free, bound := map[*chplan.FuncCall]bool{}, map[*chplan.FuncCall]bool{}
	for node, d := range w.owed {
		switch v := node.(type) {
		case *chplan.Filter:
			collectMapTypeCastCalls(free, v.Predicate)
		case *chplan.Project:
			if projectSorts(v) {
				chplan.InspectNodeExprs(node, func(e chplan.Expr) { collectMapTypeCastCalls(bound, e) })
				continue
			}
			for _, pr := range v.Projections {
				collectMapTypeCastCalls(pickCastSet(d.owes(projectionName(pr)), bound, free), pr.Expr)
			}
			for _, pr := range v.Replacements {
				collectMapTypeCastCalls(pickCastSet(d.owes(pr.Alias), bound, free), pr.Expr)
			}
		default:
			chplan.InspectNodeExprs(node, func(e chplan.Expr) { collectMapTypeCastCalls(bound, e) })
		}
	}
	for f := range bound {
		delete(free, f)
	}
	return free
}

// pickCastSet returns bound when the expression it classifies is owed,
// free otherwise.
func pickCastSet(owed bool, bound, free map[*chplan.FuncCall]bool) map[*chplan.FuncCall]bool {
	if owed {
		return bound
	}
	return free
}

// projectionName is the output column name of pr: its alias, or the name
// of the column a bare pass-through reads. An unaliased computed
// projection has no name a consumer can read it by, and is treated as
// owed.
func projectionName(pr chplan.Projection) string {
	if pr.Alias != "" {
		return pr.Alias
	}
	if ref, ok := pr.Expr.(*chplan.ColumnRef); ok {
		return ref.Name
	}
	return ""
}

// collectMapTypeCastCalls adds every Map call of e the cast applies to into
// set. It stays within e's own expression tree: a plan embedded in e is a
// node of its own walk.
func collectMapTypeCastCalls(set map[*chplan.FuncCall]bool, e chplan.Expr) {
	chplan.InspectExpr(e, func(x chplan.Expr) bool {
		if f, ok := isMapTypeCastCall(x); ok {
			set[f] = true
		}
		return true
	})
}

// castDemand is the set of a node's output columns some operator above
// reads on its way to one that can spill; all means every column.
type castDemand struct {
	all  bool
	cols map[string]bool
}

// owes reports whether d holds the column name. The empty name, an
// unnameable column, is owed whenever any column is.
func (d castDemand) owes(name string) bool {
	if d.all {
		return true
	}
	if name == "" {
		return len(d.cols) > 0
	}
	return d.cols[name]
}

// covers reports whether d holds every column o does.
func (d castDemand) covers(o castDemand) bool {
	if d.all {
		return true
	}
	if o.all {
		return false
	}
	for c := range o.cols {
		if !d.cols[c] {
			return false
		}
	}
	return true
}

// union returns the columns d or o holds.
func (d castDemand) union(o castDemand) castDemand {
	if d.all || o.all {
		return castDemand{all: true}
	}
	cols := make(map[string]bool, len(d.cols)+len(o.cols))
	for c := range d.cols {
		cols[c] = true
	}
	for c := range o.cols {
		cols[c] = true
	}
	return castDemand{cols: cols}
}

// castWalk records what each node of a plan owes ([mapTypeCastFree]).
type castWalk struct {
	owed map[chplan.Node]castDemand
}

// node records that n owes d and walks its inputs, once per growth of
// what n owes.
func (w *castWalk) node(n chplan.Node, d castDemand) {
	if n == nil {
		return
	}
	if prev, seen := w.owed[n]; seen {
		if prev.covers(d) {
			return
		}
		d = prev.union(d)
	}
	w.owed[n] = d
	chplan.InspectNodeExprs(n, func(e chplan.Expr) {
		chplan.InspectExprNodes(e, func(chplan.Expr) bool { return true }, func(sub chplan.Node) {
			w.node(sub, castDemand{all: true})
		})
	})
	switch v := n.(type) {
	case *chplan.Filter:
		w.node(v.Input, d)
	case *chplan.Limit:
		w.node(v.Input, d)
	case *chplan.Project:
		if projectSorts(v) {
			w.node(v.Input, castDemand{all: true})
			return
		}
		w.node(v.Input, projectInputDemand(v, d))
	default:
		for _, c := range n.Children() {
			w.node(c, castDemand{all: true})
		}
	}
}

// projectInputDemand returns the columns p's input owes when p's output
// owes d: the columns its owed projections read, and, for a pass-through
// Project, the columns it forwards unchanged.
func projectInputDemand(p *chplan.Project, d castDemand) castDemand {
	in := castDemand{cols: map[string]bool{}}
	read := func(e chplan.Expr) {
		chplan.InspectExpr(e, func(x chplan.Expr) bool {
			switch ref := x.(type) {
			case *chplan.ColumnRef:
				in.cols[ref.Name] = true
			case *chplan.BareIdent:
				in.cols[ref.Name] = true
			}
			return true
		})
	}
	if len(p.Projections) == 0 {
		if d.all {
			return d
		}
		for c := range d.cols {
			in.cols[c] = true
		}
		for _, pr := range p.Replacements {
			if d.owes(pr.Alias) {
				read(pr.Expr)
			}
		}
		return in
	}
	for _, pr := range p.Projections {
		if d.owes(projectionName(pr)) {
			read(pr.Expr)
		}
	}
	return in
}

// projectSorts reports whether p computes a window function, whose
// evaluation sorts every row p reads.
func projectSorts(p *chplan.Project) bool {
	sorts := false
	chplan.InspectNodeExprs(p, func(e chplan.Expr) {
		chplan.InspectExpr(e, func(x chplan.Expr) bool {
			if _, ok := x.(*chplan.WindowExpr); ok {
				sorts = true
			}
			return !sorts
		})
	})
	return sorts
}
