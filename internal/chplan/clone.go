package chplan

import "fmt"

// CloneNode returns a deep copy of n: a fresh tree that shares no mutable
// state (no node pointer, no Expr pointer, no slice backing array) with the
// original. Mutating the returned tree — including any windowed bound, any
// GroupBy/Projection slice element, or any embedded ScalarSubquery.Input —
// leaves n and every node/expr reachable from it byte-identical.
//
// CloneNode is exhaustive over every concrete Node type (the switch's
// default panics rather than silently aliasing). That exhaustiveness backs
// the solver's slicing path: ReanchorRange CLONES the O(spine-depth) re-
// gridded spine nodes and SHARES the immutable off-spine subtree across the K
// shards (a copy-on-write view, sound under the no-mutate-after-slice
// contract); CloneNode is the fallback the slicer uses to deep-copy a subtree
// when it genuinely must mutate it in isolation (e.g. the GUARDRAIL B
// nested-subquery descend, where an off-spine subtree carries its own window
// that must be zeroed). When a new Node type is added, this switch and
// TestCloneNodeExhaustive in clone_test.go fail in lock-step, forcing the
// author to extend the copy.
//
// CloneNode does NOT re-anchor anything — it is a pure copy. ReanchorRange
// composes the copy with the grid rewrite.
func CloneNode(n Node) Node {
	if n == nil {
		return nil
	}
	switch v := n.(type) {
	case *Scan:
		c := *v
		c.UnionTables = cloneStrings(v.UnionTables)
		c.Columns = cloneStrings(v.Columns)
		return &c
	case *Filter:
		return &Filter{Input: CloneNode(v.Input), Predicate: cloneExpr(v.Predicate)}
	case *SearchTraceLimit:
		c := *v
		c.Input = CloneNode(v.Input)
		return &c
	case *Project:
		return &Project{Input: CloneNode(v.Input), Projections: cloneProjections(v.Projections)}
	case *Aggregate:
		return &Aggregate{
			Input:              CloneNode(v.Input),
			GroupBy:            cloneExprs(v.GroupBy),
			GroupByAliases:     cloneStrings(v.GroupByAliases),
			AggFuncs:           cloneAggFuncs(v.AggFuncs),
			DropEmptyOnNoGroup: v.DropEmptyOnNoGroup,
		}
	case *RangeWindow:
		c := *v
		c.Input = CloneNode(v.Input)
		c.GroupBy = cloneExprs(v.GroupBy)
		c.Scalars = cloneFloats(v.Scalars)
		c.ScalarExprs = cloneExprs(v.ScalarExprs)
		return &c
	case *RangeWindowNative:
		c := *v
		c.Input = CloneNode(v.Input)
		c.GroupBy = cloneExprs(v.GroupBy)
		return &c
	case *RangeLWR:
		c := *v
		c.Input = CloneNode(v.Input)
		return &c
	case *RangeWindowResample:
		c := *v
		c.Input = CloneNode(v.Input)
		return &c
	case *RangeBucketFanout:
		c := *v
		c.Input = CloneNode(v.Input)
		c.GroupBy = cloneExprs(v.GroupBy)
		c.GroupByAliases = cloneStrings(v.GroupByAliases)
		c.AggFuncs = cloneAggFuncs(v.AggFuncs)
		return &c
	case *StepGrid:
		c := *v
		return &c
	case *AbsentOverTime:
		c := *v
		c.Input = CloneNode(v.Input)
		c.SynthLabels = cloneSynthLabels(v.SynthLabels)
		return &c
	case *TopK:
		c := *v
		c.Input = CloneNode(v.Input)
		c.KExpr = CloneNode(v.KExpr)
		c.By = cloneExprs(v.By)
		c.SortExpr = cloneExpr(v.SortExpr)
		c.Columns = cloneStrings(v.Columns)
		return &c
	case *Limit:
		return &Limit{Input: CloneNode(v.Input), Count: v.Count}
	case *OrderBy:
		return &OrderBy{Input: CloneNode(v.Input), Keys: cloneOrderKeys(v.Keys)}
	case *OneRow:
		c := *v
		return &c
	case *UnionAll:
		return &UnionAll{Inputs: cloneNodes(v.Inputs)}
	default:
		return cloneCompositeNode(n)
	}
}

