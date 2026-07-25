# Migrating to cerberus

This is the operator playbook for moving a Prometheus-backed setup onto
cerberus (ClickHouse) **without rebuilding dashboards or rewriting alerts** —
and, just as important, for *proving* what will change **before** you send real
traffic.

The whole journey is driven by the `cerberus migrate` command group, built into
the single release binary (every CLI is a subcommand of `cerberus`). It is
**read-only and offline-first**: it never writes to Prometheus,
Grafana, or ClickHouse. The only mutating steps in this guide — applying the
schema and flipping the datasource — are things **you** run by hand,
deliberately.

## What cerberus replaces — and what it does not

Cerberus replaces Prometheus's **storage and query engine**. It does **not**
replace two things, and getting this straight up front is the difference
between a smooth cutover and a blank dashboard:

- **It has no ruler.** Cerberus evaluates ad-hoc PromQL that Grafana (or your
  CLI) sends it; it does **not** evaluate your `recording` / `alerting` rules on
  a schedule. The `record:` output series a recording rule produces are not
  created by cerberus. Whatever evaluates your rules today must keep doing so,
  writing its output where cerberus can read it (into ClickHouse via your
  collector). Keep the ruler.
- **It does not ingest.** Your OpenTelemetry Collector already writes telemetry
  into ClickHouse through its ClickHouse exporter; cerberus only reads it back.
  You do **not** point any writer at cerberus.

So the migration is organised around your **real queries** — the PromQL in
recording rules, alerting rules, and Grafana panels — not around
`prometheus.yml`. A config file cannot tell you whether a query will translate
cleanly or blow up on cardinality; only the queries and the live data can.

## Before you start

You need three things in place:

