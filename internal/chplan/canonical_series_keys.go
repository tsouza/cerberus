package chplan

import "sort"

// This file turns "a whole-Map series-identity key is ordered by key" from a
// property each head's lowering happens to establish into an invariant the
// plan itself is repaired against, at the one chokepoint every plan crosses on
// its way to SQL (chsql.Emit).
//
// Background. [CanonicalAttributesExpr] explains why a ClickHouse Map compares
// POSITIONALLY and why an uncanonicalised whole-Map GROUP BY / PARTITION BY /
// LIMIT-BY key splits one logical series into two. Each head's lowering wraps
// its own identity projection, so production plans are canonical. Nothing
// enforced that, though: the emitter propagates a group key verbatim, so a plan
// assembled WITHOUT going through a head's lowering — a hand-built chplan tree,
// or a future refactor that repoints a node's Input past the canonicalising
// projection — emitted positionally-split SQL with no fixture movement and no
// error. The failure is silent and presents as a shifted number.
//
// Why the repair is a `* REPLACE` layer and not a wrap of the key expression.
// The windowed-array emitters render ONE group-key fragment and reuse it at
// every nesting level of the SELECT stack, so wrapping the key in place would
// project a column ClickHouse names `mapSort(X)` while the level above, which
// re-renders `mapSort(X)`, then fails to resolve `X`. Canonicalising the COLUMN
// instead — beneath the node that keys on it, via `SELECT * REPLACE (mapSort(X)
// AS X)` — leaves the name, and therefore every reference to it, untouched.
//
// What counts as needing repair is deliberately narrow: the rewrite fires only
// when a key is PROVABLY a raw attribute-map table column. Anything it cannot
// resolve is left alone, so it never disturbs a plan it merely fails to
// understand — and because [CanonicalAttributesExpr] is idempotent and each
// head's lowering already canonicalises, it is a no-op on every plan that
// reaches it through a head. It is defence in depth behind lowering, not a
// replacement for it.

// AttributeMapColumns is the set of table columns whose value is a label Map —
// the columns a series-identity key must not read raw. It is supplied by the
// caller because chplan does not depend on internal/schema; chsql builds it
// from the OTel-CH schema defaults.
type AttributeMapColumns map[string]struct{}

// NewAttributeMapColumns builds an [AttributeMapColumns] set, ignoring empty
// names (a schema that clears a column disables that arm entirely).
func NewAttributeMapColumns(names ...string) AttributeMapColumns {
	set := make(AttributeMapColumns, len(names))
	for _, n := range names {
		if n != "" {
			set[n] = struct{}{}
		}
	}
	return set
}

// Has reports whether name is one of the label-Map columns.
func (c AttributeMapColumns) Has(name string) bool {
	_, ok := c[name]
	return ok
}

// mapValuedFuncs are the ClickHouse functions that RETURN a Map, so a group key
// rooted at one is known to be a whole-Map key. The set is used only to decide
// whether an unwrapped key is provably a Map; being incomplete therefore costs
// a missed detection, never a false rejection.
var mapValuedFuncs = map[string]struct{}{
	"map":           {},
	"mapAdd":        {},
	"mapApply":      {},
	"mapConcat":     {},
	"mapFilter":     {},
	"mapFromArrays": {},
	"mapSubtract":   {},
	"mapUpdate":     {},
}

// CanonicalizeSeriesIdentityKeys returns root with every series-identity key
// that provably reads a raw attribute Map made key-order-independent, by
// inserting a `* REPLACE (mapSort(<col>) AS <col>)` [Project] beneath the node
// that keys on it. It is the always-run repair on the emit path, so the
// invariant holds for plans that never crossed a head's lowering.
//
// The walk is bottom-up so a node's keys are judged against inputs that have
// already been repaired, which is what makes the rewrite reach a fixpoint in
// one pass: once a column is canonical at an input, no ancestor keying on the
// same column sees it as raw. Plans that are already canonical — every plan a
// head produces — come back as the identical node.
func CanonicalizeSeriesIdentityKeys(root Node, mapCols AttributeMapColumns) Node {
	if root == nil || len(mapCols) == 0 {
		return root
	}
	out, _ := canonicalizeSeriesKeys(root, mapCols)
	return out
}

