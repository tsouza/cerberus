// See aggregate.go for the no-reflection / no-pointer-aliasing rule
// covering this file.

package traceql

import (
	"context"
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
// when unset (unbounded).
func searchTraceLimit(ctx context.Context) int64 {
	if n, ok := ctx.Value(searchTraceLimitKey{}).(int); ok && n > 0 {
		return int64(n)
	}
	return 0
}

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
func stampNestedSetTraceLimit(plan chplan.Node, limit int64, s schema.Traces) chplan.Node {
	if limit <= 0 || plan == nil {
		return plan
	}
	switch v := plan.(type) {
	case *chplan.Project:
		v.Input = stampNestedSetTraceLimit(v.Input, limit, s)
	case *chplan.NestedSetAnnotate:
		if inputGuaranteesRootInResult(v.Input, s.ParentSpanIDColumn) {
			v.TraceLimit = limit
			// Bound the row source too, not just the numbering. The numbering
			// walk is bounded by TraceLimit (boundedRootScopeFrag), but the
			// structural-union row source (v.Input) is otherwise computed over
			// every trace in the window — two recursive closures + a wide-row
			// UNION DISTINCT — and only truncated to N afterward, peaking past
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
			}
			v.Input = pushBoundedTraceGate(v.Input, gate)
		}
	}
	return plan
}

// pushBoundedTraceGate ANDs gate into every leaf Filter/Scan of a
// NestedSetAnnotate row source. The bare-root union arm and both structural
// arms (whose recursive closures are seeded by the #77 seed re-render of their
// leaf subtree) each become `... AND TraceId IN (topN)`, bounding the closures
// to the top-N traces instead of the whole window. The same immutable gate
// pointer is shared across all leaves, so every leaf emits identical SQL.
//
// The recursion mirrors the node families a select()-with-nested-set row
// source can produce (the union/structural/project/limit spine over
// Filter(Scan) / Scan leaves); isRootSpanFilter looks through a passthrough
// Project, so recursing Project.Input here gates the re-projected bare-root arm
// too. Any other node is left untouched (the gate only needs to reach the
// scans that seed the closures).
func pushBoundedTraceGate(n chplan.Node, gate chplan.Expr) chplan.Node {
	switch v := n.(type) {
	case *chplan.Filter:
		if _, ok := v.Input.(*chplan.Scan); ok {
			v.Predicate = conjoin(v.Predicate, gate)
			return v
		}
		v.Input = pushBoundedTraceGate(v.Input, gate)
		return v
	case *chplan.Scan:
		return &chplan.Filter{Input: v, Predicate: gate}
	case *chplan.StructuralJoin:
		v.Left = pushBoundedTraceGate(v.Left, gate)
		v.Right = pushBoundedTraceGate(v.Right, gate)
		return v
	case *chplan.SetOperation:
		v.Left = pushBoundedTraceGate(v.Left, gate)
		v.Right = pushBoundedTraceGate(v.Right, gate)
		return v
	case *chplan.Project:
		v.Input = pushBoundedTraceGate(v.Input, gate)
		return v
	case *chplan.Limit:
		v.Input = pushBoundedTraceGate(v.Input, gate)
		return v
	default:
		return n
	}
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
	return ok && col.Name == parentSpanIDCol && lit.V == ""
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
// Filter(Scan) (`{ <matchers> }`). Metrics pipelines (Aggregate), structural
// joins, set operations, and the already-bounded nested-set / structure-tab
// plans (handled by stampNestedSetTraceLimit) are returned unchanged: those
// either don't drain per-trace summaries or carry their own bound.
func stampSearchTraceLimit(plan chplan.Node, limit int64, start, end time.Time, s schema.Traces) chplan.Node {
	if limit <= 0 || plan == nil {
		return plan
	}
	scan, pred, ok := plainSearchSource(plan)
	if !ok {
		return plan
	}

	// Fold the request window into the predicate so both the inner ranking
	// subquery and the outer drain scan only [start, end].
	pred = andWindow(pred, start, end, s.TimestampColumn)

	var input chplan.Node = scan
	if pred != nil {
		input = &chplan.Filter{Input: scan, Predicate: pred}
	}
	return &chplan.SearchTraceLimit{
		Input:           input,
		TraceIDColumn:   s.TraceIDColumn,
		TimestampColumn: s.TimestampColumn,
		TraceLimit:      limit,
	}
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
		pred = conjoin(pred, tsBound(chplan.OpGe, start, tsCol))
	}
	if !end.IsZero() {
		pred = conjoin(pred, tsBound(chplan.OpLe, end, tsCol))
	}
	return pred
}

// conjoin ANDs two predicates, dropping a nil left arm so the first bound
// folded into an empty predicate stays bare.
func conjoin(left, right chplan.Expr) chplan.Expr {
	if left == nil {
		return right
	}
	return &chplan.Binary{Op: chplan.OpAnd, Left: left, Right: right}
}

// tsBound builds `<tsCol> <op> fromUnixTimestamp64Nano(<t.UnixNano()>)` — a
// nanosecond-precision comparison against the DateTime64(9) timestamp column,
// matching the precision the OTel-CH traces schema stores.
func tsBound(op chplan.BinaryOp, t time.Time, tsCol string) chplan.Expr {
	return &chplan.Binary{
		Op:   op,
		Left: &chplan.ColumnRef{Name: tsCol},
		Right: &chplan.FuncCall{
			Name: "fromUnixTimestamp64Nano",
			Args: []chplan.Expr{&chplan.LitInt{V: t.UnixNano()}},
		},
	}
}
