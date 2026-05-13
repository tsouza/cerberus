# RC6 R6.0 — SQL builder evaluation

**Status:** draft, awaiting maintainer signoff.
**Decision sought:** which SQL-construction strategy will RC6 adopt?
**Recommendation:** **(b) Build `internal/chsql.Builder` from scratch.** Section 6 below has the rationale.

## 1. Executive summary

The CLAUDE.md hard rule for RC6 forbids `fmt.Sprintf` (and any string concatenation) for ClickHouse SQL generation. This document evaluates how to satisfy that rule: by adopting [`huandu/go-sqlbuilder`](https://github.com/huandu/go-sqlbuilder) wrapped with a cerberus extension layer, by building a custom `internal/chsql.Builder` tailored to the existing chplan IR, or by deferring the migration entirely.

The honest reading of cerberus's current state is:

- The **security argument** for the migration is weak: every dynamic value already rides through `?` placeholders; the remaining Sprintf surface (`metadata.go` + one `range_window.go` numeric format) uses schema-config identifiers and Go-side floats, not user strings.
- The **architectural argument** is strong: RC3's optimizer rules (PREWHERE promotion, sort-key reordering, materialised-view substitution, late materialisation) need to compose SQL fragments programmatically, and the current Sprintf + `strings.Builder` mixture can't model that cleanly.
- The **existing chsql emitter is already a custom builder in miniature** — it owns a `strings.Builder` + `[]any` placeholder slice, dispatches per chplan node, handles backtick quoting via `writeIdent`, and renders parameterised aggregates already. RC6 R6.1+ work is to **make that builder a named, public API**, not to rip-and-replace it with a third-party library.

Recommendation: **(b) Custom builder**. The third-party option carries an impedance layer (mapping chplan IR onto go-sqlbuilder's API) on top of the same ~30–40% CH-specific extension layer we'd write either way. Custom is the lower-effort path because the impedance layer is the layer we delete.

## 2. Background

### CLAUDE.md hard rule

From the project root [CLAUDE.md](../CLAUDE.md):

> No raw SQL strings — refactor lands at RC6. `fmt.Sprintf` (or any string concatenation) used to build a ClickHouse query is forbidden going forward; new emitter code must go through `huandu/go-sqlbuilder` (and the cerberus extension layer wrapping it for CH-specific idioms). The existing `internal/chsql/emit_*.go` and `internal/api/prom/metadata.go` Sprintf-built SQL is grandfathered until the RC6 port (R6.1–R6.10), but no PR may add a new instance of the pattern. RC6 R6.9 wires a lint rule to enforce automatically.

The rule was written assuming option **(a)**. This evaluation re-tests that assumption against actual codebase reality.

### Roadmap context (RC6, R6.0)

The [roadmap](roadmap.md#rc6--type-safe-sql-via-go-sqlbuilder) sets the framing: RC6 reorganises the emitter so RC3's optimizer rules can compose SQL programmatically. RC3 ships **before** RC6 chronologically, so RC3 either pays a Sprintf tax for its new emit code or RC6 jumps the queue. R6.0 has explicit license to recommend reordering.

## 3. Security analysis (current state)

### Inventory: SQL-producing `fmt.Sprintf` / `fmt.Fprintf` callsites

Result of `rg "fmt\.(Sprintf|Fprintf)" internal/` filtered to SQL-producing sites:

| File                                | Line                    | Purpose                                                                                                                          | Risk class |
| ----------------------------------- | ----------------------- | -------------------------------------------------------------------------------------------------------------------------------- | ---------- |
| `internal/api/prom/metadata.go`     | 171                     | `metricMetaSQL` empty-name branch — SELECT … GROUP BY                                                                            | Low        |
| `internal/api/prom/metadata.go`     | 176                     | `metricMetaSQL` named-metric — SELECT … WHERE … =?                                                                               | Low        |
| `internal/api/prom/metadata.go`     | 351                     | `labelKeysForMatcher` — SELECT DISTINCT mapKeys wrapper                                                                          | Medium     |
| `internal/api/prom/metadata.go`     | 368                     | `labelValuesForMatcher` `__name__` branch                                                                                        | Low        |
| `internal/api/prom/metadata.go`     | 372                     | `labelValuesForMatcher` attribute branch — `Attrs[?]`                                                                            | Low        |
| `internal/api/prom/metadata.go`     | 433                     | `unionLabelNamesSQL` — UNION ALL of mapKeys per table                                                                            | Low        |
| `internal/api/prom/metadata.go`     | 445                     | `unionMetricNamesSQL` — UNION ALL of MetricName per table                                                                        | Low        |
| `internal/api/prom/metadata.go`     | 462                     | `unionLabelValuesSQL` — UNION ALL of `Attributes[?]`                                                                             | Low        |
| `internal/chsql/range_window.go`    | 154                     | `rateValueExpr` — embeds a Go-side float                                                                                         | None       |
| `internal/chsql/range_window.go`    | 222, 228, 310, 319, 328 | windowed-array idiom: formats config-derived column names + anchor timestamps + step duration into CH array-function expressions | Low        |
| `internal/chsql/emit_expr.go`       | 97                      | `" %s "` Binary.Op string (typed enum value)                                                                                     | None       |
| `internal/chsql/emit_node.go`       | 145                     | `" LIMIT %d"` Count int                                                                                                          | None       |
| `internal/chsql/structural_join.go` | 53                      | `" AS R ON L.%s = R.%s AND %s"` — config column names + rel expr                                                                 | None       |

Counts: **9 callsites in `metadata.go`** (the hand-built UNION SQL); **8 callsites inside the chsql emitter** writing into the emitter's `strings.Builder` (these are mid-fragment formatting, not full-statement assembly, but the RC6 hard rule covers them too).

Risk classes are defined as:

- **None** — no externally-controlled string enters the format. `rate_value_expr` embeds a `strconv.FormatFloat(rangeSeconds, 'f', -1, 64)` value into a constant ClickHouse expression. Zero injection surface.
- **Low** — only schema-derived identifiers (table names + column names) enter the format. These flow from `internal/schema/` config and are backtick-quoted via `quoteIdentCH`. User-supplied values ride in `?` placeholders. The only realistic attack vector is an operator-controlled `CERBERUS_SCHEMA_OVERRIDES_JSON` env value containing a backtick — which `quoteIdentCH` already escapes by doubling.
- **Medium** — `labelKeysForMatcher` and the implicit "inner SQL" pass-through in `unionLabelValuesSQL` interpolate the inner SQL string (the lowered matcher's SQL) directly into the outer query. The inner SQL itself is produced by the chsql emitter, which only uses `writeIdent` + `?` placeholders, so the injection surface is still bounded. Risk-class is "Medium" only because the *pattern* of pasting an inner SQL string into an outer string is what a builder would replace with a typed subquery composition.

### Vectors **not** closed by `?` placeholders today

After tracing every callsite:

- **Schema-config identifiers** are backtick-quoted. `CERBERUS_SCHEMA_OVERRIDES_JSON` is the only path that lets an operator inject an identifier; backtick-doubling closes that vector.
- **Map keys** (`Attributes['<key>']`) ride as `?` placeholders in the emitter (`emit_expr.go`'s MapAccess handling). The key string flows from chplan IR built by parsers — Prom's `model.LabelName` validates `[a-zA-Z_][a-zA-Z0-9_]*`; Loki's matchers go through `prometheus/prometheus/model/labels.MatchType` validation; Tempo's attribute names come from the parser's tokenizer, which restricts the lexical set. No unfiltered user string reaches a key position.
- **Regex patterns** ride as `?` placeholders in `emitLineContent` (the `match`/`extract` family).
- **Tempo's reflect-driven attribute extraction** (`internal/traceql/select.go`) walks the parsed AST — the names are bound by `traceql.SelectOperation.Attributes`, which the parser already validates.

**Conclusion for §3:** the parameterised-emitter has zero open injection vectors today. Security alone does not justify the migration. This was the roadmap's "preliminary read" and the inventory confirms it.

## 4. Project impact analysis

### Refactor scope

LoC counted from the SQL-emitting surface that the migration must touch:

| Path                                | Lines                   | Notes                                                 |
| ----------------------------------- | ----------------------- | ----------------------------------------------------- |
| `internal/chsql/emit.go`            | ~70                     | Driver: chplan.Node dispatch + emitter struct         |
| `internal/chsql/emit_node.go`       | ~185                    | Scan / Filter / Project / Aggregate / Limit / OrderBy |
| `internal/chsql/emit_expr.go`       | ~120                    | Binary / FuncCall / MapAccess / ColumnRef / literals  |
| `internal/chsql/range_window.go`    | ~430                    | Windowed-array idiom (rate / *_over_time)             |
| `internal/chsql/vector_join.go`     | ~120                    | Binary-op join shape                                  |
| `internal/chsql/structural_join.go` | ~70                     | TraceQL structural join shape                         |
| `internal/api/prom/metadata.go`     | ~510                    | Hand-built UNION SQL for /labels, /label/.../values   |
| `internal/api/loki/handler.go`      | ~30 (deferred metadata) | Deferred handlers tagged for RC6                      |

Net: **~1.5k LoC of emitter code + ~510 LoC of hand-built metadata SQL = ~2k LoC** within the migration footprint.

### Test coverage as safety net

Fixture-first refactors are what cerberus already does. The current safety net:

- **TXTAR fixtures** — 122 spec files across `test/spec/{promql,logql,traceql,chsql,optimizer}/`. Each pins the lowered chplan tree + the emitted SQL string. A behavioural drift trips the golden diff.
- **Go unit tests** — 642 tests across 15 packages. The chsql emit_*.go files have a TXTAR-driven walker (`internal/chsql/emit_test.go`) plus direct unit tests for emitter error paths.
- **E2E HTTP tests** (`-tags=e2e`) — 14 tests run by the `dashboard` CI job against the k3d-deployed stack with a real CH backend. Catches behavioural drift the TXTAR layer can't (real CH parsing + execution).
- **Playwright** — 10 scenarios via the Grafana datasource-proxy path.

Net: a behavioural regression in the refactor would surface as either a TXTAR golden diff (caught at `just check`) or an E2E failure (caught at `just e2e-up && just e2e-run`). The risk surface is bounded.

### Risk surface for the refactor itself

Subtle SQL semantic changes that pass golden updates but trip CH at runtime:

- **OR-chain ordering inside WHERE** — CH evaluates left-to-right; reordering matters for short-circuit. Mitigation: the chplan IR encodes Binary OR with deterministic Left/Right; the builder must preserve that order.
- **PREWHERE placement** — RC3 needs PREWHERE; getting the column index right matters for CH's prefetch optimisation. Mitigation: the builder API should distinguish WHERE from PREWHERE explicitly (not collapse them).
- **Alias quoting** — CH allows unquoted aliases in some positions but not others. Mitigation: always backtick-quote, the current `writeIdent` policy.
- **Lambda syntax** — CH lambdas (`(k, v) -> ...`) are bare, not function-quoted. Mitigation: a `chsql.Lambda(params, body)` helper that emits the bare form.

### Dependency exposure

If we adopt go-sqlbuilder: one new direct dependency, ~5k LoC, plus its transitive surface. go-sqlbuilder itself is dependency-light (only depends on `database/sql`). Risk is bounded.

The wider concern is **dep replacement risk** — cerberus already has a `replace github.com/hashicorp/memberlist => …` directive because Loki/Tempo/dskit's pinned memberlist doesn't propagate. Each new transitively-pulled-in dep raises the marginal risk of hitting a similar conflict.

## 5. Benefit analysis

### 5.1 Security

What new vectors does a typed builder close that `?` placeholders don't?

- **Defense in depth.** A builder makes it structurally harder to introduce a new injection vector — `Sprintf("WHERE %s = ?", userControlledIdent)` is a 30-second mistake; `Builder.Where(Eq(userControlledIdent, ?))` is a type error. The lint rule (R6.9) closes the static path; the builder closes the dynamic path.
- **Audit surface.** All identifier quoting goes through one helper. A single audit can prove the quoting is correct, rather than reading every call site.

Honest assessment: **incremental, not transformative.** The current `?`-placeholder discipline is already strong. The builder is a structural reinforcement of an existing pattern, not a new defence.

### 5.2 Architecture

This is the **primary motivation** per the roadmap, and the inventory confirms it.

- **RC3 optimizer rules need fragment composition.** PREWHERE promotion moves a predicate from WHERE to PREWHERE — that's a tree transformation on the *SQL side*, not the chplan side, because PREWHERE isn't a chplan node. Sort-key-aware predicate reordering shifts predicate order in WHERE. Materialised-view substitution rewrites the FROM clause to point at an MV instead of the base table. None of these compose cleanly when SQL is a flat string; all of them compose cleanly when SQL is a tree of typed fragments.
- **Subquery composition.** The current `emitSubquery(plan)` recursively serialises the child plan into the parent's string buffer. A builder lets us pass `*SelectBuilder` instances around without flattening — useful for late-materialisation rewrites that need to peek inside subqueries.
- **Late materialisation.** The pattern in CH is `SELECT col FROM (SELECT * FROM tbl WHERE pred ORDER BY x LIMIT N)` — the outer SELECT projects only the columns it needs, the inner reads everything. The builder needs to support nested SELECT-FROM-SELECT with column-list pushdown — Sprintf does this badly today.

### 5.3 Maintainability

- **Removes hand-quoting bugs.** Every place that backtick-quotes a CH identifier today calls `quoteIdentCH` (metadata.go) or `writeIdent` (chsql) — two copies of the same logic. The builder centralises this.
- **Removes hand-built UNIONs.** `unionLabelNamesSQL`, `unionMetricNamesSQL`, `unionLabelValuesSQL` are the bulk of the metadata.go Sprintf surface; the builder can express "UNION ALL across N tables" as one helper.
- **Single source of CH dialect knowledge.** Today, CH-specific idioms are scattered: `now64(9)` lives in handler.go, `toIntervalNanosecond` lives in chsql, `Map[?]` access lives in emit_expr.go. The builder concentrates these as named helpers.

### 5.4 Testability

- **Per-fragment introspection.** A builder lets unit tests assert on the WHERE clause alone, or the FROM clause alone, without parsing the emitted string. Today, `internal/chsql/emit_test.go` tests the whole-query output.
- **Builder unit tests.** Each helper (MapAt, Lambda, ParamAgg) gets its own focused unit test that doesn't go through the chplan → SQL path. Faster feedback on helper correctness.

### Benefit summary

| Dimension       | Where the builder helps                                             | Magnitude      |
| --------------- | ------------------------------------------------------------------- | -------------- |
| Security        | Defense in depth + audit surface                                    | Incremental    |
| Architecture    | Enables RC3 optimizer rules (PREWHERE / sort-key / MV / late mat'n) | Transformative |
| Maintainability | Centralises CH-dialect knowledge; removes UNION/quoting duplication | Moderate       |
| Testability     | Per-fragment unit tests                                             | Moderate       |

**Architecture is the load-bearing benefit.** Without it, the migration is hard to justify.

## 6. Build vs buy decision matrix

The roadmap's matrix, refined with concrete cerberus findings from the inventory above:

| Axis                         | `huandu/go-sqlbuilder` + ext.                  | Custom `internal/chsql.Builder`                  | Notes                                                                                                                                                                                                                                        |
| ---------------------------- | ---------------------------------------------- | ------------------------------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| CH idiom coverage out-of-box | partial — needs ~30–40% custom on top          | full — custom IS the layer                       | Both need MapAt / MapKeys / Lambda / ParamAgg / PREWHERE / Now64 / Array idioms. The third-party path keeps these on top of its API; the custom path makes them first-class.                                                                 |
| Upstream maintenance         | shared with broader Go community               | ours alone                                       | go-sqlbuilder is actively maintained but small. If it stalls, we'd fork — the fork cost ≈ "build custom from scratch but on top of someone else's design".                                                                                   |
| Onboarding (new contributor) | go-sqlbuilder docs + cerberus extension docs   | cerberus-only docs                               | Net learning surface is larger for the third-party path.                                                                                                                                                                                     |
| API match to chplan IR       | impedance layer needed                         | natural — builder mirrors chplan node shapes 1:1 | This is the *biggest* asymmetry. The existing emitter already maps chplan.Scan → emitScan, chplan.Filter → emitFilter, etc. Custom builder makes these symmetric (`ScanBuilder`, `FilterBuilder`); third-party builder requires translation. |
| Code volume                  | smaller core, larger glue                      | larger core, smaller glue                        | Net LoC is similar, *but* the glue layer in the third-party path adds maintenance burden disproportionate to its size — every chplan IR change ripples through the glue.                                                                     |
| Security guarantees          | type system encodes (their schema)             | we encode them (our schema)                      | Equivalent if we're disciplined. The audit surface is smaller for the custom path because it's all in `internal/chsql/`.                                                                                                                     |
| Test isolation               | builder tests vendored upstream + ours on glue | all ours, but smaller                            | Custom path has less to test because it has less surface.                                                                                                                                                                                    |
| Dependency exposure          | +1 direct dep, small transitive                | zero new deps                                    | Cerberus already manages a `replace` directive for memberlist; minimising new dep surface is a stated project value.                                                                                                                         |

### Where the decision lands

The pivotal axis is **API match to chplan IR**. cerberus's emitter is already structured as "for each chplan node type, emit its SQL shape". This is exactly the API a custom builder should expose. Wrapping a generic library that doesn't know about chplan nodes adds an impedance layer that has to be written and maintained, *on top of* the ~30–40% CH-extension layer we'd write either way.

The existing emitter is **two design moves away from a custom builder**:

1. Expose the `emitter` struct's methods (Scan, Filter, …) as a public, named `Builder` API.
2. Add the missing helpers (MapAt, Lambda, ParamAgg already exist in flavor; PREWHERE, structured subquery composition are new).

That's a much smaller scope than "vendor go-sqlbuilder, write an adapter from chplan to its API, write the extension layer on top".

## 7. Recommendation

**(b) Build `internal/chsql.Builder` from scratch.**

Specifically: expose and extend the existing chsql emitter as a named public Builder API, rather than vendoring `huandu/go-sqlbuilder`.

### Why not (a) — `huandu/go-sqlbuilder` + extension layer

- The wrapping cost is **more** than building tailored, because the wrapping layer has to bridge chplan IR (the source of truth) to go-sqlbuilder's API (a generic SQL DSL).
- The ~30–40% CH-extension layer ships either way; the third-party path keeps it as bolt-ons rather than first-class.
- Cerberus minimises dependency surface as a stated project value (memberlist replace directive is a working example of why).
- The current emitter is already a custom builder in miniature — formalising it is the lower-effort path.

### Why not (c) — defer

- The security surface is bounded (§3 confirms it) but RC3's optimizer rules need fragment composition, and the optimizer rule work is mid-RC2 (not RC6). Deferring would force RC3 to either grow Sprintf-driven for the new emit code (compounding the migration debt) or defer the optimizer rules themselves.
- The CLAUDE.md hard rule already states "new emitter code must go through" the builder — without R6.1 landing, every RC2/RC3 PR either violates the rule or postpones the new emit code.

### Why (b) — custom

- Lowest impedance against existing chplan IR.
- Smallest dependency footprint.
- Smallest audit surface for security review.
- The migration is "expose what we have + extend" rather than "vendor + adapt + extend".
- Direct path to RC3's optimizer composition needs: builder helpers map directly to optimizer rule rewrites (`Builder.Prewhere`, `Builder.Where`, `Builder.From(<materialised view>)`).

### Implications for R6.1 onward

If signed off, the R6.1+ scope changes as follows:

- **R6.1** rewrites to: "Add `internal/chsql/builder.go` as a public Builder API wrapping the existing emitter mechanics, plus the missing helpers (MapAt, MapKeys, MapFilterExcept, Now64, SubtractNanos, DateTime64Lit, Lambda, ParamAgg, and a `SelectBuilder` that supports `.Prewhere(cond...)`). Unit tests pin each helper's output. **No emitter changes yet** — pure scaffolding."
- **R6.2–R6.10** stay structurally the same (port emit_node.go, port range_window.go, port metadata.go, etc.), but each "port to go-sqlbuilder" reads as "port to chsql.Builder". The fixture-first refactor strategy is unchanged.
- **R6.9 lint rule** scope unchanged: forbid `fmt.Sprintf` in SQL-emitting files via a custom golangci-lint rule (or a `cmd/check-sql/` Go tool wired into `just check`).
- **R6.10 cleanup** unchanged: regenerate all fixtures, run compatibility, document the builder API.

### Open questions for signoff

1. Does the custom path conflict with any external integration plan (e.g., another datasource that would benefit from cerberus exporting its builder)? If yes, (a) becomes more attractive because go-sqlbuilder is a known surface.
2. Is there appetite to revisit if the custom builder grows beyond ~1500 LoC? At that size, the maintenance argument tips toward shared upstream.
3. Should the CLAUDE.md hard rule be amended to remove the go-sqlbuilder reference if (b) is chosen? Yes — see PR plan after signoff.

## 8. Signoff

| Reviewer | Decision  | Date      | Notes     |
| -------- | --------- | --------- | --------- |
| @tsouza  | *pending* | *pending* | *pending* |

Maintainer signoff unblocks R6.1. Until signoff, RC6 is paused at R6.0.
