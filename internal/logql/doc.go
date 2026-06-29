// Package logql is the LogQL head: it parses queries with the in-house
// clean-room parser in internal/logql/lsyntax, keeps a thin high-level IR
// wrapping the parser AST, and lowers expressions into the shared
// internal/chplan IR. Runtime evaluation helpers (line/label filter
// execution, templating, pattern/drain extraction) are cerberus's own
// implementations in internal/api/loki, internal/drain, and
// internal/logql/logpattern — no AGPL grafana/loki code is linked.
package logql
