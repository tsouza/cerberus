# chsql callsite rulebook

The "no raw SQL strings" hard rule (CLAUDE.md § Hard rules) has two forbidden patterns and one rulebook for distinguishing them from legitimate operator-token writes.

This document was originally a file-by-file audit triggered by the R6.2 / R6.3 ports shipping `prefix.WriteSQL("SELECT ")` etc. After R6.4–R6.7 ported every grandfathered file, the audit shrunk to the rulebook below. The bucket-2 (grandfathered) surface is **empty** as of RC6 R6.7; the lint enforcement described under R6.9 is the going-forward gate.

## Three buckets

- **Bucket 1 — Cosplay (forbidden, fail the lint gate).** `Builder.WriteSQL(" SELECT ")` / `WriteSQL(" FROM ")` / `WriteSQL(" WHERE ")` / `WriteSQL(" GROUP BY ")` / `WriteSQL(" ORDER BY ")` / `WriteSQL(" LIMIT ")` / `WriteSQL(" PREWHERE ")` / `WriteSQL(" JOIN ")` / `WriteSQL(" UNION ")` / `WriteSQL(" HAVING ")` etc. used to compose structural clause keywords. That's `fmt.Sprintf` cosplay — it bypasses `QueryBuilder`'s arg-lifecycle and nesting guarantees. Use the typed slots instead: `.Select(...)`, `.From(...)`, `.Where(...)`, `.GroupBy(...)`, `.OrderBy(...)`, `.Limit(...)`, `.Prewhere(...)`, `.Join(kind, src, on)`, `.WithRecursive(name, anchor, recursive)`, plus the `chsql.As(expr, alias)` / `QueryBuilder.SelectAs(expr, alias)` helpers for aliased projections.

- **Bucket 2 — Grandfathered (empty post-R6.7).** Pre-R6.1 emitter code that used `strings.Builder` / `fmt.Sprintf` / direct `e.b.WriteString` for SQL keywords. Originally: `emit_expr.go` (retired R6.4), `range_window.go` + `emit_node.go::emitOrderBy` (retired R6.5), `vector_join.go` + `structural_join.go` (retired R6.6), `internal/api/prom/metadata.go` (retired R6.7). **All four files now flow through `QueryBuilder`.** Any new pre-builder pattern in these files is a regression, not a grandfathered case.

- **Bucket 3 — Legitimate token writes (allowed).** `WriteSQL(" = ")` / `WriteSQL(" AS ")` / `WriteSQL(", ")` / `WriteSQL(" > ")` etc. used inside `Frag` callbacks for operators or glue that has no typed helper. Acceptable per CLAUDE.md (the rule forbids *clause keywords*, not operator tokens). Representative callsites: `internal/api/loki/index_stats.go` `timeBoundFrag` (op + glue), `internal/api/loki/index_volume.go` `aliased` (`" AS "`), `Builder.Expr`'s binary-operator glue inside `chsql/builder.go`.

## Detection greps

These greps are what the R6.9 lint gate (in-flight; lands under `cmd/check-sql/` wired into `just check-sql`) automates. Until the gate ships, run them manually before merging emitter changes:

```sh
# Cosplay (must be empty outside QueryBuilder.writeInto):
grep -RnE 'WriteSQL\(" *(SELECT|FROM|WHERE|GROUP BY|ORDER BY|LIMIT|PREWHERE|HAVING|UNION|JOIN |LEFT JOIN|INNER JOIN)' \
  internal/chsql/ internal/api/ cmd/ harness/

# Direct buffer writes for keywords (R6.5/R6.6/R6.7-style; must be empty):
grep -RnE 'WriteString\([^)]*(SELECT|FROM|WHERE|GROUP BY|ORDER BY|LIMIT|PREWHERE)' \
  internal/chsql/ internal/api/

# Sprintf-on-SQL (must be empty in SQL-emitting packages):
grep -RnE 'fmt\.Sprintf\([^)]*(SELECT|FROM|WHERE|INSERT|GROUP BY|ORDER BY)' \
  internal/chsql/ internal/api/
```

The `QueryBuilder.writeInto` implementation in `internal/chsql/builder.go` is the **only** legitimate site where those clause keywords appear as string literals — it's the implementation of the typed slots, not a caller of them. The lint gate exempts it explicitly.

## Pointers

- `internal/chsql/builder.go` — the public `Builder` + `QueryBuilder` API.
- `internal/chsql/builder_test.go` — canonical examples of `Frag` callbacks composing operator tokens (bucket-3 pattern).
- `docs/roadmap.md` § RC6 — R6.4–R6.7 ports (executed) + R6.8 (in flight) + R6.9 lint gate + R6.10 docs.
- `docs/sql-builder-evaluation.md` — the R6.0 build-vs-buy decision.
