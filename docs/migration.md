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
disk and writes its artifacts back beside them, and everything except `verify`
and `inventory` runs fully offline.

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

## Configure it

There is **one** configuration surface, and it is the same one the cerberus
server reads: `CERBERUS_*` environment variables. No migration-specific config
file, no `--config` flag. Whatever the environment leaves unset falls back to an
optional `cerberus.yaml` (working directory first, then `/etc/cerberus/`), then
to a built-in default. `cerberus config-docs` prints the whole surface from the
binary itself; [`docs/configuration.md`](configuration.md) is the same list in
prose.

### None of it points the tool at ClickHouse

That surprises people, so it is worth being blunt: **no `migrate` subcommand
ever connects to ClickHouse.** `schema` renders DDL to stdout for *you* to
apply, everything else works off local files, and the two commands that do open
a socket talk to your existing Prometheus / Loki / Tempo — never to the
database.

| Command     | Opens a connection to                            | Reads from your environment                                |
| ----------- | ------------------------------------------------ | ---------------------------------------------------------- |
| `harvest`   | nothing                                          | nothing                                                    |
| `classify`  | nothing                                          | the query budget, so previews run under production rules   |
| `explain`   | nothing                                          | the query budget, plus `CERBERUS_SCHEMA_*` table names     |
| `rulegraph` | nothing                                          | nothing                                                    |
| `schema`    | nothing — it prints DDL, you pipe it             | `CERBERUS_CH_DATABASE` and `CERBERUS_SCHEMA_*`             |
| `inventory` | your live Prometheus (and Loki / Tempo if asked) | `CERBERUS_INVENTORY_*`                                     |
| `verify`    | both backends, one pair per head                 | `CERBERUS_VERIFY_*`                                        |
| `gate`      | nothing                                          | nothing                                                    |

So the whole assess stage — `harvest`, `classify`, `rulegraph` — needs **no
configuration at all**. Run it on a laptop with the wifi off. The three stages
that do need something are below, in the order you will hit them.

### `schema` — the shape of your tables

`migrate schema` renders exactly the DDL your cerberus server would apply at
startup, from the same environment. Give it the same values you intend to give
the server, or the tables you create will not be the tables it looks for:

```sh
export CERBERUS_CH_DATABASE=otel           # server default is "default"
export CERBERUS_SCHEMA_CLUSTER=my_cluster  # only if you run ON CLUSTER DDL
cerberus migrate schema
```

It loads and validates the entire config surface, so a typo anywhere in your
`CERBERUS_*` set aborts it with the same error it would abort the server with —
which makes this a cheap way to sanity-check that set before you deploy. The
connection knobs (`CERBERUS_CH_ADDR`, username, password) play no part: nothing
connects, and *where* the DDL lands is decided entirely by the
`clickhouse-client` invocation you pipe into.

### `inventory` — your live Prometheus

A read against `/api/v1/status/tsdb` on the Prometheus you are migrating away
from. Point it there:

```sh
export CERBERUS_INVENTORY_SOURCE=http://prometheus.internal:9090
export CERBERUS_INVENTORY_WINDOW=24h
# only if you also harvested Loki or Tempo queries
export CERBERUS_INVENTORY_LOKI_SOURCE=http://loki.internal:3100
export CERBERUS_INVENTORY_LOKI_SELECTORS='{job="app"}'
export CERBERUS_INVENTORY_TEMPO_SOURCE=http://tempo.internal:3200
```

### `verify` — two URLs per head

The only stage with real setup, and the reason for the [prerequisites
above](#what-you-need-in-place). For **each head your corpus contains**, `verify`
needs a pair: the reference backend, and the cerberus meant to replace it.

| Head    | Reference backend           | Cerberus                         |
| ------- | --------------------------- | -------------------------------- |
| `prom`  | `CERBERUS_VERIFY_REF`       | `CERBERUS_VERIFY_CERBERUS`       |
| `loki`  | `CERBERUS_VERIFY_REF_LOKI`  | `CERBERUS_VERIFY_CERBERUS_LOKI`  |
| `tempo` | `CERBERUS_VERIFY_REF_TEMPO` | `CERBERUS_VERIFY_CERBERUS_TEMPO` |

The three cerberus URLs are normally the *same* address — one process serves all
three APIs. Configure only the heads you harvested; half a pair is a usage
error, never a silently skipped head. Bearer tokens and multi-tenant
`X-Scope-OrgID` values have their own variables — see the
[reference](migration-reference.md#verify-backends).

By this point cerberus itself has to be deployed, configured against ClickHouse
and serving. That is a *server* configuration job, and it is
[`docs/operations.md`](operations.md)'s subject, not this tool's.

### Flags or env, your choice

Every variable above has an equivalent flag, and the flag wins. Use env when you
are scripting the run, flags when you are exploring — the transcript below uses
flags so each command reads on its own. The exception is the output flags
(`--json`, `--out`, `--top`): those have no env fallback and stay on the command
line.

### What has to be standing, and when

| Before you run | This must already exist                                                                                                                 |
| -------------- | --------------------------------------------------------------------------------------------------------------------------------------- |
| assess         | your rule YAML and exported dashboard JSON, on local disk                                                                               |
| `inventory`    | the Prometheus you are migrating away from — still running                                                                              |
| `schema`       | a ClickHouse you can reach with `clickhouse-client`                                                                                     |
| `verify`       | ClickHouse filling via the OTel Collector, **and** cerberus deployed against it, **and** dual-write open long enough to cover `--start` |
| `gate`         | the JSON files the stages above wrote                                                                                                   |

## The whole journey, as one script

```text
ASSESS ─▶ VALIDATE ─▶ VERIFY ─▶ DECIDE ─▶ CUT OVER ─▶ DECOMMISSION
```

Here it is end to end. The rest of this guide is one section per block.

```bash
# ── ASSESS ── what have you got, and how much of it lands cleanly? ─────
# needs: your files on disk. No config, no network — except `inventory`,
# which reads the Prometheus you are leaving.
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
# needs: CERBERUS_CH_DATABASE / CERBERUS_SCHEMA_* set to whatever you will
# give the server, and a ClickHouse you can reach with clickhouse-client.
cerberus migrate schema | clickhouse-client -h clickhouse.internal --multiquery

# ── VERIFY ── replay every query against both backends and diff ───────
# needs: cerberus deployed against ClickHouse and serving, both backends
# reachable, and dual-write open across the whole --start..--end window.
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
# needs: the four JSON files above. Offline again.
cerberus migrate gate \
  --verify verify.json \
  --classify classify.json \
  --rulegraph rulegraph.json \
  --inventory inventory.json
```

The first two blocks run today, before cerberus is even provisioned — one needs
nothing but your files, the other a reachable ClickHouse. `verify` is the step
that needs the full stack standing. `gate` is offline again, and only reads what
the others wrote. The last two stages are operator actions, not commands — the
tool stops at the go/no-go and hands you the flip.

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
your corpus actually contains (see [Configure it](#configure-it) for the flags
and their env equivalents); supplying half a pair is a usage error, never a
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
