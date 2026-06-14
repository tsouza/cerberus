package optimizer

import (
	"sort"

	"github.com/tsouza/cerberus/internal/chplan"
)

// ProjectionPushdown narrows a Scan's column list to the union of columns
// referenced by an immediately-enclosing Project (and any intervening
// Filter). With the OTel CH schema's wide tables (~10+ columns including
// resource attributes), this is a real win: only the columns the plan
// actually consumes get read.
//
// Two shapes match:
//
//  1. `Project(Scan)` — narrow Scan.Columns to the column refs the
//     Projections touch.
//  2. `Project(Filter(Scan))` — push the projection THROUGH the Filter
//     down to the Scan. Safe iff Scan.Columns is empty (the Filter's
//     predicate evaluates on Scan's row shape, which after narrowing
//     must still include every column the predicate touches). The
//     narrowed Scan.Columns is the UNION of refs(Projections) ∪
//     refs(Filter.Predicate); the Filter stays in place between the
//     Project and the (now-narrowed) Scan.
//
// Shape (2) handles the `Project(Filter(Scan))` chain that lowerings
// emit directly (a projection wrapping a label-filtered scan): without
// this widening the pushdown would stop at the intervening Filter and
// leave the Scan reading every column.
//
// Two further shapes match a *mid-tree stage* node — an Aggregate or a
// RangeWindow — sitting directly over Scan or Filter(Scan):
//
//  3. `Aggregate(Scan)` / `Aggregate(Filter(Scan))` — narrow the Scan to
//     the union of the columns the Aggregate's GroupBy keys and AggFunc
//     params/args read, plus the Filter predicate's columns when present.
//  4. `RangeWindow(Scan)` / `RangeWindow(Filter(Scan))` — narrow the Scan
//     to the union of TimestampColumn + ValueColumn (the per-sample pair
//     the windowed-array idiom reads), the GroupBy + ScalarExprs column
//     refs, plus the Filter predicate's columns when present.
//
// Shapes (3) and (4) are what makes the pushdown reach the inner Scan of
// the canonical metrics shape `Project(Aggregate(Filter(Scan)))` (and the
// matrix `Project(RangeWindow(Filter(Scan)))`): the Project's pushdown in
// shapes (1)/(2) stops at the Aggregate/RangeWindow, so without these arms
// the inner Scan still reads every column (`SELECT *`). Firing on the
// stage node itself — wherever the FixedPoint walk reaches it — pushes the
// narrowed column set THROUGH the stage down to the Scan.
type ProjectionPushdown struct{}

func (ProjectionPushdown) Name() string { return "projection-pushdown" }

func (ProjectionPushdown) Apply(n chplan.Node) (chplan.Node, bool) {
	switch node := n.(type) {
	case *chplan.Project:
		switch child := node.Input.(type) {
		case *chplan.Scan:
			return applyProjectScan(node, child)
		case *chplan.Filter:
			return applyProjectFilterScan(node, child)
		default:
			return n, false
		}
	case *chplan.Aggregate:
		return applyStageScan(node, node.Input, aggregateColumns(node))
	case *chplan.RangeWindow:
		return applyStageScan(node, node.Input, rangeWindowColumns(node))
	default:
		return n, false
	}
}

// applyStageScan pushes a stage node's required-column set THROUGH an
// optional intervening Filter down to the inner Scan, narrowing the Scan
// in place. `stage` is the Aggregate / RangeWindow being rewritten,
// `input` is stage's child (the Scan or Filter(Scan)), and stageCols is
// the set of base columns the stage's own emit reads.
//
// The inner Scan is located as either `input` directly (Scan) or
// `input.(*Filter).Input` (Filter(Scan)). Any other shape bails — this
// keeps the rule total and avoids narrowing under a MetricsAggregate /
// subquery input the stage emitters read differently.
//
// The idempotence guard `len(scan.Columns) > 0` mirrors the two existing
// helpers: it stops the rule re-firing under the FixedPoint strategy and
// avoids fighting MVSubstitution, which deliberately clears Columns when
// it rewrites a Scan onto a rollup MV.
func applyStageScan(stage, input chplan.Node, stageCols []string) (chplan.Node, bool) {
	switch in := input.(type) {
	case *chplan.Scan:
		if len(in.Columns) > 0 {
			return stage, false
		}
		cols := unionSortedColumns(stageCols, nil)
		if len(cols) == 0 {
			return stage, false
		}
		newScan := *in
		newScan.Columns = cols
		return cloneStageOverInput(stage, &newScan), true
	case *chplan.Filter:
		scan, ok := in.Input.(*chplan.Scan)
		if !ok || len(scan.Columns) > 0 {
			return stage, false
		}
		cols := unionSortedColumns(stageCols, predicateColumns(in.Predicate))
		if len(cols) == 0 {
			return stage, false
		}
		newScan := *scan
		newScan.Columns = cols
		newFilter := *in
		newFilter.Input = &newScan
		return cloneStageOverInput(stage, &newFilter), true
	default:
		return stage, false
	}
}

