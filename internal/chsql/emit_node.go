package chsql

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/tsouza/cerberus/internal/chplan"
)

// subqueryFrag returns a Frag that renders n as a parenthesised
// subquery into the receiving Builder. Used to plug a child plan into
// QueryBuilder.From without flattening to a string: the args bound
// by the recursive emit walk land in the receiving Builder's args
// slice at the position the Frag is written.
//
// Internally it swaps e.b / e.args with a fresh strings.Builder + nil
// args slice, runs the recursive emit, then splices the rendered SQL
// + args into the destination Builder. The error path is captured via
// the closure variable below; emitSubqueryFrag is the wrapper that
// surfaces it.
func (e *emitter) subqueryFrag(n chplan.Node) (Frag, error) {
	// Pre-render the subquery into an isolated emitter so any chplan
	// error surfaces synchronously (before the Frag is ever spliced
	// into the outer QueryBuilder). The rendered string + args are
	// then captured for cheap replay on each Frag invocation.
	saveB, saveArgs := e.b, e.args
	e.b = strings.Builder{}
	e.args = nil
	if err := e.emitSubquery(n); err != nil {
		e.b = saveB
		e.args = saveArgs
		return nil, err
	}
	sql := e.b.String()
	args := e.args
	e.b = saveB
	e.args = saveArgs
	return func(b *Builder) {
		b.sb.WriteString(sql)
		b.args = append(b.args, args...)
	}, nil
}

// emitSelect runs the assembled QueryBuilder and splices its rendered
// SQL + args into the emitter's output. Centralises the splice
// boilerplate so the per-node emitters stay focused on slot assembly.
func (e *emitter) emitSelect(sb *QueryBuilder) {
	sql, args := sb.Build()
	e.b.WriteString(sql)
	e.args = append(e.args, args...)
}

// splice drains b's accumulated SQL + args into the emitter. Retained
// for the emitters in vector_join.go / structural_join.go that still
// compose SQL fragments through a free-standing *Builder before
// flushing to the shared emitter state.
func (e *emitter) splice(b *Builder) {
	sql, args := b.Build()
	e.b.WriteString(sql)
	e.args = append(e.args, args...)
}

func (e *emitter) emitScan(s *chplan.Scan) error {
	if err := validateScanShape(s); err != nil {
		return err
	}
	sb := NewQuery().From(scanTableFrag(s))
	if len(s.Columns) > 0 {
		cols := make([]Frag, 0, len(s.Columns))
		for _, c := range s.Columns {
			cols = append(cols, Col(c))
		}
		sb.Select(cols...)
	}
	// (Empty Select list renders as `SELECT *` — matches the
	// pre-builder emitter's behaviour for a column-less Scan.)
	e.emitSelect(sb)
	return nil
}

// validateScanShape enforces the mutual exclusion between Scan.Table and
// Scan.UnionTables. Both empty is also rejected — the emitter has no
// table to read from.
func validateScanShape(s *chplan.Scan) error {
	if s.Table == "" && len(s.UnionTables) == 0 {
		return fmt.Errorf("chsql: Scan has neither Table nor UnionTables set")
	}
	if s.Table != "" && len(s.UnionTables) > 0 {
		return fmt.Errorf("chsql: Scan has both Table=%q and UnionTables=%v set; pick one", s.Table, s.UnionTables)
	}
	return nil
}

// emitOneRow renders `SELECT 1` — a no-FROM SELECT that ClickHouse
// evaluates to a single literal row. Used by chplan.Project over
// chplan.OneRow to materialise PromQL `time()` / `vector(scalar)` /
// folded scalar-only binops as a synthetic 1-row vector.
//
// The literal `1` is emitted via InlineLit (no `?` placeholder) because
// it is part of the query shape — the row's value never reaches the
// caller; the surrounding Project replaces it with the per-call
// expressions for MetricName / Attributes / TimeUnix / Value.
func (e *emitter) emitOneRow(_ *chplan.OneRow) error {
	sb := NewQuery().Select(InlineLit(int64(1)))
	e.emitSelect(sb)
	return nil
}