// cloneCompositeNode deep-copies the join / set-op / histogram / metrics /
// trace Node families. Split out of CloneNode so each type switch stays within
// the funlen budget; together the two functions remain exhaustive over every
// planNode() implementer — the TestCloneNodeExhaustive lock-step guard proves
// no kind is missed, and the default below still panics on an unknown type so a
// new kind cannot silently alias into a re-anchored shard plan.
func cloneCompositeNode(n Node) Node {
	switch v := n.(type) {
	case *CrossJoin:
		return &CrossJoin{Left: CloneNode(v.Left), Right: CloneNode(v.Right)}
	case *SetOperation:
		c := *v
		c.Left = CloneNode(v.Left)
		c.Right = CloneNode(v.Right)
		return &c
	case *StructuralJoin:
		c := *v
		c.Left = CloneNode(v.Left)
		c.Right = CloneNode(v.Right)
		c.ExtraProjectionColumns = cloneStrings(v.ExtraProjectionColumns)
		return &c
	case *VectorJoin:
		c := *v
		c.Left = CloneNode(v.Left)
		c.Right = CloneNode(v.Right)
		c.Match.Labels = cloneStrings(v.Match.Labels)
		c.Include = cloneStrings(v.Include)
		return &c
	case *VectorSetOp:
		c := *v
		c.Left = CloneNode(v.Left)
		c.Right = CloneNode(v.Right)
		c.Match.Labels = cloneStrings(v.Match.Labels)
		return &c
	case *InfoJoin:
		return cloneInfoJoin(v)
	case *NaryVectorSetOp:
		return cloneNaryVectorSetOp(v)
	case *HistogramQuantile:
		return cloneHistogramQuantile(v)
	case *HistogramQuantileNative:
		return cloneHistogramQuantileNative(v)
	case *MetricsAggregate:
		return cloneMetricsAggregate(v)
	case *MetricsCompare:
		return cloneMetricsCompare(v)
	case *MetricsHistogramOverTime:
		return cloneMetricsHistogramOverTime(v)
	case *MetricsSecondStage:
		c := *v
		c.Input = CloneNode(v.Input)
		c.PartitionBy = cloneStrings(v.PartitionBy)
		return &c
	case *NestedSetAnnotate:
		c := *v
		c.Input = CloneNode(v.Input)
		return &c
	default:
		panic(fmt.Sprintf("chplan.CloneNode: unhandled Node type %T — extend the switch in clone.go", n))
	}
}

// cloneNaryVectorSetOp deep-copies a linearised N-ary vector set-op node,
// cloning every arm and the match-modifier label slice. Split out of
// cloneCompositeNode so that switch stays within the funlen budget.
func cloneNaryVectorSetOp(v *NaryVectorSetOp) Node {
	c := *v
	c.Arms = make([]Node, len(v.Arms))
	for i, arm := range v.Arms {
		c.Arms[i] = CloneNode(arm)
	}
	c.Match.Labels = cloneStrings(v.Match.Labels)
	return &c
}

// cloneInfoJoin deep-copies an info() label-enrichment join, cloning both
// plan inputs and the identity / data label slices. Split out of
// cloneCompositeNode so that switch stays within the funlen budget.
func cloneInfoJoin(v *InfoJoin) Node {
	c := *v
	c.Input = CloneNode(v.Input)
	c.Info = CloneNode(v.Info)
	c.IdentityLabels = cloneStrings(v.IdentityLabels)
	c.DataLabels = cloneStrings(v.DataLabels)
	return &c
}

