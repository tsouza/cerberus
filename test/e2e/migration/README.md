# Layer-14 migration scenarios

Each migration user-story in [`docs/migration-testing.md`](../../../docs/migration-testing.md)
is one executable Gherkin scenario driving the shipped `cerberus migrate` CLI.
The feature files **are** the manifest: metadata rides on tags — `@MIG-nn`
binds the story, `@tier0`/`@tier1`/`@tier2` the tier, `@archetype:<name>` the
archetype fixtures a scenario reads — so there is no second registry to keep in
step with them.

## Tree

| Path                    | What lives there                                                                    |
| ----------------------- | ----------------------------------------------------------------------------------- |
| `features/`             | one `.feature` per story, named `<MIG-id>.feature`, carrying the story as narrative |
| `steps/`                | the godog step definitions — the assertion library, untagged so it is linted        |
| `lib/`                  | repository-root discovery, binary build, offline process runs, golden comparison    |
| `tiers/tier0-offline/`  | the `migration`-tagged runner: the offline suite over committed fixtures            |
| `cmd/scenarios/`        | the enumerator projecting the features into the coverage ratchet's JSON             |
| `archetypes/`           | one directory per operator archetype: its `rules/`, its `dashboards/`, its goldens  |
| `expected/`             | goldens the offline scenarios assert against, byte for byte                         |

## Running it

```sh
just migration-tier0        # the offline suite
just migration-scenarios    # the enumerator's JSON
just migration-golden       # regenerate the goldens (refused under CI)
```

`CERBERUS_BIN` hands the runner a prebuilt binary instead of compiling one.
`MIGRATION_TAGS` narrows the run to a single story, e.g. `@MIG-10 && @tier0`.

## What a scenario may assert

A `Then` reads the artifact a command emitted as a typed value and asserts on
its fields. "The command exited zero" and "a file was produced" are not
assertions; a scenario making only those claims asserts nothing. Every scenario
also asserts a positive cardinality — a corpus with queries, a render with
statements — so an empty fixture cannot produce a green run.

The arithmetic lives in Go and the relation is named in prose, which is why no
step text carries an operator or a number. The one place a number is legitimate
feature data is a `Scenario Outline` `Examples` table, where it is the varying
parameter; even there the Tier-0 scenarios pass case *names*, and the values
those names select live in the step definitions as named constants.

An unimplemented step is not a pending step: the suite runs under godog's
`Strict` option, so an undefined, pending or ambiguous step fails the run.

## Golden derivations

A golden regenerated from a first run asserts only that the code still does
what it did. Every golden here states what it is expected to contain, and the
statement is checked against the bytes by hand before the golden is committed.

- `expected/schema/default.sql` — the DDL `cerberus migrate schema` renders
  with no `CERBERUS_*` override set. It declares the `default` database, the
  five OTel metrics tables (gauge, sum, histogram, exponential histogram,
  summary), the logs table, the traces table, the trace-id timestamp index
  table and its materialised view, plus the idempotent
  `ADD PROJECTION IF NOT EXISTS` statements the metrics tables carry. No
  statement carries a TTL clause, because retention is unset by default and
  cerberus never invents one.
- `expected/schema/metrics-ttl-override.sql` — the same render with
  `CERBERUS_SCHEMA_TTL_METRICS` set. It differs from the default golden in
  exactly five places: one `TTL toDateTime(TimeUnix) + toIntervalDay(…)`
  clause on each of the five metrics tables. The logs and traces tables are
  byte-identical to the default render, because a per-signal metrics override
  must not reach another signal.

## Archetypes

An archetype is one shape of Prometheus estate an operator arrives with. Each
directory holds the inputs (`rules/`, `dashboards/`) and the goldens the
scenarios tagged with it assert against. The fixtures are synthetic and are
shaped so that every branch a scenario claims to check is actually taken by at
least one archetype in that scenario's tag set — a corpus that never drops an
input, never rejects a query and never records a consumed series would satisfy
the per-entry checks while exercising none of them.

Fixture paths are passed to the CLI **relative to the repository root**, which
is where the harness runs it, so the provenance strings in every golden are
repository-relative and portable between a developer's checkout and a runner.

