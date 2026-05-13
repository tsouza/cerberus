# chsql callsite audit

Repo-wide inventory of every SQL-building callsite in cerberus. Categorises
each one as **cosplay** (uses `Builder.WriteSQL` for a clause keyword
where `SelectBuilder` already has a typed slot), **grandfathered**
(pre-R6.1 emitter code retained until its RC6 port milestone), or
**legitimate token write** (operator-token `WriteSQL` inside a `Frag`
that no typed helper covers).

This audit was triggered by the maintainer catching the R6.2 (PR #138)
and R6.3 (PR #140) ports shipping `prefix.WriteSQL("SELECT ")` /
`WriteSQL(" FROM ")` / `WriteSQL(" WHERE ")` — that pattern is
`fmt.Sprintf`-on-SQL with an extra step and defeats the whole point of
the typed `chsql.SelectBuilder`. This PR fixes every bucket-1 entry.

Detection greps (see CLAUDE.md hard rule "no raw SQL strings"):

```sh
grep -RnE 'WriteSQL\(" *(SELECT|FROM|WHERE|GROUP BY|ORDER BY|LIMIT|PREWHERE|HAVING|UNION|JOIN |LEFT JOIN|INNER JOIN)' internal/ cmd/ harness/
grep -RnE 'WriteSQL\(" AND "\)' internal/ cmd/ harness/
grep -RnE 'fmt\.Sprintf\([^)]*(SELECT|FROM|WHERE|INSERT|GROUP BY|ORDER BY)' internal/ cmd/ harness/
grep -RnE 'WriteString\([^)]*(SELECT|FROM|WHERE|GROUP BY|ORDER BY|LIMIT|PREWHERE)' internal/ cmd/ harness/
```

## Bucket 1 — Cosplay (FIXED in this PR)

`Builder.WriteSQL(" SELECT ")` etc. used for structural clause keywords.
Each callsite below moved to `chsql.SelectBuilder` slots.

- `internal/chsql/emit_node.go:26` — `b.WriteSQL("SELECT ")` in
  `emitScan` → `NewSelect().Select(...).From(Col(table))`.
- `internal/chsql/emit_node.go:37` — `b.WriteSQL(" FROM ")` in
  `emitScan` → `.From(Col(s.Table))`.
- `internal/chsql/emit_node.go:46` — `prefix.WriteSQL("SELECT * FROM ")`
  in `emitFilter` → `NewSelect().From(e.subqueryFrag(...))`.
- `internal/chsql/emit_node.go:56` — `suffix.WriteSQL(" WHERE ")` in
  `emitFilter` → `.Where(predicateFrag)`.
- `internal/chsql/emit_node.go:67` — `prefix.WriteSQL("SELECT ")` in
  `emitProject` → `.Select(projectionFrags...)`.
- `internal/chsql/emit_node.go:84` — `prefix.WriteSQL(" FROM ")` in
  `emitProject` → `.From(e.subqueryFrag(...))`.
- `internal/chsql/emit_node.go:92` — `prefix.WriteSQL("SELECT ")` in
  `emitAggregate` → `.Select(groupByFrags..., aggFuncFrags...)`.
- `internal/chsql/emit_node.go:119` — `prefix.WriteSQL(" FROM ")` in
  `emitAggregate` → `.From(e.subqueryFrag(...))`.
- `internal/chsql/emit_node.go:129` — `suffix.WriteSQL(" GROUP BY ")` in
  `emitAggregate` → `.GroupBy(groupByFrags...)`.
- `internal/chsql/emit_node.go:183` — `prefix.WriteSQL("SELECT * FROM ")`
  in `emitLimit` → `NewSelect().From(e.subqueryFrag(...))`.
- `internal/chsql/emit_node.go:192` — `suffix.WriteSQL(" LIMIT ")` in
  `emitLimit` → `.Limit(int(l.Count))`.

The aliased projection shape (`<expr> AS <alias>`) was inline
`prefix.WriteSQL(" AS ")` + `prefix.Ident(alias)`. This PR adds a typed
`chsql.As(expr Frag, alias string) Frag` helper plus a
`SelectBuilder.SelectAs(expr Frag, alias string)` slot so the projection
list never composes the `AS` keyword by hand again. The legacy
`internal/api/loki/index_volume.go` `aliased` local helper stays put
(out of scope; the package-private form predates the new public
`chsql.As`).

`builder_test.go` also constructed `b.WriteSQL("max(")` / `WriteSQL(")")`
patterns to stage args. Those are operator-glue around the
`Builder.Arg` / `Builder.Ident` calls (no clause keyword, no
`SelectBuilder`-replaceable shape), so they remain bucket-3
legitimate-token writes for now. They are a thin layer that can move
to typed helpers as RC6 R6.4–R6.8 expand the helper set.

## Bucket 2 — Grandfathered

Pre-R6.1 emitter code that still uses `strings.Builder` /
`fmt.Sprintf` / direct `e.b.WriteString` for SQL keywords. CLAUDE.md's
"no raw SQL strings" rule grandfathers these until the listed RC6
milestone ports them.

- `internal/chsql/emit_expr.go` — `strings.Builder.WriteString` +
  `Sprintf` for the expression tree (Binary, FuncCall, MapAccess,
  MapWithoutKeys, LineContent, FieldAccess). Retired by **R6.4**.
- `internal/chsql/range_window.go` — `e.b.WriteString("SELECT ")`
  chains + `fmt.Fprintf(&e.b, ...)` for the windowed-array idiom.
  Retired by **R6.5**.
- `internal/chsql/vector_join.go` — `header.WriteSQL("SELECT ")` /
  `" FROM "` / `" GROUP BY "` plus inline `" AND "` glue inside
  `writeVectorMatchPredicate`. Retired by **R6.6**.
- `internal/chsql/structural_join.go` — `e.b.WriteString("SELECT R.* FROM ")`
  plus `fmt.Fprintf` for trace-id / parent-span join. Retired by **R6.6**.
- `internal/chsql/emit_node.go::emitOrderBy` —
  `e.b.WriteString("SELECT * FROM ")` / `" ORDER BY "`. Pre-R6 Tempo
  `/api/search/recent` work; folds in with the **R6.5** RangeWindow port.
- `internal/api/prom/metadata.go` — `fmt.Sprintf` for label-keys /
  label-values / UNION-ALL builders (`unionLabelNamesSQL`,
  `unionMetricNamesSQL`, `unionLabelValuesSQL`, `labelKeysForMatcher`,
  `labelValuesForMatcher`). Retired by **R6.7**.

Detection greps for the bucket-2 surface:

```sh
grep -nE 'e\.b\.WriteString|fmt\.Fprintf\(&e\.b|fmt\.Sprintf' \
  internal/chsql/range_window.go \
  internal/chsql/vector_join.go \
  internal/chsql/structural_join.go \
  internal/chsql/emit_expr.go \
  internal/api/prom/metadata.go
```

None of these are touched by this PR — porting them lives in the
upcoming R6.x milestones (see `docs/roadmap.md` § RC6 R6.4–R6.8). If a
PR re-touches one of these files for an unrelated reason it should leave
the SQL composition shape alone; the port is a single mechanical commit
per milestone.

## Bucket 3 — Legitimate token writes

`WriteSQL(" = ")` / `WriteSQL(" AS ")` / `WriteSQL(", ")` /
`WriteSQL(" > ")` etc. used inside `Frag` callbacks for operators or
glue that has no typed helper. Acceptable per CLAUDE.md (the rule
forbids *clause keywords*, not operator tokens). Listed for
completeness so future audit greps can distinguish them.

Representative callsites (not exhaustive — every `Frag` in the
loki/index handlers and every operator emit in `Builder.Expr` produces
similar tokens):

- `internal/api/loki/index_stats.go` — `timeBoundFrag` writes `" "` +
  op + `" "`; `aggFrag` / `bytesAggFrag` write `(` / `)` glue.
- `internal/api/loki/index_volume.go` — `aliased` writes `" AS "`;
  `volumeGroupFrag` writes `mapFilter(...)` glue.
- `internal/chsql/builder_test.go` — `b.WriteSQL(" = ")` /
  `b.WriteSQL(" > ")` inside `Where` callbacks; these are the canonical
  documented pattern in the `SelectBuilder` doc comments.

Inside `Builder.Expr` the binary-operator glue (`b.sb.WriteByte(' ')` +
`b.sb.WriteString(string(bx.Op))` + `b.sb.WriteByte(' ')`) is similarly
operator-token, not clause-keyword.

## Audit tally (post-fix)

- **Cosplay fixed**: 11 callsites across `internal/chsql/emit_node.go`.
- **Grandfathered**: 5 files (`emit_expr.go`, `range_window.go`,
  `vector_join.go`, `structural_join.go`, `metadata.go`) + the
  pre-R6 `emitOrderBy` block, retired by R6.4–R6.7.
- **Legitimate token writes**: ~30+ callsites across `internal/api/loki/`
  and `internal/chsql/builder_test.go`; all are operator-glue inside
  Frags or test bodies, none replicate a `SelectBuilder` slot.

## Final-greps post-fix (must be empty)

```sh
grep -RnE 'WriteSQL\(" *(SELECT|FROM|WHERE|GROUP BY|ORDER BY|LIMIT|PREWHERE)' \
  internal/chsql/emit_node.go \
  internal/chsql/builder_test.go \
  internal/api/loki/ internal/api/prom/ internal/api/tempo/
```

If this grep ever resurfaces a hit outside `SelectBuilder.writeInto`
(the implementation of the typed slots), it is a regression — the typed
slots exist precisely so callers never compose those keywords by hand.
