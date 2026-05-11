// Package logql is the LogQL head: it imports grafana/loki/pkg/logql/syntax,
// keeps a thin high-level IR wrapping the parser AST, and lowers expressions
// into the shared internal/chplan IR.
//
// Stubbed in v0.1. Implementation follows the PromQL slice pattern from PR5.
package logql