// emitStepGrid renders a single-column SELECT that fans out one row
// per Prom query_range step in `[Start, End]` spaced by Step:
//
//	SELECT arrayJoin(arrayMap(i -> toDateTime64('<start>', 9) + toIntervalNanosecond(i * <step_ns>), range(0, <N>))) AS anchor_ts
//
// where N = (End-Start)/Step + 1 (end-inclusive). The output column is
// named `anchor_ts` — the PromQL no-driving-vector lowerings
// (`time()`, `vector(N)`, zero-arg date fns, `absent(...)`) consume
// it through the surrounding Project's TimeUnix projection so each
// emitted Sample lands at the right step bucket.
//
// The step / numAnchors / start time are emitted as inline SQL
// literals (no `?` placeholders) for the same reason the surrounding
// matrix RangeWindow fan-out does: they are part of the query shape,
// CH cannot prune sort keys against parameter-bound bounds, and the
// driver round-trip is one less round trip when the literal is in
// the SQL stream.
//
// When Step <= 0 this is a degenerate "single-anchor" StepGrid (the
// instant query path) — emit a single-row SELECT carrying Start as
// the anchor_ts. This shape is unreachable from the standard PromQL
// lowering (the lowering picks OneRow in that case) but keeps the
// emitter total.
func (e *emitter) emitStepGrid(g *chplan.StepGrid) error {
	if g.Step <= 0 {
		// Degenerate single-anchor StepGrid — emit a one-row SELECT
		// carrying the Start time as anchor_ts so callers reading
		// `anchor_ts` get a usable column reference in either mode.
		sb := NewQuery().Select(As(func(b *Builder) {
			b.DateTime64Lit(g.Start)
		}, "anchor_ts"))
		e.emitSelect(sb)
		return nil
	}
	stepNS := g.Step.Nanoseconds()
	// End-inclusive anchor count: anchors at Start, Start+Step, …
	// up to and including End. Matches Prom's range-query step grid
	// (the upstream evaluator emits (end-start)/step + 1 samples per
	// series in the canonical case).
	numAnchors := g.End.Sub(g.Start).Nanoseconds()/stepNS + 1
	if numAnchors < 1 {
		numAnchors = 1
	}
	start := g.Start
	sb := NewQuery().Select(As(func(b *Builder) {
		b.sb.WriteString("arrayJoin(arrayMap(i -> ")
		b.DateTime64Lit(start)
		b.sb.WriteString(" + toIntervalNanosecond(i * ")
		b.sb.WriteString(strconv.FormatInt(stepNS, 10))
		b.sb.WriteString("), range(0, ")
		b.sb.WriteString(strconv.FormatInt(numAnchors, 10))
		b.sb.WriteString(")))")
	}, "anchor_ts"))
	e.emitSelect(sb)
	return nil
}

// emitCrossJoin renders an unconditional Cartesian product as
// `SELECT * FROM (<Left>) AS L CROSS JOIN (<Right>) AS R`. Both
// subqueries get bare-uppercase aliases so CH 24.x's
// `joined_subquery_requires_alias = 1` invariant is satisfied without
// requiring callers to know the column-collision shape. Output rows
// expose the union of both sides' columns; callers that need to
// project a subset wrap the CrossJoin in a Project (or rely on the
// surrounding Filter/Project to read the columns by name).
//
// Used by the range-mode `absent(...)` lowering to fan the inner
// count-check across the StepGrid's anchor column. The StepGrid
// emits a single `anchor_ts` column on the left; the Aggregate emits
// `_cerb_n` on the right; the outer Filter reads both by bare name
// (no collision), so the L/R aliases are inert beyond satisfying
// the parser invariant.
// emitUnionAll renders an N-way UNION ALL of the per-arm subtrees.
// Every arm renders via subqueryFrag so the per-arm SELECT lands inside
// parentheses (matching ClickHouse's `(SELECT …) UNION ALL (SELECT …)`
// shape), with arg-binding positions preserved across the union by the
// recursive subquery emit. The arms are emitted in stable left-to-right
// order so the byte-stream matches the chplan IR snapshot's ordering.
//
// Zero arms is a programmer error (the lowering should never produce a
// UnionAll with no inputs); a single arm renders as just that arm's
// parenthesised subquery — the `UNION ALL` keyword is omitted because
// CH parses a bare `(SELECT …)` as a valid SELECT statement (matches
// chsql.UnionAll's Frag-level behaviour for the same shape).
//
// Used by the PromQL classic-histogram companion-suffix routing
// (internal/promql/lower.go) to fan a `_count` / `_sum` selector
// across the histogram + sum tables when both physical layouts may
// hold matching rows. See chplan.UnionAll for the structural contract.
func (e *emitter) emitUnionAll(u *chplan.UnionAll) error {
	if len(u.Inputs) == 0 {
		return fmt.Errorf("%w: UnionAll has no inputs", ErrUnsupported)
	}
	for i, in := range u.Inputs {
		if i > 0 {
			e.b.WriteString(" UNION ALL ")
		}
		if err := e.emitSubquery(in); err != nil {
			return err
		}
	}
	return nil
}

