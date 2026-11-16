package tempo

import (
	"github.com/tsouza/cerberus/internal/engine"
	traceql "github.com/tsouza/cerberus/internal/traceql/ast"
)

// rootSpanNotYetReceivedText is the rootServiceName reference Tempo's search
// combiner reports for a trace whose summary carries none
// (modules/frontend/combiner/search.go addRootSpanNotReceivedText, text from
// pkg/search.RootSpanNotYetReceivedText).
const rootSpanNotYetReceivedText = "<root span not yet received>"

// metaKeyUnsetTraceFields keys the unsetTraceFields the TraceQL Lang stashes
// in engine.Meta.Extra (set in lang.go from
// ast.RootExpr.RegroupLeavesTraceFieldUnset).
const metaKeyUnsetTraceFields = "unsetTraceFields"

// unsetTraceFields names the trace-level search-summary fields reference
// Tempo leaves empty for a query: a `by(...)` / `coalesce()` search whose
// first fetch pass read the backing intrinsic for every candidate span (see
// ast.RootExpr.RegroupLeavesTraceFieldUnset).
type unsetTraceFields struct {
	rootServiceName bool
	rootTraceName   bool
	durationMs      bool
}

// unsetTraceFieldsFor derives the set for a parsed query.
func unsetTraceFieldsFor(expr *traceql.RootExpr) unsetTraceFields {
	return unsetTraceFields{
		rootServiceName: expr.RegroupLeavesTraceFieldUnset(traceql.IntrinsicTraceRootService),
		rootTraceName:   expr.RegroupLeavesTraceFieldUnset(traceql.IntrinsicTraceRootSpan),
		durationMs:      expr.RegroupLeavesTraceFieldUnset(traceql.IntrinsicTraceDuration),
	}
}

// ApplyUnsetTraceFields clears, on every summary, the trace-level fields
// meta marks as unset — the last shaping step of a search, after the root
// lookup has patched the summaries. A cleared rootServiceName reads as
// rootSpanNotYetReceivedText, the value reference Tempo reports for it. The
// HTTP /api/search handler and the gRPC Search RPC both call it, so the two
// transports report the same fields. A Meta that carries no set (the
// plan-only /api/search/recent path, stub-querier fixtures) changes nothing.
func ApplyUnsetTraceFields(summaries []TraceSummary, meta engine.Meta) {
	u, _ := meta.Extra[metaKeyUnsetTraceFields].(unsetTraceFields)
	if u == (unsetTraceFields{}) {
		return
	}
	for i := range summaries {
		if u.rootServiceName {
			summaries[i].RootServiceName = rootSpanNotYetReceivedText
		}
		if u.rootTraceName {
			summaries[i].RootTraceName = ""
		}
		if u.durationMs {
			summaries[i].DurationMs = 0
		}
	}
}
