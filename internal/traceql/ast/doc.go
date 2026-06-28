// Package ast defines cerberus's in-house TraceQL abstract syntax tree.
//
// It is a clean-room reimplementation written from the published TraceQL
// grammar and language semantics. The exported type, field, enum, and
// method surface mirrors the shape that the TraceQL head's lowering code
// (internal/traceql) consumes, so the AST produced by cerberus's own
// parser is a drop-in replacement for the reference parser's AST as far
// as lowering is concerned.
//
// The package deliberately keeps only the structure needed to *describe*
// a parsed query — there is no span-execution machinery here. cerberus
// lowers the AST into the shared chplan IR and emits ClickHouse SQL; it
// never evaluates a query against in-memory spans, so the execution-only
// fields and methods of the reference AST have no counterpart here.
//
// Internal representation choices (e.g. how a Static stores its value)
// are cerberus's own and intentionally differ from upstream; only the
// exported contract is held stable.
package ast