// cloneStageOverInput returns a shallow clone of the stage node (Aggregate
// or RangeWindow) with its Input replaced by newInput. The clone keeps the
// optimizer's no-mutate-in-place contract: the original tree is untouched,
// the rewritten subtree is fresh.
func cloneStageOverInput(stage, newInput chplan.Node) chplan.Node {
	switch s := stage.(type) {
	case *chplan.Aggregate:
		clone := *s
		clone.Input = newInput
		return &clone
	case *chplan.RangeWindow:
		clone := *s
		clone.Input = newInput
		return &clone
	default:
		return stage
	}
}

// aggregateColumns returns the sorted, deduped set of base columns an
// Aggregate's own emit reads: the union of every GroupBy key expression
// and every AggFunc's Params + Args. emitAggregate (chsql/emit_node.go)
// selects exactly these — GroupBy in the SELECT/GROUP BY, AggFuncs as the
// reducers — so this is the complete set the narrowed Scan must carry.
func aggregateColumns(a *chplan.Aggregate) []string {
	seen := map[string]struct{}{}
	collect := collectColumn(seen)
	for _, g := range a.GroupBy {
		walkExpr(g, collect)
	}
	for _, af := range a.AggFuncs {
		for _, p := range af.Params {
			walkExpr(p, collect)
		}
		for _, ar := range af.Args {
			walkExpr(ar, collect)
		}
	}
	return sortedColumnSet(seen)
}

// rangeWindowColumns returns the sorted, deduped set of base columns a
// RangeWindow's row-shape emit reads: the bare-string TimestampColumn +
// ValueColumn (the per-sample pair the windowed-array idiom consumes —
// added literally, they are plain column names, not Exprs), plus the
// column refs walked out of GroupBy and ScalarExprs.
//
// Only the row-shape (PromQL / LogQL) RangeWindow over Scan / Filter(Scan)
// reaches this: applyStageScan bails on a MetricsAggregate input before we
// get here, so the TraceQL matrix mode (which ignores ValueColumn and
// reads the per-span Timestamp off its MetricsAggregate.Inner) is never
// narrowed by this path.
func rangeWindowColumns(r *chplan.RangeWindow) []string {
	seen := map[string]struct{}{}
	if r.TimestampColumn != "" {
		seen[r.TimestampColumn] = struct{}{}
	}
	if r.ValueColumn != "" {
		seen[r.ValueColumn] = struct{}{}
	}
	collect := collectColumn(seen)
	for _, g := range r.GroupBy {
		walkExpr(g, collect)
	}
	for _, s := range r.ScalarExprs {
		walkExpr(s, collect)
	}
	return sortedColumnSet(seen)
}

