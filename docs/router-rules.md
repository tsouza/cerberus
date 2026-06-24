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

- The catalog file that ships in the source tree —
  [`internal/routerrules/catalog/router_rules.yaml`](../internal/routerrules/catalog/router_rules.yaml)
  — contains **only generic drivers**: the rule structure and the detection
  logic, deployment-independent, with **zero baked-in numeric thresholds**.
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
   walks the embedded catalog's YAML tree and fails the build if any
   digit-bearing scalar appears in a param or condition value position. (The
   only numbers allowed in the file are `apiVersion` / `catalogVersion` and each
   rule's `since` provenance counter — structural metadata, not thresholds.)

There is **no data-derived fallback constant anywhere**. Even the percentile
*fraction* is a named `config`-kind parameter, so the number lives in the
deployment's config, never in the repo. A missing required config parameter is a
hard error naming the key — never a silent default.

## Catalog grammar

Two top-level sections, plus two version fields:

```yaml
apiVersion: routerrules.cerberus/v1   # schema-shape contract; bumped only on a breaking grammar change
catalogVersion: 1                     # content revision; bumped additively as rules grow
params: [ ... ]                       # the named-parameter registry
rules:  [ ... ]                       # the generic detectors
```

### params — name + how to resolve, never the value

Each param declares a `name` and a resolver `kind` (a closed set). The number it
resolves to comes from the deployment at runtime.

| kind                 | resolves to             | how                                                                                                                                                                                 |
| -------------------- | ----------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `config`             | scalar                  | reads a deployment config value by `key`. The **only** place an operator number enters. The CLI also accepts `--param key=value` overrides. A missing required key is a hard error. |
| `corpus_percentile`  | scalar or partition-map | `quantileExact(${fraction})(<column>)` over an optional `scope`, optionally partitioned. The fraction is **itself** a param ref (`percentile: { ref: <param> }`), never a number.   |
| `corpus_agg`         | scalar or partition-map | a simple aggregate (`agg:` one of `max`, `avg`, `min`, `stddevPop`) of a column.                                                                                                    |
| `corpus_count_ratio` | scalar                  | `countIf(<numerator_scope>) / countIf(<denominator_scope>)` over the population.                                                                                                    |

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

  - name: memory_high_watermark
    kind: corpus_percentile
    column: memory_usage
    percentile: { ref: watermark_pctile }   # fraction is a param ref, never a literal
    partition_by: [language]                 # one watermark per language
    scope: { route: A, exit_status: ok }     # aggregate over the HEALTHY population only
```

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
        - { col: memory_usage, op: gte, param: memory_high_watermark }  # a param comparison
    evidence:
      report: [count, "max(memory_usage)"]            # closed aggregate vocabulary
    finding: "route A class {shape_id}/{language} at/above {memory_high_watermark}; ..."
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
`{memory_high_watermark}`.

## The shipped rule set

The initial catalog ships seven generic detectors, each pairing an observed cost
with the recorded route:

| id                                 | severity | what it flags                                                                                             |
| ---------------------------------- | -------- | --------------------------------------------------------------------------------------------------------- |
| `oom_on_route_a`                   | critical | route-A OOMs (route B exists to avoid them; unconditional, no threshold).                                 |
| `route_a_memory_near_cap`          | high     | route-A queries at/above the per-language memory watermark — the leading indicator before a crash.        |
| `route_a_high_fanout_should_shard` | medium   | route-A queries with fan-out in the range the deployment normally shards.                                 |
| `route_a_timeout_should_shard`     | high     | route-A timeouts (time-slicing bounds per-shard wall-clock).                                              |
| `route_a_hit_sample_budget`        | high     | route-A queries that hit the sample budget (sharding keeps each shard under budget).                      |
| `route_b_overshard_low_fanout`     | medium   | route-B queries that paid k-shard overhead below the fan-out floor while finishing fast (route-B regret). |
| `route_a_slow_hot_shape`           | medium   | high-frequency slow route-A shapes — the highest aggregate payoff to re-route.                            |

A wrong-rejection rule (flagging `exit_status=rejected`) is deliberately
**excluded**: judging it needs a rejection-parity oracle the corpus alone lacks,
and shipping it would force an inline tolerance number. Excluding it is the
right call rather than inventing a magic number.

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

All ClickHouse SQL is composed via the typed `internal/chsql` Frag API
(`Call` / `Parametric` / `Eq` / `Gt` / `And` / `In` / `BareIdent` / …) — no raw
SQL strings.

## Adding a rule

Adding a detector is a declarative edit — **no Go change**:

1. If it needs a new threshold, add a `params` entry naming it and its resolver
   kind. If the number is operator-set, make it a `config` leaf; if it's learned
   from the corpus, make it a `corpus_percentile` / `corpus_agg` /
   `corpus_count_ratio`.
2. Append a `rules` entry with the condition, `group_by`, `min_support`,
   `evidence`, and `finding` text. Reference columns and `${param}`s only —
   never a number.
3. Bump `catalogVersion` and set the rule's `since` to the new version.
4. `just route-rules --validate-only` confirms the catalog still holds the
   invariant; the guard test confirms no number slipped in.
