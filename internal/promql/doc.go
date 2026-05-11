// Package promql is the PromQL head: it imports prometheus/promql/parser,
// keeps a thin high-level IR wrapping the parser AST, and lowers expressions
// into the shared internal/chplan IR.
//
// The vertical slice (parse → lower → emit) lands in seed PR5.
package promql
