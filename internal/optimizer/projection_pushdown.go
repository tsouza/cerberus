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
// Shape (2) is what `FilterProjectTranspose` produces when it pushes a
// Filter under a Project — without this widening the
// `Project(Filter(Scan))` chain is order-dependent against the
// transpose rule. See `rule_interaction_test.go` Pair 21 for the
// commutativity check that this widening unlocks.
type ProjectionPushdown struct{}

func (ProjectionPushdown) Name() string { return "projection-pushdown" }

func (ProjectionPushdown) Apply(n chplan.Node) (chplan.Node, bool) {
	p, ok := n.(*chplan.Project)
	if !ok {
		return n, false
	}
	switch child := p.Input.(type) {
	case *chplan.Scan:
		return applyProjectScan(p, child)
	case *chplan.Filter:
		return applyProjectFilterScan(p, child)
	default:
		return n, false
	}
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
	seen := make(map[string]struct{}, len(a)+len(b))
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
