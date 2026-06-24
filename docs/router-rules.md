# Router-rules catalog

`internal/routerrules` + `cmd/route-rules` is an **offline analysis** tool. It
mines the router corpus — the `cerberus_router_corpus` table, or its per-pod
JSONL fallback — and emits **findings**: shape classes where the recorded route
A/B decision is paying an observable cost the corpus shows the other route would
avoid. It changes no routing. It is a report an operator runs.

- **Route A** is a single ClickHouse query.
- **Route B** is a time-slice sharded execution.

The corpus records, per query, the A/B decision and the observed cost
(`memory_usage`, `query_duration_ms`, `read_rows`, `exit_status`, …). The
catalog analyzes that table to surface where routing should improve.

## The one invariant: generic drivers in the repo, numbers from the deployment

This is the architectural point of the feature, and a reviewer must be able to
confirm it by reading the shipped files.

- The catalog that ships in the source tree is split one file per rule under
  [`internal/routerrules/catalog/`](../internal/routerrules/catalog): a base
  [`catalog.yaml`](../internal/routerrules/catalog/catalog.yaml) carrying the
  schema-shape contract (`apiVersion` / `catalogVersion`) and the shared
  `params:` registry, plus one
  [`rules/<rule_id>.yaml`](../internal/routerrules/catalog/rules) per rule
  (filename = rule id). Every file contains **only generic drivers**: the rule
  structure and the detection logic, deployment-independent, with **zero
  baked-in numeric thresholds**. The loader embeds the base plus every
  `rules/*.yaml`, merges them in a deterministic order (base first, then rule
  files sorted by filename), and rejects a rule id declared by two files.