func canonicalizeSeriesKeys(n Node, mapCols AttributeMapColumns) (Node, bool) {
	if n == nil {
		return n, false
	}
	cur, changed := RewriteChildren(n, func(c Node) (Node, bool) {
		return canonicalizeSeriesKeys(c, mapCols)
	})
	if !changed {
		cur = n
	}
	cols := rawSeriesKeyColumns(cur, mapCols)
	if len(cols) == 0 {
		return cur, changed
	}
	repaired, _ := RewriteChildren(cur, func(c Node) (Node, bool) {
		return canonicalizingProject(c, cols), true
	})
	return repaired, true
}

// CanonicalizeSeriesKeyExprs returns keys with every entry that provably reads
// a raw attribute Map from inputs wrapped in [CanonicalAttributesExpr], leaving
// the rest untouched (and returning keys itself when nothing needs wrapping).
//
// It is the entry point for emitters that project their series-identity keys
// under an ALIAS and reference them by that alias afterwards, where wrapping
// the key expression itself is safe. [CanonicalizeSeriesIdentityKeys] is the
// one to use for a whole plan, where an unaliased key would otherwise change
// the column name every level above it is written against.
func CanonicalizeSeriesKeyExprs(keys []Expr, inputs []Node, mapCols AttributeMapColumns) []Expr {
	if len(keys) == 0 || len(mapCols) == 0 {
		return keys
	}
	var out []Expr
	for i, k := range keys {
		if _, raw := rawAttributeMapKey(k, inputs, mapCols); !raw {
			continue
		}
		if out == nil {
			out = make([]Expr, len(keys))
			copy(out, keys)
		}
		out[i] = CanonicalAttributesExpr(k)
	}
	if out == nil {
		return keys
	}
	return out
}

// rawSeriesKeyColumns names, in a stable order, the attribute-map columns that
// n keys series identity on while still reading them raw.
func rawSeriesKeyColumns(n Node, mapCols AttributeMapColumns) []string {
	var found map[string]struct{}
	for _, key := range seriesIdentityKeys(n) {
		col, raw := rawAttributeMapKey(key, n.Children(), mapCols)
		if !raw {
			continue
		}
		if found == nil {
			found = make(map[string]struct{}, 1)
		}
		found[col] = struct{}{}
	}
	if len(found) == 0 {
		return nil
	}
	cols := make([]string, 0, len(found))
	for c := range found {
		cols = append(cols, c)
	}
	sort.Strings(cols)
	return cols
}

// canonicalizingProject wraps in with the pass-through [Project] that rewrites
// each named column to its canonical key order in place.
func canonicalizingProject(in Node, cols []string) Node {
	reps := make([]Projection, 0, len(cols))
	for _, c := range cols {
		reps = append(reps, Projection{
			Expr:  CanonicalAttributesExpr(&ColumnRef{Name: c}),
			Alias: c,
		})
	}
	return &Project{Input: in, Replacements: reps}
}

// seriesIdentityKeys returns the key list by which n establishes series
// identity — the expressions that end up in a GROUP BY, PARTITION BY, or
// LIMIT n BY. Nodes that carry no such list return nil.
//
// VectorJoin / NaryVectorSetOp / MetricsCompare are absent by construction, not
// by omission: they name their join key by COLUMN NAME (AttributesColumn), so
// they read whichever expression their arm projected under that name, and the
// arm's own projection is where the invariant binds.
func seriesIdentityKeys(n Node) []Expr {
	switch v := n.(type) {
	case *Aggregate:
		return v.GroupBy
	case *MetricsAggregate:
		return v.GroupBy
	case *MetricsHistogramOverTime:
		return v.GroupBy
	case *HistogramQuantile:
		return v.GroupBy
	case *HistogramQuantileNative:
		return v.GroupBy
	case *RangeWindow:
		return v.GroupBy
	case *RangeWindowNative:
		// With the label shaping deferred, the node's OUTPUT series identity
		// is the PASS-THROUGH GroupBy keys plus the Recollapse expressions.
		// The shaping INPUTS are excluded because they are consumed entirely
		// below the merge, which re-pools them: reporting them would have the
		// walk see a raw `Attributes` map as this node's identity key and
		// splice a repair Project on top of a result the deferred tower has
		// already canonicalised. The pass-through keys are the opposite case —
		// grouped raw at the state level, re-grouped on the same raw frag at
		// the merge level and emitted verbatim — so excluding them would let a
		// pass-through attribute Map key split one logical series into two
		// positional groups, each carrying a partial rate.
		if len(v.Recollapse) > 0 {
			return recollapseIdentityKeys(v)
		}
		return v.GroupBy
	case *RangeBucketFanout:
		return v.GroupBy
	case *TopK:
		return v.By
	}
	return nil
}

