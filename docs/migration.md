# Migrating to cerberus

Move a Prometheus-backed setup onto cerberus (ClickHouse) **without rebuilding
dashboards or rewriting alerts** — and prove what changes *before* real traffic
depends on it.

A step 0 to get ready, then thirteen steps in order (3–6 can go in any order).
Each declares its **pre-conditions**, what you do, and its **post-conditions** —
so you always know whether you are ready, and whether it worked.

```text
0  prepare  ──▶  1–2  install + gather  ──▶  3–6  assess  ──▶  7–9  stand it up
                                                                  │
                                       12–13  cut over  ◀── 10–11  verify + gate
```

Configuration is built up the same way. Step 0 writes a small `cerberus.yaml`,
and each later step adds only the keys it is about to use — so you are never
reading a setting that names a file you do not have yet.

`cerberus migrate` is read-only — it never writes to Prometheus, Grafana or
ClickHouse. Steps 7, 8, 9, 12 and 13 are the ones that change anything, and you
run all five by hand.

## Two things cerberus does not do

It has **no ruler**. It answers the PromQL Grafana sends it, but it does not
evaluate your recording and alerting rules on a schedule — so whatever evaluates
them today has to keep doing so. Step 5 tells you exactly which ones matter.

It **does not ingest**. Your OpenTelemetry Collector keeps writing into
ClickHouse; you point no writer at cerberus.

Get either wrong and you cut over to blank panels. It is also why this migration
works from your **real queries** rather than from `prometheus.yml` — a config
file cannot tell you whether a query translates cleanly or falls over on
cardinality.

## Step 0: Prepare the ground

Two things have to be true before the first command is worth running: someone
has to own the ingestion change, and cerberus has to know which ClickHouse it
is talking about.

**Pre-conditions:**

- A ClickHouse you can reach, or a decision about where it will live.

**Do:**

**Plan the dual ingestion path.** Cerberus reads ClickHouse; it does not fill
it. The telemetry you are going to query has to get there from your
OpenTelemetry Collector, and that is the one part of this migration that is not
a cerberus command and cannot be hurried. At step 8 you add the Collector's
ClickHouse exporter *alongside* the Prometheus write path you already have, and
from that moment both fill in parallel. Step 10 proves parity by replaying your
queries against both backends over the same window — so it can only look back as
far as the overlap goes. Every hour you delay the Collector change is an hour
you cannot verify over later. Settle now where ClickHouse lives, who makes the
Collector change, and when.

**Write the initial `cerberus.yaml`.** One file, in the directory you run the
tool from or in `/etc/cerberus/`. It is written in the same shape as the Helm
chart's `values.yaml`, so a setting has one name whether you deploy cerberus
with the chart or run the binary yourself. An exported environment variable
overrides the file, and a CLI flag overrides both — so the file is where the
settings live and the other two are for one-off overrides.

Start with the engine, and nothing else:

```yaml
# cerberus.yaml

clickhouse:
  addr:
    - clickhouse.internal:9000 # list more hosts for a cluster
  database: otel
  username: default
query:
  maxSamples: 5000000
  timeout: 2m
```

Give these the values your **deployed** server will run with. `schema`,
`classify` and `explain` all read them, so a preview built on different values
describes a cerberus you are not going to run: `schema` would render DDL for the
wrong database, and `classify` would bound queries by the wrong per-query
budget.

Keep the ClickHouse password out of the file — export `CERBERUS_CH_PASSWORD`
instead. Add `clickhouse.protocol: http` if only port 8123 is reachable and not
the native 9000.

A key cerberus does not recognise stops it with an error naming the nearest one
that does, rather than leaving the setting silently on its default. Leave the
file out entirely and you get `localhost:9000`, database `default`, user
`default` — enough to run the offline steps, not enough to reach a real cluster.

Later steps add to this same file: step 6 adds where to measure cardinality,
step 7 adds any schema overrides, step 10 adds what to replay and against what.
Each arrives with the step that uses it.

**Post-conditions:**

- `cerberus.yaml` on disk, naming the ClickHouse and the query budget your
  server will run with.
- You know who is making the Collector change, and when.
- Nothing has changed in Prometheus, Grafana or ClickHouse. Nothing in this file
  has connected to anything — the first `migrate` command that opens a socket is
  step 6.

`cerberus config-docs` prints every engine setting the file accepts;
[`docs/configuration.md`](configuration.md) is the same list in prose, and
[`docs/migration-reference.md`](migration-reference.md) is the full flag
reference for the migration commands.