func (e *emitter) emitCrossJoin(j *chplan.CrossJoin) error {
	leftSub, err := e.subqueryFrag(j.Left)
	if err != nil {
		return err
	}
	rightSub, err := e.subqueryFrag(j.Right)
	if err != nil {
		return err
	}
	sb := NewQuery().From(aliasedFrag(leftSub, "L")).
		Join(CrossJoin, aliasedFrag(rightSub, "R"), nil)
	e.emitSelect(sb)
	return nil
}

// scanTableFrag returns the Frag that renders the Scan's table reference.
// When Database is empty (the common case) it's just the bare backtick-
// quoted table name; when Database is non-empty (synthetic single-row
// sources like `system.one`) it emits the qualified `<db>`.`<tbl>` shape
// so CH resolves the system database directly.
//
// When UnionTables is non-empty (the OTel-hostmetrics / sqlquery fallback
// the PromQL matcher path uses for unsuffixed names), the table reference
// renders as a `merge(currentDatabase(), '<regex>')` table function call.
// CH's `merge()` reads the union of all tables in the named database
// whose name matches the regex, projecting only the columns common to
// every member — the gauge / sum / histogram tables share the metric-row
// quadruple (MetricName, Attributes, TimeUnix, Value) plus all the
// envelope columns (Resource* / Scope* / ServiceName / StartTimeUnix /
// Flags / Exemplars), so the union covers everything the downstream
// LWR / RangeWindow / projection wrappers reference. The Sum-only
// columns (AggregationTemporality, IsMonotonic) drop out of the merged
// view; no metric-row consumer reads them, so the narrow is safe.
//
// The merge() call gets per-arm PREWHERE granule pruning automatically:
// CH translates a top-level predicate into the underlying tables'
// PREWHERE during the ReadFromMerge planning step (verified via
// `EXPLAIN`). The emitter therefore doesn't have to manually fan
// PREWHERE per arm — the legacy emitFilterScan path drives the single
// PREWHERE on the outer SELECT and CH does the rest.
func scanTableFrag(s *chplan.Scan) Frag {
	if len(s.UnionTables) > 0 {
		return mergeTableFrag(s.Database, s.UnionTables)
	}
	if s.Database != "" {
		return Qual(s.Database, s.Table)
	}
	return Col(s.Table)
}

// mergeTableFrag renders the CH `merge(currentDatabase(), '<regex>')`
// table-function call that backs Scan.UnionTables. The database argument
// uses `currentDatabase()` when the Scan's Database is empty so the
// fanout follows whichever database the connection-time setting selected
// (cerberus's clickhouse-go client opens its session against the
// configured CERBERUS_CH_DATABASE — `otel` by default). When the Scan
// explicitly names a Database, that literal is used directly.
//
// The regex anchors at both ends (`^…$`) so the table-name pattern
// matches only the exact members of UnionTables — a stray
// `otel_metrics_gauge_v2` (or whatever) won't accidentally pull into
// the scan. Pipe-separated alternation enumerates the member names,
// each `regexp.QuoteMeta`-escaped against accidental metacharacters
// (the OTel-CH defaults are all plain `[a-z_0-9]+` so escapeing is a
// no-op in practice but the safety net is cheap).
func mergeTableFrag(db string, tables []string) Frag {
	dbArg := "currentDatabase()"
	if db != "" {
		dbArg = "'" + escapeSingleQuotes(db) + "'"
	}
	escaped := make([]string, len(tables))
	for i, t := range tables {
		escaped[i] = regexQuoteMeta(t)
	}
	regex := "^(" + strings.Join(escaped, "|") + ")$"
	return func(b *Builder) {
		b.sb.WriteString("merge(")
		b.sb.WriteString(dbArg)
		b.sb.WriteString(", '")
		b.sb.WriteString(escapeSingleQuotes(regex))
		b.sb.WriteString("')")
	}
}