// rawAttributeMapKey reports whether key provably resolves, through inputs, to
// a raw attribute-map column with no [CanonicalMapFunc] anywhere above it, and
// names that column when it does.
//
// The key-order-preserving Map wrappers (`without(...)`, the empty-value strip)
// inherit whatever order they were handed, so the invariant binds on the map
// they read — matching where each head's lowering puts the wrap, which is what
// keeps this gate silent on canonical plans.
func rawAttributeMapKey(key Expr, inputs []Node, mapCols AttributeMapColumns) (string, bool) {
	switch v := key.(type) {
	case *FuncCall:
		if v.Name == CanonicalMapFunc {
			return "", false
		}
		if _, isMap := mapValuedFuncs[v.Name]; isMap {
			// A Map-returning call sitting bare in a key list is the binding
			// site — there is no projection above it to carry the wrap. Which
			// of its Map arguments is raw is what makes the whole key raw, so
			// an argument that is already canonical answers for the call.
			for _, arg := range v.Args {
				if col, raw := rawAttributeMapKey(arg, inputs, mapCols); raw {
					return col, true
				}
			}
		}
	case *MapWithoutKeys:
		return rawAttributeMapKey(v.Map, inputs, mapCols)
	case *MapWithoutEmptyValues:
		return rawAttributeMapKey(v.Map, inputs, mapCols)
	case *ColumnRef:
		if !mapCols.Has(v.Name) {
			return "", false
		}
		if resolvesToRawColumn(v.Name, inputs, mapCols) {
			return v.Name, true
		}
	}
	return "", false
}

// resolvesToRawColumn reports whether name is PROVABLY still the raw table
// column at every input that binds it — i.e. no projection or grouping between
// here and the scan rebinds it to a canonicalised expression.
//
// Unresolvable shapes answer false. A multi-input node (a union, a join) counts
// as raw only when every input that resolves at all agrees, so an arm that
// canonicalises suppresses the finding rather than triggering one.
func resolvesToRawColumn(name string, inputs []Node, mapCols AttributeMapColumns) bool {
	resolved, raw := false, true
	for _, in := range inputs {
		verdict, ok := columnBindingIsRaw(name, in, mapCols)
		if !ok {
			continue
		}
		resolved = true
		raw = raw && verdict
	}
	return resolved && raw
}

// columnBindingIsRaw resolves name against the columns n produces. The second
// result is false when the binding cannot be determined, which is the answer
// for any node shape this walk does not model.
func columnBindingIsRaw(name string, n Node, mapCols AttributeMapColumns) (bool, bool) {
	switch v := n.(type) {
	case nil:
		return false, false
	case *Scan:
		// A scan hands through the table's own column, so the name is raw
		// exactly when the schema calls it a label Map.
		return mapCols.Has(name), true
	case *Project:
		if len(v.Projections) == 0 {
			// `SELECT *` — a REPLACE entry rebinds the name in place;
			// anything else is whatever the input holds.
			for _, r := range v.Replacements {
				if r.Alias != name {
					continue
				}
				_, raw := rawAttributeMapKey(r.Expr, v.Children(), mapCols)
				return raw, true
			}
			break
		}
		for _, p := range v.Projections {
			if p.Alias != name {
				continue
			}
			_, raw := rawAttributeMapKey(p.Expr, v.Children(), mapCols)
			return raw, true
		}
		// An explicit projection list that does not mention the name does not
		// produce it; treat the binding as unknown rather than guessing.
		return false, false
	}
	// A node that rebinds the name as one of its own aliased group keys owns
	// the invariant for it, and is visited by the walk in its own right.
	if aliases := seriesIdentityKeyAliases(n); len(aliases) > 0 {
		for i, alias := range aliases {
			if alias != name {
				continue
			}
			keys := seriesIdentityKeys(n)
			if i >= len(keys) {
				break
			}
			_, raw := rawAttributeMapKey(keys[i], n.Children(), mapCols)
			return raw, true
		}
	}
	children := n.Children()
	if len(children) == 0 {
		return false, false
	}
	resolved, raw := false, true
	for _, c := range children {
		verdict, ok := columnBindingIsRaw(name, c, mapCols)
		if !ok {
			continue
		}
		resolved = true
		raw = raw && verdict
	}
	return raw, resolved
}

