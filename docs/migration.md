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

| Command                      | What it does                                                             | Key flags                                                                                                                                        | Network             |
| ---------------------------- | ------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------ | ------------------- |
| `cerberus migrate schema`    | Print the `CREATE` statements cerberus expects, from `CERBERUS_*` env    | *(no flags; reads `CERBERUS_*`)*                                                                                                                 | offline             |
| `cerberus migrate harvest`   | Build a machine-readable PromQL + LogQL + TraceQL corpus from your files | `--rules`, `--loki-rules`, `--dashboards`, `--out`                                                                                               | offline             |
| `cerberus migrate explain`   | Dry-run each corpus query through the read pipeline, print the SQL       | `--corpus` (or `--rules`/`--loki-rules`/`--dashboards`), `--out`                                                                                 | offline             |
| `cerberus migrate classify`  | Bucket each query as supported / unsupported / risky                     | `--corpus` (or `--rules`/`--loki-rules`/`--dashboards`), `--json`, `--out`                                                                       | offline             |
| `cerberus migrate rulegraph` | Map recording-rule outputs to the consumers that must stay materialized  | `--rules`, `--corpus`, `--json`, `--out`                                                                                                         | offline             |
| `cerberus migrate verify`    | Replay the corpus against **both** backends and diff (parity gate)       | `--corpus`, `--ref`, `--cerberus`, `--ref-token`, `--cerberus-token`, `--start`, `--end`, `--step`, `--tolerance`, `--json`, `--report`, `--out` | live (two backends) |
| `cerberus migrate inventory` | Probe a **live** Prometheus for the cardinality that drives OOM risk     | `--source`, `--top`, `--window`, `--json`, `--out`                                                                                               | live (one backend)  |
| `cerberus migrate gate`      | Fold the artifacts into one cutover go/no-go decision                    | `--verify`, `--classify`, `--rulegraph`, `--inventory`, `--high-card-series`, `--high-card-label-values`, `--json`, `--out`                      | offline             |

The legacy `migrate --schema` root flag is now the `schema` subcommand, and the
legacy `migrate --rules` root shorthand folded into `explain --rules`.

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

## The migration lifecycle

```text
ASSESS            VALIDATE      VERIFY         DECIDE      CUT OVER       DECOMMISSION
harvest           --schema      verify         gate        (manual:       (manual:
 → inventory      (render +     (diff both      (go/        flip the       after the
 → classify        review)      backends,       no-go)      datasource     retention
 → rulegraph                    diverge→zero)                URL)          runway)
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

**Classify** buckets each corpus query: *supported* (parses, lowers, and emits
SQL cleanly), *unsupported* (the offending construct is named), or
supported-but-**risky**. Read "supported" precisely: it means the query
**translates**, not that cerberus returns the same numbers — only `verify`
proves that.

**Rulegraph** links each recording rule's `record:` output series to the
dashboard/alert consumers that read it. Because cerberus has no ruler, any
**consumed** recorded series must keep being materialized after cutover, or the
panel that reads it goes silently blank. Rulegraph tells you exactly which ones;
materializing them elsewhere is a manual operator step.

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

### Verify: replay against both backends

This is the parity gate. Over the dual-write window, `verify` replays every
corpus query against reference Prometheus **and** cerberus over one
`query_range` window and diffs the results series-by-series. Each replayed
PromQL query lands as `match`, `diverge`, `unsupported`, or `error`; two further
buckets record inputs that were **not** examined — `out_of_scope` (a
non-PromQL entry with no Prometheus baseline) and `harvest_skipped` (a corpus
entry that never became a replayable query). A green run means the replayed
queries matched, not that every input was checked — read those two buckets too.
On divergence it shows the first differing point (series, timestamp, reference
value, cerberus value).

`verify` exits **non-zero (code 2)** if a single query diverges or errors —
divergence is **never** allow-listed. Run it, fix each divergence at the source,
re-run. **You are done when the diverge count reaches zero.** That number is
your permission to flip traffic — not a leap of faith.

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
  queries** also blocks (an empty corpus proves nothing).
- **classify** — any unsupported query blocks (risky ones WARN); classifying
  **zero queries** also blocks (an empty corpus proves no support coverage).
- **rulegraph** — any *consumed* recorded series blocks (it must stay
  materialized); an unparseable consumer expression also blocks, because
  "orphan ⇒ safe to drop" is unsound once a consumer was dropped.
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
# Replay the corpus against BOTH backends over one window. Exits 2 on any diverge.
cerberus migrate verify \
  --corpus corpus.json \
  --ref http://prometheus.internal:9090 \
  --cerberus http://cerberus.internal:8080 \
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
- **The gates refuse; they don't warn.** `verify` exits non-zero on any
  divergence (never allow-listed); `gate` exits non-zero on any blocking stage,
  an empty corpus, or a missing required artifact. There is no escape hatch.
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

- **PromQL only.** LogQL and TraceQL panels are counted and dropped, not
  migrated.
- **Query-result parity, not alert-firing parity.** `verify` diffs query
  results; it does not re-implement `for:` durations or Alertmanager routing.
- **File-based harvest.** Harvest inputs are rule YAML and exported dashboard
  JSON; there is no live Grafana-API source.
- **Read-only.** The tool never provisions schema or mutates Grafana; applying
  the rendered DDL and flipping the datasource are deliberate steps you run
  yourself.
