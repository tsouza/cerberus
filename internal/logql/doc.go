// Package logql is the LogQL head: it parses queries with the in-house
// clean-room parser in internal/logql/lsyntax, keeps a thin high-level IR
// wrapping the parser AST, and lowers expressions into the shared
// internal/chplan IR. Runtime evaluation helpers (line/label filter
// execution, templating, pattern/drain extraction) are still consumed
// from grafana/loki/pkg/logql/log.
package logql