// sortedColumnSet flattens a column-name set into a sorted, deduped slice.
// Shared by aggregateColumns / rangeWindowColumns so the narrowed Scan's
// Columns is reproducible across runs.
func sortedColumnSet(seen map[string]struct{}) []string {
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// applyProjectScan handles the seed `Project(Scan)` shape.
func applyProjectScan(p *chplan.Project, s *chplan.Scan) (chplan.Node, bool) {
	if len(s.Columns) > 0 {
		return p, false
	}
	cols := referencedColumns(p.Projections)
	if len(cols) == 0 {
		return p, false
	}
	newScan := *s
	newScan.Columns = cols
	newProject := *p
	newProject.Input = &newScan
	return &newProject, true
}

// applyProjectFilterScan handles `Project(Filter(Scan))`: push the
// projection's column set THROUGH the Filter down to the Scan, unioning
// in any columns the Filter's predicate references so the predicate
// remains evaluable on the narrowed row shape.
func applyProjectFilterScan(p *chplan.Project, f *chplan.Filter) (chplan.Node, bool) {
	s, ok := f.Input.(*chplan.Scan)
	if !ok || len(s.Columns) > 0 {
		return p, false
	}
	projCols := referencedColumns(p.Projections)
	if len(projCols) == 0 {
		return p, false
	}
	cols := unionSortedColumns(projCols, predicateColumns(f.Predicate))

	newScan := *s
	newScan.Columns = cols
	newFilter := *f
	newFilter.Input = &newScan
	newProject := *p
	newProject.Input = &newFilter
	return &newProject, true
}

// predicateColumns returns the set of ColumnRef names the predicate
// references, deduped and sorted. Bare-column refs only — qualified
// refs (joins, not in scope here) are not expected on a Filter directly
// over a Scan.
func predicateColumns(e chplan.Expr) []string {
	seen := map[string]struct{}{}
	walkExpr(e, collectColumn(seen))
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// collectColumn returns a walkExpr visitor that records every column
// name an expression node reads into seen. Two node kinds carry one:
// ColumnRef (by definition) and NestedArrayExists, whose Column field
// names the Nested carrier (e.g. `Events`) as a plain string rather
// than a child ColumnRef.
func collectColumn(seen map[string]struct{}) func(chplan.Expr) {
	return func(sub chplan.Expr) {
		switch v := sub.(type) {
		case *chplan.ColumnRef:
			seen[v.Name] = struct{}{}
		case *chplan.NestedArrayExists:
			seen[v.Column] = struct{}{}
		}
	}
}

// unionSortedColumns merges two already-sorted, deduped column-name
// slices into a single sorted+deduped slice. Stable, deterministic
// output keeps Scan.Columns reproducible across runs.
func unionSortedColumns(a, b []string) []string {
	// No capacity hint: `len(a)+len(b)` is only an upper-bound pre-size
	// (a and b may overlap), so it has no observable effect on the
	// result — a gremlins ARITHMETIC_BASE mutant on the `+` is
	// equivalent and unkillable. Dropping the hint removes the dead
	// arithmetic; the union result is identical.
	seen := make(map[string]struct{})
	for _, n := range a {
		seen[n] = struct{}{}
	}
	for _, n := range b {
		seen[n] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// referencedColumns returns the set of ColumnRef names referenced anywhere
// in projs, deduped and sorted for deterministic emission.
func referencedColumns(projs []chplan.Projection) []string {
	seen := map[string]struct{}{}
	for _, p := range projs {
		walkExpr(p.Expr, collectColumn(seen))
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// walkExpr visits e and every Expr reachable from it. Every chplan
// Expr kind that holds child Exprs must appear here: a missing case
// silently hides the columns the subtree reads, ProjectionPushdown
// then prunes them from the Scan, and ClickHouse fails outer-scope
// resolution with error 47 (UNKNOWN_IDENTIFIER). That exact escape —
// FieldAccess.Source unwalked, so `| select(span.http.method)`'s
// SpanAttributes carrier was pruned on the plain-filter /api/search
// arm — 502'd Grafana's showcase select panel; see
// internal/api/tempo/search_select_plain_filter_chdb_test.go for the
// end-to-end pin. There's no chplan-side helper for this yet because
// the optimizer is so far the only caller; if we add a second consumer
// it should graduate into chplan.
func walkExpr(e chplan.Expr, visit func(chplan.Expr)) {
	if e == nil {
		return
	}
	visit(e)
	switch v := e.(type) {
	case *chplan.Binary:
		walkExpr(v.Left, visit)
		walkExpr(v.Right, visit)
	case *chplan.InList:
		walkExpr(v.Left, visit)
		for _, e := range v.List {
			walkExpr(e, visit)
		}
	case *chplan.FuncCall:
		for _, a := range v.Args {
			walkExpr(a, visit)
		}
	case *chplan.MapAccess:
		walkExpr(v.Map, visit)
		walkExpr(v.Key, visit)
	case *chplan.FieldAccess:
		walkExpr(v.Source, visit)
	case *chplan.Subscript:
		walkExpr(v.Container, visit)
		walkExpr(v.Key, visit)
	case *chplan.Lambda:
		walkExpr(v.Body, visit)
	case *chplan.LabelJoin:
		walkExpr(v.Map, visit)
	case *chplan.LabelReplace:
		walkExpr(v.Map, visit)
	case *chplan.LineContent:
		walkExpr(v.Source, visit)
	case *chplan.MapWithoutEmptyValues:
		walkExpr(v.Map, visit)
	case *chplan.MapWithoutKeys:
		walkExpr(v.Map, visit)
	case *chplan.NestedArrayExists:
		walkExpr(v.Value, visit)
	}
}
