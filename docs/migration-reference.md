# Migration reference

The precise contract behind [`docs/migration.md`](migration.md): every
subcommand and flag, what each comparator calls "equal", what the report
declines to judge, and exactly what each gate blocks on.

Read the [guide](migration.md) first — this page assumes you know the shape of
the journey and want the detail behind one step of it.

## Commands

`migrate` is a command group of the single `cerberus` binary, with eight
subcommands.

| Command                      | What it does                                                                   | Key flags                                                                                                                                 | Network                                 |
| ---------------------------- | ------------------------------------------------------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------- |
| `cerberus migrate schema`    | Print the `CREATE` statements cerberus expects, from `CERBERUS_*` env          | *(no flags; reads `CERBERUS_*`)*                                                                                                          | offline                                 |
| `cerberus migrate harvest`   | Build a machine-readable PromQL + LogQL + TraceQL corpus from your files       | `--rules`, `--loki-rules`, `--dashboards`, `--out`                                                                                        | offline                                 |
| `cerberus migrate explain`   | Dry-run each corpus query through the read pipeline, print the SQL             | `--corpus` (or `--rules`/`--loki-rules`/`--dashboards`), `--out`                                                                          | offline                                 |
| `cerberus migrate classify`  | Bucket each query as supported / unsupported / risky                           | `--corpus` (or `--rules`/`--loki-rules`/`--dashboards`), `--json`, `--out`                                                                | offline                                 |
| `cerberus migrate rulegraph` | Map recording-rule outputs to the consumers that must stay materialized        | `--rules`, `--loki-rules`, `--corpus`, `--json`, `--out`                                                                                  | offline                                 |
| `cerberus migrate verify`    | Replay the corpus against each head's reference backend and diff (parity gate) | `--corpus`, per-head `--ref*`/`--cerberus*` pairs (below), `--start`, `--end`, `--step`, `--tolerance`, `--json`, `--report`, `--out`     | live (two backends per configured head) |
| `cerberus migrate inventory` | Probe live sources for the cardinality that drives OOM risk                    | `--source`, `--top`, `--window`, `--loki-source`, `--loki-selector`, `--tempo-source`, `--json`, `--out`                                  | live (Prometheus always; Loki optional) |
| `cerberus migrate gate`      | Fold the artifacts into one cutover go/no-go decision                          | `--verify`, `--classify`, `--rulegraph`, `--inventory`, `--high-card-series`, `--high-card-label-values`, `--json`, `--out`               | offline                                 |

The legacy `migrate --schema` root flag is now the `schema` subcommand, and the
legacy `migrate --rules` root shorthand folded into `explain --rules`.

### Setting fallbacks

The offline preview commands (`explain`, `classify`) load `config.FromEnv()`, so
the preview runs under the **same per-query sample budget** the production
server enforces — a query that would trip a runtime guard is not previewed as
clean.

`verify` and `inventory` also read `CERBERUS_VERIFY_*` / `CERBERUS_INVENTORY_*`
fallbacks for their connection, window, credential and (for `verify`)
`--report` flags — `--report` has a `CERBERUS_VERIFY_REPORT` fallback too — but
**not** the stdout output flags (`--json` / `--out`, and for inventory not
`--top`).

Those fallbacks resolve exactly like every other cerberus setting: environment
variable first, then a `cerberus.yaml` in the working directory or
`/etc/cerberus/`, with an explicit flag outranking both. One file therefore
configures both the migration and the gateway it migrates to. The repeatable
`--loki-selector` takes a YAML sequence, one whole selector per entry — or, from
the environment, one selector per line; neither form comma-splits, because a
stream selector contains commas of its own.

## `verify` backends

`verify` takes one **backend pair per head lane** — a reference backend and the
cerberus endpoint that replaces it. Configure only the heads your corpus
actually contains:

