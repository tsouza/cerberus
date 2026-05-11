package optimizer

import (
	"sort"

	"github.com/tsouza/cerberus/internal/chplan"
)

// ProjectionPushdown narrows a Scan's column list to the union of columns
// referenced by an immediately-enclosing Project. With the OTel CH schema's
// wide tables (~10+ columns including resource attributes), this is a real
// win: only the columns the plan actually consumes get read.
//
// v0.1 limitation: only fires for `Project(Scan)` directly. The
// `Project(Filter(Scan))` shape will be handled once we have a column-set
// analysis pass that flows down through Filter/Aggregate/RangeWindow.
type ProjectionPushdown struct{}

func (ProjectionPushdown) Name() string { return "projection-pushdown" }

func (ProjectionPushdown) Apply(n chplan.Node) (chplan.Node, bool) {
	p, ok := n.(*chplan.Project)
	if !ok {
		return n, false
	}
	s, ok := p.Input.(*chplan.Scan)
	if !ok || len(s.Columns) > 0 {
		return n, false
	}

	cols := referencedColumns(p.Projections)
	if len(cols) == 0 {
		return n, false
	}

	newScan := *s
	newScan.Columns = cols

	newProject := *p
	newProject.Input = &newScan
	return &newProject, true
}

// referencedColumns returns the set of ColumnRef names referenced anywhere
// in projs, deduped and sorted for deterministic emission.
func referencedColumns(projs []chplan.Projection) []string {
	seen := map[string]struct{}{}
	for _, p := range projs {
		walkExpr(p.Expr, func(e chplan.Expr) {
			if cr, ok := e.(*chplan.ColumnRef); ok {
				seen[cr.Name] = struct{}{}
			}
		})
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// walkExpr visits e and every Expr reachable from it. There's no
// chplan-side helper for this yet because the optimizer is so far the only
// caller; if we add a second consumer it should graduate into chplan.
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
	}
}
