package chsql

import (
	"context"
	"fmt"

	"github.com/tsouza/cerberus/internal/chplan"
)

// This file is the chsql half of the spans-scan resource-bound invariant. The
// chplan half (internal/chplan/scan_resource_bound.go) classifies the Node-tree
// scans at the emit chokepoint; this half covers the two shapes the IR descent
// cannot see:
//
//   - the emitter-synthetic recursive scans (structural closures, nested-set
//     numbering) that are rendered as Frags, not chplan *Scans — gated by
//     fromSpansScan at each FROM site;
//   - the matrix metrics inner scan (compare / exemplars) whose request window
//     is pushed at emit time by maybePushInnerScanTimeBounds and would silently
//     no-op on a zero window — gated by requireInnerSpansScanBound, a Tempo-only
//     fail-closed wrapper that fires only when the inner is a spans scan and the
//     window is absent. (PromQL keeps calling the shared maybePush helper
//     directly, so this adds no churn to the metrics matrix path.)

// ErrUnboundedSpansScan is the sentinel an emit site returns when it would
// render a FROM <spans table> with no established resource bound. It wraps
// ErrUnsupported so existing emit-error handling (and the HTTP error mapping)
// treats it as an ordinary unsupported-shape failure rather than serving a
// plan that would read full retention.
var ErrUnboundedSpansScan = fmt.Errorf("%w: unbounded spans scan", ErrUnsupported)

// spansResourceBoundKind mirrors chplan.ScanBoundKind on the emit side, minus
// the form-a (window) variant. An emit-site witness is only ever constructed for
// an emitter-synthetic spans scan — the recursive structural / nested-set arms —
// and those can never be bounded by a window alone: a recursive arm walks a
// closure across iterations, so it must be scoped by a finite TraceId set
// (form-b) or fall back to the depth-cap memory-streaming bound. The form-a
// window classification lives where it can actually apply: chplan's IR descent
// (chplan.RequireSpansScansBounded) over Node-tree scans, and the matrix-inner
// guard requireInnerSpansScanBound (which checks rw.Start/End directly, building
// no witness). spansBoundNone is the rejected zero value.
type spansResourceBoundKind int

const (
	spansBoundNone spansResourceBoundKind = iota
	// spansBoundTraceIDSet: the FROM carries a finite TraceId membership (a
	// literal InList or a `TraceId IN (<bounded subquery>)`).
	spansBoundTraceIDSet
	// spansBoundMemoryStreaming: the recursive structural arm whose seed-IN was
	// dropped to avoid CH error 49; bounded by the recursion depth cap + the
	// finite recursive working set, not a partition claim.
	spansBoundMemoryStreaming
)

// scanResourceBound is the witness an emit site passes to fromSpansScan to
// prove a spans FROM is bounded. conjuncts records the Frags that constitute the
// bound (the same predicates the caller places in the surrounding WHERE) so the
// declaration is honest about what the bound is, not merely that one exists.
type scanResourceBound struct {
	kind      spansResourceBoundKind
	conjuncts []Frag
}

// traceIDSetBound declares a form-b (finite TraceId set) bound.
func traceIDSetBound(conjuncts ...Frag) scanResourceBound {
	return scanResourceBound{kind: spansBoundTraceIDSet, conjuncts: conjuncts}
}

// memoryStreamingBound declares a recursive-arm memory-streaming bound. It is
// used only when the seed-IN pushdown is dropped to keep CH from erroring 49 on
// a recursive subquery nested in a recursive arm, so it carries no partition
// conjunct of its own.
//
// Width AND depth are both bounded, so this is not a fig-leaf "bounded":
//   - DEPTH: structuralDepthBoundFrag caps the closure at
//     defaultStructuralRecursionDepth (128) iterations.
//   - WIDTH: the recursive working table only ever holds spans of traces in the
//     SEED's result, and the seed is itself a spans scan that the resource-bound
//     invariant forces to be window- or trace-id-bounded (form-a / form-b) — the
//     structure-tab additionally caps it to the top-N traces via the
//     BoundedTraceScope #1109/#1110 fix. So the working set is
//     O(spans in the bounded seed trace set), never the whole table.
//
// The bound the seed leaf carries is verified by the chokepoint
// (RequireSpansScansBounded), so this arm cannot widen past what the seed
// already proved.
func memoryStreamingBound() scanResourceBound {
	return scanResourceBound{kind: spansBoundMemoryStreaming}
}

