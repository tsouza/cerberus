// See aggregate.go for the no-reflection / no-pointer-aliasing rule
// covering this file.

package traceql

import (
	"context"
	"sort"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// searchTraceLimitKey types the context value carrying /api/search's
// per-request `limit` (max trace summaries the response keeps) down into
// lowering. A dedicated unexported key type avoids collisions with any
// other context value.
type searchTraceLimitKey struct{}

// WithSearchTraceLimit returns a context carrying the /api/search `limit`
// (max returned trace summaries). lowerRoot reads it to bound the
// nested-set numbering walk to exactly the traces the response will keep
// (see stampNestedSetTraceLimit). n <= 0 leaves the numbering unbounded —
// the behaviour for every caller that doesn't return a bounded trace set
// (the metrics pipelines, the spec/property harnesses, /traces/{id}).
//
// The Tempo /api/search + gRPC Search handlers wrap their request context
// with this before calling Engine.Query / Engine.QueryCursor; the value
// rides through Lang.Parse → traceql.Lower (both already ctx-aware) to the
// stamping pass, so no lowering function signature changes.
func WithSearchTraceLimit(ctx context.Context, n int) context.Context {
	if n <= 0 {
		return ctx
	}
	return context.WithValue(ctx, searchTraceLimitKey{}, n)
}

// searchTraceLimit recovers the value WithSearchTraceLimit stored, or 0
// when unset (unbounded). `n >= 1` (equivalently `n > 0` for the int the
// key ever holds) rejects a non-positive value stored directly under the
// key, so only a real, positive trace limit reads back.
func searchTraceLimit(ctx context.Context) int64 {
	if n, ok := ctx.Value(searchTraceLimitKey{}).(int); ok && n >= 1 {
		return int64(n)
	}
	return 0
}

// SearchTraceLimit exposes the /api/search trace limit (WithSearchTraceLimit)
// to adapters that finalise the plan outside this package — the Tempo Lang's
// ProjectSamples wrap reads it to cap a spanset-aggregation search to the
// newest N traces server-side, the parity counterpart to the SearchTraceLimit
// node plain search already gets. 0 = unbounded.
func SearchTraceLimit(ctx context.Context) int64 { return searchTraceLimit(ctx) }

// SearchWindow exposes the /api/search request window (WithSearchWindow) to
// adapters that build the plan outside this package — the offline explain Lang
// reads it to bound a metrics-range preview's RangeWindow to the same window a
// plain search scan gets. Two zero times = no window threaded.
func SearchWindow(ctx context.Context) (time.Time, time.Time) { return searchWindowFromCtx(ctx) }

// stampNestedSetTraceLimit walks the lowered plan and sets TraceLimit on
// every NestedSetAnnotate whose input plan guarantees each returned trace's
// root span is in the result set — the precondition under which ranking the
// numbering scope by root-span Timestamp equals /api/search's result-min-
// Timestamp ranking, so the bounded scope keeps exactly the traces
// TruncateSummaries keeps (exact parity). For any other shape the annotate
// is left untouched (TraceLimit stays 0, numbering byte-identical to today).
//
// limit <= 0 (no /api/search limit on the context — metrics, tests, the
// property harness) is a no-op: the plan is returned unchanged.
//
// The walk only descends the node families a select()-with-nested-set plan
// can produce above the NestedSetAnnotate: the Project the select lowering
// emits, and any chained second select()'s Project. NestedSetAnnotate never
// appears under a metrics pipeline (select is span-shaped, not metric), so
// the metric node families are deliberately not traversed.
func stampNestedSetTraceLimit(plan chplan.Node, limit int64, start, end time.Time, s schema.Traces) chplan.Node {
	if limit <= 0 || plan == nil {
		return plan
	}
	// Window the top-N root RANKING to [start, end] so the structure tab ranks
	// the newest-N roots IN the window, not the newest-N ever. Without this a
	// historical-window search gates the row source to globally-newest roots
	// outside the window and returns an empty result (#1109 GAP-3). The same
	// nanos go onto BOTH the numbering scope (NestedSetAnnotate.Window*) and the
	// leaf gate (BoundedTraceScope.Window*) so boundedRootScopeFrag emits a
	// byte-identical subquery for each — a mismatch would strand kept rows.
	var startNano, endNano int64
	if !start.IsZero() {
		startNano = start.UnixNano()
	}
	if !end.IsZero() {
		endNano = end.UnixNano()
	}
	switch v := plan.(type) {
	case *chplan.NestedSetAnnotate:
		if inputGuaranteesRootInResult(v.Input, s.ParentSpanIDColumn) {
			v.TraceLimit = limit
			v.WindowStartNano = startNano
			v.WindowEndNano = endNano
			// Bound the row source too, not just the numbering. The numbering
			// walk is bounded by TraceLimit (boundedRootScopeFrag), but the
			// structural-union row source (v.Input) is otherwise computed over
			// every trace in the window — two recursive closures + a union of
			// two wide arms — and only truncated to N afterward, peaking past
			// the per-query memory cap (#1109 prod OOM). Push one shared
			// BoundedTraceScope gate into every leaf so the closures (seeded
			// via the #77 seed re-render of those leaves) scan only the top-N
			// traces — the IDENTICAL set the numbering uses, so no kept row is
			// stranded at the 0/0/0 LEFT-JOIN default. Gating here, inside the
			// same precondition that sets TraceLimit, keeps the two bounds in
			// lock-step (they can never fire on different shapes).
			gate := &chplan.BoundedTraceScope{
				SpansTable:         v.SpansTable,
				TraceIDColumn:      v.TraceIDColumn,
				ParentSpanIDColumn: v.ParentSpanIDColumn,
				TimestampColumn:    v.TimestampColumn,
				TraceLimit:         limit,
				WindowStartNano:    startNano,
				WindowEndNano:      endNano,
			}
			v.Input = chplan.PushLeafPredicate(v.Input, gate)
		}
		return plan
	default:
		// Descend through ANY node between the pipeline root and the
		// NestedSetAnnotate — Project (`| select(nestedSet*)`), Aggregate
		// (`| group(by(nestedSet*))` / `| min(nestedSetLeft)`), Limit, set-ops —
		// so a structure-tab numbering nested under a later pipeline stage still
		// gets the top-N RANKING bound. The gate stays correctly scoped: it only
		// fires on a NestedSetAnnotate whose input is the Drilldown structure
		// shape (inputGuaranteesRootInResult), so a plain aggregate-over-nestedSet
		// (input Filter(Scan)) is descended into but left unbounded. The metrics
		// pipelines never reach here — Lower gates this whole pass on a
		// non-metrics plan — so the generic recurse can never bound a metric leaf.
		out, _ := chplan.RewriteChildren(plan, func(c chplan.Node) (chplan.Node, bool) {
			return stampNestedSetTraceLimit(c, limit, start, end, s), true
		})
		return out
	}
}

// stampRecursiveScanWindow folds the request window onto the EMITTER-SYNTHETIC
// recursive spans scans — the nested-set numbering CTE (NestedSetAnnotate's
// anchor/step arms) and the structural-closure step arm (StructuralJoin) —
// which scan the physical spans table directly and so are NOT chplan children
// reachable by stampSearchWindow's leaf-predicate push. Setting Window* (and,
// for StructuralJoin, TimestampColumn) on the node arms the emitter's direct
// `Timestamp >= … AND Timestamp <= …` partition-prune predicate (and its
// fail-closed guard): `TraceId IN (<seed>)` membership never prunes the
// toDate(Timestamp) partitions, so without this the walk reads full retention
// behind an inert IN.
//
// This runs on ANY plan that carries a request window — search OR metrics. The
// leaf-stamp passes (stampSearchWindow etc.) are gated search-only because a
// metrics query's chplan-leaf scans take their time bound from the
// /api/metrics/query_range handler's RangeWindow wrap. But that wrap cannot
// reach BELOW a WITH RECURSIVE, so a metrics pipeline over a structural /
// nested-set source (`{ } >> { } | rate()`,
// `{ nestedSetParent<0 } | by(nestedSetParent) | rate()`) would emit a
// windowless recursive arm unless this stamp runs for it too. The bound needs
// only a non-zero [start,end] + tsCol, NOT the /api/search response trace limit
// — conflating the two would skip the metrics path, which carries no limit.
//
// Unlike stampNestedSetTraceLimit's structure-tab RANKING bound, the request
// window is safe to apply to EVERY span search shape — select(nestedSet*),
// group(by(nestedSet*)), aggregate-over-nestedSet, and the structural
// >> / << / &>> closures all legitimately scan only [start, end]. The walk
// descends every node type (Aggregate, Project, Limit, set-ops, joins) so a
// NestedSetAnnotate or StructuralJoin nested under any of them is reached. For
// the structure-tab shape stampNestedSetTraceLimit already set the same nanos
// on the NestedSetAnnotate; re-setting them here is idempotent.
//
// A zero window leaves the plan unchanged (no node opts into windowing), so
// non-windowed callers (spec/property harnesses, /traces/{id}) stay
// byte-identical.
func stampRecursiveScanWindow(plan chplan.Node, start, end time.Time, s schema.Traces) chplan.Node {
	if plan == nil {
		return plan
	}
	var startNano, endNano int64
	if !start.IsZero() {
		startNano = start.UnixNano()
	}
	if !end.IsZero() {
		endNano = end.UnixNano()
	}
	if startNano == 0 && endNano == 0 {
		return plan
	}
	return windowRecursiveScans(plan, startNano, endNano, s.TimestampColumn, s.MaxTraceDuration)
}

// windowRecursiveScans is stampRecursiveScanWindow's in-place walk. It stamps
// the window onto every NestedSetAnnotate / StructuralJoin it reaches and
// recurses generically through every other node so deeply-nested closures are
// covered. A closure inside a trace-cohort subquery is part of that per-trace
// subquery, so it takes the window widened by lookback on each set side, the
// window the cohort's own leaves read (perTraceWindow).
func windowRecursiveScans(n chplan.Node, startNano, endNano int64, tsCol string, lookback time.Duration) chplan.Node {
	switch v := n.(type) {
	case *chplan.NestedSetAnnotate:
		// TimestampColumn is already set by the select / group / aggregate
		// lowering; only the window bounds need stamping here.
		v.WindowStartNano = startNano
		v.WindowEndNano = endNano
		v.Input = windowRecursiveScans(v.Input, startNano, endNano, tsCol, lookback)
		return v
	case *chplan.StructuralJoin:
		// Lowering leaves TimestampColumn "" on the structural join; setting it
		// alongside the window arms both the emit-time push and its fail-closed
		// guard (chsql.requireSpansScanWindow).
		v.TimestampColumn = tsCol
		v.WindowStartNano = startNano
		v.WindowEndNano = endNano
		v.Left = windowRecursiveScans(v.Left, startNano, endNano, tsCol, lookback)
		v.Right = windowRecursiveScans(v.Right, startNano, endNano, tsCol, lookback)
		return v
	case *chplan.Filter:
		// Same Expr-slot blind spot chplan.PushLeafPredicate documents: the
		// trace-cohort subquery of a spanset-aggregate operand
		// (`({A} >> {B} | count() > 1)`) can itself hold a structural closure,
		// and RewriteChildren cannot reach a Node behind an Expr. Descend it explicitly; the input recurse below is
		// what the generic default would have done.
		// The cohort's window is already widened, so a cohort nested inside
		// it reads the same window (zero further lookback).
		const alreadyWidened time.Duration = 0
		cohortStart, cohortEnd := widenWindowNanos(startNano, endNano, lookback)
		for _, in := range chplan.CohortSubqueries(v.Predicate) {
			in.Subquery = windowRecursiveScans(in.Subquery, cohortStart, cohortEnd, tsCol, alreadyWidened)
		}
		v.Input = windowRecursiveScans(v.Input, startNano, endNano, tsCol, lookback)
		return v
	default:
		out, _ := chplan.RewriteChildren(n, func(c chplan.Node) (chplan.Node, bool) {
			return windowRecursiveScans(c, startNano, endNano, tsCol, lookback), true
		})
		return out
	}
}

// stampSearchWindow folds the /api/search request window into every leaf
// Filter/Scan of a compound search row source (`&&` / `||`, structural
// >>/<</&>>, select(nestedSet*)) so it scans only [start, end] instead of full
// retention, and the widened per-trace window (perTraceWindow) into every
// per-trace subquery the row source carries. The plain-search shape is ALREADY windowed by stampSearchTraceLimit
// (which wraps it in a SearchTraceLimit node), and this pass runs AFTER it and
// does not descend SearchTraceLimit, so plain search stays byte-identical (no
// double-fold).
//
// Gated on limit > 0 — the exact "this is /api/search, not a metrics pipeline"
// discriminator (only the HTTP + gRPC search handlers set WithSearchTraceLimit;
// the metrics handlers lower with a bare ctx). A metrics plan therefore never
// reaches this pass, and even if it did the recursion's default case skips the
// Aggregate/RangeWindow families. A zero/absent window yields a nil predicate
// and the plan is returned unchanged (the search handlers clamp a windowless
// request to DefaultSearchLookback, so on the search path the window is always
// present).
//
// The window predicate reaches only chplan leaf scans. The structural closures'
// recursive `t`-scan and the nested-set numbering CTE are emitter-synthetic
// (not chplan children), so this leaf pass cannot reach them — the sibling
// stampRecursiveScanWindow pass stamps the request window onto those synthetic
// scans directly (a Timestamp partition-prune predicate on the physical
// `otel_traces` scan inside the recursive arm), which the inert `TraceId IN`
// seed membership can never deliver. Without it the closure / numbering CTE
// re-scans full retention (the GAP-3 OOM).
func stampSearchWindow(plan chplan.Node, limit int64, start, end time.Time, s schema.Traces) chplan.Node {
	if limit <= 0 || plan == nil {
		return plan
	}
	window := andWindow(nil, start, end, s.TimestampColumn)
	if window == nil {
		return plan
	}
	return chplan.PushLeafWindow(plan, window, perTraceWindow(start, end, s))
}

// inputGuaranteesRootInResult reports whether every trace n emits is
// guaranteed to carry its own root span (ParentSpanId = "") in the result —
// the precondition for bounding the numbering walk by root-span Timestamp.
//
// The recognised shape is the Grafana Traces Drilldown structure-tab input:
// a `||` SetOperation where (1) one arm is a bare root-span filter
// (`{ nestedSetParent < 0 }`, lowered to Filter(ParentSpanId = "") over a
// Scan, optionally re-projected) — which re-adds every trace's root — and
// (2) BOTH arms emit only spans belonging to root-bearing traces, so every
// trace in the result carries its root.
//
// Requirement (2) is load-bearing for the bound's correctness, not just its
// optimality: the bound (numbering scope + the BoundedTraceScope row-source
// gate) ranks/keeps traces by their ROOT span, so any trace admitted to the
// result WITHOUT a root span (e.g. a `{ kind = server }` arm matching a
// rootless trace under sampling / ingest lag) would be silently dropped by
// the gate — a wrong result, not a boundary reorder. Gating on (2) keeps the
// result set faithful: every admitted trace has a root, and the only residual
// approximation is the start-time RANKING (root-span vs result-min Timestamp;
// see boundedRootScopeFrag), which only shuffles the kept set at the N-th
// boundary under intra-trace clock skew.
//
// A non-bare-root arm is root-bearing-only when it is a POSITIVE
// descendant/child structural join seeded from a root filter
// (`{ root } &>> { x }` / `>>` / `&>` / `>`): every span it emits descends
// from a root, so its trace is root-bearing. Negated / ancestor / parent /
// sibling arms make no such guarantee and leave the select unbounded.
func inputGuaranteesRootInResult(n chplan.Node, parentSpanIDCol string) bool {
	set, ok := n.(*chplan.SetOperation)
	if !ok || set.Op != chplan.SetUnion {
		return false
	}
	leftRoot := isRootSpanFilter(set.Left, parentSpanIDCol)
	rightRoot := isRootSpanFilter(set.Right, parentSpanIDCol)
	if !leftRoot && !rightRoot {
		return false // no arm re-adds the roots
	}
	return armEmitsOnlyRootBearingTraces(set.Left, parentSpanIDCol) &&
		armEmitsOnlyRootBearingTraces(set.Right, parentSpanIDCol)
}

// armEmitsOnlyRootBearingTraces reports whether every span n emits belongs to
// a trace that carries a root span (ParentSpanId = "") — the per-arm half of
// inputGuaranteesRootInResult. Two shapes qualify: a bare root-span filter
// (it emits roots), and a positive descendant/child structural join whose
// ANCESTOR side (Left) is a root filter (every emitted span descends from a
// root, and union forms also re-emit those roots). Any other shape — a bare
// non-root filter, a negated/ancestor/parent/sibling structural join, a
// nested set-op — is conservatively rejected.
func armEmitsOnlyRootBearingTraces(n chplan.Node, parentSpanIDCol string) bool {
	if isRootSpanFilter(n, parentSpanIDCol) {
		return true
	}
	sj, ok := n.(*chplan.StructuralJoin)
	if !ok || sj.Op.IsNegated() {
		return false
	}
	switch sj.Op.Positive() {
	case chplan.StructuralDescendant, chplan.StructuralChild:
		return isRootSpanFilter(sj.Left, parentSpanIDCol)
	default:
		return false
	}
}

// isRootSpanFilter reports whether n is a root-span filter
// (`ParentSpanId = ""`) over a Scan, looking through a bare passthrough
// Project (the union-arm-alignment lowering re-projects the plain arm to
// match the structural arm's column list without changing its rows).
func isRootSpanFilter(n chplan.Node, parentSpanIDCol string) bool {
	if p, ok := n.(*chplan.Project); ok {
		n = p.Input
	}
	f, ok := n.(*chplan.Filter)
	if !ok {
		return false
	}
	if _, ok := f.Input.(*chplan.Scan); !ok {
		return false
	}
	b, ok := f.Predicate.(*chplan.Binary)
	if !ok || b.Op != chplan.OpEq {
		return false
	}
	col, ok := b.Left.(*chplan.ColumnRef)
	if !ok {
		return false
	}
	lit, ok := b.Right.(*chplan.LitString)
	return ok && col.Name == parentSpanIDCol && lit.V == rootParentSpanID
}

// searchWindowKey types the context value carrying /api/search's request
// time window ([start, end]) down into lowering, where stampSearchTraceLimit
// folds it into the bounded plain-search scan. A dedicated unexported key
// type avoids collisions with any other context value.
type searchWindowKey struct{}

// searchWindow pairs the request's start/end bounds threaded from the
// /api/search handler. Either or both may be the zero time.Time, meaning the
// corresponding bound was omitted (no predicate on that side).
type searchWindow struct {
	start time.Time
	end   time.Time
}

// WithSearchWindow returns a context carrying the /api/search request window
// so stampSearchTraceLimit can fold a `Timestamp >= start AND Timestamp <= end`
// predicate into the bounded scan. When both bounds are zero (no window on the
// request) the context is returned unchanged — the no-op path for every caller
// that doesn't pass a time range.
//
// Sibling to WithSearchTraceLimit: the Tempo /api/search handler wraps its
// request context with both before calling Engine.Query; the values ride
// through Lang.Parse → traceql.Lower to the stamping pass.
func WithSearchWindow(ctx context.Context, start, end time.Time) context.Context {
	if start.IsZero() && end.IsZero() {
		return ctx
	}
	return context.WithValue(ctx, searchWindowKey{}, searchWindow{start: start, end: end})
}

// searchWindowFromCtx recovers the bounds WithSearchWindow stored, or two
// zero times when unset (no window).
func searchWindowFromCtx(ctx context.Context) (time.Time, time.Time) {
	if w, ok := ctx.Value(searchWindowKey{}).(searchWindow); ok {
		return w.start, w.end
	}
	return time.Time{}, time.Time{}
}

// stampSearchTraceLimit wraps a plain-search row source in a
// chplan.SearchTraceLimit, pushing the request's `limit` into SQL so
// /api/search drains only the N newest traces instead of every matching row
// (the summaries-drain OOM). The request time window is folded into the
// scan predicate at the same time, so a windowed inner top-N ranking and
// the outer drain both scan only the window — never the whole table.
//
// Both search entry points — the HTTP /api/search handler and the gRPC
// streaming /search RPC — now clamp a windowless request (both start/end
// zero — e.g. a hand-rolled `q={}` with no time range) to a recent-lookback
// window before threading it here (see internal/api/tempo/handler.go
// handleSearch and internal/api/tempo/grpc/search.go Search, both using
// DefaultSearchLookback), so on every search path andWindow always folds a
// `Timestamp` bound and the inner `GROUP BY TraceId` is always windowed — the
// whole-table aggregation is unreachable from the search path. The lowering
// still tolerates a zero window for non-search callers (the metrics pipelines,
// the spec/property harnesses, /traces/{id}): they pass no limit, so
// stampSearchTraceLimit is a no-op for them and the absent window never reaches
// andWindow.
//
// limit <= 0 (no /api/search limit on the context — metrics, tests, the
// property harness, /traces/{id}) is a no-op: the plan is returned unchanged.
//
// Only the plain-search shape is matched — a bare Scan (`{}`) or a
// Filter(Scan) (`{ <matchers> }`), alone or as the Input of a
// TraceLookupFilter (`{ rootName = name }`, whose output rows are its Input's
// span rows, so ranking them by trace is the same top-N). Metrics pipelines (Aggregate), structural
// joins, set operations, and the already-bounded nested-set / structure-tab
// plans (handled by stampNestedSetTraceLimit) are returned unchanged: those
// either don't drain per-trace summaries or carry their own bound.
func stampSearchTraceLimit(plan chplan.Node, limit int64, start, end time.Time, s schema.Traces) chplan.Node {
	if limit <= 0 || plan == nil {
		return plan
	}
	if lookup, ok := plan.(*chplan.TraceLookupFilter); ok {
		input, ok := boundPlainSearchSource(lookup.Input, lookup.Predicate, start, end, s)
		if !ok {
			return plan
		}
		// The per-trace lookups and any membership subquery in the joined
		// predicate scan the spans table too; stampSearchWindow does not
		// enter the SearchTraceLimit built here, so they take the per-trace
		// window now, as the plain shape's cohort does below.
		bounded := *lookup
		bounded.Input = input
		if window := perTraceWindow(start, end, s); window != nil {
			bounded.Lookups = make([]chplan.Node, len(lookup.Lookups))
			for i, l := range lookup.Lookups {
				bounded.Lookups[i] = chplan.PushLeafPredicate(l, window)
			}
			chplan.PushLeafPredicateIntoCohort(bounded.Predicate, window)
		}
		return &chplan.SearchTraceLimit{Input: &bounded, TraceLimit: limit}
	}
	input, ok := boundPlainSearchSource(plan, nil, start, end, s)
	if !ok {
		return plan
	}
	return &chplan.SearchTraceLimit{Input: input, TraceLimit: limit}
}

// boundPlainSearchSource returns the plain-search row source plan — a Scan or
// a Filter(Scan), see plainSearchSource — with the request window folded into
// its predicate and its Scan closed over the columns the search response, the
// trace ranking, its own predicate and alsoRead (the predicate of a
// TraceLookupFilter above it, nil otherwise) read off a span row. ok is false
// for any other shape.
func boundPlainSearchSource(plan chplan.Node, alsoRead chplan.Expr, start, end time.Time, s schema.Traces) (chplan.Node, bool) {
	scan, pred, ok := plainSearchSource(plan)
	if !ok {
		return nil, false
	}

	// A trace-scoped intrinsic (traceDuration / rootName / rootServiceName)
	// carries its per-trace aggregate in an InSubquery inside this
	// predicate — a Node behind an Expr slot, invisible to every
	// RewriteChildren-based walk. stampSearchWindow's cohort descent cannot
	// reach it either: that pass does not enter a SearchTraceLimit, which is
	// exactly what stampSearchTraceLimit is about to build. Left unstamped,
	// the aggregate GROUPs BY TraceId over full retention behind a
	// membership test that looks bounded — the traces-drilldown
	// unbounded-scan class.
	//
	// Stamp it before the outer fold, using the same leaf push the
	// spanset-aggregate cohort already goes through. The cohort reads its
	// traces' spans in the per-trace window; the span rows below stay in the
	// request window.
	if window := perTraceWindow(start, end, s); window != nil {
		chplan.PushLeafPredicateIntoCohort(pred, window)
	}

	// Fold the request window into the predicate so both the inner ranking
	// subquery and the outer drain scan only [start, end].
	pred = andWindow(pred, start, end, s.TimestampColumn)
	closedScan := *scan
	closedScan.Columns = searchTraceLimitInputColumns(scan.Columns, conjoin(pred, alsoRead), s)
	scan = &closedScan

	if pred == nil {
		return scan, true
	}
	return &chplan.Filter{Input: scan, Predicate: pred}, true
}

// searchTraceLimitInputColumns closes the plain-search Scan over the columns
// consumed by the search response projection, the trace-ranking drain, and
// the query predicate. A per-trace lookup column a TraceLookupFilter joins in
// (traceScopedLookups) is not a span column and stays out. SearchTraceLimit is only constructed for the bounded
// search path, whose caller always applies the canonical sample projection.
func searchTraceLimitInputColumns(existing []string, predicate chplan.Expr, s schema.Traces) []string {
	columns := make(map[string]struct{}, len(existing))
	add := func(name string) {
		if name != "" {
			columns[name] = struct{}{}
		}
	}
	for _, name := range existing {
		add(name)
	}
	add(s.TraceIDColumn)
	add(s.SpanIDColumn)
	add(s.ParentSpanIDColumn)
	add(s.SpanNameColumn)
	add(s.ResourceAttributesColumn)
	add(s.TimestampColumn)
	add(s.DurationColumn)
	chplan.InspectExpr(predicate, func(expr chplan.Expr) bool {
		if ref, ok := expr.(*chplan.ColumnRef); ok && !isTraceScopedLookupColumn(ref.Name) {
			add(ref.Name)
		}
		return true
	})

	result := make([]string, 0, len(columns))
	for name := range columns {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

// plainSearchSource matches the plain-search row source the trace-limit
// pushdown applies to: a bare *chplan.Scan (predicate nil) or a
// *chplan.Filter whose Input is a *chplan.Scan (predicate = the filter's).
// Any other shape returns ok=false so stampSearchTraceLimit leaves the plan
// untouched.
func plainSearchSource(plan chplan.Node) (scan *chplan.Scan, predicate chplan.Expr, ok bool) {
	switch v := plan.(type) {
	case *chplan.Scan:
		return v, nil, true
	case *chplan.Filter:
		if sc, isScan := v.Input.(*chplan.Scan); isScan {
			return sc, v.Predicate, true
		}
	}
	return nil, nil, false
}

// andWindow conjoins the existing predicate with the request's time-window
// bounds. A zero start/end contributes no bound; when neither bound is set
// and pred is nil, the result is nil (the caller emits a bare Scan).
func andWindow(pred chplan.Expr, start, end time.Time, tsCol string) chplan.Expr {
	if !start.IsZero() {
		pred = conjoin(pred, chplan.TimestampBound(chplan.OpGe, start, tsCol))
	}
	if !end.IsZero() {
		pred = conjoin(pred, chplan.TimestampBound(chplan.OpLe, end, tsCol))
	}
	return pred
}

// perTraceWindow is the request window a per-trace subquery reads its traces'
// spans in: [start - L, end + L], where L is s.MaxTraceDuration, the
// configured maximum trace duration. A trace no longer than L that overlaps
// [start, end] lies wholly inside it, so its root span and full extent
// resolve even when they lie outside the request window. A zero start/end
// contributes no bound, as in andWindow.
func perTraceWindow(start, end time.Time, s schema.Traces) chplan.Expr {
	if !start.IsZero() {
		start = start.Add(-s.MaxTraceDuration)
	}
	if !end.IsZero() {
		end = end.Add(s.MaxTraceDuration)
	}
	return andWindow(nil, start, end, s.TimestampColumn)
}

// widenWindowNanos is perTraceWindow's widening over a nanosecond window, as
// the recursive-scan stamps carry it: each set (non-zero) side moves outward
// by lookback, and an unset side stays 0.
func widenWindowNanos(startNano, endNano int64, lookback time.Duration) (int64, int64) {
	if startNano != 0 {
		startNano -= lookback.Nanoseconds()
	}
	if endNano != 0 {
		endNano += lookback.Nanoseconds()
	}
	return startNano, endNano
}

// conjoin ANDs two predicates, dropping a nil left arm so the first bound
// folded into an empty predicate stays bare.
func conjoin(left, right chplan.Expr) chplan.Expr {
	if left == nil {
		return right
	}
	return &chplan.Binary{Op: chplan.OpAnd, Left: left, Right: right}
}