// seriesIdentityKeyAliases returns the SELECT-list aliases n projects its
// series-identity keys under, positionally aligned with [seriesIdentityKeys].
// Nodes whose keys carry no alias slot return nil — their keys reach the SELECT
// list unaliased, so a downstream reference by column name resolves to whatever
// the key expression itself rendered.
func seriesIdentityKeyAliases(n Node) []string {
	switch v := n.(type) {
	case *Aggregate:
		return v.GroupByAliases
	case *MetricsAggregate:
		return v.GroupByAliases
	case *MetricsHistogramOverTime:
		return v.GroupByAliases
	case *HistogramQuantile:
		return v.GroupByAliases
	case *HistogramQuantileNative:
		return v.GroupByAliases
	case *RangeBucketFanout:
		return v.GroupByAliases
	case *RangeWindowNative:
		// Only the deferred-shaping shape aliases its keys; the two-level one
		// projects GroupBy unaliased, so it keeps the nil fall-through.
		if len(v.Recollapse) > 0 {
			return recollapseIdentityKeyAliases(v)
		}
	}
	return nil
}

// recollapseIdentityKeys / recollapseIdentityKeyAliases answer a deferred-shaping
// [RangeWindowNative]'s output series identity: its PASS-THROUGH GroupBy keys in
// GroupBy order, followed by its Recollapse expressions. The two lists are read
// POSITIONALLY against each other by [columnBindingIsRaw], so they must be built
// from the same partition — a pass-through key's alias is its own column name,
// which is what the merge and outer levels emit it under.
func recollapseIdentityKeys(r *RangeWindowNative) []Expr {
	pass, _ := recollapsePassThroughGroupBy(r)
	keys := make([]Expr, 0, len(pass)+len(r.Recollapse))
	for _, i := range pass {
		keys = append(keys, r.GroupBy[i])
	}
	return append(keys, projectionExprs(r.Recollapse)...)
}

func recollapseIdentityKeyAliases(r *RangeWindowNative) []string {
	pass, cols := recollapsePassThroughGroupBy(r)
	aliases := make([]string, 0, len(pass)+len(r.Recollapse))
	for _, i := range pass {
		aliases = append(aliases, cols[i])
	}
	return append(aliases, projectionAliases(r.Recollapse)...)
}

// recollapsePassThroughGroupBy returns the pass-through GroupBy indexes and the
// GroupBy column names they index into. A GroupBy that is not the plain-column
// shape the deferred-shaping emitter requires has no pass-through set at all:
// such a node is rejected before it renders, so the identity is reported as the
// shaped keys alone rather than guessed at.
func recollapsePassThroughGroupBy(r *RangeWindowNative) (pass []int, cols []string) {
	cols, computed := r.GroupByColumns()
	if computed != nil {
		return nil, nil
	}
	pass, _ = r.PartitionRecollapseGroupBy(cols)
	return pass, cols
}

// projectionExprs and projectionAliases split a [Projection] list into the two
// positionally-aligned halves [seriesIdentityKeys] and
// [seriesIdentityKeyAliases] answer with.
func projectionExprs(ps []Projection) []Expr {
	out := make([]Expr, len(ps))
	for i := range ps {
		out[i] = ps[i].Expr
	}
	return out
}

func projectionAliases(ps []Projection) []string {
	out := make([]string, len(ps))
	for i := range ps {
		out[i] = ps[i].Alias
	}
	return out
}