- Every dynamic **parameter** (a threshold, a watermark, a cap, a
  percentile cutoff) is **per-deployment** and resolved **at runtime from the
  database** (the deployment's own corpus aggregates) and/or the deployment's
  config. It is **never** a literal in the YAML or in Go.
- Example generic rule: *"flag `route=A` queries whose `memory_usage` exceeds
  `{memory_high_watermark}`"*, where `{memory_high_watermark}` is a **named
  parameter** resolved per-deployment (a percentile of *this* deployment's own
  `memory_usage` distribution). The file carries the **name** and the
  **resolver kind**, never the number.

This mirrors the self-tuning generic-loop / local-constants split: the shipped
catalog generalizes across all deployments; the numbers come from each
deployment's own data.

### How the invariant is enforced (three independent ways)

1. **Structural — the number is unrepresentable.** The condition AST
   (`condition.go`) has node types `And/Or/Not`, `EnumCmp` (a category
   comparison), and `ParamCmp` (a comparison against a *named* parameter). There
   is **no number-literal node**. A bare number in a comparison operand position
   cannot be parsed into the model, so it is impossible by construction — not
   merely rejected.
2. **Schema validation at load** (`validate.go`) additionally rejects: dangling
   `${param}` references, unknown resolver kinds, unknown columns (checked
   against the canonical corpus column allow-list in `columns.go`), duplicate
   rule ids, cyclic parameter dependencies, an `apiVersion` mismatch, and a
   numeric scalar smuggled into any enum or scope slot.
3. **A CI guard test**
   ([`catalog/router_rules_test.go`](../internal/routerrules/catalog/router_rules_test.go))
   walks the YAML tree of **every** embedded catalog file (the base plus each
   `rules/<rule_id>.yaml`) and fails the build if any digit-bearing scalar
   appears in a param or condition value position. (The only numbers allowed are
   `apiVersion` / `catalogVersion` and each rule's `since` provenance counter —
   structural metadata, not thresholds.)

There is **no data-derived fallback constant anywhere**. Even the percentile
*fraction* is a named `config`-kind parameter, so the number lives in the
deployment's config, never in the repo. A missing required config parameter is a
hard error naming the key — never a silent default.

## Catalog grammar

Two top-level sections, plus two version fields. The shipped catalog is split
across files — the base `catalog.yaml` carries the version fields and `params:`,
and each `rules/<rule_id>.yaml` carries one entry under `rules:` — but the
merged in-memory shape is exactly:

```yaml
apiVersion: routerrules.cerberus/v1   # schema-shape contract; bumped only on a breaking grammar change
catalogVersion: 1                     # content revision; bumped additively as rules grow
params: [ ... ]                       # the named-parameter registry (base catalog.yaml)
rules:  [ ... ]                       # the generic detectors (one per rules/<rule_id>.yaml)
```

A single-file catalog with all four sections is still a valid override passed via
`--catalog <path>`; the split is how the *embedded default* ships.

### params — name + how to resolve, never the value

Each param declares a `name` and a resolver `kind` (a closed set). The number it
resolves to comes from the deployment at runtime.

| kind                 | resolves to             | how                                                                                                                                                                                                                                                                                                                        |
| -------------------- | ----------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `config`             | scalar                  | reads a deployment config value by `key`. The **only** place an operator number enters. The CLI also accepts `--param key=value` overrides. A missing required key is a hard error.                                                                                                                                        |
| `config_scaled`      | scalar                  | the **product** of two already-declared params: `ref` (a fraction) × `scale_by` (a magnitude). Lets a rule gate on a tunable fraction of an operator-configured absolute (e.g. a near-cap threshold = `0.8 × query.max_memory_bytes`) while the catalog stays number-free — both operands enter through deployment config. |
| `corpus_percentile`  | scalar or partition-map | `quantileExact(${fraction})(<column>)` over an optional `scope`, optionally partitioned. The fraction is **itself** a param ref (`percentile: { ref: <param> }`), never a number.                                                                                                                                          |
| `corpus_agg`         | scalar or partition-map | a simple aggregate (`agg:` one of `max`, `avg`, `min`, `stddevPop`) of a column.                                                                                                                                                                                                                                           |
| `corpus_count_ratio` | scalar                  | `countIf(<numerator_scope>) / countIf(<denominator_scope>)` over the population.                                                                                                                                                                                                                                           |

`scope`, `numerator_scope`, and `denominator_scope` are enum-equality filters
(e.g. `{ route: A, exit_status: ok }`). They may reference only the three enum
columns (`route`, `exit_status`, `language`) and are validated to carry a valid
category token — never a number.

`partition_by: [<column>]` makes a corpus param **partition-keyed**: one value
per partition (e.g. one memory watermark per `language`). The partition column
must be a `group_by` column of every rule that uses the param, so each
per-partition result can be anchored to a group key.

```yaml
params:
  - name: watermark_pctile          # the fraction is a config leaf — the number lives in the deployment
    kind: config
    key: router_rules.watermark_percentile

  - name: memory_hard_cap                   # the configured query memory cap
    kind: config
    key: query.max_memory_bytes

  - name: memory_near_cap_fraction          # fraction of the cap to alarm at (e.g. 0.8)
    kind: config
    key: router_rules.memory_near_cap_fraction

  - name: memory_near_cap                    # = memory_near_cap_fraction × memory_hard_cap
    kind: config_scaled
    ref: memory_near_cap_fraction            # the fraction
    scale_by: memory_hard_cap                # the configured absolute cap
```

#### Empty populations are no signal, not zero

A `corpus_percentile` / `corpus_agg` watermark whose scoped sub-population is
**empty** (zero rows match the `scope`) resolves to **no signal**, not to a
watermark of `0`. The distinction matters for **fire-gate** params — those a rule
references in a `>=`/`>`/`<` condition. If an empty fire-gate normalized to `0`, a
`fanout >= 0` gate would fire on *every* row (the inverse of safe). So a scalar
fire-gate over an empty population is marked no-signal, and the evaluator
**skips** any rule that gates on it, reporting the skip with a structured reason
(rule id + the offending param) rather than firing or silently dropping it. The
canonical case is `fanout_route_b_floor` (the route-B fanout p95) on a deployment
that has never routed to route B: the floor cannot be learned, so
`route_a_high_fanout_should_shard` and `route_b_overshard_low_fanout` are skipped,
not fired on the whole route-A population.

A **message-only** param — one referenced solely in a finding template, never in
a condition, such as `cerberus_reject_ratio` — keeps resolving an empty
population to `0`: there, `0` is the correct "no rejections observed" value. The
fire-gate-vs-message distinction is structural (referenced in a condition vs only
in a message), so no extra annotation is needed in the catalog.

### rules — generic detectors

A rule pairs an observed-cost signal with the route the corpus recorded, so each
finding states the wrong-route overlap.

```yaml
rules:
  - id: route_a_memory_near_cap
    severity: high                       # low | medium | high | critical
    since: 1                             # catalog revision the rule first appeared in
    status: active                       # active | experimental | deprecated
    applies_to: [promql, logql, traceql] # language scope; omit = all
    group_by: [shape_id, language]       # the class the finding is reported per
    min_support: { ref: min_class_support }  # drop classes thinner than this (a param)
    condition:
      all:
        - { col: route,        op: eq,  enum: A }     # a category comparison
        - { col: exit_status,  op: eq,  enum: ok }
        - { col: memory_usage, op: gte, param: memory_near_cap }  # a param comparison
    evidence:
      report: [count, "max(memory_usage)"]            # closed aggregate vocabulary
    finding: "route A class {shape_id}/{language} peaks at/above {memory_near_cap} — near the configured memory cap; ..."
    action: lower_route_b_threshold
```

Condition grammar:

- Combinators: `all:` (AND), `any:` (OR), `not:`.
- A leaf comparison has `col`, `op`, and **exactly one** operand:
  - `enum:` — a category or list of categories, allowed only against the enum
    columns (`route`, `exit_status`, `language`). Ops: `eq`, `in`.
  - `param:` — a reference to a declared numeric parameter, the **only** path to
    a numeric comparison. Ops: `eq`, `gt`, `gte`, `lt`, `lte`.
- There is no `value:` key and no numeric literal production: a number cannot be
  written into a condition.

`{column}` and `{param}` placeholders in the `finding` string are substituted at
runtime with the class's group-key values and the resolved numbers, so the
operator sees concrete values (`… at/above 4.2 GiB`) even though the file said
`{memory_near_cap}`.

## The shipped rule set

The catalog ships twelve generic detectors (`catalogVersion: 2`). The first
seven (`since: 1`) pair an observed cost with the recorded route; the next five
(`since: 2`) generalize beyond the route-A/route-B framing and attribute each
finding by the **solver decision reason** (see below).

### catalogVersion 1 — observed-cost / recorded-route pairs

| id                                 | severity | what it flags                                                                                                                                                                                                                                        |
| ---------------------------------- | -------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `oom_on_route_a`                   | critical | route-A OOMs (route B exists to avoid them; unconditional, no threshold).                                                                                                                                                                            |
| `route_a_memory_near_cap`          | high     | route-A queries whose peak memory is at/above a fraction (`memory_near_cap_fraction`, default 0.8) of the configured cap (`query.max_memory_bytes`) — the leading indicator before an OOM. Gated on proximity to the actual cap, not the corpus p95. |
| `route_a_high_fanout_should_shard` | medium   | route-A queries with fan-out in the range the deployment normally shards.                                                                                                                                                                            |
| `route_a_timeout_should_shard`     | high     | route-A timeouts (time-slicing bounds per-shard wall-clock).                                                                                                                                                                                         |
| `route_a_hit_sample_budget`        | high     | route-A queries that hit the sample budget (sharding keeps each shard under budget).                                                                                                                                                                 |
| `route_b_overshard_low_fanout`     | medium   | route-B queries that paid k-shard overhead below the fan-out floor while finishing fast (route-B regret).                                                                                                                                            |
| `route_a_slow_hot_shape`           | medium   | high-frequency route-A shapes that are slow **relative to their own language's duration norm** (the corpus p95) — a self-relative tail signal, not an absolute SLA breach; the highest aggregate payoff to re-route (grouped by decision reason).    |

### catalogVersion 2 — reason-attributed and shape-geometry detectors

| id                                 | severity     | what it flags                                                                                                      |
| ---------------------------------- | ------------ | ------------------------------------------------------------------------------------------------------------------ |
| `failure_cluster_by_reason`        | critical     | hard failures (OOM/timeout) on **any** route, grouped by `decision_reason` so the operator picks the right lever.  |
| `route_b_still_failing`            | high         | route-B classes that **still** OOM/timeout — more sharding will not help; surfaces `max(k_shards)` as evidence.    |
| `cerberus_side_rejection_pressure` | high         | `sample_budget` / `breaker` / `rejected` clusters — decided before CH dispatch; neither route addresses them.      |
| `heavy_shape_geometry_failing`     | high         | failing classes whose `cumulative_d` sits in their own per-language tail (the geometry the solver uses to route).  |
| `read_amplification_hot_shape`     | experimental | healthy scans reading above their own per-shape `read_rows` tail — a missing-PREWHERE / late-materialisation hint. |

The original analysis dropped a naive *wrong-rejection* rule (flagging
`exit_status=rejected` against a parity oracle) because judging it needs a
rejection-parity oracle the corpus alone lacks. `cerberus_side_rejection_pressure`
ships the buildable form instead: it fires on the cerberus-side rejection
*cluster* (gated by `min_support`) and surfaces the deployment-wide rejection
share as message context (`{cerberus_reject_ratio}`, a `corpus_count_ratio`
scalar) — context, never a gate, so no inline tolerance number is needed.

### decision_reason is an attribution column, never a condition operand

The catalogVersion-2 failure rules group by `decision_reason` — the
shadow-header value the solver records to explain each non-route decision
(`routed`, `below-threshold`, `not-sliceable`, `instant`, `high-D`, `now64`,
`grid-mismatch`, `incommensurate`, `scalar-heavy`; see
[`internal/solver/decision.go`](../internal/solver/decision.go)). It is a
**grouping / attribution** column: it tells the operator *which solver path*
produced the failure, so they can pick the right lever (shard vs cap vs reject
vs rewrite) — the catalog never encodes that branch in a number. It is **never**
a condition operand (the grammar classifies it `ColumnGroup`, rejected in a
leaf). Because the rules only group by it, they are immune to token drift; the
finding message is the only place the token surfaces, so it always shows
whatever token the corpus actually carries. A meta-test
([`test/regression/router_corpus_seed_test.go`](../test/regression/router_corpus_seed_test.go))
pins the seed corpus's `decision_reason` tokens to the solver's `Reason*`
constants so the fixtures never drift from production.

## Running it

```sh
# Against the per-pod JSONL fallback (no ClickHouse needed):
just route-rules --source jsonl --corpus-path /var/lib/cerberus/router-corpus

# Against the live corpus table (uses the deployment's own CERBERUS_CH_* env):
just route-rules --source chtable --since 720h

# Validate the catalog only (the invariant gate; CI-runnable, resolves nothing):
just route-rules --validate-only

# Supply a config-kind parameter inline:
just route-rules --source jsonl --corpus-path ./corpus \
  --param router_rules.watermark_percentile=0.95 \
  --param router_rules.min_rows_per_class=50
```

Flags:

| flag                             | default   | meaning                                            |
| -------------------------------- | --------- | -------------------------------------------------- |
| `--catalog PATH`                 | embedded  | use a catalog file instead of the shipped one      |
| `--source` (`chtable` / `jsonl`) | `jsonl`   | corpus backend                                     |
| `--corpus-path PATH`             | —         | JSONL file or directory (jsonl source)             |
| `--since DURATION`               | unbounded | `event_time` lookback window (e.g. `720h`)         |
| `--param KEY=VALUE`              | —         | repeatable; config-kind parameter override         |
| `--format` (`table` / `json`)    | `table`   | output format                                      |
| `--validate-only`                | off       | load + validate the catalog, resolve nothing, exit |
| `--experimental`                 | off       | also evaluate `status: experimental` rules         |

The CLI exits non-zero only on an operational or validation error — **never on
findings** (findings are the expected output). The conservative ClickHouse scan
settings (single-threaded, deprioritised, read-only, row/byte/time-capped) make
the `chtable` source safe to run against a production cluster.

## Coupling to the corpus

`routerrules` depends on three stable contracts only, and imports neither
`optcorpus` nor `chclient`:

1. the table name `cerberus_router_corpus` (re-declared in `columns.go`);
2. the column contract (the full column list + types in `columns.go`, which is
   the allow-list the validator checks every `col:` against — a drifted column
   fails at load, not query time);
3. a narrow `CHConn interface { Query(ctx, sql, args...) (driver.Rows, error) }`,
   re-declared locally (a `*chclient.Client.Conn()` satisfies it from `cmd/`).

The evaluator never branches on backend: a rule's condition lowers to a typed
`chsql` Frag (the CH path) **or** an in-Go row matcher (the JSONL path) from the
*same* AST, so the two backends produce identical findings. A `chdb`-tagged
parity test seeds one fixture as JSONL and as a CH table and asserts the findings
match.

### Why parity-on-chDB is not enough: the strict-scan integration lane

The `chdb`-tagged parity test executes the CH path's SQL, but against chDB
(libchdb behind chdb-go's Parquet `database/sql` driver), which **leniently
coerces** result-column types into whatever Go destination a `Scan` supplies — a
`UInt64` `count()` or an integer-typed `quantileExact` lands happily in a
`*float64`. Production cerberus does not use chDB: the offline analysis talks to
a real ClickHouse over the native protocol via `clickhouse-go/v2`, whose `Scan`
is **strict** — a column type that doesn't match the destination is a hard error
(`converting … to … is unsupported`, code 47) the operator sees as a 502. This
is exactly the class of bug `#1064` was: the corpus SELECTs returned integer CH
types but the cursor scanned them into `*float64`/`*int64`; every chDB parity
test stayed green while the read path 502'd against real ClickHouse. The fix
wraps every integer-returning aggregate in `toFloat64(…)`/`toInt64(…)`
([`source_ch.go`](../internal/routerrules/source_ch.go)) so the wire type
matches the scan destination.

Because the chDB parity lane is structurally blind to that class, and because
compose-smoke / e2e drive the data plane (never the offline corpus reconciler),
an `integration`-tagged real-CH lane covers both corpus seams end-to-end against
a real `clickhouse/clickhouse-server` (testcontainers-go, the same harness the
strict-scan gateway differential uses):
[`realch_integration_test.go`](../internal/routerrules/realch_integration_test.go)
runs the **WRITE** path (`optcorpus.CHTableSink` — real CREATE-TABLE DDL plus the
columnar `Enum8` batch, then reads the rows back to assert the `route` /
`exit_status` enums round-tripped) and the **READ** path
(`routerrules.chCorpusSource` — every strict-scanned aggregate the catalog
resolves), plus the upstream `system.query_log` reconciler read
(`optcorpus.CHQueryLogSource`). It is wired into the informational
[`strict-scan.yml`](../.github/workflows/strict-scan.yml) lane via
`just router-corpus-integration` and runs on PR + push + nightly.

### The effectiveness fixture

`testdata/effectiveness.jsonl` is a fabricated corpus (no production-identifying
values) calibrated to a realistic query mix: a PromQL-dominant, range-heavy
healthy majority on route A, plus an injected failure surface a healthy
deployment lacks — OOM / timeout / sample-budget / breaker / rejected clusters, a
route-B failing cluster with non-zero `k_shards`, a route-B overshard-regret
class, a high-fanout route-A class, and a high-geometry sub-population. It proves
the catalog is **effective**, not just well-formed: default-lane tests assert that
every rule fires on its planted pathology with the expected class set and support,
that the hard-failure rules stay quiet on the healthy majority (a real
false-positive check), and that the resolved watermarks are non-degenerate. A
maintainer-reviewable golden pins exactly which shapes get flagged and with which
action; the `effectiveness_no_route_b.jsonl` variant exercises the
empty-population no-signal skip. Both backends run the fixture under the parity
test, so the JSONL and CH paths must agree finding-for-finding.

### The detection-fidelity benchmark

The effectiveness fixture answers "does every rule fire on its planted
pathology?" as a binary. The benchmark answers a graded version of the same
question — **how consistently does each rule detect its planted pathologies,
measured as precision / recall / F1 against a synthetic labeled corpus, and how
does that detection degrade as the deployment is tuned off-nominal?**

This is a *detection-fidelity / regression* gate, not a real-world
effectiveness measurement. The corpus labels share provenance with the rules'
p95-watermark thresholds — the same model decides both what counts as a planted
pathology and what each rule fires on — so a perfect score proves the rules
detect what the model says they should and pins that against regressions. It
does **not** prove the rules catch real production incidents; that would require
labels grounded in real incident outcomes, which this corpus does not have.

- `benchmark.go` generates a fabricated, seed-deterministic corpus (no
  production-identifying values) shaped like a real deployment — a healthy,
  PromQL-dominant, range-heavy majority on route A plus a planted failure
  surface — where every class carries a ground-truth label: the rule ids that
  *should* fire on it, and a pathology severity (`severe` far past the watermark,
  `marginal` just past it). The in-memory source folds these rows through the
  same matcher + `quantileExact` path the JSONL/CH backends use.
- `metrics.go` scores the catalog against the labels at the CLASS granularity
  (TP = labeled class the rule fired on; FP = unlabeled class it fired on; FN =
  labeled class it stayed silent on) and reports per-rule precision / recall / F1
  plus a micro-averaged overall. At the nominal p95 / min-support-5 operating
  point the catalog scores **precision 1.000 / recall 1.000 / F1 1.000** over the
  12 rules — every planted pathology detected, zero false positives on the healthy
  majority. (A perfect score here is a self-consistency result, by construction —
  see the provenance caveat above.)
- `sweep.go` turns that single point into a sensitivity surface: it sweeps
  `watermark_percentile`, `min_rows_per_class`, and pathology prevalence around
  the nominal and reports how recall/precision (and severe-vs-marginal recall)
  hold up. The shape is the design's stated tradeoff made measurable: loosening
  the watermark or the support floor floods false positives (precision falls
  toward ~0.33 at p50); over-tightening sheds marginal recall first; a rare
  deployment (low prevalence) starves its *marginal* pathologies below the support
  floor and loses their detection, while *severe* pathologies are detected at
  every operating point. A guard test fails if any swept axis is inert (its whole
  grid collapses to one identical score), so the sweep can't silently grow a dead
  knob.
- The regression-floor tests pin numeric recall/precision/F1 minimums at the
  nominal point and across the operationally sane tuning range (the only place
  numbers belong — the shipped catalog stays number-free; the floors live in
  test code). The overall recall/precision floors hold at nominal prevalence;
  severe-recall is floored at 1.0 across the whole grid including off-nominal
  prevalence. Adversarial corpora (monochrome-healthy, distribution-shifted
  across seeds) and a multi-rule-interaction test guard the edges.
- `route-rules benchmark` runs the whole thing from the CLI: it scores the
  embedded catalog over the generated corpus and prints the metric table, no
  corpus file required (`--seed`, `--min-support`, and `--param` tune it). The
  `chdb`-tagged parity test re-runs the benchmark through the CH SQL backend and
  asserts identical findings and identical metrics, so the numbers are not an
  artefact of the in-memory source.

All ClickHouse SQL is composed via the typed `internal/chsql` Frag API
(`Call` / `Parametric` / `Eq` / `Gt` / `And` / `In` / `BareIdent` / …) — no raw
SQL strings.

## Adding a rule

Adding a detector is a declarative edit — **no Go change**:

1. If it needs a new threshold, add a `params` entry in the base
   `catalog/catalog.yaml` naming it and its resolver kind. If the number is
   operator-set, make it a `config` leaf; if it's learned from the corpus, make
   it a `corpus_percentile` / `corpus_agg` / `corpus_count_ratio`.
2. Drop a new `catalog/rules/<rule_id>.yaml` file (filename = the rule id) with a
   single entry under `rules:` carrying the condition, `group_by`, `min_support`,
   `evidence`, and `finding` text. Reference columns and `${param}`s only —
   never a number. The loader merges it automatically (rule files are sorted by
   filename for a deterministic order; a duplicate id across two files is a hard
   load error).
3. Bump `catalogVersion` in the base and set the rule's `since` to the new
   version.
4. `just route-rules --validate-only` confirms the catalog still holds the
   invariant; the guard test confirms no number slipped in.

## Why generic drivers + dynamic params (and not a tuned ruleset)

A router-rules catalog that shipped concrete thresholds — "OOM above 4 GiB",
"shard above 8× fan-out" — would encode **one** deployment's cost surface and
quietly mis-fire on every other. The catalog instead ships only rule
*structure* and *named parameter references*; every threshold, watermark, cap,
and percentile cutoff is a named parameter resolved per-deployment at runtime:

- **`config`** — a number the operator sets in their own deployment config
  (e.g. `router_rules.watermark_percentile`, or reuses an existing knob like
  `query.max_memory_bytes`). This is the only place an operator number enters.
- **`config_scaled`** — the product of two config params (`ref` × `scale_by`),
  e.g. `memory_near_cap = memory_near_cap_fraction × query.max_memory_bytes`.
  Lets a rule gate on a tunable fraction of the deployment's **actual**
  configured cap (rather than the corpus p95, which ~5% of any healthy
  population trivially exceeds), still without a number in the catalog.
- **`corpus_percentile` / `corpus_agg`** — learned from the deployment's own
  corpus (a per-shape `read_rows` tail, a per-language `cumulative_d`
  watermark). Self-relative, so a shape is judged against its own history.
- **`corpus_count_ratio`** — a deployment-wide scalar (`countIf(num scope) /
  countIf(den scope)`) surfaced as message context, never a gate.

The result: a small audit surface (the base `catalog/catalog.yaml` plus one
`catalog/rules/<rule_id>.yaml` per rule) with a
no-numbers invariant enforced three independent ways (a condition AST with no
number-literal node, a load-time validator, and the `TestEmbeddedCatalogHasNoNumbers`
guard). The same rule fires correctly whether a deployment's pain is broad
PromQL aggregations, high-cardinality TraceQL `compare()`, or LogQL line scans —
because the *number* that decides "pathological" comes from that deployment's
own data, not the catalog.

## Limitations / future grammar work

- **No group-aggregate-vs-fleet node.** The grammar cannot express "this
  group's cost share exceeds the fleet's" inside a condition, so Pareto
  cost-share rules are deferred. `corpus_count_ratio` resolves a deployment-wide
  scalar (usable only as message context), not a per-group fraction.
- **No time-windowed param kind.** Params resolve over the whole `--since`
  window; drift/regression rules that compare two windows are deferred.
- **No arithmetic in params.** "0.8 × cap" cannot be expressed in the catalog;
  a deployment that wants a self-relative warn-floor supplies the product as one
  `config` number, or uses a `corpus_percentile` instead. This is why the
  early-warning memory rule stays a `corpus_percentile` of the healthy
  population rather than a hand-computed fraction of the hard cap.

## Academic references

The detector design draws on the database-systems literature on cost-based
optimization, cardinality-estimation error, memory-aware admission control, and
self-driving / continuously-tuned systems:

1. P. G. Selinger, M. M. Astrahan, D. D. Chamberlin, R. A. Lorie, T. G. Price.
   *Access Path Selection in a Relational Database Management System.* ACM
   SIGMOD 1979, pp. 23–34. — cost-based optimization and the divergence between
   predicted and observed cost from stale catalog statistics. (Grounds the
   whole corpus-vs-decision premise: a router decision made on estimated cost is
   audited against realized cost.)
2. V. Leis, A. Gubichev, A. Mirchev, P. Boncz, A. Kemper, T. Neumann. *How Good
   Are Query Optimizers, Really?* PVLDB 9(3), 2015, pp. 204–215. — cardinality
   estimation is the dominant source of optimizer cost error. (Grounds
   `read_rows`/`cumulative_d` as the realized-cardinality signals to mine.)
3. Y. Wu, et al. *Robust Query-Driven Cardinality Estimation under Changing
   Workloads.* PVLDB 16, 2023. — estimator drift under shifting workloads; act
   on outcome-changing errors, recomputed per window. (Grounds the
   `corpus_percentile` watermarks recomputed over the `--since` window.)
4. *LearnedWMP: Workload Memory Prediction Using Distribution of Query
   Templates.* arXiv:2401.12103, 2024; with Microsoft SQL Server memory-grant /
   `RESOURCE_SEMAPHORE` admission-control documentation. — OOM/spill as a
   dominant failure mode cured by admission control + rewrite/cap rather than
   more parallelism. (Grounds `failure_cluster_by_reason`, `route_b_still_failing`,
   `cerberus_side_rejection_pressure`.)
5. A. Pavlo, et al. *Self-Driving Database Management Systems.* CIDR 2017; L. Ma,
   D. Van Aken, et al. *Query-based Workload Forecasting for Self-Driving DBMS.*
   SIGMOD 2018 (OtterTune). — continuous, bidirectional tuning against the live
   workload. (Grounds the per-deployment parameter-resolution model and the
   route-B-regret rule.)
6. *(Boundary, cited for the out-of-scope rationale.)* R. Avnur, J. M.
   Hellerstein. *Eddies: Continuously Adaptive Query Processing.* ACM SIGMOD
   2000. — mid-query, per-tuple reoptimization yields no post-hoc corpus signal;
   the router is a coarse-grained admission decision, so eddy-style adaptivity
   is explicitly out of scope for this catalog.