// cloneHistogramQuantile deep-copies a classic-histogram quantile node.
// Split out of cloneCompositeNode so that switch stays within the funlen
// budget.
func cloneHistogramQuantile(v *HistogramQuantile) Node {
	c := *v
	c.Input = CloneNode(v.Input)
	c.PhiExpr = cloneExpr(v.PhiExpr)
	c.GroupBy = cloneExprs(v.GroupBy)
	c.GroupByAliases = cloneStrings(v.GroupByAliases)
	return &c
}

// cloneHistogramQuantileNative deep-copies a native-histogram quantile node.
// Split out of cloneCompositeNode so that switch stays within the funlen
// budget.
func cloneHistogramQuantileNative(v *HistogramQuantileNative) Node {
	c := *v
	c.Input = CloneNode(v.Input)
	c.PhiExpr = cloneExpr(v.PhiExpr)
	c.GroupBy = cloneExprs(v.GroupBy)
	c.GroupByAliases = cloneStrings(v.GroupByAliases)
	return &c
}

// cloneMetricsAggregate deep-copies a TraceQL metrics aggregate node, cloning
// its attribute expr, group-by exprs/aliases/display-names, quantile slice,
// and inner plan. Split out of cloneCompositeNode so that switch stays within
// the funlen budget.
func cloneMetricsAggregate(v *MetricsAggregate) Node {
	c := *v
	c.Attr = cloneExpr(v.Attr)
	c.GroupBy = cloneExprs(v.GroupBy)
	c.GroupByAliases = cloneStrings(v.GroupByAliases)
	c.GroupByDisplayNames = cloneStrings(v.GroupByDisplayNames)
	c.Quantiles = cloneFloats(v.Quantiles)
	c.Inner = CloneNode(v.Inner)
	return &c
}

// cloneMetricsCompare deep-copies a TraceQL compare() node, cloning its
// selection / pairs exprs and both plan inputs. Split out of
// cloneCompositeNode so that switch stays within the funlen budget.
func cloneMetricsCompare(v *MetricsCompare) Node {
	c := *v
	c.Selection = cloneExpr(v.Selection)
	c.Pairs = cloneExpr(v.Pairs)
	c.RootLookup = CloneNode(v.RootLookup)
	c.Inner = CloneNode(v.Inner)
	return &c
}

// cloneMetricsHistogramOverTime deep-copies a TraceQL histogram_over_time
// node, cloning its attribute expr, group-by exprs/aliases/display-names, and
// inner plan. Split out of cloneCompositeNode so that switch stays within the
// funlen budget.
func cloneMetricsHistogramOverTime(v *MetricsHistogramOverTime) Node {
	c := *v
	c.Attr = cloneExpr(v.Attr)
	c.GroupBy = cloneExprs(v.GroupBy)
	c.GroupByAliases = cloneStrings(v.GroupByAliases)
	c.GroupByDisplayNames = cloneStrings(v.GroupByDisplayNames)
	c.Inner = CloneNode(v.Inner)
	return &c
}