1. **ClickHouse receiving your telemetry** via the OpenTelemetry Collector's
   ClickHouse exporter — the same OTel-shaped tables cerberus reads (see the
   [version requirements](../README.md#version-requirements)).
2. **A dual-write / shadow window.** For a period, data flows into **both**
   Prometheus **and** ClickHouse at the same time. This overlap is what makes a
   real before/after comparison possible. You never cut over cold.
3. **Your real queries as files** — Prometheus recording/alerting rule YAML and
   exported Grafana dashboard JSON. These are the harvest inputs. (Harvesting
   from a live Grafana API is **not** a shipped capability today; export the
   dashboards to JSON first.)

## The `migrate` tool

`migrate` is a command group of the single `cerberus` binary, with eight
subcommands.

| Command                      | What it does                                                                   | Key flags                                                                                                                                        | Network                                 |
| ---------------------------- | ------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------ | --------------------------------------- |
| `cerberus migrate schema`    | Print the `CREATE` statements cerberus expects, from `CERBERUS_*` env          | *(no flags; reads `CERBERUS_*`)*                                                                                                                 | offline                                 |
| `cerberus migrate harvest`   | Build a machine-readable PromQL + LogQL + TraceQL corpus from your files       | `--rules`, `--loki-rules`, `--dashboards`, `--out`                                                                                               | offline                                 |
| `cerberus migrate explain`   | Dry-run each corpus query through the read pipeline, print the SQL             | `--corpus` (or `--rules`/`--loki-rules`/`--dashboards`), `--out`                                                                                 | offline                                 |
| `cerberus migrate classify`  | Bucket each query as supported / unsupported / risky                           | `--corpus` (or `--rules`/`--loki-rules`/`--dashboards`), `--json`, `--out`                                                                       | offline                                 |
| `cerberus migrate rulegraph` | Map recording-rule outputs to the consumers that must stay materialized        | `--rules`, `--loki-rules`, `--corpus`, `--json`, `--out`                                                                                         | offline                                 |
| `cerberus migrate verify`    | Replay the corpus against each head's reference backend and diff (parity gate) | `--corpus`, per-head `--ref*`/`--cerberus*` pairs (see below), `--start`, `--end`, `--step`, `--tolerance`, `--json`, `--report`, `--out`        | live (two backends per configured head) |
| `cerberus migrate inventory` | Probe live sources for the cardinality that drives OOM risk                    | `--source`, `--top`, `--window`, `--loki-source`, `--loki-selector`, `--tempo-source`, `--json`, `--out`                                         | live (Prometheus always; Loki optional) |
| `cerberus migrate gate`      | Fold the artifacts into one cutover go/no-go decision                          | `--verify`, `--classify`, `--rulegraph`, `--inventory`, `--high-card-series`, `--high-card-label-values`, `--json`, `--out`                      | offline                                 |

The legacy `migrate --schema` root flag is now the `schema` subcommand, and the
legacy `migrate --rules` root shorthand folded into `explain --rules`.

`verify` takes one **backend pair per head lane** — a reference backend and the
cerberus endpoint that replaces it. You configure only the heads your corpus
actually contains:

| Head    | Reference       | Cerberus              | Bearer tokens                                 | Multi-tenant reference |
| ------- | --------------- | --------------------- | --------------------------------------------- | ---------------------- |
| `prom`  | `--ref`         | `--cerberus`          | `--ref-token`, `--cerberus-token`             | *(n/a)*                |
| `loki`  | `--ref-loki`    | `--cerberus-loki`     | `--ref-loki-token`, `--cerberus-loki-token`   | `--ref-loki-org-id`    |
| `tempo` | `--ref-tempo`   | `--cerberus-tempo`    | `--ref-tempo-token`, `--cerberus-tempo-token` | `--ref-tempo-org-id`   |

At least one **complete** pair is required; supplying one side of a pair is a
usage error, and so is supplying a lane's `-token` / `-org-id` flags with no URL
pair at all — never a silently skipped head or a silently discarded credential.
The `--*-org-id` flags send `X-Scope-OrgID` to the **reference** backend only — a
multi-tenant Loki or Tempo rejects an unscoped read, while cerberus reads no
tenant header at all, so there is deliberately no cerberus-side equivalent. The
tenant id is a *routing* parameter, not a credential, so it is recorded in the
`--report` diagnostic and reproduced in the repro command; the bearer tokens are
credentials and appear in no artifact. `--tolerance` is a single value
shared by every lane: the gate proves the same number on all three heads under
one definition of equality.

The offline preview commands (`explain`, `classify`) load `config.FromEnv()`
so the preview runs with the **same per-query sample budget** the production
server enforces — a query that would trip a runtime guard is not previewed as
clean. `verify` and `inventory` also read `CERBERUS_VERIFY_*` /
`CERBERUS_INVENTORY_*` environment fallbacks for their connection, window,
credential, and (for `verify`) `--report` flags — `--report` has a
`CERBERUS_VERIFY_REPORT` fallback too — but not the stdout output flags (`--json`
/ `--out`, and for inventory not `--top`), so the same run can be driven from
flags or env.

`explain` previews the SQL for a query as an *instant* evaluation for rules and a
*range* (`query_range`) evaluation for panels. The instant/rule SQL matches what
the server runs. The range/panel SQL uses the **fan-out** lowering for the
range-window operators (`rate` / `changes` / `resets` / `*_over_time`, staleness);
a live deployment with the experimental native `timeSeries*ToGrid` aggregates
enabled (auto-selected on CH 25.9+) lowers those differently, so the previewed
range SQL **may differ** from what such a deployment runs. The tool is offline and
cannot know the target's ClickHouse version.

The SQL preview covers all **three heads**: a PromQL corpus query lowers against
the metrics schema, a LogQL query against the logs schema (`otel_logs`), and a
TraceQL query against the traces schema (`otel_traces`) — each emitting real
ClickHouse SQL. A TraceQL search query (`{ span.http.status_code = 500 }`)
previews as the bounded `/api/search` scan; a TraceQL metrics query
(`{ } | rate()`) previews as the `/api/metrics/query_range` matrix. Both are
bounded to a fixed lookback window so the emitted scan is partition-pruned, the
same shape the server runs. `classify` additionally flags a TraceQL
structural-join query (`{...} >> {...}`) **risky**: it lowers cleanly (so it is
supported) but runs a per-trace recursive closure — the fan-out worth reviewing
before cutover.

## The migration lifecycle

```text
ASSESS            VALIDATE      VERIFY          DECIDE      CUT OVER       DECOMMISSION
harvest           --schema      verify          gate        (manual:       (manual:
 → inventory      (render +     (diff each      (go/        flip the       after the
 → classify        review)      head's lane,    no-go)      datasource     retention
 → rulegraph                    diverge→zero)               URL)           runway)
```

The offline stages (`ASSESS`, `VALIDATE`) you can run today, before cerberus is
even provisioned. `VERIFY` and `DECIDE` need the dual-write window live. The
last two stages are **operator actions, not commands** — the tool deliberately
stops at the go/no-go and hands you the flip.

### Assess: harvest, inventory, classify, rulegraph

**Harvest** collapses every rule file and exported dashboard into one
deterministic `corpus.json` spanning all three heads: Prometheus rules
(`--rules`) and Prometheus dashboard panels harvest as PromQL, Loki rules
(`--loki-rules`, same YAML shape) and Loki panels as LogQL, and Tempo panels
(TraceQL read from the panel's `query` field) as TraceQL — each query tagged
with its language and provenance. Every dropped item (unreadable file,
unsupported datasource, empty expr) is counted and reported — nothing is
silently discarded.

**Inventory** probes the **live** source Prometheus's
`/api/v1/status/tsdb` endpoint and ranks the top head-block series and label
cardinality. This is the number that drives OOM risk, and it exists **only** at
runtime — it is not in any config or dashboard. Inventory refuses to infer it
from `prometheus.yml`. A source that 404s the status endpoint is a hard error.
`--loki-source` optionally adds a per-selector section, ranking the
`--loki-selector` set the operator supplies by streams matched via Loki's
`/loki/api/v1/index/stats` endpoint — Loki exposes no whole-tenant top-N
cardinality call the way Prometheus's TSDB status does, so the operator names
what to rank instead of the tool guessing. `--tempo-source` records a fixed,
specifically-reasoned out-of-scope entry rather than a fabricated number:
Tempo's span/block storage has no head-block or ranked-cardinality-stats API
analogous to either of the other two heads, so no such proxy is computed.

**Classify** buckets each corpus query: *supported* (parses, lowers, and emits
SQL cleanly), *unsupported* (the offending construct is named), or
supported-but-**risky**. Read "supported" precisely: it means the query
**translates**, not that cerberus returns the same numbers — only `verify`
proves that.

**Rulegraph** links each recording rule's `record:` output series to the
dashboard/alert consumers that read it. Because cerberus has no ruler, any
**consumed** recorded series must keep being materialized after cutover, or the
panel that reads it goes silently blank. Rulegraph tells you exactly which ones;
materializing them elsewhere is a manual operator step. `--loki-rules` extends
this to Loki's ruler, which has a real recording/alerting rule format of its
own: only `record:` output series are harvested from it, tagged with a
`loki-rule:` source so they're distinguishable from Prometheus-sourced ones,
and linked by the same PromQL-shaped consumers (a dashboard panel or a
Prometheus rule reading the remote-written metric by name) rulegraph already
scans. Loki `alert:` rules are never harvested for this graph — a LogQL
alerting expr is a log-stream selector, not a metric-name reference, so it can
never itself consume a recorded series, and feeding it through the PromQL
extractor would only manufacture spurious unparseable-consumer skips. Tempo
has no rule concept in this sense: its metrics-generator is a fixed-shape,
config-driven span-metric emitter with no user-authored recording/alerting
rule file for a `--tempo-rules` flag to point at, so no such flag exists.

### Validate: render the schema

Preview the exact tables cerberus expects — offline, no database connection.
The output is byte-identical to what the server applies at startup, because it
reads the same `CERBERUS_*` environment, and it pipes straight into
`clickhouse-client`:

```bash
cerberus migrate schema | clickhouse-client -h clickhouse.internal --multiquery
```

Applying the DDL is a **deliberate, separate step you run yourself** — the tool
only renders it.

### Verify: replay against each head's reference backend

This is the parity gate. Over the dual-write window, `verify` replays every
corpus query against the reference backend for **its own head** — PromQL against
reference Prometheus, LogQL against reference Loki, TraceQL against reference
Tempo — **and** against cerberus over one window, then diffs the two results.

Each query is judged by the comparator its **result shape** selects, because
different shapes have different definitions of equality:

- **`metric-matrix`** — PromQL, LogQL metric queries and TraceQL metrics queries.
  Two backends agree when they return the same series, step-aligned, with values
  agreeing within `--tolerance`.
- **`log-stream`** — a LogQL selector, with or without pipeline stages, that
  returns log lines. Two backends agree when, for every `(stream label set,
  nanosecond timestamp, log line)` triple, both returned it the **same number of
  times**, over the part of the window both fully cover.
- **`trace-search`** — a TraceQL spanset filter that returns trace summaries.
  When neither backend hit the request limit, both answered completely and
  agreement means exact **trace-ID set equality** plus exact field equality on
  every returned summary. When either hit the limit, its result is a prefix of a
  ranking neither wire contract fixes, so the gate does **not** claim set parity:
  it reports both side counts and the residue by name, and still field-diffs
  every trace both backends returned, so a real field bug is never masked by
  truncation. Result order is never a compared dimension.
- **`trace-by-id`** — a single-trace fetch. No query language names a trace ID,
  so this shape never comes from the corpus: every trace-search comparison
  DERIVES one fetch per trace ID both backends returned (capped, so one broad
  search cannot balloon into thousands of round-trips), and each is judged as
  its own comparison unit. A trace is a **set of spans**, not a sequence and not
  a batch layout, so span order and the resource/scope batch partition a
  backend renders it in are never compared. Two backends agree when they return
  the same span-ID set, the same total span count (so a duplicate span
  surfaces), and, for every span, the same name, kind, parent, start, duration
  and status, plus the same attribute **key** set. An attribute's *value* is
  compared only when its type round-trips deterministically through the
  OTel-CH string-map carrier (string, int, bool); a double/array/kvlist/bytes
  value is counted, never diffed — but its key is still compared, so a
  genuinely missing attribute still blocks.
- **`tag-discovery`** — a tag/label-name or tag/label-value enumeration. These
  probes are corpus-**anchored**, not harvested: an unfiltered tag-name
  enumeration runs once per head your corpus touches, and a tag-value probe
  runs once per distinct label or attribute key that head's own queries
  reference. Agreement is exact **set equality**, scoped where the wire
  contract scopes it — Tempo's v2 tag-names surface is diffed per scope
  (resource, span, intrinsic), not as one flat set. The only exception is a
  reference response the reference itself reports as partial, which is
  declined by name with both job counts rather than diffed against cerberus's
  complete answer.

Each replayed query lands as `match`, `diverge`, `undecidable`, `unsupported`, or
`error`, and the report carries a **per-head roll-up** alongside the aggregate,
split one level further into a row per `(head, shape)` **family**. That split is
what stops a healthy family masking a quiet one: a Loki lane that diffed 412 log
entries has not thereby proved its metric panels. Every lane you configured gets
a row, including one whose corpus entries all landed out of scope — a configured
lane is never silently absent from the table. Each family row also carries a
`compared` count in **its own unit** (series, log entries, traces, spans, tag
values), which is the only number that is *evidence*: two empty results agree, so
a family can record nothing but matches while the comparator diffed nothing at
all. A head that ran trace-search but never derived a trace-by-id probe (no
search returned a trace both backends have) carries no trace-by-id row at all —
that is a non-blocking note in the report, not a dead family: there was nothing
to derive from, which is different from something replayed and compared nothing.

The report also names, with an exact count, every dimension no comparator could
judge — the `not judged` section. This is the opposite of an allow-list: it
suppresses nothing and blesses nothing, it states that no verdict was reached on
a named dimension and how much of the result that covers. On the log-stream
lane there are up to three:

- `log-entry-order` — entry order within a stream, and stream order across the
  response, are not compared. Order is not a wire contract on both sides, so
  equality is defined on the entry multiset instead. Every entry is still
  compared; only its position is not.
- `log-structured-metadata` — the replay does not request categorized labels
  (the two backends encode them as structurally different objects), so every
  entry is judged on `(labels, timestamp, line)`.
- `log-truncation-band` — when both results hit the replay limit, the entries at
  or below the deeper side's oldest timestamp cover different depths on the two
  sides and are not judged. Everything strictly newer than that boundary is
  provably complete on both sides and *is* compared, so a real divergence in the
  interior still blocks.

On the trace-search lane there are three more:

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

On the trace-by-id lane there is one:

- `span-attribute-value-type` — an attribute whose reference type the OTel-CH
  string-map carrier cannot round-trip (double, array, kvlist, bytes) has its
  *value* declined; the count is how many attributes that covers. The
  attribute's *key* is still compared on every span, so a genuinely missing
  attribute still blocks.

On the tag-discovery lane there is one:

- `tag-discovery-reference-partial` — the reference reported its *own*
  tag/tag-value enumeration as partial (it completed fewer jobs than it
  started), so the values it did not return are not evidence of absence and
  set membership is not judged against cerberus's complete answer.

Three further buckets record inputs that were **not** examined:

- `unconfigured` — a replayable query whose head lane had no backend pair, or no
  wire contract for that query's shape. This is a property of your invocation,
  not of the query, and it **blocks**: the gate cannot claim parity for a query it
  never ran. Supply that head's pair, or harvest a narrower corpus.
- `out_of_scope` — a query whose *shape* has no definition of equality at all: a
  TraceQL `compare()`, an expression the parser rejects, a query language this
  build has no lane for. Each entry names its `kind` and the specific `reason`
  the gate did not judge it.
- `harvest_skipped` — a corpus entry that never became a replayable query.

A green run means the replayed queries matched, not that every input was checked
— read those buckets too. On divergence the report shows the first differing
point, anchored in the vocabulary of the shape it came from (a series and a step,
a stream and a nanosecond timestamp, a trace and a span within it and the field
that differed, or a scope and a tag) and the lane it happened on.

The non-matrix replay parameters — the log-stream `limit` of 5000 and its
`backward` direction, the trace-search `limit` of 1000 and its spans-per-set of
100 — are **pinned constants, not flags**, and are recorded in the `--report`
artifact. They decide how much of a result is truncated, i.e. how much the gate
can judge, so an operator knob would silently change what parity *means* between
two runs. Trace-by-id and tag-discovery probes carry no limit of their own to
pin: a trace-by-id fetch is a direct row-by-id lookup, and a discovery probe
enumerates whatever the window covers.

`verify` exits **non-zero (code 2)** if a single query diverges or errors, if a
head with replayable queries had no backend pair to judge them, if the run
replayed nothing at all, or if any family replayed queries and compared none of
them. Divergence is **never** allow-listed, and no absence of evidence is ever
counted as passing — `verify`'s own banner and exit code apply exactly the rules
`migrate gate` applies to the same report, so the two can never disagree. Run it,
fix each divergence at the source, re-run. **You are done when the diverge count
reaches zero, with every head configured and every family comparing something.**
That is your permission to flip traffic — not a leap of faith.

For a failing run, add `--report diagnostics.json` to capture the full
machine-readable diagnostics (with a copy-pasteable repro command; backend URLs
and credentials are redacted) — that file is what you attach to a bug report.

> If the cerberus you verify against has experimental native ClickHouse
> aggregates enabled (the `timeSeries*ToGrid` family, auto-selected on CH 25.9+),
> a sub-observable last-bit rounding difference can surface as a `diverge`.
> Verify against the exact configuration you intend to run, and see the
> [exactness-vs-scale tradeoff](performance.md#native-rate-exactness-vs-scale-should-i-enable-it).
> Raise `--tolerance` only as a deliberate decision, never to paper over a real diff.

### Decide: the cutover gate

`gate` is a pure-offline aggregator. It reads the JSON artifacts the other
stages emit (`--verify`, `--classify`, `--rulegraph`, `--inventory`) and folds
them into **one** PASS/FAIL verdict with a per-stage checklist. It **refuses**
(exits **code 3**), it never merely warns, on any blocking input:

- **verify** — any divergence or error blocks; a parity run that replayed **zero
  queries** also blocks (an empty or all-out-of-scope corpus proves nothing); a
  **`(head, shape)` family that compared nothing** blocks even when another
  family is green (40 matched PromQL queries must not mask a Loki lane that
  judged none of its 12, a lane's 412 diffed log entries must not vouch for its
  metric panels, and a family whose every response was empty "matched" without
  comparing anything); and any **unconfigured** replayable query blocks. A lane
  you configured that had no replayable query at all is reported as a caveat
  naming that head — it does not block, because those entries are honestly out of
  scope, but it is never silently omitted. `undecidable` comparisons and the
  named limitations behind them are reported as caveats and do not block on their
  own; the evidence counter blocks independently, so an undecidable verdict can
  never green-light a family that compared nothing.
- **classify** — any unsupported query blocks (risky ones WARN); classifying
  **zero queries** also blocks (an empty corpus proves no support coverage).
- **rulegraph** — any *consumed* recorded series blocks (it must stay
  materialized); an unparseable consumer expression also blocks, because
  "orphan ⇒ safe to drop" is unsound once a consumer was dropped. A
  Loki-ruler-sourced recorded series (from `--loki-rules`) is the identical
  `RecordedNode` shape as a Prometheus one, distinguished only by its
  `loki-rule:` source prefix, and blocks through this exact same rule — the
  reported reason names the source, so the two are never visually
  indistinguishable.
- **inventory** — advisory everywhere: high cardinality WARNs and never
  blocks, uniformly across every head the operator asked about. A
  high-cardinality Loki stream selector (from `--loki-source`) WARNs exactly
  like a high-cardinality Prometheus metric; a present Tempo section (from
  `--tempo-source`) always WARNs with its fixed out-of-scope reason. A head
  the operator never asked about is simply absent from the decision — never
  presented as "checked, nothing found."
- A **missing required artifact** blocks — `verify`, `classify`, and
  `rulegraph` are required; `inventory` is advisory (high cardinality WARNs,
  never blocks).

Exit 0 — and only exit 0 — means you are cleared to cut over.

### Cut over: flip the datasource (manual)

This is an operator action, not a command:

- Point the Grafana Prometheus **datasource URL** at cerberus (or swap DNS /
  the service in front of it). Dashboards and alert rules are unchanged — that
  is the whole point.
- Flip your read-path panels first; leave anything that pages **for last**, once
  you have watched the read path stay green.
- Keep dual-write running as a safety net.

### Decommission: retire Prometheus (manual)

Also manual, and not urgent. Keep Prometheus's write/storage path until your
ClickHouse retention covers the longest window your dashboards and alerts look
back over — that runway is your rollback. Only then retire the old storage.

## A full transcript

The commands pipe together into one assess → verify → gate flow:

```bash
# ── ASSESS ────────────────────────────────────────────────────────────
# Harvest every real query (PromQL + LogQL + TraceQL) into one deterministic corpus.
cerberus migrate harvest \
  --rules './prometheus/rules/*.yml' \
  --loki-rules './loki/rules/*.yml' \
  --dashboards ./grafana/dashboards \
  --out corpus.json

# How cleanly does each map onto cerberus PromQL?
cerberus migrate classify --corpus corpus.json --json --out classify.json

# Which recording-rule outputs must stay materialized after cutover?
cerberus migrate rulegraph \
  --rules './prometheus/rules/*.yml' \
  --loki-rules './loki/rules/*.yml' \
  --corpus corpus.json \
  --json --out rulegraph.json

# Probe the LIVE Prometheus for the cardinality that drives OOM risk.
cerberus migrate inventory \
  --source http://prometheus.internal:9090 \
  --top 50 --json --out inventory.json

# ── VALIDATE ──────────────────────────────────────────────────────────
# Render the schema cerberus expects (byte-identical to server startup).
cerberus migrate schema | clickhouse-client -h clickhouse.internal --multiquery

# ── VERIFY ────────────────────────────────────────────────────────────
# Replay each query against its head's reference backend and cerberus over one
# window. Configure a pair per head your corpus contains. Exits 2 on any diverge
# — or on a head whose replayable queries had no pair to judge them.
cerberus migrate verify \
  --corpus corpus.json \
  --ref http://prometheus.internal:9090 \
  --cerberus http://cerberus.internal:8080 \
  --ref-loki http://loki.internal:3100 \
  --cerberus-loki http://cerberus.internal:8080 \
  --ref-tempo http://tempo.internal:3200 \
  --cerberus-tempo http://cerberus.internal:8080 \
  --start -1h --end now --step 60s \
  --json --out verify.json \
  --report verify-diagnostics.json

# ── DECIDE ────────────────────────────────────────────────────────────
# Fold every artifact into one go/no-go. Exit 0 = cleared; exit 3 = no-go.
cerberus migrate gate \
  --verify verify.json \
  --classify classify.json \
  --rulegraph rulegraph.json \
  --inventory inventory.json
```

`verify`'s window flags default to `--start -1h --end now --step 60s`, so they
are optional; supply them when you want a specific window.

## What the tool will not tell you

Being honest about the blind spots is the whole point of the tool. It never
pretends to know these:

- **Cardinality is runtime, not config.** A query whose *shape* looks fine can
  still exhaust memory on a metric with millions of label combinations. That
  number lives only in the running TSDB; `inventory` reads it to **rank risk**,
  but it does **not** predict cerberus's exact memory, and `explain`/`classify`
  flag dangerous *shapes*, never row counts.
- **Translate ≠ match.** `classify` proving a query is *supported* proves it
  translates and emits SQL — it is **not** proof the results match your old
  Prometheus. Only `verify` proves parity.
- **Only `verify` earns the flip.** The diverge-count-zero result is the
  permission to cut over. Nothing upstream of it is.
- **A green gate must rest on evidence, not on silence.** A match over two empty
  results, a lane whose every query was unsupported, and a corpus whose every
  entry is out of scope all look like "no divergences" — and all three block.
  Both `verify` and `gate` key that rule on the number of comparison units
  actually diffed, per `(head, shape)` family.
- **A limitation is not a tolerance.** Where two backends cannot be meaningfully
  compared on some dimension, the report names that dimension and counts the
  units the statement covers. It never widens what "equal" means, never
  suppresses a difference, and never scores as agreement — and if a limitation
  swallowed a whole result, the evidence counter is zero and the run blocks.
- **The gates refuse; they don't warn.** `verify` exits non-zero on any
  divergence (never allow-listed), on any replayable query whose head lane it was
  not given the backends to judge, and on any family that compared nothing; `gate`
  exits non-zero on any blocking stage, an empty corpus, a family that compared
  nothing, or a missing required artifact. There is no escape hatch and no per-head opt-out — the honest way to
  narrow the gate is to harvest a narrower corpus, which is visible and diffable.
- **Experimental ClickHouse paths may deviate.** Verify against the exact
  configuration you will run in production (see the note under *Verify*).

Anything the tool cannot resolve — an unreadable file, an unsupported-datasource
panel, an unparseable expression — is **counted and reported**, never silently
skipped.

## Continuous verification

Migration is not a one-shot. The scheduled **Layer 14** end-to-end lane turns
the whole operator journey — harvest → explain → classify → rulegraph → schema →
verify → gate — into executable scenarios against real ClickHouse and a real
reference Prometheus across eight archetypes. Its design, the 26 user-stories,
and the tier/build plan live in
[`docs/migration-testing.md`](migration-testing.md).

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
