package optimizer

// Pattern matching for the cerberus plan IR.
//
// Design lineage: Apache Calcite's `org.apache.calcite.rel.rules` operands
// (declarative match → transform) and Spark Catalyst's `Rule[LogicalPlan]`
// quasiquote-style pattern matching. Calcite's rule-engine matches a tree of
// `RelOptRuleOperand`s against a `RelNode`; Catalyst leans on Scala pattern
// matching directly. Cerberus splits the difference: the chplan IR is
// hand-rolled Go structs, so we provide a small combinator API that builds
// up a `Pattern` tree which can be matched and bound against a `chplan.Node`
// tree.
//
// The combinators in this file are pure value constructors — they don't
// touch the tree. `Pattern.Match` is the only place that walks a candidate
// node. Bindings flow back to the caller (a `PatternRule`'s `Apply`
// function) via a `Bindings` map keyed on the names supplied to `Capture`.
//
// The baseline rules (filter-fusion, constant-fold, projection-pushdown)
// keep their bespoke type-switch shape; the transpose family and
// MVSubstitution are built on top of `PatternRule`.

import (
	"reflect"

	"github.com/tsouza/cerberus/internal/chplan"
)

// NodeKind identifies a concrete chplan node type. The chplan package
// itself does not define an enum; we identify kinds by their `reflect.Type`
// (specifically the pointer-receiver type — every chplan node satisfies
// `chplan.Node` via a pointer receiver, e.g. `*chplan.Filter`).
//
// Callers acquire a kind via the package-level helpers (`KindFilter`,
// `KindScan`, ...) or `KindOf` for a one-off lookup from an existing node.
type NodeKind struct {
	t reflect.Type
}

// KindOf returns the NodeKind of n. Useful for asserting "kind preserved"
// in tests.
func KindOf(n chplan.Node) NodeKind {
	if n == nil {
		return NodeKind{}
	}
	return NodeKind{t: reflect.TypeOf(n)}
}

// Predefined kinds for every concrete chplan operator. Kept in lockstep
// with the chplan package; new operators land here when they land there.
var (
	KindScan           = NodeKind{t: reflect.TypeOf((*chplan.Scan)(nil))}
	KindFilter         = NodeKind{t: reflect.TypeOf((*chplan.Filter)(nil))}
	KindProject        = NodeKind{t: reflect.TypeOf((*chplan.Project)(nil))}
	KindAggregate      = NodeKind{t: reflect.TypeOf((*chplan.Aggregate)(nil))}
	KindRangeWindow    = NodeKind{t: reflect.TypeOf((*chplan.RangeWindow)(nil))}
	KindLimit          = NodeKind{t: reflect.TypeOf((*chplan.Limit)(nil))}
	KindOrderBy        = NodeKind{t: reflect.TypeOf((*chplan.OrderBy)(nil))}
	KindVectorJoin     = NodeKind{t: reflect.TypeOf((*chplan.VectorJoin)(nil))}
	KindVectorSetOp    = NodeKind{t: reflect.TypeOf((*chplan.VectorSetOp)(nil))}
	KindStructuralJoin = NodeKind{t: reflect.TypeOf((*chplan.StructuralJoin)(nil))}
	KindTopK           = NodeKind{t: reflect.TypeOf((*chplan.TopK)(nil))}
)

// Bindings is the result of a successful pattern match: a map from the
// names supplied to `Capture` combinators to the concrete nodes they bound.
//
// A nil map means "the pattern didn't use Capture" — every pattern returns
// a usable (possibly empty) map on success; callers should check the
// boolean rather than the map's nilness.
type Bindings map[string]chplan.Node

// Get returns the node bound under name and whether it was present. A
// convenience wrapper around the underlying map access.
func (b Bindings) Get(name string) (chplan.Node, bool) {
	if b == nil {
		return nil, false
	}
	n, ok := b[name]
	return n, ok
}

// Pattern is a value that knows how to match against a `chplan.Node`. On a
// successful match it returns a fresh `Bindings` map; on no match it
// returns `(nil, false)`. Patterns are immutable and safe to share across
// rules and goroutines.
type Pattern interface {
	Match(n chplan.Node) (Bindings, bool)
}

// Any matches any non-nil node. Use it as a leaf wildcard inside
// `WithChildren`, or wrap it with `Capture` to bind an arbitrary subtree
// to a name.
func Any() Pattern { return anyPattern{} }

type anyPattern struct{}

func (anyPattern) Match(n chplan.Node) (Bindings, bool) {
	if n == nil {
		return nil, false
	}
	return Bindings{}, true
}

// Kind matches a single node of the given kind, regardless of its
// children. It does not recurse into children — use `WithChildren` for
// shape-aware matching.
func Kind(k NodeKind) Pattern { return kindPattern{k: k} }

type kindPattern struct{ k NodeKind }

func (p kindPattern) Match(n chplan.Node) (Bindings, bool) {
	if n == nil {
		return nil, false
	}
	if reflect.TypeOf(n) != p.k.t {
		return nil, false
	}
	return Bindings{}, true
}

// Capture wraps p and, on a successful match, additionally binds the
// matched node under name. The wrapped pattern's own bindings are
// preserved; if it bound a node under the same name, this Capture
// overrides it (the outermost Capture wins).
func Capture(name string, p Pattern) Pattern {
	return capturePattern{name: name, inner: p}
}

type capturePattern struct {
	name  string
	inner Pattern
}

func (p capturePattern) Match(n chplan.Node) (Bindings, bool) {
	inner, ok := p.inner.Match(n)
	if !ok {
		return nil, false
	}
	if inner == nil {
		inner = Bindings{}
	}
	inner[p.name] = n
	return inner, true
}

// WithChildren matches a node whose own shape matches `parent` and whose
// immediate children (in order, as returned by `chplan.Node.Children()`)
// match the supplied child patterns. The number of supplied child
// patterns must equal the candidate node's child count for a match.
//
// Bindings from `parent` and every child pattern are merged into a single
// `Bindings` map. If two patterns bind the same name, the later one
// (child patterns first to last, then parent — parent wins last because
// it conceptually owns the node) wins. Rules that need disjoint names
// should pick disjoint names.
func WithChildren(parent Pattern, children ...Pattern) Pattern {
	return withChildrenPattern{parent: parent, children: children}
}

type withChildrenPattern struct {
	parent   Pattern
	children []Pattern
}

func (p withChildrenPattern) Match(n chplan.Node) (Bindings, bool) {
	if n == nil {
		return nil, false
	}
	kids := n.Children()
	if len(kids) != len(p.children) {
		return nil, false
	}
	parentB, ok := p.parent.Match(n)
	if !ok {
		return nil, false
	}
	out := Bindings{}
	for i, cp := range p.children {
		cb, ok := cp.Match(kids[i])
		if !ok {
			return nil, false
		}
		for k, v := range cb {
			out[k] = v
		}
	}
	for k, v := range parentB {
		out[k] = v
	}
	return out, true
}
