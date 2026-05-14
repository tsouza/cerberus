package optimizer

import "github.com/tsouza/cerberus/internal/chplan"

// FilterProjectTranspose rewrites `Filter(Project(X), p)` → `Project(Filter(X, p))`
// when the Filter's predicate references only columns that the Project
// passes through unchanged from X.
//
// Lineage: Calcite's `FilterProjectTransposeRule` — see
// https://calcite.apache.org/javadocAggregate/org/apache/calcite/rel/rules/FilterProjectTransposeRule.html.
// The shape unlocks downstream rules: pushing the Filter under a Project
// lets `ProjectionPushdown` (currently fires on `Project(Scan)` only) see
// the new `Project(Filter(...))` chain through a future column-set pass,
// and exposes the Filter to `PREWHERE` promotion once the Scan becomes
// the Filter's immediate input again.
//
// Safety. The Filter sees the Project's *output* columns. Pushing it
// underneath the Project means the same predicate must be evaluable
// against the Project's *input* columns. We allow the rewrite only when
// every `ColumnRef` the predicate touches refers to a name that the
// Project passes through unchanged — i.e. there is a `Projection` whose
// `Expr` is `*ColumnRef{Name: N}` (same name as the predicate
// reference, no qualifier) and whose `Alias` is empty or equal to `N`.
//
// Conservative cases the rule leaves alone:
//
//   - Empty Projections (`SELECT *`) — could be safely pushed, but the
//     emitter renders that as a no-op layer; flatening is out of scope here.
//   - Projections that rename (`X AS Y`) — a predicate on `Y` cannot
//     be pushed past the rename without rewriting the predicate.
//   - Projections that compute (`X + 1 AS Y`) — same as above; future
//     work can substitute, but the seed rule stays conservative.
//   - `ColumnRef` with a non-empty `Qualifier` — qualifier semantics
//     belong to joins, which are not in scope for filter-project
//     transposition.
//
// Built on the `PatternRule` scaffold — declarative `Match` /
// `Transform` rather than the legacy `Rule.Apply` type-switch.
// FilterProjectTranspose is exposed as a constructor returning a `Rule`
// so callers can register it alongside the legacy three rules.
func FilterProjectTranspose() Rule {
	return &PatternRule{
		RuleName: "filter-project-transpose",
		Match: WithChildren(
			Capture("filter", Kind(KindFilter)),
			Capture("project", Kind(KindProject)),
		),
		Transform: transposeFilterProject,
	}
}

func transposeFilterProject(b Bindings) chplan.Node {
	fNode, ok := b.Get("filter")
	if !ok {
		return nil
	}
	pNode, ok := b.Get("project")
	if !ok {
		return nil
	}
	f, ok := fNode.(*chplan.Filter)
	if !ok {
		return nil
	}
	p, ok := pNode.(*chplan.Project)
	if !ok {
		return nil
	}

	passthrough := passthroughColumns(p.Projections)
	if passthrough == nil {
		// `nil` (not "empty") means "the Project has a renaming or
		// computed entry" — bail. An *empty* but non-nil set is
		// returned only when the Project has zero entries (`SELECT *`),
		// which we also decline to handle in this seed rule.
		return nil
	}
	if !onlyReferencesPassthrough(f.Predicate, passthrough) {
		return nil
	}

	newFilter := &chplan.Filter{
		Input:     p.Input,
		Predicate: f.Predicate,
	}
	newProject := *p
	newProject.Input = newFilter
	return &newProject
}

// passthroughColumns returns the set of column names a Project passes
// through unchanged, or nil to signal "this Project has a non-passthrough
// entry, decline the rewrite outright".
//
// An empty (but non-nil) Projections slice — the `SELECT *` shape —
// returns nil too: the rule declines to handle it because the emitter's
// `*` semantics make the column set indeterminate at the IR level.
func passthroughColumns(projs []chplan.Projection) map[string]struct{} {
	if len(projs) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(projs))
	for _, p := range projs {
		cr, ok := p.Expr.(*chplan.ColumnRef)
		if !ok {
			return nil
		}
		if cr.Qualifier != "" {
			return nil
		}
		if p.Alias != "" && p.Alias != cr.Name {
			return nil
		}
		out[cr.Name] = struct{}{}
	}
	return out
}

// onlyReferencesPassthrough reports whether every `*ColumnRef` reachable
// from e is bare (empty Qualifier) and names a column in the supplied
// passthrough set.
func onlyReferencesPassthrough(e chplan.Expr, passthrough map[string]struct{}) bool {
	ok := true
	walkExpr(e, func(sub chplan.Expr) {
		cr, isCol := sub.(*chplan.ColumnRef)
		if !isCol {
			return
		}
		if cr.Qualifier != "" {
			ok = false
			return
		}
		if _, found := passthrough[cr.Name]; !found {
			ok = false
		}
	})
	return ok
}
