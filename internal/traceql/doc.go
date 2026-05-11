// Package traceql is the TraceQL head: it imports grafana/tempo/pkg/traceql,
// keeps a thin high-level IR wrapping the parser AST, and lowers expressions
// into the shared internal/chplan IR.
//
// Stubbed in v0.1. Implementation follows the PromQL slice pattern from PR5.
package traceql
