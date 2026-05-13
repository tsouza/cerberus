# chsql callsite rulebook

> **Historical document (as of RC6 R6.11e).** The cosplay/grandfathered
> categorisations below are now of historical interest only:
> `Builder.WriteSQL` was renamed to the unexported `writeSQL` in R6.11e,
> so external packages cannot raw-write clause keywords by construction.
> The Sprintf scanner (`cmd/check-sql/`, R6.9) was retired in the same
> PR. Reviewer discipline + the typed surface (`QueryBuilder` slots +
> typed Frag constructors) are the going-forward enforcement.
>
> The bucket-3 (legitimate operator-token) discussion is still live —
> it describes the in-package Frag layer, which remains the right shape
> for operator glue.

The "no raw SQL strings" hard rule (CLAUDE.md § Hard rules) has one positive direction (typed slots + Frags) and one rulebook for the operator-token Frag layer.

## Three buckets (historical categorisation)

- ~~**Bucket 1 — Cosplay (forbidden, fail the lint gate).** `Builder.WriteSQL(" SELECT ")` / `WriteSQL(" FROM ")` / `WriteSQL(" WHERE ")` / `WriteSQL(" GROUP BY ")` / `WriteSQL(" ORDER BY ")` / `WriteSQL(" LIMIT ")` / `WriteSQL(" PREWHERE ")` / `WriteSQL(" JOIN ")` / `WriteSQL(" UNION ")` / `WriteSQL(" HAVING ")` etc. used to compose structural clause keywords.~~ **Retired in R6.11e:** the `WriteSQL` method is now unexported (`writeSQL`); external packages cannot construct this shape at all. Use the typed slots instead: `.Select(...)`, `.From(...)`, `.Where(...)`, `.GroupBy(...)`, `.OrderBy(...)`, `.Limit(...)`, `.Prewhere(...)`, `.Join(kind, src, on)`, `.WithRecursive(name, anchor, recursive)`, plus the `chsql.As(expr, alias)` / `QueryBuilder.SelectAs(expr, alias)` helpers for aliased projections.

- ~~**Bucket 2 — Grandfathered (empty post-R6.7).**~~ Closed out by R6.7; the file list is preserved here for archaeology: `emit_expr.go` (retired R6.4), `range_window.go` + `emit_node.go::emitOrderBy` (retired R6.5), `vector_join.go` + `structural_join.go` (retired R6.6), `internal/api/prom/metadata.go` (retired R6.7). All four files now flow through `QueryBuilder`.

- **Bucket 3 — Legitimate token writes (in-package only, allowed).** `writeSQL(" = ")` / `writeSQL(" AS ")` / `writeSQL(", ")` / `writeSQL(" > ")` etc. used inside `Frag` callbacks for operators or glue that has no typed helper. Acceptable per CLAUDE.md (the rule forbids *clause keywords*, not operator tokens). Since `writeSQL` is now unexported, this pattern is confined to `internal/chsql/*.go` — external packages compose operator tokens via the typed `Frag` constructors (`Eq` / `And` / `Or` / `Paren` / `Cast` / `In` / `Like` / `Add` / etc.) instead.

## Pointers

- `internal/chsql/builder.go` — the public `Builder` + `QueryBuilder` API.
- `internal/chsql/builder_test.go` — canonical examples of `Frag` callbacks composing operator tokens (bucket-3 pattern).
- `docs/roadmap.md` § RC6 — R6.4–R6.7 ports (executed) + R6.8 (in flight) + R6.9 lint gate + R6.10 docs.
- `docs/sql-builder-evaluation.md` — the R6.0 build-vs-buy decision.