// cloneExpr returns a deep copy of e. Exhaustive over every concrete Expr
// type; the default panics so a new Expr type can't silently alias into a
// re-anchored shard plan. Mirrors inspectExpr's switch in walk_expr.go.
func cloneExpr(e Expr) Expr {
	if e == nil {
		return nil
	}
	switch v := e.(type) {
	case *ColumnRef:
		c := *v
		return &c
	case *LitString:
		c := *v
		return &c
	case *InlineString:
		c := *v
		return &c
	case *LitInt:
		c := *v
		return &c
	case *LitFloat:
		c := *v
		return &c
	case *LitBool:
		c := *v
		return &c
	case *BareIdent:
		c := *v
		return &c
	case *Binary:
		return &Binary{Op: v.Op, Left: cloneExpr(v.Left), Right: cloneExpr(v.Right)}
	case *FuncCall:
		return &FuncCall{Name: v.Name, Args: cloneExprs(v.Args)}
	case *InList:
		return &InList{Left: cloneExpr(v.Left), List: cloneExprs(v.List), Negated: v.Negated}
	case *FieldAccess:
		return &FieldAccess{Source: cloneExpr(v.Source), Path: v.Path}
	case *MapAccess:
		return &MapAccess{Map: cloneExpr(v.Map), Key: cloneExpr(v.Key)}
	case *Subscript:
		return &Subscript{Container: cloneExpr(v.Container), Key: cloneExpr(v.Key)}
	case *LineContent:
		c := *v
		c.Source = cloneExpr(v.Source)
		return &c
	case *LabelJoin:
		c := *v
		c.Map = cloneExpr(v.Map)
		c.Srcs = cloneStrings(v.Srcs)
		return &c
	case *LabelReplace:
		c := *v
		c.Map = cloneExpr(v.Map)
		return &c
	case *Lambda:
		return &Lambda{Params: cloneStrings(v.Params), Body: cloneExpr(v.Body)}
	case *MapWithoutKeys:
		return &MapWithoutKeys{Map: cloneExpr(v.Map), Keys: cloneStrings(v.Keys)}
	case *MapWithoutEmptyValues:
		return &MapWithoutEmptyValues{Map: cloneExpr(v.Map)}
	case *NestedArrayExists:
		c := *v
		c.Value = cloneExpr(v.Value)
		return &c
	case *ScalarSubquery:
		// chplan.Walk does NOT recurse into ScalarSubquery.Input (it is an
		// Expr, not a Node child), so a node-only copy walk would miss the
		// embedded plan subtree entirely. Copy it explicitly here.
		return &ScalarSubquery{Input: CloneNode(v.Input)}
	case *BoundedTraceScope:
		// Pure leaf (column names + limit, no embedded Node) — a flat value
		// copy, like the literals above.
		c := *v
		return &c
	default:
		panic(fmt.Sprintf("chplan.cloneExpr: unhandled Expr type %T — extend the switch in clone.go", e))
	}
}

func cloneNodes(in []Node) []Node {
	if in == nil {
		return nil
	}
	out := make([]Node, len(in))
	for i := range in {
		out[i] = CloneNode(in[i])
	}
	return out
}

func cloneExprs(in []Expr) []Expr {
	if in == nil {
		return nil
	}
	out := make([]Expr, len(in))
	for i := range in {
		out[i] = cloneExpr(in[i])
	}
	return out
}

func cloneProjections(in []Projection) []Projection {
	if in == nil {
		return nil
	}
	out := make([]Projection, len(in))
	for i := range in {
		out[i] = Projection{Expr: cloneExpr(in[i].Expr), Alias: in[i].Alias}
	}
	return out
}

func cloneAggFuncs(in []AggFunc) []AggFunc {
	if in == nil {
		return nil
	}
	out := make([]AggFunc, len(in))
	for i := range in {
		out[i] = AggFunc{
			Name:   in[i].Name,
			Params: cloneExprs(in[i].Params),
			Args:   cloneExprs(in[i].Args),
			Alias:  in[i].Alias,
		}
	}
	return out
}

func cloneOrderKeys(in []OrderKey) []OrderKey {
	if in == nil {
		return nil
	}
	out := make([]OrderKey, len(in))
	for i := range in {
		out[i] = OrderKey{Expr: cloneExpr(in[i].Expr), Desc: in[i].Desc}
	}
	return out
}

func cloneSynthLabels(in []SynthLabel) []SynthLabel {
	if in == nil {
		return nil
	}
	out := make([]SynthLabel, len(in))
	copy(out, in)
	return out
}

func cloneStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func cloneFloats(in []float64) []float64 {
	if in == nil {
		return nil
	}
	out := make([]float64, len(in))
	copy(out, in)
	return out
}