| Head    | Reference     | Cerberus           | Bearer tokens                                 | Multi-tenant reference |
| ------- | ------------- | ------------------ | --------------------------------------------- | ---------------------- |
| `prom`  | `--ref`       | `--cerberus`       | `--ref-token`, `--cerberus-token`             | *(n/a)*                |
| `loki`  | `--ref-loki`  | `--cerberus-loki`  | `--ref-loki-token`, `--cerberus-loki-token`   | `--ref-loki-org-id`    |
| `tempo` | `--ref-tempo` | `--cerberus-tempo` | `--ref-tempo-token`, `--cerberus-tempo-token` | `--ref-tempo-org-id`   |

At least one **complete** pair is required. Supplying one side of a pair is a
usage error, and so is supplying a lane's `-token` / `-org-id` flags with no URL
pair at all — never a silently skipped head, never a silently discarded
credential.

`--*-org-id` sends `X-Scope-OrgID` to the **reference** backend only: a
multi-tenant Loki or Tempo rejects an unscoped read, while cerberus reads no
tenant header at all, so there is deliberately no cerberus-side equivalent. A
tenant id is a *routing* parameter, not a credential, so it is recorded in the
`--report` diagnostic and reproduced in the repro command; bearer tokens are
credentials and appear in no artifact.

`--tolerance` is a single value shared by every lane: the gate proves the same
number on all three heads under one definition of equality.

The window flags default to `--start -1h --end now --step 60s`, so they are
optional; supply them when you want a specific window.

## What `explain` previews

`explain` renders the SQL for a query as an *instant* evaluation for rules and a
*range* (`query_range`) evaluation for panels. The instant/rule SQL matches what
the server runs. The range/panel SQL uses the **fan-out** lowering for the
range-window operators (`rate` / `changes` / `resets` / `*_over_time`,
staleness); a live deployment with the experimental native `timeSeries*ToGrid`
aggregates enabled (auto-selected on CH 25.9+) lowers those differently, so the
previewed range SQL **may differ** from what such a deployment runs. The tool is
offline and cannot know the target's ClickHouse version.

The preview covers all three heads: a PromQL query lowers against the metrics
schema, a LogQL query against the logs schema (`otel_logs`), a TraceQL query
against the traces schema (`otel_traces`) — each emitting real ClickHouse SQL. A
TraceQL search query (`{ span.http.status_code = 500 }`) previews as the bounded
`/api/search` scan; a TraceQL metrics query (`{ } | rate()`) previews as the
`/api/metrics/query_range` matrix. Both are bounded to a fixed lookback window
so the emitted scan is partition-pruned — the same shape the server runs.

`classify` additionally flags a TraceQL structural-join query (`{...} >> {...}`)
**risky**: it lowers cleanly, so it is supported, but it runs a per-trace
recursive closure — a fan-out worth reviewing before cutover.

## What each assess command reads