// requireScanResourceBound fails closed when table is the emitter's spans table
// and the witness proves no bound. When emitterSpansTable is empty (PromQL /
// metrics matrix emit, where no spans table is under enforcement) or the table
// being scanned is some other table, it is a no-op — the invariant is scoped to
// the TraceQL spans table.
func requireScanResourceBound(emitterSpansTable, table string, b scanResourceBound) error {
	if emitterSpansTable == "" || table != emitterSpansTable {
		return nil
	}
	if b.kind == spansBoundNone {
		return fmt.Errorf(
			"%w: FROM %q rendered with no resource bound (window / trace-id set / memory-streaming)",
			ErrUnboundedSpansScan, table,
		)
	}
	return nil
}

// fromSpansScan renders a FROM reference to the spans table only after the
// witness passes requireScanResourceBound. Callers wrap the returned Frag with
// aliasedFrag(...) as needed; the conjuncts that prove the bound must already be
// placed in the surrounding WHERE by the caller. A spansBoundNone witness over
// the emitter's spans table returns ErrUnboundedSpansScan and renders nothing.
func (e *emitter) fromSpansScan(spansTable string, b scanResourceBound) (Frag, error) {
	if err := requireScanResourceBound(e.spansTable, spansTable, b); err != nil {
		return nil, err
	}
	return Col(spansTable), nil
}

// requireInnerSpansScanBound is the Tempo-only fail-closed wrapper for the
// matrix metrics inner scan (compare / exemplars). It fires only when the inner
// relation is itself a scan of spansTable AND the RangeWindow carries no
// request window (Start or End zero) — exactly the silent no-op that
// maybePushInnerScanTimeBounds would leave behind. PromQL never reaches here
// (its inner is a metrics table, and it calls maybePush directly), so the
// shared gate is untouched and the metrics matrix path keeps zero churn.
func requireInnerSpansScanBound(rw *chplan.RangeWindow, inner chplan.Node, spansTable string) error {
	if spansTable == "" || findScanTable(inner) != spansTable {
		return nil
	}
	if rw.Start.IsZero() || rw.End.IsZero() {
		return fmt.Errorf(
			"%w: metrics inner scan of %q reached emit without a request window — "+
				"maybePushInnerScanTimeBounds would silently no-op, scanning full retention; "+
				"the handler must thread a non-zero [start,end] onto the RangeWindow",
			ErrUnboundedSpansScan, spansTable,
		)
	}
	return nil
}

// spansTableKey is the unexported context key carrying the TraceQL spans table
// name into the emit chokepoint, so chsql.Emit can scope RequireSpansScansBounded
// to the spans table. PromQL / LogQL callers never set it, leaving the top-level
// IR descent a no-op for those heads.
type spansTableKey struct{}

// WithSpansTable returns ctx carrying the TraceQL spans table name. The engine
// threads it onto the emit context for the Tempo head (engine.emitForHead, via
// the Lang's SpansTable() method), so chsql.Emit's RequireSpansScansBounded runs
// over every Tempo plan — search, structural, nested-set, metrics, trace-by-id.
// The root-lookup path (chsql.Emit called directly, not through the engine) sets
// it itself. An empty table leaves ctx unchanged.
func WithSpansTable(ctx context.Context, table string) context.Context {
	if table == "" {
		return ctx
	}
	return context.WithValue(ctx, spansTableKey{}, table)
}

// spansTableFromCtx recovers the spans table set by WithSpansTable, or "" when
// unset (PromQL / metrics matrix emit — the table-scoped no-op case).
func spansTableFromCtx(ctx context.Context) string {
	if t, ok := ctx.Value(spansTableKey{}).(string); ok {
		return t
	}
	return ""
}