| Archetype               | Corpus                                  | What it is there to exercise                                                       |
| ----------------------- | --------------------------------------- | ---------------------------------------------------------------------------------- |
| `kube-prometheus-stack` | 10 PromQL                               | colon-named recording rules, a rule-to-rule chain, and a `__name__` regex panel    |
| `prometheus-thanos`     | 6 PromQL                                | the longest ranges in the corpus set, which drive the retention runway             |
| `mimir-cortex`          | 7 PromQL                                | per-tenant recording rules read by both an alert and a dashboard                   |
| `victoriametrics`       | 7 PromQL                                | a MetricsQL-tainted corpus: five queries cerberus has no equivalent for            |
| `already-otel`          | 5 PromQL                                | dotted OTLP metric names, reachable only through an explicit `__name__` matcher    |
| `saas-repatriation`     | 5 PromQL, 2 dropped                     | SaaS-dialect exprs, a foreign datasource, an empty target, a copy-pasted rule      |
| `three-signal`          | 4 PromQL, 1 LogQL, 1 TraceQL, 1 dropped | all three heads in one dashboard, plus a query pinned by an `@` modifier           |
| `regulated-airgapped`   | 7 PromQL                                | a clean estate: nothing unsupported, nothing dropped, every recorded series orphan |

### Derivations

- **`corpus.json`** (all eight) — one entry per rule and per dashboard target,
  sorted by (source, expr). `saas-repatriation` is the one whose entry count is
  not simply "rules + targets": its rule file carries the same alert twice, so
  the two identical entries collapse to one, and two dashboard targets (a
  Datadog datasource, an empty expr) are dropped-with-reason rather than
  harvested — 3 rule blocks + 5 targets, minus 1 duplicate and 2 drops, is 5
  queries and 2 skips.
- **`explain.txt`** (all eight) — one block per corpus query, in the corpus's
  own order. A query cerberus can lower carries its emitted SQL and the tables
  it reads; one it cannot carries `UNSUPPORTED:` and the parser's message. The
  SQL is parameterised and its time bounds come from a fixed dry-run instant,
  so the report is byte-stable across runs and machines.
- **`kube-prometheus-stack/expected/rulegraph.json`** — 4 recorded series.
  `node:node_cpu_utilisation:avg1m` is read by the `NodeCPUSaturation` alert
  and by the `cluster:node_cpu:ratio` recording rule (2 consumers);
  `cluster:node_cpu:ratio` and `node:node_memory_utilisation:ratio` are each
  read by one dashboard panel; the network rate rule is read by nothing and is
  the single orphan. The `{__name__=~"node_.*_bytes_total"}` panel cannot be
  reduced to concrete names, so it is a counted skip rather than a consumer of
  nothing — under-linking would leave a needed series wrongly marked orphan.
- **`victoriametrics/expected/classify.json`** — 5 of 7 queries unsupported:
  `keep_metric_names`, `rollup()`, `range_avg()`, `label_set()` and the
  `default` operator are MetricsQL, not PromQL. The two survivors are ordinary
  `sum(rate(...))`.
- **`saas-repatriation/expected/classify.json`** — 3 of 5 unsupported: the
  Datadog monitor expression (`avg:system.cpu.user{*}.as_rate()`) and the two
  SaaS dashboard functions (`week_before`, `anomalies`). The harvester's 2
  drops are carried through unchanged.
- **`prometheus-thanos/expected/lookback.json`** — reach per query:
  `[30d]` availability rule → 30d; `[7d] offset 7d` burn-rate rule → 14d (the
  offset adds to the range); `avg_over_time(x[1h:5m])[14d:1h]` → 14d1h (the
  inner window is re-evaluated at the outer window's oldest anchor, so the
  ranges add); `predict_linear(x[6h], …)` → 6h; `rate(x[5m])` → 5m; the alert
  reading a recorded series → 0s. The longest is therefore 30d, which the
  `metrics-ttl-override` retention (90d) covers.
- **`three-signal/expected/lookback.json`** — longest 5m. Its value is the
  accounting, not the number: the LogQL and TraceQL panels are counted as
  outside the PromQL walk, and the `@ end()` panel is counted as pinned to the
  request window rather than folded in as reaching back no distance.
- **`victoriametrics/expected/lookback.json`** — longest 5m, with the five
  MetricsQL queries counted as unparseable. Together with the two above, the
  three lists a lookback can put an entry in are all exercised.
- **`gate.json`** (all eight) — every Tier-0 fold runs without parity
  evidence, which is a required stage, so every archetype refuses. What differs
  is *what else* objects: `regulated-airgapped` is the only one where parity is
  the sole blocker (nothing unsupported, nothing dropped, every recorded series
  orphan), which is what makes the MIG-26 assertion sharp. The six clean-corpus
  archetypes additionally block on consumed recorded series that must stay
  materialised; `victoriametrics` and `saas-repatriation` additionally block on
  their unsupported queries and on inputs the rule graph could not read.
