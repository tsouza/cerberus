# SQL builder evaluation

**Status:** decision landed; `internal/chsql.Builder` ships as the canonical SQL emission surface.
**Decision:** **(b) Build `internal/chsql.Builder` from scratch.**

## Decision summary

The CLAUDE.md hard rule forbids `fmt.Sprintf` (and any string concatenation) for ClickHouse SQL generation. The evaluation weighed three paths:

- **(a)** Adopt [`huandu/go-sqlbuilder`](https://github.com/huandu/go-sqlbuilder) wrapped with a cerberus extension layer.
- **(b)** Build a custom `internal/chsql.Builder` tailored to chplan IR.
- **(c)** Defer the migration entirely.

The honest reading of cerberus's state at the time of the decision:

- The **security argument** for the migration is weak: every dynamic value already rides through `?` placeholders; the remaining Sprintf surface (`metadata.go` + one `range_window.go` numeric format) uses schema-config identifiers and Go-side floats, not user strings.
- The **architectural argument** is strong: optimizer rules (PREWHERE promotion, sort-key reordering, materialised-view substitution, late materialisation) need to compose SQL fragments programmatically, and the Sprintf + `strings.Builder` mixture can't model that cleanly.
- The **existing chsql emitter is already a custom builder in miniature** — it owns a `strings.Builder` + `[]any` placeholder slice, dispatches per chplan node, handles backtick quoting via `writeIdent`, and renders parameterised aggregates already. The work was to **make that builder a named, public API**, not to rip-and-replace it with a third-party library.

## Decision matrix

| Axis                         | `huandu/go-sqlbuilder` + ext.                | Custom `internal/chsql.Builder`                  | Notes                                                                                                                                                                      |
| ---------------------------- | -------------------------------------------- | ------------------------------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| CH idiom coverage out-of-box | partial — needs ~30–40% custom on top        | full — custom IS the layer                       | Both need MapAt / MapKeys / Lambda / ParamAgg / PREWHERE / Now64 / Array idioms.                                                                                           |
| Upstream maintenance         | shared with broader Go community             | ours alone                                       | If go-sqlbuilder stalls, the fork cost ≈ "build custom from scratch but on top of someone else's design".                                                                  |
| Onboarding (new contributor) | go-sqlbuilder docs + cerberus extension docs | cerberus-only docs                               | Net learning surface is larger for the third-party path.                                                                                                                   |
| API match to chplan IR       | impedance layer needed                       | natural — builder mirrors chplan node shapes 1:1 | The existing emitter already maps chplan.Scan → emitScan, chplan.Filter → emitFilter, etc. Custom builder makes these symmetric; third-party builder requires translation. |
| Code volume                  | smaller core, larger glue                    | larger core, smaller glue                        | Net LoC is similar; glue maintenance burden is disproportionate.                                                                                                           |
| Security guarantees          | type system encodes (their schema)           | we encode them (our schema)                      | Equivalent if we're disciplined. The audit surface is smaller for the custom path because it's all in `internal/chsql/`.                                                   |
| Dependency exposure          | +1 direct dep, small transitive              | zero new deps                                    | Cerberus already manages a `replace` directive for memberlist; minimising new dep surface is a stated project value.                                                       |

The pivotal axis is **API match to chplan IR**. The existing emitter is structured as "for each chplan node type, emit its SQL shape" — exactly the API a custom builder should expose. Wrapping a generic library adds an impedance layer on top of the same ~30–40% CH-extension layer cerberus would write either way.

## Why not (a)

- The wrapping cost is **more** than building tailored, because the wrapping layer has to bridge chplan IR (the source of truth) to go-sqlbuilder's API (a generic SQL DSL).
- The ~30–40% CH-extension layer ships either way; the third-party path keeps it as bolt-ons rather than first-class.
- Cerberus minimises dependency surface as a stated project value (the `memberlist` replace directive is a working example of why).
- The current emitter is already a custom builder in miniature — formalising it is the lower-effort path.

## Why not (c)

- Optimizer rules need fragment composition. Deferring would force later RCs to either grow Sprintf-driven for the new emit code (compounding the migration debt) or defer the optimizer rules themselves.
- The CLAUDE.md hard rule already states "new emitter code must go through" the builder — without the scaffolding landing, every new emit PR either violates the rule or postpones the new emit code.

## Outcome

Path **(b)** is implemented; see [`docs/roadmap.md` § RC6](roadmap.md#rc6--type-safe-sql-via-custom-internalchsqlbuilder) for the milestone breakdown. The scaffolding landed first (`internal/chsql/builder.go`); the existing emitter was ported file-by-file behind the typed surface; the remaining loki + tempo helpers were swept into the typed API; a lint gate guards against regressions (later retired once the typed API became the only public emission surface); CLAUDE.md promoted "no raw SQL strings" to a top-level non-negotiable.

Signoff: @tsouza.
