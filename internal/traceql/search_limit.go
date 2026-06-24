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
		}
	}
	return plan
}

// inputGuaranteesRootInResult reports whether every trace n emits is
// guaranteed to carry its own root span (ParentSpanId = "") in the result —
// the precondition for bounding the numbering walk by root-span Timestamp.
//
// The recognised shape is the Grafana Traces Drilldown structure-tab input:
// a `||` SetOperation one of whose arms is a bare root-span filter
// (`{ nestedSetParent < 0 }`, lowered to Filter(ParentSpanId = "") over a
// Scan, optionally re-projected). The union re-adds every matched trace's
// root, so result-min(Timestamp) per trace == root.Timestamp. This is the
// only OOM-prone shape; gating on it keeps the bound exact-parity-safe by
// construction and leaves all other selects unbounded.
func inputGuaranteesRootInResult(n chplan.Node, parentSpanIDCol string) bool {
	set, ok := n.(*chplan.SetOperation)
	if !ok || set.Op != chplan.SetUnion {
		return false
	}
	return isRootSpanFilter(set.Left, parentSpanIDCol) || isRootSpanFilter(set.Right, parentSpanIDCol)
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
