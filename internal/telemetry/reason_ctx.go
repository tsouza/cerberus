package telemetry

import "context"

// A request-scoped override for the `cerberus_error_reason` label.
//
// WHY THIS EXISTS. The reason label was derived from the response status
// code alone, and on cerberus's wire that derivation cannot separate the
// cases operators most need separated. All three heads answer a query
// wall-clock timeout with 503, because upstream Prometheus and Loki answer
// it with 503 and the differential harnesses assert byte-parity with them —
// so moving the status to 504 to make the label right would be a real
// compatibility regression, not a fix. The result was that
// ReasonTimeout and ReasonResourceExhausted were structurally unreachable
// (nothing in the tree ever wrote a 408, 504, 429 or 507) while
// ReasonBackendUnavailable silently absorbed every timeout, leaving a
// slow query indistinguishable from a ClickHouse outage on the one metric
// an operator pages from.
//
// The fix keeps the wire bytes exactly as they are and lets the handler
// that already KNOWS what went wrong say so, out of band: it writes the
// reason into a cell the middleware installed on the request context, and
// the middleware prefers that over its status-derived guess. A handler
// that says nothing is classified exactly as before, and a call made
// outside the middleware (a test, a non-HTTP caller) is a no-op rather
// than an error — the cell is simply absent.
type reasonCell struct {
	reason string
}

type reasonCellKey struct{}

// WithReasonCell installs an empty reason cell on ctx and returns both.
// QueryMiddleware calls it once per request; the cell it returns is read
// back in the middleware's own outcome defer. Callers other than the
// middleware have no reason to install one.
func WithReasonCell(ctx context.Context) (context.Context, *reasonCell) {
	cell := &reasonCell{}
	return context.WithValue(ctx, reasonCellKey{}, cell), cell
}

// SetReason records the reason a request failed, overriding the reason the
// middleware would otherwise derive from the response status. It is a
// no-op when no cell is installed, so a handler may call it
// unconditionally. The last call wins: a handler that classifies once at
// the point of failure will not be second-guessed by an outer wrapper that
// classifies again from a coarser view.
//
// Pass one of the Reason* constants. A value outside that closed set would
// widen the label's cardinality, so anything unrecognised is ignored.
func SetReason(ctx context.Context, reason string) {
	if !knownReason(reason) {
		return
	}
	cell, ok := ctx.Value(reasonCellKey{}).(*reasonCell)
	if !ok {
		return
	}
	cell.reason = reason
}

// reasonFor reads back what SetReason recorded, or "" when nothing did.
func (c *reasonCell) reasonFor() string {
	if c == nil {
		return ""
	}
	return c.reason
}

// knownReason gates SetReason to the closed enum. ReasonNone is excluded
// deliberately: it is the success value, and a handler cannot override a
// failure into a success without also changing the status the result label
// is derived from.
//
// Membership is DERIVED from ErrorReasons rather than re-typed here. This
// gate fails CLOSED — an unrecognised value is silently ignored, not
// reported — so a hand-written copy that fell behind the enum would make a
// newly added reason vanish at runtime with nothing failing anywhere
// (cerberus issue #3184 found four such copies, and this is the only one
// whose staleness is invisible).
func knownReason(reason string) bool {
	if reason == ReasonNone {
		return false
	}
	for _, r := range ErrorReasons() {
		if r == reason {
			return true
		}
	}
	return false
}
