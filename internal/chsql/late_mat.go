package chsql

import (
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// Late materialisation for wide-column scans (RC3 R3.7).
//
// When a query reads "fat" columns (logs.Body, logs.ResourceAttributes,
// traces.SpanAttributes, …) but only keeps a small fraction of the rows
// (LIMIT + a selective WHERE), fetching those wide columns for every row
// before the filter applies wastes most of the IO. The Tsinghua VLDB
// 2025 paper "Selective Late Materialization in Modern Analytical
// Databases" (http://people.iiis.tsinghua.edu.cn/~huanchen/publications/
// slm-vldb25.pdf) frames the fix: defer fetching the wide columns until
// after the filter+limit, then JOIN back by row identity.
//
// The cerberus heuristic gate is a structural match on the plan tree:
//
//	Project(cols including wide)
//	  Limit(n)
//	    Filter(pred)        (optional — Limit alone is enough)
//	      Scan(table)
//
// plus four conditions checked by isLateMatCandidate:
//   1. The table has a registered WideColumns list (logs / traces).
//   2. The Project includes at least one wide column.
//   3. There's a Limit downstream of the Scan.
//   4. The table exposes a unique RowKey tuple (used as the JOIN key).
//
// On a match the emitter renders two SELECTs joined by row identity:
//
//	SELECT scan.*, w.<wide_col>, …
//	FROM (
//	    SELECT <thin-cols + row-key>
//	    FROM <table>
//	    [WHERE <pred>]
//	    LIMIT <n>
//	) AS scan
//	INNER JOIN <table> AS w
//	  ON scan.<rk1> = w.<rk1> AND scan.<rk2> = w.<rk2> AND …
//
// Otherwise emission falls through to the canonical single-SELECT path.

// lateMatShape pairs the set of wide column names with the row-key tuple
// for a registered table. The two metadata bits are coupled — a table
// with wide columns but no row key cannot participate in the rewrite
// (the JOIN would multiply rows) — so we keep them together.
type lateMatShape struct {
	wide   []string
	rowKey []string
}

// lateMatShapeFor returns the registered lateMatShape for the given
// table name, or (zero, false) if the table is not registered for
// late materialisation.
//
// The registry is populated from the default OTel-CH schema at package
// init time. Custom-schema deployments overriding table names via
// Config currently bypass the rewrite — the registry keys on the
// default table names. Threading custom schemas into chsql is left to
// a follow-up; the seed value here is the OTel default + a small
// indirection layer that future PRs can extend.
func lateMatShapeFor(table string) (lateMatShape, bool) {
	s, ok := lateMatShapes[table]
	return s, ok
}

// lateMatShapes seeds the registry from schema defaults. Mutating the
// schema package's struct fields at runtime won't reflect here — the
// registry snapshots the defaults once. That's intentional: chsql
// would otherwise reach into a stateful config singleton, complicating
// the test surface.
var lateMatShapes = func() map[string]lateMatShape {
	out := map[string]lateMatShape{}
	l := schema.DefaultOTelLogs()
	if l.HasUniqueRowKey() && len(l.WideColumns) > 0 {
		out[l.LogsTable] = lateMatShape{wide: l.WideColumns, rowKey: l.RowKey}
	}
	tr := schema.DefaultOTelTraces()
	if tr.HasUniqueRowKey() && len(tr.WideColumns) > 0 {
		out[tr.SpansTable] = lateMatShape{wide: tr.WideColumns, rowKey: tr.RowKey}
	}
	return out
}()

// lateMatMatch carries the matched plan fragments + the table metadata
// needed by the emitter. Filter is optional (nil when the trigger
// pattern was Project(Limit(Scan)) with no WHERE clause); the other
// three nodes are always populated on a match.
type lateMatMatch struct {
	project *chplan.Project
	limit   *chplan.Limit
	filter  *chplan.Filter // may be nil
	scan    *chplan.Scan
	shape   lateMatShape
	// wideInProjection lists the wide columns that the Project actually
	// references — the subset to fetch via the JOIN. Iteration order
	// matches schema.WideColumns to keep emission deterministic.
	wideInProjection []string
	// thinColumns is the set of column names the inner SELECT must
	// project: every projection column that's NOT in wideInProjection,
	// every column referenced by the predicate, plus the row-key
	// tuple. Used to keep the inner SELECT narrow.
	thinColumns []string
}

// isLateMatCandidate inspects the plan rooted at p and reports whether
// the late-materialisation rewrite applies, returning the matched
// fragments + computed column splits.
//
// The four guard conditions (in order, each returning nil/false on a
// miss):
//
//  1. p is *chplan.Project with at least one Projection.
//  2. p.Input is *chplan.Limit with Count > 0; that Limit's Input is
//     either *chplan.Filter wrapping a *chplan.Scan, or a bare
//     *chplan.Scan. (Project(Limit(Filter(Scan))) and
//     Project(Limit(Scan)) both match.)
//  3. The Scan's table has a lateMatShape with non-empty WideColumns
//     AND non-empty RowKey.
//  4. At least one projection column-ref points at a wide column.
//
// A miss on any condition returns (nil, false) and the caller falls
// through to the canonical single-SELECT emission path.
func isLateMatCandidate(p chplan.Node) (*lateMatMatch, bool) {
	proj, ok := p.(*chplan.Project)
	if !ok || len(proj.Projections) == 0 {
		return nil, false
	}
	lim, ok := proj.Input.(*chplan.Limit)
	if !ok || lim.Count <= 0 {
		return nil, false
	}

	var (
		filt *chplan.Filter
		scan *chplan.Scan
	)
	switch inner := lim.Input.(type) {
	case *chplan.Filter:
		s, sok := inner.Input.(*chplan.Scan)
		if !sok {
			return nil, false
		}
		filt = inner
		scan = s
	case *chplan.Scan:
		scan = inner
	default:
		return nil, false
	}

	shape, ok := lateMatShapeFor(scan.Table)
	if !ok {
		return nil, false
	}

	// Build the wide-column lookup and walk the projection list to
	// find references.
	wideSet := stringSet(shape.wide)
	projCols := projectionColumnNames(proj.Projections)

	wideInProj := make([]string, 0, len(shape.wide))
	for _, w := range shape.wide {
		// Preserve schema order in the output.
		if _, hit := projCols[w]; hit {
			wideInProj = append(wideInProj, w)
		}
	}
	if len(wideInProj) == 0 {
		return nil, false
	}

	// Thin columns: every projection ColumnRef that is NOT wide, plus
	// every predicate column ref (so the inner WHERE can resolve
	// them), plus the row-key tuple. Order: deterministic —
	// projection non-wide cols first (in declaration order), then
	// predicate cols not already present, then row-key cols not
	// already present.
	thinSet := map[string]struct{}{}
	thin := []string{}
	addThin := func(name string) {
		if _, dup := thinSet[name]; dup {
			return
		}
		thinSet[name] = struct{}{}
		thin = append(thin, name)
	}
	for _, pr := range proj.Projections {
		c, isCol := pr.Expr.(*chplan.ColumnRef)
		if !isCol {
			continue
		}
		if _, isWide := wideSet[c.Name]; isWide {
			continue
		}
		addThin(c.Name)
	}
	if filt != nil {
		for _, c := range collectColumnRefs(filt.Predicate) {
			addThin(c)
		}
	}
	for _, rk := range shape.rowKey {
		addThin(rk)
	}

	return &lateMatMatch{
		project:          proj,
		limit:            lim,
		filter:           filt,
		scan:             scan,
		shape:            shape,
		wideInProjection: wideInProj,
		thinColumns:      thin,
	}, true
}

// stringSet returns a presence map keyed by xs. Used for the wide /
// projection / thin column lookups; small enough that the map is
// strictly faster than a linear scan once the projection list grows
// past a handful of entries (the OTel logs case touches 10+ columns).
func stringSet(xs []string) map[string]struct{} {
	out := make(map[string]struct{}, len(xs))
	for _, x := range xs {
		out[x] = struct{}{}
	}
	return out
}

// projectionColumnNames returns the set of bare ColumnRef names in the
// projection list. Non-column expressions (function calls, literals,
// arithmetic) don't reference a wide column for the purpose of the
// gate — even if a wide column appears inside a `length(Body)` call,
// the SELECT still avoids fetching `Body` proper. Treating only direct
// ColumnRefs as wide-projection signals is the conservative choice.
//
// A future refinement walks ColumnRefs inside arbitrary expressions to
// widen the rewrite's applicability. R3.7 starts with the simple form
// so the gate's reasoning is auditable.
func projectionColumnNames(ps []chplan.Projection) map[string]struct{} {
	out := map[string]struct{}{}
	for _, p := range ps {
		if c, ok := p.Expr.(*chplan.ColumnRef); ok && c.Qualifier == "" {
			out[c.Name] = struct{}{}
		}
	}
	return out
}

// emitLateMat renders the two-stage SELECT for a matched
// late-materialisation candidate. The shape mirrors the godoc at the
// top of this file: an inner SELECT that fetches only thin columns +
// the row key + applies the predicate + LIMIT, INNER JOINed against
// the original table aliased as `w` to fetch the wide columns for the
// surviving rows.
//
// Aliases: the inner SELECT is wrapped via `AS scan` (lowercase, bare —
// matches the structural_join.go / vector_join.go convention of using
// unquoted single-token aliases). The outer JOIN side is aliased
// `AS w`. Each projection's ColumnRefs are rewritten via qualifyExpr
// so wide-column refs render as `w.<col>` and the rest as
// `scan.<col>`; that keeps the rendered ON clause unambiguous (both
// sides expose the row-key column names).
//
// Errors from the predicate / projection expression render flow back
// through the same pre-flight pattern used by emitFilter / emitProject
// — chplan errors surface synchronously rather than from inside a
// Frag callback.
func (e *emitter) emitLateMat(m *lateMatMatch) error {
	// Pre-flight every projection expression and the (optional)
	// predicate. Mirrors emitProject / emitFilter so chplan errors
	// land at the caller, not inside a render closure.
	for _, pr := range m.project.Projections {
		if err := (&Builder{}).Expr(pr.Expr); err != nil {
			return err
		}
	}
	if m.filter != nil {
		if err := (&Builder{}).Expr(m.filter.Predicate); err != nil {
			return err
		}
	}

	// Inner SELECT: thin columns + row key, WHERE pred (if any),
	// LIMIT n. The thin column list already contains the row-key
	// columns (isLateMatCandidate appends them last), so we just
	// project each name.
	inner := NewQuery().From(Col(m.scan.Table))
	for _, c := range m.thinColumns {
		inner.Select(Col(c))
	}
	if m.filter != nil {
		pred := m.filter.Predicate
		inner.Where(func(b *Builder) { _ = b.Expr(pred) })
	}
	inner.Limit(m.limit.Count)

	// Outer SELECT: render each projection expression with the
	// scan/w qualifier rewrite. The inner subquery is aliased
	// `scan` and the JOIN side `w`; row-key cols come from `scan`
	// (already projected), wide cols come from `w`.
	wideSet := stringSet(m.wideInProjection)
	outer := NewQuery().
		From(aliasedFrag(inner.Frag(), "scan")).
		Join(InnerJoin, aliasedFrag(Col(m.scan.Table), "w"), lateMatJoinPredicate(m.shape.rowKey))

	for _, pr := range m.project.Projections {
		expr := qualifyExpr(pr.Expr, wideSet)
		outer.SelectAs(func(b *Builder) { _ = b.Expr(expr) }, pr.Alias)
	}

	e.emitSelect(outer)
	return nil
}

// lateMatJoinPredicate returns a Frag rendering the row-key JOIN's ON
// clause: `scan.<rk1> = w.<rk1> AND scan.<rk2> = w.<rk2> AND …`. The
// row-key tuple is guaranteed non-empty by isLateMatCandidate's gate;
// rk is rendered via And + Eq so each conjunct stays in the typed
// QueryBuilder surface and writeInto's PREWHERE/WHERE positional-`?`
// invariants hold.
func lateMatJoinPredicate(rowKey []string) Frag {
	parts := make([]Frag, 0, len(rowKey))
	for _, c := range rowKey {
		parts = append(parts, Eq(Qual("scan", c), Qual("w", c)))
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return And(parts...)
}

// qualifyExpr returns a copy of expr with every bare ColumnRef
// rewritten to carry a qualifier — `w` for the columns named in
// wideSet, `scan` for everything else. The outer SELECT references
// row-key and thin columns from `scan.*` (the inner aliased subquery)
// and wide columns from `w.*` (the JOINed-back table side); without
// the qualifier, CH would see an ambiguous reference (both sides
// expose the same column name for the row-key tuple).
//
// The rewrite is a shallow copy: literals and other non-ColumnRef
// nodes are returned unchanged. ColumnRef is a pointer type, so we
// allocate a fresh ColumnRef rather than mutating the input.
func qualifyExpr(e chplan.Expr, wideSet map[string]struct{}) chplan.Expr {
	switch v := e.(type) {
	case *chplan.ColumnRef:
		side := "scan"
		if _, isWide := wideSet[v.Name]; isWide {
			side = "w"
		}
		return &chplan.ColumnRef{Name: v.Name, Qualifier: side}
	case *chplan.Binary:
		return &chplan.Binary{
			Op:    v.Op,
			Left:  qualifyExpr(v.Left, wideSet),
			Right: qualifyExpr(v.Right, wideSet),
		}
	case *chplan.FuncCall:
		args := make([]chplan.Expr, len(v.Args))
		for i, a := range v.Args {
			args[i] = qualifyExpr(a, wideSet)
		}
		return &chplan.FuncCall{Name: v.Name, Args: args}
	case *chplan.MapAccess:
		return &chplan.MapAccess{
			Map: qualifyExpr(v.Map, wideSet),
			Key: qualifyExpr(v.Key, wideSet),
		}
	case *chplan.FieldAccess:
		return &chplan.FieldAccess{
			Source: qualifyExpr(v.Source, wideSet),
			Path:   v.Path,
		}
	default:
		return e
	}
}