**`harvest`** — Prometheus rules (`--rules`) and Prometheus dashboard panels
harvest as PromQL; Loki rules (`--loki-rules`, same YAML shape) and Loki panels
as LogQL; Tempo panels (TraceQL read from the panel's `query` field) as TraceQL.
Each query is tagged with its language and provenance. Every dropped item — an
unreadable file, an unsupported datasource, an empty expr — is counted and
reported.

**`inventory`** — probes the live source Prometheus's `/api/v1/status/tsdb` and
ranks top head-block series and label cardinality. That number exists only at
runtime; inventory refuses to infer it from `prometheus.yml`, and a source that
404s the status endpoint is a hard error.

`--loki-source` adds a per-selector section, ranking the `--loki-selector` set
you supply by streams matched via Loki's `/loki/api/v1/index/stats`. Loki
exposes no whole-tenant top-N cardinality call the way Prometheus's TSDB status
does, so the operator names what to rank rather than the tool guessing.

`--tempo-source` records a fixed, specifically-reasoned out-of-scope entry
rather than a fabricated number: Tempo's span/block storage has no head-block or
ranked-cardinality-stats API analogous to either other head.

**`rulegraph`** — links each recording rule's `record:` output series to the
dashboard/alert consumers that read it. `--loki-rules` extends this to Loki's
ruler: only `record:` output series are harvested from it, tagged with a
`loki-rule:` source prefix so they stay distinguishable from Prometheus-sourced
ones, and linked by the same PromQL-shaped consumers (a dashboard panel or a
Prometheus rule reading the remote-written metric by name) rulegraph already
scans.

Loki `alert:` rules are never harvested for this graph — a LogQL alerting expr
is a log-stream selector, not a metric-name reference, so it can never consume a
recorded series, and feeding it through the PromQL extractor would only
manufacture spurious unparseable-consumer skips. Tempo has no rule concept in
this sense: its metrics-generator is a fixed-shape, config-driven span-metric
emitter with no user-authored rule file for a `--tempo-rules` flag to point at,
so no such flag exists.

## How `verify` decides two results agree

Each query is judged by the comparator its **result shape** selects, because
different shapes have different definitions of equality.

**`metric-matrix`** — PromQL, LogQL metric queries, TraceQL metrics queries. Two
backends agree when they return the same series, step-aligned, with values
agreeing within `--tolerance`.

**`log-stream`** — a LogQL selector, with or without pipeline stages, returning
log lines. Two backends agree when, for every `(stream label set, nanosecond
timestamp, log line)` triple, both returned it the **same number of times**, over
the part of the window both fully cover.

**`trace-search`** — a TraceQL spanset filter returning trace summaries. When
neither backend hit the request limit, both answered completely and agreement
means exact **trace-ID set equality** plus exact field equality on every returned
summary. When either hit the limit, its result is a prefix of a ranking neither
wire contract fixes, so the gate does **not** claim set parity: it reports both
side counts and the residue by name, and still field-diffs every trace both
backends returned, so a real field bug is never masked by truncation. Result
order is never a compared dimension.

**`trace-by-id`** — a single-trace fetch. No query language names a trace ID, so
this shape never comes from the corpus: every trace-search comparison **derives**
one fetch per trace ID both backends returned (capped, so one broad search cannot
balloon into thousands of round-trips), and each is judged as its own comparison
unit. A trace is a **set of spans**, not a sequence and not a batch layout, so
span order and the resource/scope batch partition a backend renders it in are
never compared. Two backends agree when they return the same span-ID set, the
same total span count (so a duplicate span surfaces), and, for every span, the
same name, kind, parent, start, duration and status, plus the same attribute
**key** set. An attribute's *value* is compared only when its type round-trips
deterministically through the OTel-CH string-map carrier (string, int, bool); a
double/array/kvlist/bytes value is counted, never diffed — but its key is still
compared, so a genuinely missing attribute still blocks.

**`tag-discovery`** — a tag/label-name or tag/label-value enumeration. These
probes are corpus-**anchored**, not harvested: an unfiltered tag-name enumeration
runs once per head your corpus touches, and a tag-value probe runs once per
distinct label or attribute key that head's own queries reference. Agreement is
exact **set equality**, scoped where the wire contract scopes it — Tempo's v2
tag-names surface is diffed per scope (resource, span, intrinsic), not as one
flat set. The only exception is a reference response the reference itself
reports as partial, which is declined by name with both job counts rather than
diffed against cerberus's complete answer.

### Verdicts and roll-ups

Each replayed query lands as `match`, `diverge`, `undecidable`, `unsupported` or
`error`. The report carries a **per-head roll-up** alongside the aggregate,
split one level further into a row per `(head, shape)` **family** — that split is
what stops a healthy family masking a quiet one: a Loki lane that diffed 412 log
entries has not thereby proved its metric panels.

Every lane you configured gets a row, including one whose corpus entries all
landed out of scope; a configured lane is never silently absent from the table.
Each family row carries a `compared` count in **its own unit** (series, log
entries, traces, spans, tag values), which is the only number that is
*evidence*: two empty results agree, so a family can record nothing but matches
while the comparator diffed nothing at all.

A head that ran trace-search but never derived a trace-by-id probe (no search
returned a trace both backends have) carries no trace-by-id row at all. That is
a non-blocking note in the report, not a dead family: there was nothing to derive
from, which is different from something replayed and compared nothing.

On divergence the report shows the first differing point, anchored in the
vocabulary of the shape it came from — a series and a step, a stream and a
nanosecond timestamp, a trace and a span within it and the field that differed,
or a scope and a tag — and the lane it happened on.

## What the report declines to judge

The `not judged` section names, with an exact count, every dimension no
comparator could judge. This is the opposite of an allow-list: it suppresses
nothing and blesses nothing. It states that no verdict was reached on a named
dimension, and how much of the result that covers.

**log-stream**, up to three:

- `log-entry-order` — entry order within a stream, and stream order across the
  response, are not compared. Order is not a wire contract on both sides, so
  equality is defined on the entry multiset instead. Every entry is still
  compared; only its position is not.
- `log-structured-metadata` — the replay does not request categorized labels (the
  two backends encode them as structurally different objects), so every entry is
  judged on `(labels, timestamp, line)`.
- `log-truncation-band` — when both results hit the replay limit, the entries at
  or below the deeper side's oldest timestamp cover different depths on the two
  sides and are not judged. Everything strictly newer than that boundary is
  provably complete on both sides and *is* compared, so a real divergence in the
  interior still blocks.

**trace-search**, three:

- `trace-search-truncated` — at least one side returned exactly the replay limit,
  so each result is a prefix of a ranking neither backend fixes and set
  membership is not judged. The count is the number of trace IDs exactly one
  backend returned; every trace **both** returned is still field-diffed, and a
  field difference there still blocks.
- `trace-search-spanset-capped` — which matched spans survive the spans-per-set
  cap is unspecified upstream, so a capped spanset is judged on its uncapped
  `matched` total and its kept-span count, never on which spans it kept.
- `trace-search-reference-partial` — the reference reported its *own* search as
  partial (it completed fewer jobs than it started), so the traces it did not
  return are not evidence of absence and membership is not judged against
  cerberus's complete answer.

The search response's aggregate `metrics` block is not diffed at all: cerberus's
`inspectedTraces` is a ClickHouse row count and its `inspectedBytes` /
`totalBlocks` are hard zeros, while the reference's counters describe a sharded
block search cerberus has no analogue for. The one counter pair that *is* read is
the reference's own job accounting, which is what raises
`trace-search-reference-partial`. `serviceStats` and a spanset's per-span
`attributes` are likewise not comparable — cerberus does not model them — and are
named here rather than diffed into a permanent divergence.

**trace-by-id**, one:

- `span-attribute-value-type` — an attribute whose reference type the OTel-CH
  string-map carrier cannot round-trip (double, array, kvlist, bytes) has its
  *value* declined; the count is how many attributes that covers. The attribute's
  *key* is still compared on every span, so a genuinely missing attribute still
  blocks.

**tag-discovery**, one:

- `tag-discovery-reference-partial` — the reference reported its *own* tag /
  tag-value enumeration as partial (it completed fewer jobs than it started), so
  the values it did not return are not evidence of absence and set membership is
  not judged against cerberus's complete answer.

### Inputs that were not examined

Three further buckets record what never reached a comparator:

- `unconfigured` — a replayable query whose head lane had no backend pair, or no
  wire contract for that query's shape. This is a property of your invocation,
  not of the query, and it **blocks**: the gate cannot claim parity for a query
  it never ran. Supply that head's pair, or harvest a narrower corpus.
- `out_of_scope` — a query whose *shape* has no definition of equality at all: a
  TraceQL `compare()`, an expression the parser rejects, a query language this
  build has no lane for. Each entry names its `kind` and the specific `reason`.
- `harvest_skipped` — a corpus entry that never became a replayable query.

A green run means the replayed queries matched, not that every input was
checked. Read those buckets too.

### Pinned replay constants

The non-matrix replay parameters — the log-stream `limit` of 5000 and its
`backward` direction, the trace-search `limit` of 1000 and its spans-per-set of
100 — are **pinned constants, not flags**, and are recorded in the `--report`
artifact. They decide how much of a result is truncated, i.e. how much the gate
can judge, so an operator knob would silently change what parity *means* between
two runs. Trace-by-id and tag-discovery probes carry no limit of their own to
pin: a trace-by-id fetch is a direct row-by-id lookup, and a discovery probe
enumerates whatever the window covers.

### `verify` exit code

`verify` exits **2** if a single query diverges or errors, if a head with
replayable queries had no backend pair to judge them, if the run replayed
nothing at all, or if any family replayed queries and compared none of them.
Divergence is never allow-listed, and no absence of evidence is ever counted as
passing — `verify`'s own banner and exit code apply exactly the rules
`migrate gate` applies to the same report, so the two can never disagree.

## What `gate` blocks on

`gate` reads the JSON artifacts the other stages emit (`--verify`, `--classify`,
`--rulegraph`, `--inventory`) and folds them into one PASS/FAIL verdict with a
per-stage checklist. It **refuses** (exit **3**); it never merely warns.

**verify** — any divergence or error blocks. A parity run that replayed **zero
queries** blocks (an empty or all-out-of-scope corpus proves nothing). A
**`(head, shape)` family that compared nothing** blocks even when another family
is green: 40 matched PromQL queries must not mask a Loki lane that judged none of
its 12, a lane's 412 diffed log entries must not vouch for its metric panels, and
a family whose every response was empty "matched" without comparing anything. Any
**unconfigured** replayable query blocks.

A lane you configured that had no replayable query at all is reported as a
caveat naming that head — it does not block, because those entries are honestly
out of scope, but it is never silently omitted. `undecidable` comparisons and the
named limitations behind them are caveats and do not block on their own; the
evidence counter blocks independently, so an undecidable verdict can never
green-light a family that compared nothing.

**classify** — any unsupported query blocks (risky ones WARN); classifying
**zero queries** also blocks, since an empty corpus proves no support coverage.

**rulegraph** — any *consumed* recorded series blocks (it must stay
materialized). An unparseable consumer expression also blocks, because
"orphan ⇒ safe to drop" is unsound once a consumer was dropped. A
Loki-ruler-sourced recorded series (from `--loki-rules`) is the identical
`RecordedNode` shape as a Prometheus one, distinguished only by its `loki-rule:`
source prefix, and blocks through this same rule — the reported reason names the
source, so the two are never visually indistinguishable.

**inventory** — advisory everywhere: high cardinality WARNs and never blocks,
uniformly across every head you asked about. A high-cardinality Loki stream
selector (from `--loki-source`) WARNs exactly like a high-cardinality Prometheus
metric; a present Tempo section (from `--tempo-source`) always WARNs with its
fixed out-of-scope reason. A head you never asked about is simply absent from the
decision — never presented as "checked, nothing found."

**Missing artifacts** — `verify`, `classify` and `rulegraph` are required, and a
missing one blocks. `inventory` is advisory.

Exit 0 — and only exit 0 — means you are cleared to cut over.

## Scope (v1)

- **Three comparison families.** `verify` judges the **metric matrix** — PromQL,
  LogQL metric queries, TraceQL metrics queries — the **LogQL log stream**, and
  the **TraceQL trace search**, each under its own definition of equality. A
  TraceQL `compare()` selects its attribute inventory by a topN ranking neither
  backend's wire contract specifies, so no definition of equality holds for it;
  it is counted and reported `out_of_scope` with the reason, never silently
  dropped and never guessed at.
- **Query-result parity, not alert-firing parity.** `verify` diffs query
  results; it does not re-implement `for:` durations or Alertmanager routing.
- **File-based harvest.** Harvest inputs are rule YAML and exported dashboard
  JSON; there is no live Grafana-API source.
- **Read-only.** The tool never provisions schema or mutates Grafana; applying
  the rendered DDL and flipping the datasource are deliberate steps you run
  yourself.
