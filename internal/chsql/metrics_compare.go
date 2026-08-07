package chsql

import (
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// This file emits chplan.MetricsCompare — TraceQL's
// `| compare({...}, topN[, start, end])` first-stage metrics operator.
//
// The SQL produces the RAW per-(cohort, attribute, value[, anchor])
// counts; the Tempo HTTP handler
// (internal/api/tempo/metrics_query_range_compare.go) mirrors upstream
// Tempo's BaselineAggregator on top of the row stream — top-N per
// (cohort, attribute), per-attribute totals, zero-filled anchor grids,
// and the `__meta_type` label scheme. Top-N is deliberately NOT pushed
// into the SQL: the totals series count every occurrence (not just the
// top-N survivors), so the full value inventory must reach the handler
// anyway.
//
// Bare shape (no RangeWindow wrapper — TXTAR / chdb layer):
//
//	SELECT `is_selection`, tupleElement(kv, ?) AS `attr`,
//	       tupleElement(kv, ?) AS `val`, toFloat64(count(?)) AS `Value`
//	FROM (
//	  SELECT <Selection> AS `is_selection`, arrayJoin(<Pairs>) AS `kv`
//	  FROM (<Inner>) [AS s LEFT JOIN (<RootLookup>) AS r ON s.<TraceID> = r.<TraceID>]
//	)
//	GROUP BY `is_selection`, `attr`, `val`
//	ORDER BY `is_selection`, `attr`, `val`
//
// The ORDER BY pins a deterministic row order for the bare shape only
// (GROUP BY output order is otherwise unspecified); the matrix shape
// leaves ordering to the handler, matching the other metrics emitters.
//
// Matrix shape (RangeWindow wrapper — /api/metrics/query_range):
//
//	SELECT `is_selection`, `attr`, `val`, anchor_ts, toFloat64(count(?)) AS `Value`
//	FROM (
//	  SELECT `is_selection`, tupleElement(kv, ?) AS `attr`,
//	         tupleElement(kv, ?) AS `val`, <sample-side anchor fanout> AS anchor_ts
//	  FROM (
//	    SELECT <Selection> AS `is_selection`, arrayJoin(<Pairs>) AS `kv`, <tsCol>
//	    FROM (<Inner>) [LEFT JOIN <RootLookup>] [WHERE <(Start-range, End] bounds>]
//	  )
//	)
//	GROUP BY `is_selection`, `attr`, `val`, anchor_ts
//
// Anchors with no rows emit nothing — the handler zero-fills the grid
// (upstream's counts arrays are zero-initialised across all intervals).

// compareJoinLeftAlias / compareJoinRightAlias are the bare table
// aliases the LEFT JOIN uses. Following vector_join's `L`/`R`
// convention; `s` (spans) / `r` (roots) read better in EXPLAIN output.
const (
	compareJoinLeftAlias  = "s"
	compareJoinRightAlias = "r"
)

// compareScanBound carries the (Start-range, End] Timestamp window the
// matrix path pushes INTO each MergeTree scan of the compare join so
// ClickHouse can partition/granule-prune both legs. lo / hi are the
// strict-lower / inclusive-upper Frags from innerScanTsBoundsFrags; a
// nil compareScanBound (the bare time-collapsed shape, which has no
// anchor grid) leaves both scans unbounded — byte-stable against the
// pinned bare fixtures. That bare path is reached only by the golden /
// chdb test lanes; every prod compare() enters through a RangeWindow
// (/api/metrics/query_range), so the unbounded bare root leg is not a
// live OOM surface.
//
// Why the bound must live inside the scans, not above the join: CH
// 24.12 cannot push a predicate sitting on the SELECT that wraps
// `s LEFT JOIN r` down into either MergeTree input, so a window filter
// landed there scans the full span table on both legs (the prod
// traces-drilldown OOM: a 15-min compare read ~731M rows before the
// 2 GiB cap killed it). compareBaseQuery therefore attaches lo/hi to
// the `s` subquery's own WHERE. The same is true one level deeper for
// the `r` (root) leg: a `TraceId IN (...)` filter sitting ABOVE
// RootLookup's own GROUP BY (boundedRootLeg's wrap, below) restricts
// only the aggregate's OUTPUT — CH still has to materialize the whole
// aggregate over the unpruned scan first, which is what actually kept
// OOMing in prod despite that wrap already existing.
// windowRootLookupTraceIDSeed pushes the same TraceId-IN predicate into
// the Filter directly beneath RootLookup's own Scan (see
// emitRangeWindowCompare), below the GROUP BY. That push alone does NOT
// prune the scan, though: TraceId is not the spans table's MergeTree
// sort key, so CH still reads every root span across retention to test
// IN-membership (the residual prod OOM — ~1.4B rows). The scan is pruned
// only by a DIRECT Timestamp predicate. For root-scoped selections, the seed
// additionally conjoins the request window because every root is in-window.
// For non-root selections, an enabled trace_id_ts lookup supplies a wider,
// selected-trace envelope that retains older roots; without that opt-in lookup,
// the scan keeps TraceId-IN alone, unpruned but lossless. rootSeeded records
// when the push succeeded, so
// compareBaseQuery can skip boundedRootLeg's now-redundant wrap instead
// of re-filtering an already-seeded result by the same trace-id set.
//
// rootEnvelope carries the scalar-binding body those pushed-down bounds read
// (rootLookupTraceIDTsBounds' envelope) alongside the flag that says they
// landed, so compareBaseQuery declares the binding on the very relation it
// renders the readers into. Non-nil only when rootSeeded is true: bounds that
// never reached a scan have no reader, and an unread scalar WITH still costs a
// full trace_id_ts scan.
type compareScanBound struct {
	lo, hi       Frag
	rootSeeded   bool
	rootEnvelope *QueryBuilder
}

// compareBaseQuery builds the innermost SELECT — cohort flag +
// arrayJoin'd attribute pairs (+ extraCols, used by the matrix path to
// carry the timestamp column up to the fanout level) over Inner,
// LEFT JOINed against RootLookup when present. When bound is non-nil the
// Timestamp window is pushed into both join legs (see compareScanBound).
func (e *emitter) compareBaseQuery(m *chplan.MetricsCompare, bound *compareScanBound, extraCols ...string) (*QueryBuilder, error) {
	if m.Selection == nil {
		return nil, fmt.Errorf("%w: MetricsCompare.Selection is nil", ErrUnsupported)
	}
	if m.Pairs == nil {
		return nil, fmt.Errorf("%w: MetricsCompare.Pairs is nil", ErrUnsupported)
	}
	if m.Inner == nil {
		return nil, fmt.Errorf("%w: MetricsCompare.Inner is nil", ErrUnsupported)
	}

	inner, err := e.subqueryFrag(m.Inner)
	if err != nil {
		return nil, err
	}

	// Left ('s') leg: wrap the inner spanset in a SELECT * that carries
	// the Timestamp window in its own WHERE so the bound sits BELOW the
	// join, inside the span scan, where CH can prune it. When unbounded
	// the inner Frag is aliased directly (byte-stable bare shape).
	sLeg := inner
	if bound != nil {
		sLeg = NewQuery().Select(Star()).From(inner).Where(bound.lo, bound.hi).Frag()
	}

	qb := NewQuery()
	if m.RootLookup != nil {
		if m.TraceIDColumn == "" {
			return nil, fmt.Errorf("%w: MetricsCompare.RootLookup set but TraceIDColumn empty", ErrUnsupported)
		}
		root, rerr := e.subqueryFrag(m.RootLookup)
		if rerr != nil {
			return nil, rerr
		}
		if bound != nil {
			root = bindRootLookupTraceIDTsEnvelope(root, bound.rootEnvelope)
		}
		// Right ('r') leg: bound the per-trace root lookup to the same
		// windowed cohort the 's' leg scans, restricting root rows to
		// `TraceId IN (<bounded s trace-ids>)`. When emitRangeWindowCompare
		// has already pushed that same predicate into RootLookup's own
		// scan-level Filter (bound.rootSeeded — see
		// windowRootLookupTraceIDSeed), boundedRootLeg's wrap here would
		// only re-filter an already-seeded result by the identical
		// trace-id set, so it's skipped. It stays as the correctness
		// fallback for the rare RootLookup shape the scan-level pushdown
		// can't reach (rootLookupSpansTable finds no matching Scan): a
		// plain Timestamp bound on the root leg would drop enrichment for
		// traces whose root span straddles the window edge, so seeding by
		// the cohort's trace-id set is what keeps every root the LEFT
		// JOIN can match (the join output is determined by s.TraceId,
		// already windowed) while still scoping the leg to the cohort.
		if bound != nil && !bound.rootSeeded {
			root = e.boundedRootLeg(root, m.TraceIDColumn, inner, bound)
		}
		qb.From(aliasedFrag(sLeg, compareJoinLeftAlias)).
			Join(
				LeftJoin,
				aliasedFrag(root, compareJoinRightAlias),
				Eq(
					qualColFrag(compareJoinLeftAlias, m.TraceIDColumn),
					qualColFrag(compareJoinRightAlias, m.TraceIDColumn),
				),
			)
	} else {
		qb.From(sLeg)
	}

	sel := m.Selection
	qb.SelectAs(func(b *Builder) { _ = b.Expr(sel) }, compareSelOut(m))
	pairs := m.Pairs
	qb.SelectAs(Call("arrayJoin", func(b *Builder) { _ = b.Expr(pairs) }), "kv")
	for _, c := range extraCols {
		col := c
		qb.Select(func(b *Builder) { b.Ident(col) })
	}
	return qb, nil
}

// boundedRootLeg wraps the RENDERED (post-aggregate) root-lookup subquery,
// restricting its output rows to
// `<traceID> IN (SELECT <traceID> FROM (<bounded inner>) AS _cmp_seed)`,
// where the bounded inner is the same windowed span scan the 's' leg uses.
// This does NOT prune RootLookup's underlying MergeTree scan — the
// predicate sits above the GROUP BY it wraps, and CH does not push a
// filter on an aggregate's output back down through the GROUP BY into the
// scan beneath it. It only narrows which already-aggregated rows the join
// sees, which is necessary for correctness (e.g. when the scan-level
// pushdown below can't reach RootLookup's shape) but does nothing for the
// scan-level OOM windowRootLookupTraceIDSeed exists to fix. _cmp_seed
// aliases the seed subquery so CH's analyzer resolves the projected
// trace-id column. See compareScanBound for the why.
func (e *emitter) boundedRootLeg(root Frag, traceIDCol string, inner Frag, bound *compareScanBound) Frag {
	boundedInner := NewQuery().Select(Star()).From(inner).Where(bound.lo, bound.hi)
	seedIDs := NewQuery().
		Select(Col(traceIDCol)).
		From(aliasedFrag(boundedInner.Frag(), "_cmp_seed"))
	return NewQuery().
		Select(Star()).
		From(root).
		Where(In(Col(traceIDCol), Spliced(seedIDs))).
		Frag()
}

// compareSeedNode builds `SELECT <traceIDCol> FROM (<inner>)` [with
// <inner> additionally filtered to (lo, hi] on tsCol when either bound is
// set] — the EXACT same relation compareBaseQuery uses for the "s"/cohort
// leg (see compareBaseQuery's sLeg and boundedRootLeg's seedIDs), expressed
// as a chplan.Node rather than a rendered Frag so it can be embedded as a
// chplan.InSubquery.Subquery inside RootLookup's own Filter.Predicate (a
// chplan.Expr slot the emitted "s" leg's Frag-level bound cannot reach).
//
// FIDELITY: inner must be the fully-filtered spanset the query's `{...}`
// pipeline already resolved to (whatever resource/service/attribute
// filters compareBaseQuery's "s" leg applies) — not a bare Scan plus only
// a time range — otherwise the seed set would be wider than the query
// actually intends and would silently change which traces receive
// root-name/root-service enrichment. The SAME fidelity requirement applies
// to lo/hi: they must be the caller's actual scan-bound values (matching
// innerScanTsBoundsFrags' (Start-Offset-range, End-Offset] window, not the
// raw [Start, End] request window) — a narrower bound here silently
// excludes traces the "s" leg's own scan still includes.
func compareSeedNode(inner chplan.Node, traceIDCol string, lo, hi chplan.Expr) chplan.Node {
	windowed := chplan.CloneNode(inner)
	if pred := conjoinExpr(lo, hi); pred != nil {
		windowed = &chplan.Filter{Input: windowed, Predicate: pred}
	}
	return &chplan.Project{
		Input:       windowed,
		Projections: []chplan.Projection{{Expr: &chplan.ColumnRef{Name: traceIDCol}}},
	}
}

// rootLookupSpansTable returns the table name of the Scan embedded inside
// root (RootLookup's own Aggregate→Filter→Scan chain — see
// traceql.compareRootLookup), or "" if none is reachable. Deriving the
// table name from RootLookup itself, rather than threading it in from the
// emit context, means windowRootLookupTraceIDSeed below fires on every
// emit path — the bare shape, the RangeWindow/matrix shape, and the
// TXTAR spec/chdb golden lane — not only "real" Tempo requests that
// happen to thread WithSpansTable.
func rootLookupSpansTable(root chplan.Node) string {
	var table string
	chplan.Walk(root, func(n chplan.Node) bool {
		if sc, ok := n.(*chplan.Scan); ok {
			if table == "" {
				table = sc.Table
			}
			return false
		}
		return true
	})
	return table
}

// windowRootLookupTraceIDSeed returns a clone of the root-lookup relation
// with `<traceIDCol> IN (<seed>)` AND-ed — as a chplan.InSubquery — into the
// Filter that sits directly on the spans Scan, so the membership predicate
// lands below the per-trace root aggregate's own GROUP BY (a predicate
// sitting above the GROUP BY, like the old post-aggregate wrap in
// boundedRootLeg, does not get pushed back down through it by ClickHouse).
// That placement scopes the aggregate's inputs to the cohort but does NOT
// prune the scan (TraceId is not the sort key — see the tsLo/tsHi note below).
//
// This replaces the previous Timestamp-only push: TraceId is the bare,
// unaliased GROUP BY key of RootLookup's own Aggregate, with no HAVING
// clause or window function between the scan and the GROUP BY, so a
// predicate that only references TraceId membership commutes freely across
// GROUP BY — filtering by TraceId before vs. after GROUP BY produces
// IDENTICAL groups. A Timestamp bound on the root span's own Timestamp is
// instead LOSSY: it silently drops the enrichment for any trace whose root
// span started before the request window (a normal case for a long-lived
// trace whose matched span is late). A trace_id_ts-derived envelope can bound
// that non-root case when its opt-in readiness contract is enabled — see
// internal/chsql's
// compareRootLookup / TestEmitRangeWindowCompare_JoinScanPushdown history
// for the full incident writeup.
//
// The rewrite only touches a Scan whose Table matches the one
// rootLookupSpansTable derives from root itself, leaving any other shape
// untouched (defensive — the compare lowering always produces the
// Aggregate→Filter→Scan root lookup, but a future schema may not). Returns
// the input unchanged and seeded=false when seed is nil or no matching
// spans scan is found — the caller (emitRangeWindowCompare) uses seeded to
// decide whether boundedRootLeg's post-aggregate wrap is still needed as a
// correctness fallback.
//
// tsLo/tsHi, when non-nil, additionally conjoin a lossless Timestamp envelope
// onto the same scan Filter. The TraceId-IN seed alone bounds the cohort
// but cannot PRUNE the scan: TraceId is not the spans table's MergeTree sort
// key, so CH reads every root span across full retention to test membership
// (the compare OOM/timeout — read_rows ~1.4B in prod). A direct Timestamp bound
// prunes by partition/PK, but it is only LOSSLESS to add when the caller has
// established the seed's roots all fall inside the window (root-scoped
// selection — gated on m.InnerRootScoped in emitRangeWindowCompare). For a
// non-root selection, the caller may instead pass a trace_id_ts-derived
// envelope; it contains each selected trace's full span lifetime. Without the
// opt-in lookup, the caller passes nil tsLo/tsHi and keeps #1214's
// unbounded-but-lossless behavior.
func windowRootLookupTraceIDSeed(root chplan.Node, traceIDCol string, seed chplan.Node, tsLo, tsHi chplan.Expr) (windowed chplan.Node, seeded bool) {
	if seed == nil {
		return root, false
	}
	spansTable := rootLookupSpansTable(root)
	if spansTable == "" {
		return root, false
	}
	clone := chplan.CloneNode(root)
	in := &chplan.InSubquery{Left: &chplan.ColumnRef{Name: traceIDCol}, Subquery: seed}
	pred := conjoinExpr(conjoinExpr(tsLo, tsHi), in)
	if pushPredicateToSpansScanFilter(clone, spansTable, pred) {
		return clone, true
	}
	return root, false
}

// tsBoundExprs builds the lower (`>= fromUnixTimestamp64Nano(startNano)`) and
// upper (`<= fromUnixTimestamp64Nano(endNano)`) request-window comparison
// expressions for tsCol, mirroring the search lowering's tsBound shape so the
// seed's window matches the rest of the search path. Either side is nil when
// its nanosecond value is 0.
func tsBoundExprs(tsCol string, startNano, endNano int64) (lo, hi chplan.Expr) {
	fromNano := func(nano int64) chplan.Expr {
		return &chplan.FuncCall{
			Name: "fromUnixTimestamp64Nano",
			Args: []chplan.Expr{&chplan.LitInt{V: nano}},
		}
	}
	if startNano != 0 {
		lo = &chplan.Binary{Op: chplan.OpGe, Left: &chplan.ColumnRef{Name: tsCol}, Right: fromNano(startNano)}
	}
	if endNano != 0 {
		hi = &chplan.Binary{Op: chplan.OpLe, Left: &chplan.ColumnRef{Name: tsCol}, Right: fromNano(endNano)}
	}
	return lo, hi
}

// rootLookupTraceIDTsBounds derives a physical Timestamp envelope for the root
// scan from the trace_id_ts rows belonging to seed. Unlike the
// request window, [min(Start), max(End)+1s] contains an older root and its late
// matching child, so conjoining it preserves Tempo's root attributes. The
// trace_id_ts gate is enabled only after its MV is known populated; without
// that contract, callers leave the three metadata fields empty and this helper
// declines to add a bound. A half-configured triple is treated as not
// configured — an envelope over an empty table or column identifier would be a
// broken query, not a weaker bound. seed must be non-nil; the caller only
// reaches here once it has one.
//
// The bound-construction itself is chplan.TraceIDTsEnvelopeBounds (#1438,
// #1445) — this wrapper supplies the cohort predicate (an IN-subquery over
// seed, since a MetricsCompare root lookup covers however many trace ids seed
// selects, unlike the single-trace Eq the Tempo by-id handler uses) and the
// envelope query that predicate lives in.
//
// envelope is the body of the scalar binding the two bounds read: one
// `(min(Start), max(End))` tuple over the cohort's trace_id_ts rows, which
// bindRootLookupTraceIDTsEnvelope declares on the rendered root-lookup
// relation — the same statement the bounds are pushed into, so the leg is
// self-contained. Collapsing the two aggregates into one tuple is what takes
// the seed from three renders per statement to two (the binding body, and the
// root scan's own membership gate): a cohort here is a whole subquery, so each
// bound that carries its own copy of it is another scan of the spans table.
// Measured on chDB over a 2M-span OTel-shaped table whose cohort envelope
// spans 30 days, the root leg runs ~15% faster in the bound form.
//
// The seed's third render — the `TraceId IN (<seed>)` gate on the root scan —
// stays a subquery deliberately. Binding the id SET instead (a groupArray
// scalar tested with `has`) would take the seed to a single render, but `has`
// against a bound array cannot drive the primary index of a
// (TraceId, …)-ordered trace_id_ts table the way `IN (<subquery>)` does: the
// same measurement puts that form 4.3x slower than today's shape.
//
// The envelope covers the WHOLE cohort, so one long-lived trace widens the
// bound every other trace is read under. The narrower alternative — bounding
// each root span by its own trace's Start/End, correlated per row — was
// measured against it on a cohort containing a deliberate straggler (one trace
// whose root precedes the rest of the cohort by a month, the only mix in which
// the two forms can differ). The correlated form reads exactly the same parts,
// rows and marks and returns the same rows; conjoining both is strictly slower
// than the cohort-wide bound alone. Two things make that so: a per-row bound is
// not a constant, so it can prune neither partitions nor the primary index the
// way the cohort-wide constants do; and under the `TraceId IN (<seed>)` gate
// every span of a selected trace already lies inside that trace's own
// [Start, End] by trace_id_ts's construction, so the correlated predicate
// excludes nothing the gate has not excluded already (#1443). The straggler's
// cost is real — the cohort-wide envelope reads its whole span of partitions —
// but it is the price of reading that trace at all, not an artefact of the
// bound's width.
func rootLookupTraceIDTsBounds(
	m *chplan.MetricsCompare, seed chplan.Node, timestampColumn string,
) (lo, hi chplan.Expr, envelope *QueryBuilder) {
	if m.RootLookupTraceIDTsTable == "" ||
		m.RootLookupTraceIDTsStartColumn == "" || m.RootLookupTraceIDTsEndColumn == "" {
		return nil, nil, nil
	}
	cohortPred := &chplan.InSubquery{
		Left:     &chplan.ColumnRef{Name: m.TraceIDColumn},
		Subquery: chplan.CloneNode(seed),
	}
	envelope = NewQuery().
		Select(Call(
			"tuple",
			Call("min", Col(m.RootLookupTraceIDTsStartColumn)),
			Call("max", Col(m.RootLookupTraceIDTsEndColumn)),
		)).
		From(Col(m.RootLookupTraceIDTsTable)).
		Where(func(b *Builder) { _ = b.Expr(cohortPred) })
	lo, hi = chplan.TraceIDTsEnvelopeBounds(chplan.TraceIDTsEnvelopeAlias, timestampColumn)
	return lo, hi, envelope
}

// bindRootLookupTraceIDTsEnvelope declares rootLookupTraceIDTsBounds' envelope
// on root — the rendered root-lookup relation whose own scan filter carries the
// only readers of the binding — and returns the bound relation. A nil envelope
// (no trace_id_ts gate, or bounds that never reached a scan) returns root
// untouched, keeping the unbound shape byte-identical.
//
// The declaration belongs on the INNERMOST statement containing every reader,
// not on the compare statement's outermost SELECT. Both placements are legal
// and both cost the same: a ClickHouse WITH alias resolves from subqueries
// nested arbitrarily deep inside the statement that declares it, and the body
// is evaluated exactly once either way. Measured on chDB against a ~330ms
// binding body read twice from inside a join leg's own nested scan filter, the
// leg-level declaration ran 363ms and the outermost-SELECT declaration 377ms —
// one body evaluation in both, the difference inside run-to-run noise; the same
// body inlined per reader instead of bound grew 344ms to 412ms.
//
// What the inner placement buys is locality. The bounds live several subqueries
// down, inside the root lookup's scan-level Filter (windowRootLookupTraceIDSeed
// puts them there); declaring the binding at the top left the root leg a
// fragment that could not be read, reasoned about, or executed without the rest
// of the statement around it. Declared here, the leg is a complete statement:
// whatever renders it — the compare join, or a perf guard extracting the leg to
// EXPLAIN its part pruning in isolation — gets the binding with it.
//
// The wrapper is a `SELECT * FROM (<root>)`, the same typed shape emitBound
// uses to attach chplan.BoundedTraceScopeAlias to a rendered plan: `SELECT *`
// forwards the root lookup's own column names and row shape unchanged, so the
// join's `ON s.TraceId = r.TraceId` resolves exactly as it did.
func bindRootLookupTraceIDTsEnvelope(root Frag, envelope *QueryBuilder) Frag {
	if envelope == nil {
		return root
	}
	return NewQuery().
		WithScalar(chplan.TraceIDTsEnvelopeAlias, envelope).
		Select(Star()).
		From(root).
		Frag()
}

// pushPredicateToSpansScanFilter walks n in place looking for the Filter
// that sits directly on a Scan of spansTable (or a bare such Scan) and
// conjoins pred into its predicate. n must be a solely-owned clone. Returns
// true once it has folded pred, false if no matching spans scan is
// reachable.
func pushPredicateToSpansScanFilter(n chplan.Node, spansTable string, pred chplan.Expr) bool {
	switch v := n.(type) {
	case *chplan.Filter:
		if sc, ok := v.Input.(*chplan.Scan); ok && sc.Table == spansTable {
			v.Predicate = conjoinExpr(v.Predicate, pred)
			return true
		}
		return pushPredicateToSpansScanFilter(v.Input, spansTable, pred)
	case *chplan.Aggregate:
		return pushPredicateToSpansScanFilter(v.Input, spansTable, pred)
	case *chplan.Project:
		return pushPredicateToSpansScanFilter(v.Input, spansTable, pred)
	}
	return false
}

// conjoinExpr ANDs right into left, dropping a nil right (so a one-sided window
// stays bare) and returning right when left is nil.
func conjoinExpr(left, right chplan.Expr) chplan.Expr {
	switch {
	case right == nil:
		return left
	case left == nil:
		return right
	default:
		return &chplan.Binary{Op: chplan.OpAnd, Left: left, Right: right}
	}
}

// compareSelOut / compareAttrOut / compareValOut / compareValueOut
// resolve the output aliases with the same defaults the lowering pins
// (internal/traceql/metrics_compare.go).
func compareSelOut(m *chplan.MetricsCompare) string {
	if m.SelAlias != "" {
		return m.SelAlias
	}
	return "is_selection"
}

func compareAttrOut(m *chplan.MetricsCompare) string {
	if m.AttrAlias != "" {
		return m.AttrAlias
	}
	return "attr"
}

func compareValOut(m *chplan.MetricsCompare) string {
	if m.ValAlias != "" {
		return m.ValAlias
	}
	return "val"
}

func compareValueOut(m *chplan.MetricsCompare) string {
	if m.ValueAlias != "" {
		return m.ValueAlias
	}
	return "Value"
}

// compareTupleElementFrag renders `tupleElement(kv, <idx>)` — the
// attr (idx 1) / val (idx 2) projection out of the arrayJoin'd pair.
// The index binds as a positional arg (clickhouse-go interpolates it
// client-side, so CH sees the constant literal tupleElement requires).
func compareTupleElementFrag(idx int64) Frag {
	return Call("tupleElement", BareIdent("kv"), Lit(idx))
}

// compareCountValueFrag renders `toFloat64(count(?))` with the bound
// literal 1 — same reducer shape as the other metrics emitters, pinned
// to Float64 so chclient.Sample.Value scans cleanly.
func compareCountValueFrag() Frag {
	return Call(
		"toFloat64",
		func(b *Builder) {
			_ = b.Expr(&chplan.FuncCall{Name: "count", Args: []chplan.Expr{&chplan.LitInt{V: 1}}})
		},
	)
}

// emitMetricsCompare renders the bare (time-collapsed) shape.
func (e *emitter) emitMetricsCompare(m *chplan.MetricsCompare) error {
	base, err := e.compareBaseQuery(m, nil)
	if err != nil {
		return err
	}

	selA, attrA, valA := compareSelOut(m), compareAttrOut(m), compareValOut(m)
	outer := NewQuery().From(base.Frag())
	outer.Select(Col(selA))
	outer.SelectAs(compareTupleElementFrag(1), attrA)
	outer.SelectAs(compareTupleElementFrag(2), valA)
	outer.SelectAs(compareCountValueFrag(), compareValueOut(m))
	outer.GroupBy(Col(selA), Col(attrA), Col(valA))
	outer.OrderBy(Col(selA), false).OrderBy(Col(attrA), false).OrderBy(Col(valA), false)

	return e.emitSelect(outer)
}

// emitRangeWindowCompare renders the matrix shape — one row per
// (cohort, attr, val, anchor) with the per-anchor count. Mirrors
// emitRangeWindowHistogram's anchor machinery (sample-side fanout,
// scan-bound pushdown); see the package comment at the top of this
// file for the SQL skeleton.
func (e *emitter) emitRangeWindowCompare(r *chplan.RangeWindow, m *chplan.MetricsCompare) error {
	if r.TimestampColumn == "" {
		return fmt.Errorf("%w: RangeWindow.TimestampColumn unset (required for MetricsCompare input)", ErrUnsupported)
	}
	if r.Step <= 0 {
		return fmt.Errorf("%w: RangeWindow wrapping MetricsCompare requires Step > 0", ErrUnsupported)
	}

	end := endExprFrag(r)
	stepNS := r.Step.Nanoseconds()
	rangeDur := r.Range
	if rangeDur == 0 {
		rangeDur = r.Step
	}
	rangeNS := rangeDur.Nanoseconds()

	var numAnchors int64
	switch {
	case r.OuterRange > 0:
		numAnchors = r.OuterRange.Nanoseconds()/stepNS + 1
	case !r.Start.IsZero() && !r.End.IsZero():
		span := r.End.Sub(r.Start).Nanoseconds()
		if span < 0 {
			return fmt.Errorf("%w: RangeWindow.Start > End", ErrUnsupported)
		}
		numAnchors = span/stepNS + 1
	default:
		numAnchors = 1
	}

	tsCol := r.TimestampColumn
	// Push the (Start-range, End] Timestamp window INTO each scan of the
	// compare join (the 's' span scan + the seeded root leg) rather than
	// above the join where CH 24.12 cannot prune it. Gated on both Start
	// and End being set, matching maybePushInnerScanTimeBounds' contract.
	// Fail closed if the compare inner is a spans scan with no request window:
	// without Start/End the bound is nil and each MergeTree leg scans full
	// retention. Scoped to e.spansTable (threaded onto the emit context for the
	// Tempo head), so it enforces in prod but is a no-op for an isolated emit
	// that did not set a spans table.
	if err := requireInnerSpansScanBound(r, m.Inner, e.spansTable); err != nil {
		return err
	}
	var bound *compareScanBound
	if !r.Start.IsZero() && !r.End.IsZero() {
		lo, hi := innerScanTsBoundsFrags(tsCol, r.Start, r.End, r.Offset.Nanoseconds(), rangeNS)
		bound = &compareScanBound{lo: lo, hi: hi}
	}
	// Push a `TraceId IN (<windowed cohort>)` seed directly onto the
	// root-lookup's physical scan (below its own GROUP BY), so the membership
	// predicate applies to the aggregate's inputs rather than being stuck above
	// the GROUP BY like boundedRootLeg's wrap (it does not prune the scan on its
	// own — the root-scoped or trace_id_ts Timestamp bound below does that). TraceId is
	// RootLookup's bare, unaliased GROUP BY key with no HAVING/window
	// function in between, so a TraceId-membership predicate commutes freely
	// across the GROUP BY — pushing it into the scan's Filter produces the
	// same groups as filtering after. This is unlike boundedRootLeg's
	// post-aggregate `TraceId IN (...)` wrap (kept as a correctness fallback
	// in compareBaseQuery, skipped via bound.rootSeeded whenever this
	// pushdown succeeds): that wrap sits ABOVE the GROUP BY, so CH must
	// materialize the full aggregate before it can apply it — the actual OOM
	// cause. A direct request-window Timestamp bound on the root span is LOSSY
	// for a non-root selection — it silently drops root-name/root-service
	// enrichment for any trace whose root started before the window. The
	// root-scoped shape uses that direct bound because its roots are in-window;
	// a non-root shape instead uses the enabled trace_id_ts envelope, which
	// contains every selected trace's root, while letting CH partition/PK-prune
	// (TraceId-IN alone cannot: TraceId is not the sort key). That
	// root-scoped shape is exactly the traces-drilldown "Comparison" whose
	// unpruned root scan reads full retention and OOMs.
	// windowRootLookupTraceIDSeed derives the spans
	// table from RootLookup itself (rootLookupSpansTable), not from
	// e.ctxSpansTable, so this fires uniformly on every emit path — no
	// context-threading gate needed here.
	// envelope is the trace_id_ts cohort envelope's scalar-binding body, set by
	// the non-root-scoped arm below. It travels to compareBaseQuery on the
	// bound, which declares it on the root-lookup relation itself — the same
	// statement whose scan filter carries the bounds that read it (see
	// bindRootLookupTraceIDTsEnvelope).
	if bound != nil && m.RootLookup != nil {
		// The seed must bound Inner by the SAME (Start-Offset-range, End-Offset]
		// window the 's' leg's own scan uses (innerScanTsBoundsFrags above) —
		// not the raw [Start, End] request window. Otherwise a trace whose only
		// matching span falls in the anchor lookback slice (Start-range, Start)
		// — the normal case for every anchor before the last — would satisfy
		// the 's' leg's bound but miss the seed's, silently dropping its
		// root-name/root-service enrichment. offsetNS's sign convention matches
		// offsetShiftedTimeFrag: shiftedNano = wallNano - offsetNS.
		offsetNS := r.Offset.Nanoseconds()
		lo, hi := tsBoundExprs(tsCol, r.Start.UnixNano()-offsetNS-rangeNS, r.End.UnixNano()-offsetNS)
		seed := compareSeedNode(m.Inner, m.TraceIDColumn, lo, hi)
		windowed := *m
		// A root-scoped selection can use the request window directly. For a
		// non-root selection, use the enabled trace_id_ts relation instead: its
		// selected-trace envelope retains roots older than the request window.
		// compareSeedNode yields nil for an Inner it cannot seed; with no cohort
		// to look up there is no envelope to derive either, so the scan keeps
		// #1214's unbounded-but-lossless shape.
		var rootLo, rootHi chplan.Expr
		var envelope *QueryBuilder
		switch {
		case m.InnerRootScoped:
			rootLo, rootHi = lo, hi
		case seed != nil:
			rootLo, rootHi, envelope = rootLookupTraceIDTsBounds(m, seed, tsCol)
		}
		rootLookup, seeded := windowRootLookupTraceIDSeed(m.RootLookup, m.TraceIDColumn, seed, rootLo, rootHi)
		windowed.RootLookup = rootLookup
		boundCopy := *bound
		boundCopy.rootSeeded = seeded
		if seeded {
			boundCopy.rootEnvelope = envelope
		}
		// A !seeded arm leaves rootEnvelope nil: the bounds never reached a
		// scan, so nothing reads the binding and declaring it would buy a full
		// trace_id_ts scan for no reader.
		bound = &boundCopy
		m = &windowed
	}
	base, err := e.compareBaseQuery(m, bound, tsCol)
	if err != nil {
		return err
	}

	selA, attrA, valA := compareSelOut(m), compareAttrOut(m), compareValOut(m)

	fanout := NewQuery().From(base.Frag())
	fanout.Select(Col(selA))
	fanout.SelectAs(compareTupleElementFrag(1), attrA)
	fanout.SelectAs(compareTupleElementFrag(2), valA)
	fanout.SelectAs(
		sampleAnchorFanoutFrag(end, func(b *Builder) { b.Ident(tsCol) }, stepNS, rangeNS, numAnchors),
		RangeWindowAnchorAlias,
	)

	outer := NewQuery().From(fanout.Frag())
	outer.Select(Col(selA), Col(attrA), Col(valA), Col(RangeWindowAnchorAlias))
	outer.SelectAs(compareCountValueFrag(), compareValueOut(m))
	outer.GroupBy(Col(selA), Col(attrA), Col(valA), Col(RangeWindowAnchorAlias))

	return e.emitSelect(outer)
}