// escapeSingleQuotes doubles every single-quote in s so it can be
// embedded inside a single-quoted SQL string literal.
func escapeSingleQuotes(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// regexQuoteMeta returns s with every RE2 regex metacharacter escaped.
// We don't import `regexp` solely for QuoteMeta — the OTel-CH default
// table names are all plain `[a-z_0-9]+`, and the override surface in
// `internal/schema/otel.go` is config-driven so a user could in principle
// supply a name with a regex metacharacter. The escape list covers the
// RE2 surface CH's `merge()` regex argument understands.
func regexQuoteMeta(s string) string {
	const meta = `\.+*?()|[]{}^$`
	var out strings.Builder
	out.Grow(len(s))
	for _, r := range s {
		if strings.ContainsRune(meta, r) {
			out.WriteByte('\\')
		}
		out.WriteRune(r)
	}
	return out.String()
}

func (e *emitter) emitFilter(f *chplan.Filter) error {
	// Pre-flight the predicate so a chplan error surfaces here, not
	// inside the Where-render callback (where the error has no path
	// to the caller without re-introducing splice plumbing).
	if err := (&Builder{}).Expr(f.Predicate); err != nil {
		return err
	}

	// Filter(Scan(table)) is the case the codegen specialises: emit
	// `SELECT * FROM <table> [PREWHERE …] WHERE …` directly. This is
	// the only shape where CH's PREWHERE granule-skipping fires —
	// PREWHERE wrapping a subquery is a syntax error. For every other
	// input shape we fall back to the historical wrapped form.
	if scan, ok := f.Input.(*chplan.Scan); ok {
		return e.emitFilterScan(f, scan)
	}

	sub, err := e.subqueryFrag(f.Input)
	if err != nil {
		return err
	}
	pred := func(b *Builder) { _ = b.Expr(f.Predicate) }
	e.emitSelect(NewQuery().From(sub).Where(pred))
	return nil
}

// emitFilterScan renders a Filter directly above a Scan as the fused
// `SELECT [cols] FROM <table> [PREWHERE p1 AND …] WHERE q1 AND …`
// shape — the only context where ClickHouse will apply PREWHERE
// granule pruning. The conjuncts of f.Predicate are flattened, sorted
// by sort-key prefix, then partitioned into PREWHERE / WHERE buckets
// based on the table shape's wide-column metadata.
//
// When scan.Columns is non-empty, the SELECT list mirrors it (and the
// wide-column projection check uses it to decide whether PREWHERE
// promotion is worthwhile). An empty Columns slice renders as
// `SELECT *`, which CH treats as touching every column — PREWHERE
// promotion always activates in that case when the table shape has
// wide columns registered.
func (e *emitter) emitFilterScan(f *chplan.Filter, scan *chplan.Scan) error {
	if err := validateScanShape(scan); err != nil {
		return err
	}
	// For a UnionTables scan every member table shares the metric-row
	// shape (the OTel-CH metrics tables all order by (ServiceName,
	// MetricName, Attributes, TimeUnix) and carry the same wide columns
	// — see internal/chsql/tableshape.go). Resolving against the first
	// member is correct for shape lookup; the PREWHERE/WHERE split CH
	// then translates uniformly to every arm of the merge() fanout.
	shapeKey := scan.Table
	if shapeKey == "" && len(scan.UnionTables) > 0 {
		shapeKey = scan.UnionTables[0]
	}
	shape := tableShapeFor(shapeKey)
	conjuncts := flattenAnd(f.Predicate)
	conjuncts = orderedConjuncts(conjuncts, shape)

	var prewhereExprs, whereExprs []chplan.Expr
	if projectionTouchesWide(scan.Columns, shape) {
		prewhereExprs, whereExprs = partitionPrewhere(conjuncts, shape)
	} else {
		whereExprs = conjuncts
	}

	sb := NewQuery().From(scanTableFrag(scan))
	if len(scan.Columns) > 0 {
		cols := make([]Frag, 0, len(scan.Columns))
		for _, c := range scan.Columns {
			cols = append(cols, Col(c))
		}
		sb.Select(cols...)
	}

	// Re-assemble each bucket as a single AND-chain Frag so the rendered
	// SQL preserves the existing parenthesisation that emitter.Expr
	// emits for a Binary AND. Emitting one Frag per conjunct via
	// QueryBuilder.Where(...) would change the surface shape from
	// `(a AND b) AND c` to `(a) AND (b) AND (c)` for the legacy
	// fixtures — semantically equivalent but a churning byte diff.
	if len(prewhereExprs) > 0 {
		sb.Prewhere(conjunctionFrag(prewhereExprs))
	}
	if len(whereExprs) > 0 {
		sb.Where(conjunctionFrag(whereExprs))
	}
	e.emitSelect(sb)
	return nil
}

// conjunctionFrag returns a Frag that renders exprs joined with " AND ".
// Each expr renders via Builder.Expr, which already wraps a Binary in
// parens; so a list of N exprs renders as `e1 AND e2 AND …` (no extra
// outer parens, mirroring the legacy emitter's Binary{OpAnd} output
// shape).
func conjunctionFrag(exprs []chplan.Expr) Frag {
	return func(b *Builder) {
		for i, x := range exprs {
			if i > 0 {
				b.sb.WriteString(" AND ")
			}
			_ = b.Expr(x)
		}
	}
}

func (e *emitter) emitProject(p *chplan.Project) error {
	// Late materialisation: when this Project sits atop a
	// Limit(Filter?(Scan)) over a wide-column table AND the projection
	// references a wide column, emit the two-stage rewrite (inner thin
	// SELECT + JOIN back for wide columns) instead of the canonical
	// single-SELECT shape. See late_mat.go for the gate + emission.
	if m, ok := isLateMatCandidate(p); ok {
		return e.emitLateMat(m)
	}

	sub, err := e.subqueryFrag(p.Input)
	if err != nil {
		return err
	}
	sb := NewQuery().From(sub)
	if len(p.Projections) > 0 {
		// Pre-flight every projection expression so a chplan error
		// surfaces synchronously rather than from inside the Frag
		// render.
		for _, pr := range p.Projections {
			if err := (&Builder{}).Expr(pr.Expr); err != nil {
				return err
			}
		}
		for _, pr := range p.Projections {
			expr := pr.Expr
			sb.SelectAs(func(b *Builder) { _ = b.Expr(expr) }, pr.Alias)
		}
	}
	e.emitSelect(sb)
	return nil
}

func (e *emitter) emitAggregate(a *chplan.Aggregate) error {
	// Pre-flight all expressions so chplan errors surface synchronously.
	for _, g := range a.GroupBy {
		if err := (&Builder{}).Expr(g); err != nil {
			return err
		}
	}
	for _, af := range a.AggFuncs {
		for _, p := range af.Params {
			if err := (&Builder{}).Expr(p); err != nil {
				return err
			}
		}
		for _, ar := range af.Args {
			if err := (&Builder{}).Expr(ar); err != nil {
				return err
			}
		}
	}
	if len(a.GroupBy) == 0 && len(a.AggFuncs) == 0 {
		return fmt.Errorf("%w: Aggregate with no GroupBy keys and no AggFuncs", ErrUnsupported)
	}

	sub, err := e.subqueryFrag(a.Input)
	if err != nil {
		return err
	}

	// Aggregate(GroupBy=[], …) corner case: CH's "1-row-per-aggregate-
	// only-query" semantics emit a single row of zeros even over empty
	// input. PromQL/LogQL spec says an aggregation over the empty set
	// produces no result, so callers flag `DropEmptyOnNoGroup` and the
	// emitter wraps the aggregate with a `count() > 0` guard. TraceQL's
	// `| count() = 0` idiom expects the CH-default 1-row-of-zeros and
	// leaves the flag false.
	if len(a.GroupBy) == 0 && a.DropEmptyOnNoGroup {
		return e.emitAggregateNoGroup(a, sub)
	}

	sb := NewQuery().From(sub)
	for i, g := range a.GroupBy {
		expr := g
		alias := ""
		if i < len(a.GroupByAliases) {
			alias = a.GroupByAliases[i]
		}
		sb.SelectAs(func(b *Builder) { _ = b.Expr(expr) }, alias)
	}
	for _, af := range a.AggFuncs {
		af := af
		sb.Select(aggFuncFrag(af))
	}

	// GROUP BY mirrors the SELECT-list group-by expressions (without
	// aliases — CH groups by the underlying expression, not the alias).
	groupFrags := make([]Frag, 0, len(a.GroupBy))
	for _, g := range a.GroupBy {
		expr := g
		groupFrags = append(groupFrags, func(b *Builder) { _ = b.Expr(expr) })
	}
	sb.GroupBy(groupFrags...)
	e.emitSelect(sb)
	return nil
}

// emitAggregateNoGroup renders the `Aggregate(GroupBy=[], …)` shape as
// a count()-guarded two-layer SELECT so empty input produces 0 output
// rows (PromQL/LogQL spec) rather than CH's default 1-row-of-zeros for
// aggregate-only queries.
//
// Shape:
//
//	SELECT <alias_1>, <alias_2>, …
//	FROM (
//	    SELECT <agg_1> AS <alias_1>, …, count() AS _cerb_n
//	    FROM (<input>)
//	) WHERE _cerb_n > 0
//
// When an AggFunc has an empty Alias, a synthetic `_cerb_agg_<i>`
// alias is minted on the inner SELECT and referenced from the outer
// (without emitting AS on the outer projection — equivalent to the
// pre-wrap bare aggregate output column shape). All real chplan
// callsites carry non-empty aliases today; the synthetic-alias branch
// is a forward-compat hedge.
func (e *emitter) emitAggregateNoGroup(a *chplan.Aggregate, sub Frag) error {
	const guardAlias = "_cerb_n"
	inner := NewQuery().From(sub)
	outerCols := make([]Frag, 0, len(a.AggFuncs))
	for i, af := range a.AggFuncs {
		af := af
		alias := af.Alias
		if alias == "" {
			alias = fmt.Sprintf("_cerb_agg_%d", i)
			af.Alias = alias
		}
		inner.Select(aggFuncFrag(af))
		outerCols = append(outerCols, Col(alias))
	}
	inner.Select(As(Call("count"), guardAlias))

	outer := NewQuery().From(inner.Frag())
	for _, c := range outerCols {
		outer.Select(c)
	}
	outer.Where(Gt(Col(guardAlias), InlineLit(int64(0))))
	e.emitSelect(outer)
	return nil
}

// intReturningAggregates names the CH aggregates whose natural return
// type is integer (UInt64 for `count`/`countIf`). The plain Aggregate
// path projects these into the `Value` column, which the prom/loki
// cursor scans as `*float64`; clickhouse-go/v2 refuses to coerce
// UInt64 → *float64 at Scan time and the call surfaces as a 502
// (`converting UInt64 to *float64 is unsupported`). The matrix path
// has long wrapped each reducer in `toFloat64(...)` (see
// chsql/range_window.go::metricsReducerFrag); aggFuncFrag here is the
// equivalent guard for the instant/aggregate path.
//
// `sum`/`min`/`max`/`avg` over the OTel `Value` column (Float64) stay
// Float64 in CH, so they don't need the wrap. If cerberus ever
// projects `sum(Duration)` (an Int64 column) we'd need to expand this
// set — for the metric tables in use today only the count() family
// breaks the scan.
var intReturningAggregates = map[string]bool{
	"count":   true,
	"countIf": true,
}

// aggFuncFrag returns a Frag rendering `<name>[(<params>)](<args>) [AS <alias>]`
// via Builder.ParamAgg + As. The expression-render errors surface from
// the pre-flight loop in emitAggregate before the Frag ever runs, so the
// rendering path here is infallible.
//
// Aggregates whose CH return type is integer (see intReturningAggregates)
// are wrapped in `toFloat64(...)` so the resulting column scans cleanly
// into chclient.Sample.Value (`*float64`).
func aggFuncFrag(af chplan.AggFunc) Frag {
	mkExpr := func(x chplan.Expr) func(b *Builder) {
		return func(b *Builder) { _ = b.Expr(x) }
	}
	params := make([]func(b *Builder), 0, len(af.Params))
	for _, p := range af.Params {
		params = append(params, mkExpr(p))
	}
	args := make([]func(b *Builder), 0, len(af.Args))
	for _, a := range af.Args {
		args = append(args, mkExpr(a))
	}
	body := func(b *Builder) { b.ParamAgg(af.Name, params, args) }
	if intReturningAggregates[af.Name] {
		inner := body
		body = func(b *Builder) {
			b.sb.WriteString("toFloat64(")
			inner(b)
			b.sb.WriteByte(')')
		}
	}
	return As(body, af.Alias)
}

// emitRangeWindow lives in range_window.go — full windowed-array idiom.

func (e *emitter) emitLimit(l *chplan.Limit) error {
	sub, err := e.subqueryFrag(l.Input)
	if err != nil {
		return err
	}
	sb := NewQuery().From(sub)
	if l.Count > 0 {
		sb.Limit(l.Count)
	}
	e.emitSelect(sb)
	return nil
}

// emitTopK renders `SELECT * FROM (<input>) ORDER BY <sortExpr> [DESC]
// LIMIT K [BY <by_exprs>...]` — ClickHouse's `LIMIT N BY <expr>`
// extension is the natural shape for PromQL's `topk(K, v) by (g)`
// (per-partition top-K) and `topk(K, v)` (whole-result top-K, no
// partitioning).
//
// K <= 0 is a programmer error — topk(0, ...) is meaningless and the
// PromQL lowering should have rejected it upstream; emit an error so
// the plan tree doesn't silently produce an unbounded result.
//
// When t.KExpr != nil (computed-K, e.g. `topk(scalar(metric_count),
// v)`), the literal-K LIMIT shape is replaced with a `row_number()
// OVER (PARTITION BY <by> ORDER BY <sortExpr> [DESC]) <= K` predicate
// because ClickHouse does not accept a subquery directly in a LIMIT
// clause. The K subquery is wrapped as `(SELECT toUInt64(Value) FROM
// (<k_subtree>) LIMIT 1)` — PromQL `scalar()` produces a Sample-shape
// (MetricName, Attributes, TimeUnix, Value) and we read the `Value`
// column. The integer cast guards against the (rare) case where the
// scalar evaluates to a non-integer; PromQL semantics truncate.
func (e *emitter) emitTopK(t *chplan.TopK) error {
	if t.SortExpr == nil {
		return fmt.Errorf("%w: TopK with nil SortExpr", ErrUnsupported)
	}
	if t.KExpr == nil && t.K <= 0 {
		return fmt.Errorf("%w: TopK with non-positive K=%d", ErrUnsupported, t.K)
	}
	if t.KExpr != nil && t.K > 0 {
		return fmt.Errorf("%w: TopK with both literal K=%d and KExpr set", ErrUnsupported, t.K)
	}
	// Pre-flight expressions so chplan errors surface synchronously.
	if err := (&Builder{}).Expr(t.SortExpr); err != nil {
		return err
	}
	for _, by := range t.By {
		if err := (&Builder{}).Expr(by); err != nil {
			return err
		}
	}

	if t.KExpr != nil {
		return e.emitTopKComputed(t)
	}

	sub, err := e.subqueryFrag(t.Input)
	if err != nil {
		return err
	}
	sortExpr := t.SortExpr

	// Inner SELECT applies the ORDER BY + LIMIT BY combo. We keep it as
	// `SELECT *` so the inner subquery's column names flow through to
	// LIMIT BY's resolution unambiguously. If we instead aliased the
	// columns here (e.g. `Attributes AS Attributes`), CH's name
	// resolution for `LIMIT BY Attributes['key']` would prefer the
	// SELECT-list alias over the FROM subquery's column — fine when
	// they're the same type, but fragile when an outer wrapper rewrites
	// the alias's type (e.g. the chDB roundtrip runner wraps Map columns
	// in toJSONString(...) on the outermost SELECT).
	inner := NewQuery().From(sub).OrderBy(
		func(b *Builder) { _ = b.Expr(sortExpr) },
		t.Desc,
	).Limit(t.K)
	if len(t.By) > 0 {
		byFrags := make([]Frag, 0, len(t.By))
		for _, by := range t.By {
			expr := by
			byFrags = append(byFrags, func(b *Builder) { _ = b.Expr(expr) })
		}
		inner.LimitBy(byFrags...)
	}

	// Outer SELECT projects the canonical column list. When `Columns`
	// is empty we emit a bare `SELECT *` so the row arity matches the
	// inner subquery verbatim.
	outer := NewQuery().From(inner.Frag())
	if len(t.Columns) > 0 {
		cols := make([]Frag, 0, len(t.Columns))
		for _, c := range t.Columns {
			cols = append(cols, Col(c))
		}
		outer.Select(cols...)
	}
	e.emitSelect(outer)
	return nil
}

// emitTopKComputed renders the computed-K variant of TopK. CH's LIMIT
// clause requires a constant integer, so the per-partition top-K with
// a subquery-derived K flows through a rank-based filter:
//
//	SELECT <Columns> FROM (
//	  SELECT *, row_number() OVER (PARTITION BY <By> ORDER BY <SortExpr> [DESC]) AS _rn
//	  FROM (<input>)
//	) WHERE _rn <= (SELECT toUInt64(`Value`) FROM (<KExpr>) LIMIT 1)
//
// `By` empty omits PARTITION BY (the rank fires across the whole
// result). `_rn` is a CH-safe synthetic alias the emitter pins; the
// canonical PromQL Sample shape (MetricName/Attributes/TimeUnix/Value)
// does not use leading-underscore columns, so the alias does not
// collide with the inner subquery's columns.
//
// The K subquery is wrapped in `toUInt64(...)` so a non-integer scalar
// (PromQL semantics permit a float K, truncated to int) does not
// surface as a CH "cannot compare UInt64 and Float64" error. The
// trailing `LIMIT 1` guards against the unusual case where the scalar
// subtree returns multiple rows — CH's scalar-subquery binding refuses
// non-unique results, and PromQL's `scalar()` is documented to return
// NaN in that case (which would coerce to 0 here and reject every row).
func (e *emitter) emitTopKComputed(t *chplan.TopK) error {
	sub, err := e.subqueryFrag(t.Input)
	if err != nil {
		return err
	}
	kSub, err := e.subqueryFrag(t.KExpr)
	if err != nil {
		return err
	}

	sortExpr := t.SortExpr
	sortFrag := func(b *Builder) { _ = b.Expr(sortExpr) }

	partitionBy := make([]Frag, 0, len(t.By))
	for _, by := range t.By {
		byExpr := by
		partitionBy = append(partitionBy, func(b *Builder) { _ = b.Expr(byExpr) })
	}

	// `row_number() OVER (PARTITION BY <by> ORDER BY <sortExpr> [DESC])`.
	rankFrag := Window(
		Call("row_number"),
		partitionBy,
		[]OrderKey{{Expr: sortFrag, Desc: t.Desc}},
	)

	// Inner SELECT projects all input columns + the synthetic rank alias.
	// `*` forwards every column from the input subquery so the outer
	// SELECT's column-list (Columns) still resolves; we don't know the
	// input column list at this layer (it varies per upstream schema).
	ranked := NewQuery().From(sub).Select(
		Star(),
		As(rankFrag, "_rn"),
	)

	// K subquery: `(SELECT toUInt64(Value) FROM (<k_subtree>) LIMIT 1)`.
	// The toUInt64 cast handles fractional scalars (PromQL truncates K to
	// int); LIMIT 1 enforces single-row scalar-subquery semantics.
	kSelect := NewQuery().
		Select(Call("toUInt64", Col("Value"))).
		From(kSub).
		Limit(1)
	kSubquery := Subquery(kSelect)

	outer := NewQuery().From(ranked.Frag()).Where(Lte(Col("_rn"), kSubquery))
	if len(t.Columns) > 0 {
		cols := make([]Frag, 0, len(t.Columns))
		for _, c := range t.Columns {
			cols = append(cols, Col(c))
		}
		outer.Select(cols...)
	}
	e.emitSelect(outer)
	return nil
}

// emitOrderBy renders `SELECT * FROM (<input>) ORDER BY <k1> [DESC], …`
// via QueryBuilder.OrderBy. Empty Keys is a programmer error — emit an
// error so the plan tree doesn't silently lose its sort intent.
func (e *emitter) emitOrderBy(o *chplan.OrderBy) error {
	if len(o.Keys) == 0 {
		return fmt.Errorf("%w: OrderBy with no keys", ErrUnsupported)
	}
	// Pre-flight every key expression so chplan errors surface
	// synchronously rather than from inside the Frag render.
	for _, k := range o.Keys {
		if err := (&Builder{}).Expr(k.Expr); err != nil {
			return err
		}
	}
	sub, err := e.subqueryFrag(o.Input)
	if err != nil {
		return err
	}
	sb := NewQuery().From(sub)
	for _, k := range o.Keys {
		expr := k.Expr
		sb.OrderBy(func(b *Builder) { _ = b.Expr(expr) }, k.Desc)
	}
	e.emitSelect(sb)
	return nil
}
