// Package cerbtrace wires cerberus's pipeline-level OpenTelemetry spans.
//
// Each pipeline stage (parse → lower → optimize → emit → execute) is a
// short-lived span emitted from the package that performs the work, so a
// single PromQL/LogQL/TraceQL query — once otelhttp has put its request
// span on the context (RC4 R4.2) — produces a five-span chain whose
// shape is uniform across the three heads.
//
// Span attribute keys are interned here so the contract is one file
// long. The keys follow the `cerberus.*` namespace for cerberus-owned
// fields; the standard `db.system` / `db.statement` semantic-conventions
// strings are stamped verbatim by the chclient execute span.
package cerbtrace

import (
	"go.opentelemetry.io/otel/attribute"

	"github.com/tsouza/cerberus/internal/chplan"
)

// Span names emitted by the pipeline stages. Stable strings — dashboards
// and trace queries pivot on them.
const (
	SpanParse    = "parse"
	SpanLower    = "lower"
	SpanOptimize = "optimize"
	SpanEmit     = "emit"
	SpanExecute  = "execute"
)

// Attribute keys. Wrapped in `attribute.Key` so callers stay typed.
const (
	// AttrQL is the query language: "promql", "logql", or "traceql".
	AttrQL = attribute.Key("cerberus.ql")
	// AttrQuery is the original query string (truncated to MaxQueryLen).
	AttrQuery = attribute.Key("cerberus.query")
	// AttrPlanNodeCount is the number of chplan nodes in the lowered tree.
	AttrPlanNodeCount = attribute.Key("cerberus.plan_node_count")
	// AttrRulesApplied counts how many optimizer-rule applications
	// produced a tree change. It is a rough proxy for how much rewriting
	// the optimizer actually did.
	AttrRulesApplied = attribute.Key("cerberus.rules_applied")
	// AttrSQLLength is the length in bytes of the emitted ClickHouse SQL.
	AttrSQLLength = attribute.Key("cerberus.sql_length")
)

// MaxQueryLen / MaxStatementLen cap the string attributes so a pathological
// query doesn't bloat every span's wire payload. Truncation preserves the
// prefix and appends an ellipsis marker.
const (
	MaxQueryLen     = 256
	MaxStatementLen = 1024
)

// ellipsis is the UTF-8 marker appended by Truncate to flag that the
// value was cut. "…" is a 3-byte sequence (E2 80 A6), accounted for
// when bounding the output length.
const ellipsis = "…"

// Truncate returns s clipped so that the result is at most maxLen
// bytes. When truncation occurs the suffix "…" is appended to flag
// that the value was cut, and the prefix shrinks to leave room for
// that 3-byte UTF-8 marker. For maxLen ≤ len(ellipsis) the function
// degrades to a plain byte-clip with no ellipsis.
func Truncate(s string, maxLen int) string {
	if maxLen <= 0 || len(s) <= maxLen {
		return s
	}
	if maxLen <= len(ellipsis) {
		return s[:maxLen]
	}
	return s[:maxLen-len(ellipsis)] + ellipsis
}

// CountNodes walks a chplan tree and returns the total node count. Used
// as the `cerberus.plan_node_count` attribute on the lower span.
func CountNodes(n chplan.Node) int {
	if n == nil {
		return 0
	}
	count := 0
	chplan.Walk(n, func(chplan.Node) bool {
		count++
		return true
	})
	return count
}

// ParseAttrs returns the attribute pair stamped on the `parse` span —
// the QL identifier and the (truncated) query string. The API handlers
// use this when opening their parse span.
func ParseAttrs(ql, query string) []attribute.KeyValue {
	return []attribute.KeyValue{
		AttrQL.String(ql),
		AttrQuery.String(Truncate(query, MaxQueryLen)),
	}
}
