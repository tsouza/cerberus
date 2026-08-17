package chplan

// AggCombinator is a ClickHouse aggregate-function combinator: a suffix
// composed onto a base aggregate's name that changes what rows it
// consumes or how partial states combine (CH's own
// "Combinators for Aggregate Functions" family — `-If`, `-Array`,
// `-Merge`, `-State`, and others cerberus does not emit yet). The set
// below is sealed on the same "cerberus's own lowerings, not CH's full
// manual" principle [Fn] documents: a new combinator arrives as a
// deliberate vocabulary addition, never a bare string.
type AggCombinator string

const (
	// CombIf is CH's `-If` combinator: the base aggregate consumes only
	// the rows where its own TRAILING argument (appended positionally
	// after the base aggregate's normal arguments) evaluates true —
	// `argMinIf(arg, val, cond)` is `argMin(arg, val)` restricted to rows
	// where cond holds. The combinator itself carries no argument of its
	// own; Args on the owning AggFunc still lists every argument
	// (including cond) positionally.
	CombIf AggCombinator = "If"
)

// AggFunc is one aggregate-function call in an Aggregate node's projection
// list. The emitter resolves Fn (and any Combinators) and renders
// `<fn><combinators>(<Args>) AS <Alias>` for plain aggregates and
// `<fn><combinators>(<Params>)(<Args>) AS <Alias>` for parameterised
// aggregates (e.g. CH `quantile(0.95)(value)`).
//
// Fn is the sealed BASE function symbol — `argMin`, never `argMinIf`.
// Combinators is the ordered list of CH combinator suffixes composed onto
// it; nil/empty for a plain (uncombined) aggregate. Splitting the
// CH-suffixed spelling into Fn + Combinators (rather than a single
// literal Fn symbol per combined spelling, e.g. FnArgMinIf) keeps the
// structure a rule can pattern-match on ("this is argMin, restricted by
// something") instead of a string a rule would have to parse.
type AggFunc struct {
	Fn          Fn
	Combinators []AggCombinator
	// Params is the parameter list for parameterised aggregates (CH-style).
	// Nil/empty for plain aggregates.
	Params []Expr
	Args   []Expr
	Alias  string
}

// Equal reports structural equality with another AggFunc.
func (a AggFunc) Equal(other AggFunc) bool {
	if a.Fn != other.Fn || a.Alias != other.Alias ||
		len(a.Args) != len(other.Args) || len(a.Params) != len(other.Params) ||
		len(a.Combinators) != len(other.Combinators) {
		return false
	}
	for i := range a.Combinators {
		if a.Combinators[i] != other.Combinators[i] {
			return false
		}
	}
	for i := range a.Params {
		if !a.Params[i].Equal(other.Params[i]) {
			return false
		}
	}
	for i := range a.Args {
		if !a.Args[i].Equal(other.Args[i]) {
			return false
		}
	}
	return true
}

// Aggregate groups rows by the GroupBy expressions and computes the
// AggFuncs. SQL form: `SELECT <GroupBy>, <AggFuncs> FROM <Input> GROUP BY
// <GroupBy>`.
//
// GroupByAliases is an optional parallel slice (same length as GroupBy
// when non-empty); each entry aliases the matching group-key column in
// the SELECT list so a wrapping Project can reference it by name. Empty
// means "no aliases" — emit the raw expression.
//
// DropEmptyOnNoGroup controls the PromQL/LogQL "aggregation over empty
// input produces no result" semantics. When true AND GroupBy is empty,
// the emitter wraps the aggregate with a `count() > 0` guard so an
// empty Input projects 0 outer rows instead of CH's default 1-row-of-
// zeros for aggregate-only queries. PromQL / LogQL set it; TraceQL
// (whose `| count() = 0` idiom requires a 0 row for empty input) does
// not. Has no effect when GroupBy is non-empty.
//
// Having is an optional post-aggregation predicate rendered as a real SQL
// `HAVING <Having>` clause. Nil means "no HAVING". This is deliberately
// NOT the same shape as a runtime guard riding as an extra unreferenced
// AggFunc in the SELECT list: ClickHouse's column-pruning analyzer drops
// a SELECT-list expression nothing downstream reads — including a
// `throwIf(...)` planted purely for its side effect — so a guard that
// needs to fire unconditionally belongs in Having, where it gates row
// production and cannot be pruned as dead code. Has no effect when
// GroupBy is empty and DropEmptyOnNoGroup is set (that path renders a
// different two-layer shape — see emitAggregateNoGroup); Having and
// DropEmptyOnNoGroup are not combined by any caller today.
type Aggregate struct {
	Input              Node
	GroupBy            []Expr
	GroupByAliases     []string
	AggFuncs           []AggFunc
	Having             Expr
	DropEmptyOnNoGroup bool
}

func (*Aggregate) planNode() {}

func (a *Aggregate) Children() []Node { return []Node{a.Input} }

func (a *Aggregate) Equal(other Node) bool {
	o, ok := other.(*Aggregate)
	if !ok || len(a.GroupBy) != len(o.GroupBy) || len(a.GroupByAliases) != len(o.GroupByAliases) ||
		len(a.AggFuncs) != len(o.AggFuncs) || a.DropEmptyOnNoGroup != o.DropEmptyOnNoGroup {
		return false
	}
	for i := range a.GroupByAliases {
		if a.GroupByAliases[i] != o.GroupByAliases[i] {
			return false
		}
	}
	for i := range a.GroupBy {
		if !a.GroupBy[i].Equal(o.GroupBy[i]) {
			return false
		}
	}
	for i := range a.AggFuncs {
		if !a.AggFuncs[i].Equal(o.AggFuncs[i]) {
			return false
		}
	}
	if (a.Having == nil) != (o.Having == nil) {
		return false
	}
	if a.Having != nil && !a.Having.Equal(o.Having) {
		return false
	}
	return a.Input.Equal(o.Input)
}
