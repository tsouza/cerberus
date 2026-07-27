# Migrating to cerberus

Move a Prometheus-backed setup onto cerberus (ClickHouse) **without rebuilding
dashboards or rewriting alerts** — and prove what changes *before* real traffic
depends on it.

The work is driven by `cerberus migrate`, a command group in the release
binary. It is read-only: it never writes to Prometheus, Grafana or ClickHouse.
The only two steps that change anything — applying the schema and flipping the
datasource — are ones **you** run by hand.

## Install the binary

```sh
brew install tsouza/tap/cerberus
cerberus --version
```

Put it on the machine that holds your rule YAML and exported dashboard JSON —
a laptop or a jump host, not the cluster. The tool reads those files off local
disk and writes its artifacts back beside them, and everything except `verify`,
`inventory` and `rulegraph` runs fully offline.

Only stable releases publish a formula, so `brew` never hands you a prerelease.
If you would rather not use Homebrew, the same binary ships as a
[release archive](https://github.com/tsouza/cerberus/releases) (linux / darwin ×
amd64 / arm64, each with a [SLSA](https://slsa.dev) provenance attestation). Or
run it out of the container image, whose entrypoint is `cerberus` itself:

```sh
docker run --rm -v "$PWD:/w" -w /w ghcr.io/tsouza/cerberus:<tag> \
  migrate harvest --rules rules/ --out corpus.json
```

Every path you hand the container form has to be visible inside the mount,
which is why the rest of this guide assumes a local binary.

## What you need in place

1. **ClickHouse receiving your telemetry** through the OpenTelemetry
   Collector's ClickHouse exporter — the same OTel-shaped tables cerberus reads
   (see the [version requirements](../README.md#version-requirements)).
2. **A dual-write window.** For a while, data flows into Prometheus *and*
   ClickHouse at once. That overlap is what makes a real before/after
   comparison possible. You never cut over cold.
3. **Your queries as files** — Prometheus recording/alerting rule YAML and
   exported Grafana dashboard JSON. Harvesting from a live Grafana API is not a
   shipped capability; export the dashboards first.

### Two things cerberus does not do

It has **no ruler**. It answers the PromQL Grafana sends it, but it does not
evaluate your recording and alerting rules on a schedule — so whatever
evaluates them today has to keep doing so.

It **does not ingest**. Your OpenTelemetry Collector keeps writing into
ClickHouse; you point no writer at cerberus.

Get either wrong and you cut over to blank panels. It is also why this
migration is organised around your **real queries** — the PromQL in rules and
panels — rather than around `prometheus.yml`. A config file cannot tell you
whether a query translates cleanly or falls over on cardinality. Only the
queries and the live data can.

## The whole journey, as one script

```text
ASSESS ─▶ VALIDATE ─▶ VERIFY ─▶ DECIDE ─▶ CUT OVER ─▶ DECOMMISSION
```

Here it is end to end. The rest of this guide is one section per block.

```bash
# ── ASSESS ── what have you got, and how much of it lands cleanly? ─────
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

# ── VALIDATE ── create the tables cerberus expects ────────────────────
cerberus migrate schema | clickhouse-client -h clickhouse.internal --multiquery

# ── VERIFY ── replay every query against both backends and diff ───────
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

# ── DECIDE ── fold it all into one go/no-go. Exit 0 = cleared ─────────
cerberus migrate gate \
  --verify verify.json \
  --classify classify.json \
  --rulegraph rulegraph.json \
  --inventory inventory.json
```

The first two blocks run today, before cerberus is even provisioned. `verify`
and `gate` need the dual-write window live. The last two stages are operator
actions, not commands — the tool stops at the go/no-go and hands you the flip.

For every flag, every exit code and the exact rules each gate applies, see the
[migration reference](migration-reference.md).

## Assess

Four commands, in this order. All offline except `inventory`.

**`harvest`** collapses every rule file and exported dashboard into one
deterministic `corpus.json` spanning all three heads: Prometheus rules and
panels as PromQL, Loki rules (`--loki-rules`) and panels as LogQL, Tempo panels
as TraceQL — each query tagged with its language and where it came from.
Anything dropped (unreadable file, unsupported datasource, empty expr) is
counted and reported; nothing is silently discarded.

**`classify`** buckets each query as *supported*, *unsupported* (naming the
construct that failed), or supported-but-**risky**. Read "supported" precisely:
the query **translates**. Whether it returns the same numbers is what `verify`
is for.

**`rulegraph`** links each recording rule's `record:` output to the dashboards
and alerts that read it. Cerberus has no ruler, so any *consumed* recorded
series must keep being materialized after cutover or the panel reading it goes
quietly blank. Rulegraph names exactly which ones; keeping them materialized is
your job.

**`inventory`** is the one that goes on the network. It reads the live
Prometheus's `/api/v1/status/tsdb` and ranks top series and label cardinality —
the number that drives OOM risk, and the one number that exists *only* at
runtime. It ranks risk; it does not predict cerberus's memory use.

Want to see the SQL a query becomes? `cerberus migrate explain --corpus
corpus.json` dry-runs each one through the read pipeline and prints it — useful
when `classify` calls something risky and you want to see why.

## Validate

Render the tables cerberus expects, offline, with no database connection. The
output is byte-identical to what the server applies at startup — it reads the
same `CERBERUS_*` environment — so it pipes straight into `clickhouse-client`:

```bash
cerberus migrate schema | clickhouse-client -h clickhouse.internal --multiquery
```

Applying the DDL is a deliberate step you run yourself. The tool only renders
it.

## Verify

This is the parity gate, and it is the only thing that earns you the flip.

Over the dual-write window, `verify` replays every corpus query against the
reference backend **for its own head** — PromQL against Prometheus, LogQL
against Loki, TraceQL against Tempo — and against cerberus over the same
window, then diffs the two results. You configure one backend pair per head
your corpus actually contains; supplying half a pair is a usage error, never a
silently skipped head.

Each query lands as `match`, `diverge`, `undecidable`, `unsupported` or
`error`, and the report rolls up per head *and* per `(head, shape)` family —
because a Loki lane that diffed 412 log entries has not thereby proved its
metric panels. Every family also carries a `compared` count in its own unit
(series, log entries, traces, spans, tag values). That count is the only number
that is evidence: two empty results agree, so a family can be all matches while
having diffed nothing at all.

`verify` exits **2** if a query diverges or errors, if a head with replayable
queries had no backend pair, if the run replayed nothing, or if any family
compared nothing. Divergence is never allow-listed and absence of evidence
never counts as passing.

Run it, fix each divergence at the source, run it again. **You are done when
the diverge count is zero, with every head configured and every family
comparing something.** That is your permission to flip traffic — not a leap of
faith.

On a failing run, add `--report diagnostics.json`: full machine-readable
diagnostics with a copy-pasteable repro command, backend URLs and credentials
redacted. That is the file to attach to a bug report.

> If the cerberus you verify against has the experimental native ClickHouse
> aggregates enabled (`timeSeries*ToGrid`, auto-selected on CH 25.9+), a
> sub-observable last-bit rounding difference can surface as a `diverge`.
> Verify against the exact configuration you intend to run — see the
> [exactness-vs-scale tradeoff](performance.md#native-rate-exactness-vs-scale-should-i-enable-it).
> Raise `--tolerance` only as a deliberate decision, never to paper over a real
> difference.

The comparators, the dimensions no comparator can judge, and the pinned replay
limits are all in the [migration reference](migration-reference.md#how-verify-decides-two-results-agree).

## Decide

`gate` is a pure-offline aggregator: it reads the JSON the other stages emitted
and folds them into **one** PASS/FAIL verdict with a per-stage checklist. It
refuses (exit **3**) rather than warning:

- **`verify`** blocks on any divergence or error, on a run that replayed
  nothing, on a family that compared nothing, and on a replayable query whose
  head had no backend pair.
- **`classify`** blocks on any unsupported query, and on classifying nothing at
  all. Risky queries only warn.
- **`rulegraph`** blocks on any *consumed* recorded series, and on a consumer
  expression it could not parse.
- **`inventory`** never blocks. High cardinality warns, on every head.

`verify`, `classify` and `rulegraph` are required artifacts, so a missing one
blocks too. Exit 0 — and only exit 0 — means you are cleared to cut over.

## Cut over

An operator action, not a command:

- Point the Grafana Prometheus **datasource URL** at cerberus, or swap DNS /
  the service in front of it. Dashboards and alert rules are unchanged — that
  is the whole point.
- Flip read-path panels first. Leave anything that **pages** for last, once you
  have watched the read path stay green.
- Keep dual-write running as a safety net.

## Decommission

Also manual, and not urgent. Keep Prometheus's write and storage path until
your ClickHouse retention covers the longest window your dashboards and alerts
look back over. That runway is your rollback. Only then retire the old storage.

## What the tool will not tell you

Being honest about the blind spots is the point of the tool.

- **Cardinality is runtime, not config.** A query whose shape looks fine can
  still exhaust memory on a metric with millions of label combinations.
  `inventory` ranks that risk; it does not predict cerberus's memory.
- **Translate ≠ match.** `classify` proving a query *supported* proves it
  translates. Only `verify` proves the numbers agree.
- **A green gate must rest on evidence, not silence.** A match over two empty
  results, a lane whose every query was unsupported, and an all-out-of-scope
  corpus all look like "no divergences" — and all three block.
- **A limitation is not a tolerance.** Where two backends cannot be compared on
  some dimension, the report names it and counts what it covers. It never
  widens what "equal" means, and if a limitation swallowed a whole result, the
  evidence count is zero and the run blocks.
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
