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

## R6.12 closer — Raw + Concat retired

R6.12 (executed) retired the public `chsql.Raw(sql string) Frag` escape hatch and the unconstrained `chsql.Concat(parts ...Frag) Frag` joiner. They were the last public surface that accepted opaque SQL bytes from a caller; both are gone. The replacements:

- **`chsql.BareIdent(name string)`** — emits `name` verbatim with no backtick quoting. Narrow trust contract: caller guarantees `name` matches `[a-zA-Z_][a-zA-Z0-9_]*`. Used for lambda parameter / synthetic-alias references that the CH parser must read as a bare identifier (`mapFilter((k, v) -> k IN (?), col)` — `k` is not a column).
- **`chsql.InlineLit(v any)`** — emits an int / int64 / float64 / string literal inline (no `?` binding) with CH-safe quoting. For values that are part of the query *shape* (array literals, default sentinels, lambda predicates). Prefer `Lit` (`?`-bound) when the value is user / plan data.
- **`chsql.Array(elems ...Frag)`** — CH array literal `[e0, e1, …]`. Empty list renders as `[]`.
- **`chsql.Subscript(container, key Frag)`** — CH map / array subscript `<container>[<key>]`. Typed Frag form of `Builder.MapAt` for the general case where container and key are arbitrary expressions.
- **`chsql.If(cond, then, else Frag)`** — CH `if(<cond>, <then>, <else>)` with structural arity (three fixed slots).
- **`chsql.Lambda1(param string, body Frag)`** — single-parameter lambda `<p> -> <body>` (no parens around the parameter). Multi-parameter lambdas use `Builder.Lambda` directly.
- **`chsql.Subquery(s Subqueryable)`** — wraps a `Subqueryable` (`*QueryBuilder` or `PreRenderedSQL`) in parens and splices its args at the Frag's position. Used for QueryBuilder-into-QueryBuilder composition and as the documented interop with the legacy string-typed `chsql.Emit`.
- **`chsql.PreRenderedSQL{SQL, Args}`** — adapter holding an already-rendered (sql, args) pair so legacy `chsql.Emit` output can flow through `Subquery` without raw-string composition. A future R6.x milestone will port `chsql.Emit` to return a `*QueryBuilder` and retire this type.

The package-private `verbatim(sql string) Frag` (in `builder.go`) is the in-package successor to `Raw` for synthetic emitter-controlled tokens that don't fit a typed constructor — local CTE / alias names pinned by golden fixtures, qualifier-prefixed references like `c._depth` / `t.<col>` that the recursive CTE walks, and bare references like `anchor_ts` / `ts` inside the range-window emitter's arrayFilter / WHERE clauses. External packages can't reach it; in-package callers use it sparingly.

The only string-typed inputs to the chsql public surface now are:

- identifier names: `Ident` / `Col` / `Qual` (backtick-quoted) and `BareIdent` (unquoted, with the documented trust contract);
- function names: the `name` arg of `Call` / `Parametric`;
- type names: the `typ` arg of `Cast`;
- inline literals via `InlineLit` (validated by type-switch);
- opaque SQL via `PreRenderedSQL.SQL` (documented one-shot escape for legacy-emitter interop).

## Pointers

- `internal/chsql/builder.go` — the public `Builder` + `QueryBuilder` API, plus the R6.12.f-introduced `BareIdent` / `InlineLit` / `Array` / `Subscript` / `If` / `Lambda1` / `Subquery` / `PreRenderedSQL` constructors.
- `internal/chsql/builder_test.go` — canonical examples of `Frag` callbacks composing operator tokens (bucket-3 pattern) and the new typed constructors.
- `docs/roadmap.md` § RC6 — R6.4–R6.7 ports (executed) + R6.9 lint gate + R6.10 docs + R6.12 Raw/Concat retirement.
- `docs/sql-builder-evaluation.md` — the R6.0 build-vs-buy decision.
