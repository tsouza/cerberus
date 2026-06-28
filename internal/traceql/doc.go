// Package traceql is the TraceQL head: it parses queries with the in-house
// clean-room parser in internal/traceql/ast, keeps a thin high-level IR
// wrapping that AST, and lowers expressions into the shared internal/chplan IR.
package traceql
