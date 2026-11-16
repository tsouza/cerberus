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

// FinishTraceSummaries is the last shaping step of a search, after the
// root lookup has patched the summaries. It clears, on every summary, the
// trace-level fields meta marks as unset, then reports every empty
// rootServiceName as rootSpanNotYetReceivedText, as reference Tempo's search
// combiner does: a regrouped search's cleared field and a trace with no root
// span read the same. The HTTP search handlers and the gRPC Search RPC all
// call it, so the transports report the same fields. A Meta that carries no
// set (the plan-only /api/search/recent path, stub-querier fixtures) clears
// nothing.
func FinishTraceSummaries(summaries []TraceSummary, meta engine.Meta) {
	u, _ := meta.Extra[metaKeyUnsetTraceFields].(unsetTraceFields)
	for i := range summaries {
		if u.rootServiceName {
			summaries[i].RootServiceName = ""
		}
		if u.rootTraceName {
			summaries[i].RootTraceName = ""
		}
		if u.durationMs {
			summaries[i].DurationMs = 0
		}
		if summaries[i].RootServiceName == "" {
			summaries[i].RootServiceName = rootSpanNotYetReceivedText
		}
	}
}