## Step 1: Install the binary

**Pre-conditions:**

- Step 0 done.
- A machine that holds — or can hold — your rule YAML and exported dashboard
  JSON. A laptop or a jump host, not the cluster: the tool reads those files off
  local disk and writes its artifacts back beside them. Your `cerberus.yaml`
  belongs there too.

**Do:**

```sh
brew install tsouza/tap/cerberus
cerberus --version
```

Only stable releases publish a cask, so `brew` never hands you a prerelease.
If you would rather not use Homebrew, the same binary ships as a
[release archive](https://github.com/tsouza/cerberus/releases) (linux / darwin ×
amd64 / arm64, each with a [SLSA](https://slsa.dev) provenance attestation). Or
run it out of the container image, whose entrypoint is `cerberus` itself:

```sh
docker run --rm -v "$PWD:/w" -w /w ghcr.io/tsouza/cerberus:<tag> \
  migrate harvest --rules rules/ --out corpus.json
```

Every path you hand the container form has to be visible inside the mount, which
is why the rest of this guide assumes a local binary.

**Post-conditions:**

- `cerberus --version` prints a version.
- Nothing anywhere else has changed.

## Step 2: Get your queries onto disk

**Pre-conditions:**

- Step 1 done.

**Do:**

Copy your Prometheus recording and alerting rule YAML to the machine, and export
your Grafana dashboards to JSON (dashboard → *Share* → *Export* → *Save to
file*, or the Grafana HTTP API). Harvesting from a live Grafana API is not a
shipped capability — export first.

**Post-conditions:**

- A directory of rule YAML and a directory of dashboard JSON, both readable
  locally. If you also run Loki, its rule YAML too.

## Step 3: Harvest your queries into a corpus

**Pre-conditions:**

- Step 2 done.
- No configuration and no network needed.

**Do:**

```bash
cerberus migrate harvest \
  --rules './prometheus/rules/*.yml' \
  --loki-rules './loki/rules/*.yml' \
  --dashboards ./grafana/dashboards \
  --out corpus.json
```

**Post-conditions:**

- `corpus.json` exists: one deterministic corpus spanning all three heads —
  Prometheus rules and panels as PromQL, Loki rules and panels as LogQL, Tempo
  panels as TraceQL — each query tagged with its language and where it came
  from.
- Everything dropped (unreadable file, unsupported datasource, empty expr) is
  counted and reported. Nothing is silently discarded, so a surprising total is
  a signal to look, not to shrug.

## Step 4: Classify the corpus

**Pre-conditions:**

- Step 3 done: `corpus.json` on disk.
- The `query.maxSamples` and `query.timeout` you wrote in step 0 are the ones
  your production server will run with — they are what bounds this preview.
  Still no network.

**Do:**

```bash
cerberus migrate classify --corpus corpus.json --json --out classify.json
```

To see the SQL a specific query becomes — useful when `classify` calls something
risky and you want to know why — add:

```bash
cerberus migrate explain --corpus corpus.json
```

**Post-conditions:**

- `classify.json` exists, with every query bucketed as *supported*,
  *unsupported* (naming the construct that failed), or supported-but-**risky**.
- You know which queries cerberus cannot express at all. Read "supported"
  precisely: the query **translates**. Whether it returns the same numbers is
  what step 10 is for.

## Step 5: Map recording-rule consumers

**Pre-conditions:**

- Step 3 done, and the rule files from step 2 still on disk.
- No configuration, no network.

**Do:**

```bash
cerberus migrate rulegraph \
  --rules './prometheus/rules/*.yml' \
  --loki-rules './loki/rules/*.yml' \
  --corpus corpus.json \
  --json --out rulegraph.json
```

**Post-conditions:**

- `rulegraph.json` links each recording rule's `record:` output to the
  dashboards and alerts that read it.
- You have the list of recorded series that must keep being materialized after
  cutover. Cerberus has no ruler, so any *consumed* recorded series that stops
  being produced turns its panel quietly blank. Rulegraph names them; keeping
  them materialized is your job, and step 12 depends on it.

## Step 6: Measure cardinality on the live Prometheus

**Pre-conditions:**

- The Prometheus you are migrating away from is running and reachable from this
  machine. This is the first step that goes on the network.

**Configure:**

Add to your `cerberus.yaml` — where to measure, and over what window:

```yaml
migrate:
  inventory:
    source: http://prometheus.internal:9090
    window: 24h
```

Add the sub-blocks below **only** for the heads your step-3 corpus actually
contains. If you harvested no LogQL, leave `loki:` out; same for `tempo:`.

```yaml
migrate:
  inventory:
    loki:
      source: http://loki.internal:3100
      selectors:
        - '{job="checkout"}'
        - '{job="cart"}'
    tempo:
      source: http://tempo.internal:3200
```

**Do:**

```bash
cerberus migrate inventory \
  --source http://prometheus.internal:9090 \
  --top 50 --json --out inventory.json
```

The `--source` flag overrides the file, so either alone is enough.

**Post-conditions:**

- `inventory.json` ranks top series and label cardinality, read from
  `/api/v1/status/tsdb`.
- You know which metrics carry the OOM risk — the one number that exists *only*
  at runtime. It ranks that risk; it does not predict cerberus's memory use.

## Step 7: Provision the ClickHouse schema

**Pre-conditions:**

- A ClickHouse you can reach with `clickhouse-client`.
- The `clickhouse.database` you wrote in step 0 is the one you will give the
  server in step 9 — otherwise the tables you create here are not the tables it
  will look for.

**Configure:**

Nothing to add for the default OTel layout. Add to your `cerberus.yaml` only
what departs from it, and give the server the same values in step 9:

| Setting                                    | When you need it                                                     |
| ------------------------------------------ | -------------------------------------------------------------------- |
| `schema.cluster`                           | You run `ON CLUSTER` DDL — the name goes into the rendered `CREATE`. |
| `schema.logs.table`, `schema.traces.table` | Your tables are not named the OTel defaults.                         |

**Do:**

```bash
cerberus migrate schema                                         # review it first
cerberus migrate schema | clickhouse-client -h clickhouse.internal --multiquery
```

`migrate schema` renders offline and opens no connection — the pipe is what
touches the database, and applying the DDL is a deliberate step you run
yourself. Its output is byte-identical to what the server applies at startup,
because it reads the same configuration. It also loads and validates the *whole*
config surface, so a typo anywhere in your `cerberus.yaml` aborts it with the
same error it would abort the server with — a cheap pre-deploy check.

**Post-conditions:**

- The tables cerberus expects exist, in the database you named.
- Nothing reads or writes them yet.

## Step 8: Start dual-write

**Pre-conditions:**

- Step 7 done.
- An OpenTelemetry Collector already collecting your telemetry.

**Do:**

The ingestion change you planned in step 0. Not a cerberus command — collector
configuration. Add the OTel Collector's
[ClickHouse exporter](../README.md#version-requirements) pointed at the database
from step 7, and **keep the existing Prometheus write path running**. For a
while, data flows into both.

**Post-conditions:**

- The same telemetry lands in Prometheus and in ClickHouse.
- Note the wall-clock time. That overlap is what makes a real before/after
  comparison possible, and step 10's `--start` cannot reach back before it. You
  never cut over cold.

## Step 9: Deploy cerberus against ClickHouse

**Pre-conditions:**

- Step 8 done, with enough data accumulated to cover the window you intend to
  verify over.
- The server gets the `clickhouse` and `schema` settings from your
  `cerberus.yaml`, unchanged — same database, same overrides, or it reads
  different tables from the ones step 7 created. The `migrate` keys are for the
  tool and mean nothing to the server; leaving them in is harmless. Running the
  server is its own subject: [`docs/operations.md`](operations.md).

**Do:**

Deploy the binary, container image or Helm chart and wait for `/readyz`.

**Post-conditions:**

- One endpoint serving the Prometheus, Loki and Tempo APIs, reading ClickHouse.
- No writer points at it. Nothing in Grafana points at it yet either.

## Step 10: Verify parity

This is the parity gate, and it is the only thing that earns you the flip.

**Pre-conditions:**

- Steps 3 and 9 done.
- Dual-write open across the entire `--start`..`--end` window you ask for — the
  overlap you started at step 8 is the hard limit on how far back you can look.

**Configure:**

Add to your `cerberus.yaml`: what to replay, and the two backends to replay it
against. `corpus.json` is the file step 3 produced.

```yaml
migrate:
  verify: # alongside the `inventory:` block from step 6
    corpus: corpus.json
    ref: http://prometheus.internal:9090
    cerberus: http://cerberus.internal:8080
    start: -6h
    end: now
    step: 60s
```

Leave the window out and you replay `-1h`→`now` at `60s`. Widen it as far as
your dual-write overlap allows — a longer window is more evidence.

One `ref` / `cerberus` pair per head your corpus contains — PromQL at the top
level, LogQL and TraceQL in sub-blocks. Add these only for the heads you
harvested, and when you do add one it is **both sides or neither**: half a pair
is an error, never a silently skipped head.

```yaml
migrate:
  verify:
    loki:
      ref: http://loki.internal:3100
      cerberus: http://cerberus.internal:8080
    tempo:
      ref: http://tempo.internal:3200
      cerberus: http://cerberus.internal:8080
```

Credentials stay out of the file. Export a bearer token per URL above —
`CERBERUS_VERIFY_REF_TOKEN`, `..._CERBERUS_TOKEN`, `..._REF_LOKI_TOKEN`,
`..._CERBERUS_LOKI_TOKEN`, `..._REF_TEMPO_TOKEN`, `..._CERBERUS_TEMPO_TOKEN`. A
multi-tenant reference Loki or Tempo also wants
`CERBERUS_VERIFY_REF_LOKI_ORG_ID` / `CERBERUS_VERIFY_REF_TEMPO_ORG_ID`, which
send `X-Scope-OrgID`; cerberus itself reads no tenant header, deliberately, so
there is no cerberus-side equivalent.

Two settings you will only want once a run tells you so:
`migrate.verify.tolerance`, if two samples may differ by more than the default
`1e-9` and still match — a deliberate decision, never a way to paper over a real
difference — and `migrate.verify.report`, to write the full JSON diagnostics to
a file on a failing run.

**Do:**

```bash
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
```

It replays every corpus query against the reference backend **for its own
head** — PromQL against Prometheus, LogQL against Loki, TraceQL against Tempo —
and against cerberus over the same window, then diffs the two results.

Run it, fix each divergence at the source, run it again.

> If the cerberus you verify against has the experimental native ClickHouse
> aggregates enabled (`timeSeries*ToGrid`, auto-selected on CH 25.9+), a
> sub-observable last-bit rounding difference can surface as a `diverge`. Verify
> against the exact configuration you intend to run — see the
> [exactness-vs-scale tradeoff](performance.md#native-rate-exactness-vs-scale-should-i-enable-it).
> Raise `--tolerance` only as a deliberate decision, never to paper over a real
> difference.

**Post-conditions:**

- `verify.json` exists, with every query landed as `match`, `diverge`,
  `undecidable`, `unsupported` or `error`, rolled up per head *and* per
  `(head, shape)` family — because a Loki lane that diffed 412 log entries has
  not thereby proved its metric panels.
- Every family carries a `compared` count in its own unit (series, log entries,
  traces, spans, tag values). That count is the only number that is evidence:
  two empty results agree, so a family can be all matches while having diffed
  nothing at all.
- **You are done when the exit code is 0** — meaning the diverge count is zero,
  every head is configured, and every family compared something. `verify` exits
  **2** if a query diverges or errors, if a head with replayable queries had no
  backend pair, if the run replayed nothing, or if any family compared nothing.
  Divergence is never allow-listed and absence of evidence never counts as
  passing.
- On a failing run, `verify-diagnostics.json` holds full machine-readable
  diagnostics with a copy-pasteable repro command, backend URLs and credentials
  redacted. That is the file to attach to a bug report.

The comparators, the dimensions no comparator can judge, and the pinned replay
limits are all in the
[migration reference](migration-reference.md#how-verify-decides-two-results-agree).

## Step 11: Take the go/no-go

**Pre-conditions:**

- `verify.json`, `classify.json` and `rulegraph.json` on disk — all three are
  required, and a missing one blocks. `inventory.json` is optional.
- No configuration, no network.

**Do:**

```bash
cerberus migrate gate \
  --verify verify.json \
  --classify classify.json \
  --rulegraph rulegraph.json \
  --inventory inventory.json
```

`gate` is a pure-offline aggregator: it folds the JSON the earlier stages
emitted into **one** PASS/FAIL verdict with a per-stage checklist. It refuses
(exit **3**) rather than warning:

- **`verify`** blocks on any divergence or error, on a run that replayed
  nothing, on a family that compared nothing, and on a replayable query whose
  head had no backend pair.
- **`classify`** blocks on any unsupported query, and on classifying nothing at
  all. Risky queries only warn.
- **`rulegraph`** blocks on any *consumed* recorded series, and on a consumer
  expression it could not parse.
- **`inventory`** never blocks. High cardinality warns, on every head.

**Post-conditions:**

- Exit **0** — and only exit 0 — means you are cleared to cut over.
- Exit **3** means you are not, and the checklist names which stage said so. Fix
  it and rerun from the affected step.

## Step 12: Cut over

**Pre-conditions:**

- Step 11 exited 0.
- Every *consumed* recorded series step 5 named is still being materialized by
  something — cerberus will not do it for you.

**Do:**

An operator action, not a command:

- Point the Grafana Prometheus **datasource URL** at cerberus, or swap DNS / the
  service in front of it. Dashboards and alert rules are unchanged — that is the
  whole point.
- Flip read-path panels first. Leave anything that **pages** for last, once you
  have watched the read path stay green.
- Keep dual-write running.

**Post-conditions:**

- Grafana reads from cerberus.
- Prometheus is still being written to, and is still your rollback.

## Step 13: Decommission Prometheus storage

**Pre-conditions:**

- Your ClickHouse retention now covers the longest window your dashboards and
  alerts look back over. That runway is your rollback; until it exists, do not
  take this step.

**Do:**

Stop the Prometheus write and storage path. Not urgent — there is no prize for
doing it early.

**Post-conditions:**

- One storage backend. Whatever evaluates your recording and alerting rules is
  still running, because cerberus has no ruler.

## The whole thing as one script

Steps 3–6, 7 and 10–11 are the commands. Here they are in one paste:

```bash
# ── ASSESS (steps 3–6) ── what have you got, and how much lands cleanly? ──
cerberus migrate harvest \
  --rules './prometheus/rules/*.yml' \
  --loki-rules './loki/rules/*.yml' \
  --dashboards ./grafana/dashboards \
  --out corpus.json

cerberus migrate classify --corpus corpus.json --json --out classify.json

cerberus migrate rulegraph \
  --rules './prometheus/rules/*.yml' \
  --loki-rules './loki/rules/*.yml' \
  --corpus corpus.json \
  --json --out rulegraph.json

cerberus migrate inventory \
  --source http://prometheus.internal:9090 \
  --top 50 --json --out inventory.json

# ── VALIDATE (step 7) ── create the tables cerberus expects ──────────────
cerberus migrate schema | clickhouse-client -h clickhouse.internal --multiquery

# ── VERIFY (step 10) ── replay every query against both backends and diff ─
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

# ── DECIDE (step 11) ── fold it into one go/no-go. Exit 0 = cleared ──────
cerberus migrate gate \
  --verify verify.json \
  --classify classify.json \
  --rulegraph rulegraph.json \
  --inventory inventory.json
```

For every flag and every exit code, see the
[migration reference](migration-reference.md).

## What the tool will not tell you

Being honest about the blind spots is the point of the tool.

- **Cardinality is runtime, not config.** A query whose shape looks fine can
  still exhaust memory on a metric with millions of label combinations. Step 6
  ranks that risk; it does not predict cerberus's memory.
- **Translate ≠ match.** Step 4 proving a query *supported* proves it
  translates. Only step 10 proves the numbers agree.
- **A green gate must rest on evidence, not silence.** A match over two empty
  results, a lane whose every query was unsupported, and an all-out-of-scope
  corpus all look like "no divergences" — and all three block.
- **A limitation is not a tolerance.** Where two backends cannot be compared on
  some dimension, the report names it and counts what it covers. It never widens
  what "equal" means, and if a limitation swallowed a whole result, the evidence
  count is zero and the run blocks.
- **The gates refuse; they don't warn.** There is no escape hatch and no
  per-head opt-out. The honest way to narrow the gate is to harvest a narrower
  corpus, which is visible and diffable.

Anything the tool cannot resolve — an unreadable file, an unsupported-datasource
panel, an unparseable expression — is counted and reported, never silently
skipped.

## Keep verifying

Migration is not one-shot. The scheduled **Layer 14** end-to-end lane runs this
whole journey — harvest → explain → classify → rulegraph → schema → verify →
gate — against real ClickHouse and a real reference Prometheus across eight
archetypes. Its design and the tier plan live in
[`docs/migration-testing.md`](migration-testing.md).

## Where to go next

- [Migration reference](migration-reference.md) — every flag, every comparator,
  every exit code, and what each gate blocks on.
- [`docs/operations.md`](operations.md) — the runtime contract once you are on
  cerberus: configuration, lifecycle, scaling.
