# Migration-scenario end-to-end testing (Layer 14)

This is the design for the scheduled end-to-end lane that turns every one of
the 26 migration user-stories into an executable scenario, `MIG-01 … MIG-26`.
It sits above [Layer 13](test-strategy.md#layer-13--live-stack-chaos-real-faults)
in the [14-layer test map](test-strategy.md) as the most-realistic, slowest
layer — it asserts an **operator-workflow** contract (the journey a team takes
to move onto cerberus) rather than a code contract.

The lane drives the merged `cerberus migrate` CLI documented in
[`migration.md`](migration.md) against real ClickHouse and a real reference
Prometheus, over eight migration archetypes. The alert-firing and
recording-rule-write-back scenarios go **beyond** the `cerberus migrate` tool's
documented v1 scope (which is read-only and does query-result parity, not
alert-firing parity): they stand up a real external ruler as lab
infrastructure. Layer 14 is a harness around the whole operator journey, not
a claim that the `cerberus migrate` tool itself grew those capabilities.

The harness code, compose files, seeders, and workflow land in follow-up build
PRs in the [phase order](#8-phased-build-order) below. This document is the
canonical anchor: the 26 stories in [section 4](#4-the-26-migration-user-stories)
are the source of truth the coverage ratchet diffs against.

## 1. Goal and placement

**Goal.** Each migration user-story becomes one executable scenario driving the
merged `cerberus migrate` CLI plus the real dual-backend / ruler stacks the parity
stories require. The lane proves the operator journey end-to-end: harvest →
explain → classify → rulegraph → schema → verify → gate → cut over →
decommission, against real ClickHouse and a real reference Prometheus.

**Why scheduled, not a PR gate.** The heavier tiers stand up multi-container
stacks (Prometheus + OTel collector + ClickHouse + cerberus, plus a shadow
ruler + Alertmanager) and seed minutes of rolling telemetry with target
restarts. That is far too heavy and too slow to sit on the required PR path. It
runs on the **same trigger posture as the existing `dashboard` lane**: nightly
`schedule` cron + `workflow_dispatch` + **informational on push-to-main**, and
never on `pull_request`, so it is not a branch-protection check. Informational
does **not** mean tolerated — a red migration lane is a real failure to fix or
revert from main, exactly as the [CI-gate inventory](test-strategy.md#ci-gates)
already says of every informational lane.

**Placement — new Layer 14.** It slots directly above Layer 13 (live-stack
chaos): the slowest layer, asserting a workflow contract rather than a code
contract. `docs/test-strategy.md` carries a single at-a-glance row pointing
here; the full scenario map, tier design, and workflow shape live in this
document.

## 2. The `cerberus migrate` CLI surface this lane drives

Every scenario composes only the merged CLI. There are **eight subcommands**
(`harvest`, `explain`, `classify`, `rulegraph`, `verify`, `inventory`, `gate`,
`schema`). The exact flags each scenario relies on:

| Command                      | Flags this lane uses                                                                                                                                                                                                                                                | Output                                                      | Network                               |
| ---------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------- | ------------------------------------- |
| `cerberus migrate harvest`   | `--rules <paths/globs>`, `--dashboards <dir>`, `--out <file>`                                                                                                                                                                                                       | corpus JSON (stdout or `--out`)                             | offline                               |
| `cerberus migrate explain`   | `--corpus <file>` (or `--rules`/`--dashboards`), `--out <file>`                                                                                                                                                                                                     | text explain report; no `--json`                            | offline                               |
| `cerberus migrate classify`  | `--corpus <file>` (or `--dashboards`), `--json`, `--out <file>`                                                                                                                                                                                                     | classification ledger                                       | offline                               |
| `cerberus migrate rulegraph` | `--rules <paths/globs>`, `--corpus <file>`, `--json`, `--out <file>`                                                                                                                                                                                                | dependency graph                                            | offline                               |
| `cerberus migrate verify`    | `--corpus <file>`, one pair per head (`--ref`/`--cerberus`, `--ref-loki`/`--cerberus-loki`, `--ref-tempo`/`--cerberus-tempo`) + their `-token` / `-org-id` variants, `--start`, `--end`, `--step`, `--tolerance <eps>`, `--json`, `--report <file>`, `--out <file>` | parity report; non-zero exit on divergence                  | live (2 backends per configured head) |
| `cerberus migrate inventory` | `--source <url>`, `--top <n>`, `--window <dur>`, `--json`, `--out <file>`                                                                                                                                                                                           | inventory (stdout or `--out`)                               | live (one Prometheus)                 |
| `cerberus migrate gate`      | `--verify`, `--classify`, `--inventory`, `--rulegraph`, `--json`, `--out <file>`                                                                                                                                                                                    | fold decision; non-zero exit on a blocking stage            | offline                               |
| `cerberus migrate schema`    | *(no flags; reads `CERBERUS_*`)*                                                                                                                                                                                                                                    | `CREATE` statements from `CERBERUS_*` env                   | offline                               |

Two capability facts shape the scenarios below and are the reason several cells
differ from a naive reading:

- **`verify` is strictly two-way per head.** Each head lane takes exactly one
  reference backend and one cerberus endpoint and diffs their results; a run may
  configure up to three such lanes (`prom`, `loki`, `tempo`) but never a third
  backend *within* a lane. There is no oracle leg. Any scenario that wants a
  semantic oracle for a non-Prometheus source composes **two two-way runs**
  feeding both backends identical synthetic data (see
  [section 5](#5-comparison-modes--the-honesty-contract)).
- **`verify --tolerance` is a single flat absolute epsilon.** It is the
  definition of "the same float", not a counter-aware or downsample-aware mode,
  and it is shared by every head lane so no lane can be judged loosely out of
  view. It has no per-shape sibling: a log line, a stream label set, a trace ID,
  a span set and a timestamp have no float axis, so those are compared exactly.
  A structural,
  counter-aware long-range delta (downsampling) therefore cannot live inside
  `verify`; it needs a dedicated harness comparator outside the zero-diverge
  gate.

> **Harness comparator — reuse, don't reinvent.** The verify-tier scenarios
> drive the shipped `verify` command, whose differential engine is
> `internal/migrateverify` (`Compare` / `Verify`). That package **is** the
> single in-process Go comparator, shared by the operator's `verify` command and
> this lane — do not write a second comparator inside the harness. This is
> deliberately kept separate from the CI compatibility gate
> (`compatibility/{prometheus,loki,tempo}`), which grades cerberus with the
> **upstream `promql-compliance-tester`** as an *independent* compliance oracle.
> Two comparators exist on purpose: cerberus's own (operator + this migration
> lane) and an outside oracle (compliance) — never fold the compat gate onto
> cerberus's own comparator, or it would grade cerberus with cerberus's code.

## 3. Three infrastructure tiers

**Tier 0 — offline fixtures (no backend).** Pure `cerberus migrate` CLI over checked-in
rule files, exported Grafana dashboard JSON, and canned corpus/schema fixtures.
No Docker, no network, no ClickHouse — air-gap-faithful, seconds to run. Drives
`harvest`, `explain`, `classify`, `rulegraph`, `--schema` (render), and `gate`
(the pure aggregator). Cheap enough that it *may* also run per-PR later, but it
ships informational-first.

**Tier 1 — dual-backend compose.**
`test/e2e/migration/tiers/tier1-dual/docker-compose.dual.yml`: reference
**Prometheus** + reference **Loki** + reference **Tempo**, alongside an **OTel
collector** (clickhouseexporter → the OTel-shaped tables cerberus reads) **+
ClickHouse + cerberus**. All three signals live in one stack because an
unconfigured head is a *blocking* verify verdict: a metrics-only substrate
fails the moment the corpus carries one LogQL or TraceQL query. cerberus serves
all three heads on one address, mirroring the operator journey where every
`--cerberus*` URL is the same endpoint and only the `--ref*` URLs differ.

Both sides are fed the *same* fixture over one window: the seeder builds one
in-memory fixture and writes it twice — direct `INSERT` into ClickHouse, and
push into each reference backend. This is the only place ground truth exists
for a differential diff. Drives `inventory` (probe live Prometheus), `verify`
(diff both backends), and the live half of schema/label/histogram/retention
validation.

Two roles are split, not merged. The collector's clickhouseexporter
(`create_schema: true`) is the **sole schema authority** — cerberus runs with
auto-create off, so its readiness probe is a live drift detector between the
exporter's table names and cerberus's read-side defaults. The seeder owns
**data only**, and waits for the exporter's tables before it inserts.

The reference Loki and Tempo configurations are the compatibility harnesses'
own files, bind-mounted read-only: the tree holds exactly one reference config
per signal, so no second definition exists to drift. The reference image tags
are likewise single-sourced — the compatibility compose files are the pin site,
tier-1 restates them, and `test/regression/fork_version_skew_test.go` asserts
the restatement is byte-identical, which transitively binds tier-1 to the
`go.mod` parser versions. The ClickHouse tag tracks `quickstart_clickhouse`
rather than `chdb_substrate`: the migration lane models the deployment surface
an operator runs, not the SQL-parity substrate the chDB suites run on
(asserted by `.github/scripts/clickhouse-version-sync.mjs`).

The stack is named per checkout. Docker Compose scopes containers, networks and
volumes by project name, so two checkouts that resolve the same name are one
stack to the daemon: `up` adopts the other's containers and `down -v
--remove-orphans` destroys them, silently and with a green exit code. Every
compose file in the tree therefore spells its project name
`<stable-base>${COMPOSE_PROJECT_SUFFIX:-}`, and `scripts/compose-project-suffix.sh`
derives that variable from the checkout's own path — empty in a primary checkout
(and so in CI, where the project name is exactly its stable base), a short path
hash in a linked worktree. The Justfile exports it to every recipe, `.envrc`
exports it to an interactive shell, and each compatibility harness script
derives it too, so a stack is isolated however it is launched rather than by
each launcher remembering a flag. `test/regression/compose_project_isolation_test.go`
pins the shape, including the absence of any `container_name`, which would
escape project scoping and collide however the project is named.

### 3.0. Which cerberus the lane proves

Every tier job resolves its cerberus through one module,
`.github/scripts/migration-artifact.mjs`, before any scenario runs. Two workflow
inputs travel together and decide the answer:

| `cerberus_image` | `expect_version`   | What the lane proves                                                                                                                                                 |
| ---------------- | ------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| unset            | unset              | **This tree.** The CLI is `go build`-ed from source; the compose stacks run `cerberus:migration-tier1` (plus this checkout's suffix), built from `Dockerfile.local`. |
| a tag            | that tag's version | **That artifact.** The image is pulled, the CLI is `docker cp`-ed out of it, and the compose stacks run the same image.                                              |
| one of the two   | —                  | An error.                                                                                                                                                            |

A half pair is rejected rather than defaulted, because the failure it produces
is invisible: the job would build from source while its name, its logs, and the
roll-up all say it exercised a released artifact.

The CLI comes **out of the image** rather than out of a release tarball, so on
the artifact path the binary the scenarios exec and the server the stack answers
from are the same bytes. A tarball would leave the two free to diverge, and the
30 scenarios would pass across a mixed pair.

Whatever binary the module produced is then held to a version stamp before the
lane proceeds: it runs `--version`, requires exit 0 and exactly one non-empty
line, and string-compares the result. The three build paths stamp differently by
construction, which is what makes the comparison a real discriminator:

| Build                                      | Stamp               | Read back by               |
| ------------------------------------------ | ------------------- | -------------------------- |
| `go build ./cmd/cerberus` (no ldflags)     | `dev`               | `lib.SourceBuildVersion`   |
| `Dockerfile.local` (`-X main.Version=e2e`) | `e2e`               | `lib.LocalImageVersion`    |
| goreleaser                                 | the release version | the `expect_version` input |

The Go side reads the same stamps from `test/e2e/migration/lib/provenance.go`.
`lib.BuildCerberus` probes the CLI it hands each tier suite, and the tier-1
substrate test probes the *server* over `GET /info`, so the two halves of the
stack are checked independently — on a from-source run they legitimately
disagree (`dev` CLI, `e2e` server), and only the artifact path collapses them
onto one version. `CERBERUS_EXPECT_VERSION` is therefore exported only on the
artifact path; on the source path each half falls back to its own stamp.

The stacks are pinned so nothing can quietly rebuild over the artifact under
test. `docker-compose.dual.yml` declares
`image: ${CERBERUS_IMAGE:-cerberus:migration-tier1${COMPOSE_PROJECT_SUFFIX:-}}`
with `pull_policy: never`, and the `up` recipes carry no `--build`: the image is
put into the daemon exactly once, by `just migration-cerberus-image` (which
builds the local tag from `Dockerfile.local` or pulls anything else), before
`up` runs. A missing image aborts `up` loudly instead of silently recompiling
whatever tree the runner is holding. `test/regression/migration_tier1_test.go`
pins every copy of the tag and every copy of the stamps equal, and pins the
resolver step into all three tier jobs.

### 3.1. The all-signal seeder

`test/e2e/migration/cmd/seed` builds one in-memory fixture from an archetype's
data declaration and writes it four times over: batch `INSERT` into
`otel_metrics_{gauge,sum,histogram}`, `otel_logs` and `otel_traces`; snappy
`prompb` remote-write into reference Prometheus; `/loki/api/v1/push` into
reference Loki; OTLP/gRPC into reference Tempo. Every writer consumes the same
Go values, so no timestamp, value or label is ever re-derived per backend.

Its first action is one OTLP warm-up record per signal, sent to the collector
under names no corpus query selects. That makes table creation unconditional
rather than dependent on the exporter's start-time behaviour; the seeder then
polls for all six `otel_*` tables against a hard deadline before it writes a
single fixture row. Nothing in the seeder creates a table.

**The semantic hotspots are declared, never incidental.** Three fields in the
declaration exist purely so MIG-17's corpus has a real divergence to fail on.
`restarts` zeroes a named counter series' running total mid-window, so
`rate()` / `increase()` cross a genuine pod-restart reset rather than a
monotonic climb. `scrape_health.down` makes a named service's `up` report 0 for
a contiguous run of samples and then recover, which is the firing edge and the
resolve edge `up == 0` selects. `scrape_health.stops` ends one service's `up`
early, so that series has to disappear from BOTH backends exactly one instant
lookback after its last sample — the TSDB stale-marker versus ClickHouse gap
model section 5 requires cerberus to match rather than tolerate. Nothing
synthesises `up`: on both sides it is an ordinary gauge that has to be written
to be queryable, so an archetype that declares no `scrape_health` block has no
`up` at all and a staleness query against it would compare two empty answers.

Each shape is inert unless two files that cannot see each other agree — the
declaration has to inject it inside the queried window, and the committed
corpus has to select the very series it was injected into.
`test/regression/migration_tier1_test.go` holds them together offline: it fails
when a declared shape lands where no replayed step reads it (an outage between
two steps, a disappearance edge off the step grid), and when the corpus stops
naming the series a shape was injected into.

The fixture carries exactly one deliberate asymmetry between the two sides:
reference Prometheus additionally receives `seed.PreIngestWindow` of gauge
history ending one sample step BELOW the first ClickHouse row, on the fixture's
own label sets and its own grid. That models the operator's incumbent having
recorded since long before ClickHouse ingest was switched on, and it is what
gives MIG-23's ingest-start boundary two distinguishable sides to be measured
against — without it both backends are equally empty below the boundary and the
split-read story compares two empty answers. It is written first, because
reference Prometheus rejects an out-of-order sample outright, and it is written
to the incumbent only, because ClickHouse's earliest row IS the boundary. No
lane query can reach it: the earliest instant any of them reads is
`verify_start`, and the deepest lookback a corpus range selector may carry
bottoms out exactly at `seed_start` (pinned offline by
`test/regression/migration_tier1_test.go`).

**Determinism.** Five devices, and the fixture is byte-identical run to run
apart from its offset from the epoch:

- one RNG, seeded with a fixed value and consumed in a fixed order;
- cardinality declared in `fixture.json`, never derived from a counter — the
  metric label sets are a real CROSS JOIN of the service and status-code lists,
  because a correlated `index % len` derivation collapses
  `len(services) × len(codes)` series into `len(services)`;
- trace and span identifiers hashed from `(service, trace index, span index)`;
- ClickHouse `Map` columns written in sorted key order, because the driver
  otherwise serialises a Go map in randomised iteration order and the same
  logical label set lands under two different stored representations;
- a rolling anchor truncated to the step grid. One anchor serves all three
  signals: cross-signal correlation needs the three inside one window, and a
  fixed date is unusable because Tempo's live store clamps span timestamps
  outside its ingestion slack and the clamped block metadata makes search
  return nothing.

**Ingestion skew** is closed by two mechanisms answering two different
questions. *Closed-window querying* answers "do both sides cover the same
interval?": the seeder publishes a manifest carrying
`[verify_start, verify_end]` and the step, and the lane reads its window from
there instead of one ending at `now`. `cerberus migrate verify`'s own
`--start -1h --end now` defaults are untouched — a real operator legitimately
wants a live window against their own Prometheus. *Metric-driven readiness
gates* answer "has the reference finished ingesting?": Loki is flushed
synchronously and then gated on an empty flush queue, zero in-memory chunks, a
chunks-flushed delta of at least one per pushed stream and a fresh TSDB index
upload, all in a single poll; Tempo on a completed live-store block that the
querier's blocklist has picked up *and* `/api/search` returning, per service,
exactly the trace identities this run pushed — the live store cuts blocks on
second-scale timers, so the block counters say nothing about whether the whole
fixture is searchable, which is the only question the lane goes on to ask;
Prometheus — whose remote-write receiver has
no flush stage — data-side, on an instant query returning exactly the declared
series count with its last sample exactly at the anchor. Every gate is bound to
the payload this run produced rather than to an absolute counter. Only the reference
side needs a gate: cerberus reads ClickHouse directly, so its visibility is
bounded by the `INSERT` round-trip.

The gates carry no percentage floors, no cardinality tolerances, no latches and
no retry-then-continue. A missing upstream metric never makes a gate pass: a
signal asserted `== 0` is a hard error when absent, because coercing it to zero
would satisfy the condition, while a signal asserted `>= n` reads as "not yet"
and keeps the gate waiting until it fails with the un-observed metric named.

**Proof.** `test/e2e/migration/tier1_parity_test.go` rebuilds the fixture
in-process from the same declaration and the manifest's window, then compares
each side against that oracle independently — never one side's length against
the other's, which two empty results satisfy. It asserts the metric matrices
group-for-group and sample-for-sample with no tolerance (every fixture value is
a whole number or a negative power of two, so the arithmetic is exact), the log
entries timestamp-and-line exact, and the trace-search results as 16-byte trace
identities, with each backend's wire rendering asserted separately — reference
Tempo strips leading zeros from trace-id hex, and cerberus emits the canonical
fixed-width form the spec requires, so the difference is pinned rather than
normalised away. A negative control runs last: it injects a series scaled far
beyond any float tolerance into the ClickHouse side and requires the comparison
to observe the disagreement at every step, and the other services to stay
identical. Without it, "both sides agree" would also be the reading of a
harness that had silently stopped comparing anything.

**Tier 2 — ruler tier.** The Tier-1 stack **+ a real query-only external
ruler** (a Prometheus/Thanos ruler in rule-eval-only mode, or Grafana-managed
alerting) pointed at cerberus's HTTP API, writing recording-rule output back
through the OTLP collector into ClickHouse, plus a **dead-end Alertmanager
receiver** (a null/webhook sink that computes but never pages). This is the only
place alert-firing parity (`for:` / `keep_firing_for` hold-down, staleness
resolve edges, the recording-rule write-back loop) can be proven, because
result-diffing does not model it. The ruler is real infrastructure, not the
`cerberus migrate` tool.

Tier 2 is an overlay, merged behind Tier 1's compose file rather than launched
on its own, and it declares no project name of its own. That is what puts the
ruler and its Alertmanager sink on the same network as the cerberus and
ClickHouse services they talk to, and it is also what carries Tier 1's
per-checkout suffix onto the merged project, so both tiers land in one isolated
stack. Compose resolves the project from the last `name:` among the merged
files, so a `name:` added here — even the identical string — would rename the
merged project and would have to re-derive the suffix to stay in step.

## 4. The 26 migration user-stories

These are the canonical anchor. Each maps to exactly one scenario in
[section 6](#6-story--scenario-map); the coverage ratchet fails the lane if any
row here has no scenario, or if a scenario references a story not on this list.

### ASSESS

| ID     | User-story                                                                                                                               |
| ------ | ---------------------------------------------------------------------------------------------------------------------------------------- |
| MIG-01 | As an operator I harvest a deduplicated, provenance-tagged query corpus from my rule files and exported dashboards, offline.             |
| MIG-02 | As an operator I inventory the runtime cardinality of my live Prometheus so I can rank OOM risk before migrating.                        |
| MIG-03 | As an operator I classify every harvested query as PromQL-pure / rewritable / no-equivalent, with the offending dialect construct named. |
| MIG-04 | As an operator I build the recording-rule dependency graph so I know which derived series must stay materialized.                        |
| MIG-05 | As an operator I get an offline explain (SQL + touched tables, or `UNSUPPORTED`) for every query with no backend.                        |

### TEST

| ID     | User-story                                                                                                                                                        |
| ------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| MIG-06 | As an operator I confirm the ingest bridge reconstructs each metric type (counter/gauge/classic-histogram/native-exp-histogram/summary) into the right CH table.  |
| MIG-07 | As an operator I confirm the collector scrape path is equivalent to Prometheus scraping (series presence + scrape meta-metrics + classic `_bucket` survival).     |
| MIG-08 | As an operator I soak-replay my heaviest queries under fault injection and confirm graceful degradation + a working rollback.                                     |
| MIG-09 | As an operator I stand up a shadow external ruler against cerberus and confirm the ruler → cerberus → CH → recorded-series loop, firing into a dead-end receiver. |

### VALIDATE

| ID     | User-story                                                                                                                                                                            |
| ------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| MIG-10 | As an operator I diff the rendered cerberus schema against my live collector-created CH tables and resolve every deviation.                                                           |
| MIG-11 | As an operator I confirm label and metric-name mapping (resource attributes → Prometheus label names, dots→underscores, suffixes) so my queries and alert routing keys are unchanged. |
| MIG-12 | As an operator I confirm metric-type and histogram fidelity (temporality, classic `le`-bucket layout, exp-histogram quantiles).                                                       |
| MIG-13 | As an operator I confirm every recording-rule output lands in the CH landing zone (or is explicitly rewritten/dropped with sign-off).                                                 |
| MIG-14 | As an operator I confirm my longest query lookback fits within the configured CH TTL per table.                                                                                       |
| MIG-15 | As an operator I confirm multi-tenant isolation (no cross-tenant reads, per-tenant read budget enforced).                                                                             |

### VERIFY

| ID     | User-story                                                                                                                                           |
| ------ | ---------------------------------------------------------------------------------------------------------------------------------------------------- |
| MIG-16 | As an operator I run the differential query-parity harness over my full corpus until the diverge count reaches zero.                                 |
| MIG-17 | As an operator I verify the semantic hotspots (counter-reset, staleness/absence, histogram quantiles) specifically, not just as a spot check.        |
| MIG-18 | As an operator I verify alert-firing parity (fire/resolve timestamps, active labels, annotations) between the incumbent and shadow rulers.           |
| MIG-19 | As an operator I verify recording-rule output parity sample-by-sample against the incumbent's own recorded series.                                   |
| MIG-20 | As an operator I verify long-range panels against the incumbent's downsampled aggregates within a documented, counter-aware delta.                   |
| MIG-21 | As an operator I verify cross-signal correlation (exemplar → trace, trace → logs, span-metrics, service-graph) with `trace_id` as an indexed column. |

### CUTOVER

| ID     | User-story                                                                                                                                              |
| ------ | ------------------------------------------------------------------------------------------------------------------------------------------------------- |
| MIG-22 | As an operator I stand cerberus up as an additional datasource and move / revert read traffic per datasource by URL change alone, each step reversible. |
| MIG-23 | As an operator I split historical reads: queries before CH ingest-start route to the incumbent read path; the boundary is configurable and observable.  |
| MIG-24 | As an operator I stage the cutover order (informational → dashboards → recording → paging last), each stage gated on parity evidence.                   |

### DECOMMISSION

| ID     | User-story                                                                                                     |
| ------ | -------------------------------------------------------------------------------------------------------------- |
| MIG-25 | As an operator I audit residual incumbent read traffic and block teardown until it is zero, in a staged order. |
| MIG-26 | As an operator I gate teardown on the CH retention runway meeting the incumbent's retention/compliance window. |

## 5. Comparison modes — the honesty contract

The lane exists to tell the truth about a migration, so it inherits every
no-escape-hatch rule the rest of cerberus enforces. The apparent tension
between "diverge count must reach zero" and "histogram/downsample tolerance" is
resolved by making the comparator explicit per scenario. There are exactly
seven comparison modes, and no scenario mixes them silently:

1. **Exact parity — `cerberus migrate verify`, diverge count zero.** The default
   `--tolerance` is a *tiny* absolute epsilon: it is the definition of "the same
   float" so that IEEE-754 last-bit noise between two independent evaluators does
   not read as a divergence. It is uniform, stated up front, and is **not** an
   allow-list. Under it, any real divergence is a cerberus bug and the scenario
   stays **RED** until fixed (a fix PR is spun out; the divergence is never
   tolerated in place). This is the mode for MIG-16, the MIG-17 counter-reset
   and **staleness** cases, and the MIG-13 / MIG-19 recording-rule value diffs.
   Staleness is included here deliberately: the TSDB stale-marker versus CH
   last-value/gap model is *documented* so the expected answer is well-defined,
   and cerberus must match that expected answer — the documentation defines
   correctness, it does not license a tolerance.

2. **Estimator epsilon — exp-histogram quantiles.** `histogram_quantile` over
   exponential histograms is an estimator, and both backends estimate. A bounded,
   stated, *uniform* `--tolerance` is the correct definition of equality for an
   estimator — still a single epsilon, still not a per-case allow-list — and it
   applies only after the exp-histogram path is independently confirmed healthy
   (MIG-12). This is `cerberus migrate verify` with a larger-but-declared epsilon, and the
   diverge count under that epsilon must still reach zero.

3. **Structural tolerant comparator — downsample only (MIG-20).** Long-range
   panels served from raw or MV-rollup data are compared against the incumbent's
   5m/1h *downsampled counter-aware aggregates*. The delta here is structural (a
   different aggregation granularity), counter-aware, and derivable from the
   downsampling math **before** the run. Because `verify` has only a flat
   absolute epsilon and no counter-aware/downsample mode, this comparison lives
   in a **dedicated harness comparator, explicitly outside the zero-diverge
   `verify` gate**. It is not an allow-list: the accepted band is declared up
   front from the aggregation math, and any excursion beyond it fails.

4. **Entry-multiset equality — LogQL log streams.** A log result is not a matrix
   and has no float axis, so no epsilon applies to it at all. Two backends agree
   when, for every `(stream label set, nanosecond timestamp, log line)` triple,
   both returned it the same number of times, over the part of the window both
   fully cover. Multiplicity is compared, not collapsed: a line returned once by
   one backend and twice by the other is a real double-emit, and a set comparison
   would hide it. Order is excluded from the *definition* of equality — not
   tolerated — because it is provably non-informative on at least one side, and
   `verify` says so with the count of entries that statement covers. This is
   `cerberus migrate verify` under the same zero-diverge rule as mode 1.

5. **Two-regime trace-set parity — TraceQL searches.** A search result is a set
   of trace summaries with no float axis, so no epsilon applies to it either.
   When neither backend hit the request limit, both answered completely and
   equality is exact trace-ID set equality plus exact field equality on every
   returned summary — including `durationMs`, which is an integer millisecond
   count on both sides and is therefore compared with `==` rather than through
   an epsilon that would be exact equality wearing a tolerance's clothes. When
   either backend hit the limit, its result is a prefix of a ranking neither wire
   contract fixes, so membership is *undecidable* and is reported as such with
   both side counts and the exact residue; every trace both backends returned is
   still field-diffed, so a field bug is never masked by truncation. Order is not
   a compared dimension in either regime. This is `cerberus migrate verify` under
   the same zero-diverge rule as mode 1.

6. **Structural span-set equality — trace-by-id.** A trace is a set of spans,
   not a sequence and not a batch layout, so span order and the resource/scope
   batch partition a backend happens to render it in are never compared. Two
   backends agree on a trace when they return the same span-ID set, the same
   total span count (so a duplicate span on either side surfaces rather than
   silently collapsing), and, for every span, the same name, kind, parent,
   start, duration and status, plus the same attribute KEY set. An attribute's
   VALUE is compared only when its type round-trips deterministically through
   the OTel-CH string-map carrier (string, int, bool); a double/array/kvlist/
   bytes value is counted, never diffed, but its KEY is still compared, so a
   genuinely missing attribute is still caught. There is no top-level corpus
   entry for this mode — no query language names a trace ID — so every
   trace-by-id fetch is DERIVED from a trace-search result: one fetch per trace
   ID both backends returned, bounded per search so a corpus of broad searches
   cannot turn one run into thousands of round-trips. This is
   `cerberus migrate verify` under the same zero-diverge rule as mode 1, judged
   against evidence the run itself derives rather than the corpus supplies.

7. **Tag/label set equality — discovery, asymmetric on cardinality.** Tag-name
   and tag-value enumeration is set equality, scoped where the wire contract
   scopes it (Tempo's v2 tag-names surface is diffed per scope bucket rather
   than as one flat set), but the two directions of
   "present on one side only" are not judged the same way. A tag or value the
   reference has but cerberus does not is always a real divergence: cerberus's
   own enumeration is one unbounded ClickHouse `DISTINCT`, complete-or-errored
   by construction, never silently capped. A tag or value cerberus has but the
   reference does not is declared UNDECIDABLE, never diffed: both Tempo and
   Loki cap discovery cardinality server-side and return 200 with a
   silently-truncated set when the cap trips, and neither wire response names
   whether that happened, so this direction cannot be told apart from a real
   cerberus over-report. It is named by count, not silently dropped. A
   reference response the reference ITSELF reports as partial is a second,
   separate exception, declined by name with both job counts rather than
   diffed against cerberus's complete answer. These probes are
   corpus-ANCHORED rather than harvested: an unfiltered tag-name enumeration
   runs once per head the corpus touches, and a tag-VALUE probe runs once per
   distinct label or attribute key that head's own queries reference. This is
   `cerberus migrate verify` under the same zero-diverge rule as mode 1 for
   every dimension it can judge.

**Alert-firing parity is eval-interval-quantized (MIG-18).** Two independent
rulers on independent evaluation schedules produce sub-interval fire/resolve
skew that is not a cerberus artifact. The comparator quantizes fire/resolve
timestamps to the shared evaluation interval; that quantization is the correct
definition of "fired at the same evaluation", not an allow-list. Under it, the
multi-window multi-burn-rate SLO deltas must hold zero across the **full bake
window**, not a spot check.

**A zero diverge count is necessary, not sufficient — the run must have compared
something.** Every mode above defines what "equal" means; none of them says
anything about a query that produced nothing to compare. Two empty matrices are
equal, a lane whose backend rejected every query has no disagreements to report,
and a corpus whose every entry is out of scope has no queries at all — all three
would read as a clean zero-diverge run. `verify` therefore counts the *comparison
units it actually diffed* per `(head, result shape)` family — series for a matrix,
log entries for a stream, traces for a search — and both `verify` and `gate`
refuse a run that replayed
nothing, or a family that replayed units and compared none of them. The split is
per family, not per head, because one lane judges more than one shape and a
family's evidence must not vouch for its sibling's absence. Treating absence
of evidence as parity would be the largest allow-list of all: one that covers
every query at once.

**A dimension that cannot be compared is named and counted, never ignored.** Two
backends sometimes make no wire-contract promise about something — the order of
entries within a log stream, the members of a truncated result below its
boundary, which matched spans survive a spans-per-set cap. The honest answer is
neither to diff it (manufacturing a divergence
that means nothing) nor to drop it silently. `verify` reports each such dimension
as a **limitation**: a code, the exact count of comparison units it covers, and
a sentence stating what was and was not judged. It is the opposite of an
allow-list — it widens no definition of equality and suppresses no difference —
and it cannot green-light anything, because a limitation that swallowed a whole
result leaves the evidence counter at zero and the run blocks.

**A truncated trace search is undecidable, not tolerated.** A TraceQL search
returns trace summaries in a ranking neither backend's wire contract fixes, so
once a limit truncates the result two correct backends may legitimately return
different subsets. Mode 5 splits on exactly that: under the limit both answers
are complete and set equality is a real assertion; at the limit no interior is
provable, so membership gets a verdict of its own — `undecidable` — carrying both
side counts and the residue, rather than a claim in either direction. Undecidable
is non-blocking on its own and can green-light nothing: the family's evidence
counter is the traces actually field-diffed, so a search whose overlap is empty
compares zero traces and blocks like any other dead family. Inventing a tolerant
overlap threshold — "call it equal if 90% of the IDs match" — would be an
allow-list wearing a comparator's clothes, and asserting exact set equality
across a truncated pair would manufacture a permanent false divergence that an
operator learns to ignore. Neither is a definition of equality; the two-regime
rule is.

**No three-way `verify`.** For a non-Prometheus source (VictoriaMetrics, a SaaS
export) the semantic oracle is a **reference Prometheus fed the same synthetic
data**, and cerberus is diffed against *it* with an ordinary two-way `verify`.
The source's own dialect results are not PromQL-comparable, so the source is
only the corpus/dialect origin: classify it (MIG-03), hand-rewrite the
translatable subset to PromQL, then two-way-`verify` the rewritten PromQL
against reference-Prometheus + cerberus over identical synthetic data. Language
rewrite work is explicitly out of scope for the tool and is flagged, not
performed, by the lane.

## 6. Story → scenario map

Scenario ids track the story order. The **Tier(s)** column lists every tier a
scenario's PASS assertion needs; a split-tier scenario (MIG-10, MIG-13, MIG-14, MIG-26)
declares two, and carries one tagged `Scenario` per tier
([section 6.1](#61-harness-shape)) rather than a single tier. A scenario is
never downgraded to a cheaper tier to make it pass
([section 6.3](#63-honesty-guardrails)). Every CLI
cell uses only the verified flags from [section 2](#2-the-cerberus-migrate-cli-surface-this-lane-drives).

### ASSESS scenarios

| ID     | Tier(s) | CLI                                                                                                              | Fixtures                                                                                                                                              | PASS assertion                                                                                                                                                                                                                   |
| ------ | ------- | ---------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| MIG-01 | 0       | `cerberus migrate harvest --rules archetypes/<a>/rules --dashboards archetypes/<a>/dashboards --out corpus.json` | rule files + exported dashboard JSON per archetype; a golden expected-corpus                                                                          | Corpus lists every distinct expression with provenance (`grafana:<dash>`, `grafana-alert`, `rules-file`); refs expanded or listed as an explicit **dropped** count with reason; byte-stable across two runs; zero network.       |
| MIG-02 | 1       | `cerberus migrate inventory --source http://prometheus:9090 --top 50 --json > inventory.json`                    | Tier-1 Prometheus seeded so `/api/v1/status/tsdb` + `/api/v1/metadata` return real head-series/label cardinality; a high-churn label (`container_id`) | Top-N metrics/labels ranked by realized cardinality; high-churn dimensions flagged; a `--source` that 404s `/status/tsdb` exits non-zero (assert the hard-error path too).                                                       |
| MIG-03 | 0       | `cerberus migrate classify --corpus corpus.json --json --out classify.json`                                      | VictoriaMetrics + SaaS corpora carrying MetricsQL/DDSketch/NRQL constructs                                                                            | Every query bucketed PromQL-pure / rewritable / no-equivalent with the offending construct quoted; no dialect-only query silently dropped — each carries a decision; hard parse-error distinguished from translatable-but-lossy. |
| MIG-04 | 0       | `cerberus migrate rulegraph --rules archetypes/<a>/rules --corpus corpus.json --json --out rulegraph.json`       | kube-prometheus-stack mixin rules with colon-named recorded series + consuming dashboards/alerts                                                      | Each recorded series marked consumed or orphan; consumers that must stay materialized listed; unparseable exprs counted, never dropped; deterministic across runs.                                                               |
| MIG-05 | 0       | `cerberus migrate explain --corpus corpus.json --out explain.txt`                                                | full multi-archetype corpus                                                                                                                           | Each query yields byte-exact SQL + touched tables **or** an `UNSUPPORTED` entry naming the unsupported symbol; no network; every `UNSUPPORTED` maps back to its source; identical bytes on re-run.                               |

### TEST scenarios

| ID     | Tier(s) | CLI                                                                                                                                         | Fixtures                                                                                               | PASS assertion                                                                                                                                                                                                                                                                                                    |
| ------ | ------- | ------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| MIG-06 | 1       | drive OTLP through the collector; assert via cerberus `/api/v1/query` + direct CH type probe; `cerberus migrate explain` for touched tables | synthetic counter/gauge/classic-histogram/native-exp-histogram/summary pushed through the bridge       | Each metric's landed CH table + type asserted against expectation; name-dotting + resource-attribute placement asserted on landed rows; a mis-reconstructed type fails loudly — no pass on "a row landed somewhere".                                                                                              |
| MIG-07 | 1       | curl cerberus for `up`, `scrape_duration_seconds`, `scrape_samples_scraped`; classic `_bucket` presence                                     | Prometheus scrape config + equivalent collector prometheusreceiver over the same targets               | Series present under one path but absent under the other listed with the relabel/`honor_labels` reason; scrape meta-metrics produced under the collector path; classic `_bucket` histograms survive in a `histogram_quantile`-consumable form.                                                                    |
| MIG-08 | 1       | replay heaviest harvested queries at production QPS; fault-inject via `docker compose kill/pause/stop` on CH / cerberus / collector         | widest-window × highest-cardinality queries from the kube-prometheus-stack + Thanos corpora; loaded CH | Any query tripping a resource-bound guard or Go-side result-buffering OOM is listed; `query.maxSamples` + result-buffering bound proven to stop one heavy range query OOMing the gateway; p50/p95/p99 + memory captured; a `docker compose kill` shows graceful degradation + a working datasource-flip rollback. |
| MIG-09 | 2       | stand up query-only ruler → cerberus HTTP → CH; assert recorded series selectable via cerberus                                              | Tier-2 shadow ruler + dead-end Alertmanager; a small recording+alerting rule set                       | Ruler evaluates rules against cerberus and lands recording-rule output back into CH; those recorded series become selectable through cerberus; the shadow ruler fires into a null receiver (computes, never pages); the full loop validated in-lab.                                                               |

**What MIG-08's shipped scenario does not yet reach.** The Tier-1 scenario
asserts the fault-injection and rollback half of that PASS cell — a paused
ClickHouse is refused inside cerberus's own per-query wall-clock cap, with a
named error envelope, measurably slower than the healthy p50/p95/p99 the same
query just recorded, while the reference Prometheus still serves the identical
series set and cerberus recovers it on resume. Five clauses of the cell are
outside what it exercises, and none of them is retired by it: no memory figure
is captured beside the latencies; neither `query.maxSamples` nor the Go-side
result-buffering bound is proven to stop a heavy range query exhausting the
gateway; the fault is `docker compose pause` only, so `kill` and `stop` — and
with them a hard process death rather than a freeze — stay unexercised, as do
faults injected at cerberus and the collector rather than at ClickHouse; the
heaviest query is synthesised from the archetype's own fixture declaration
instead of drawn from the harvested corpus, so "heaviest harvested at
production QPS" is a replay of one wide, high-churn range query at bounded
concurrency; and the `prometheus-thanos` corpus half has no seeded Tier-1
fixture, so only `kube-prometheus-stack` is soaked. Each remains owed by
MIG-08, not reassigned and not dropped.

#### How MIG-06 and MIG-07 discharge those PASS assertions, and what they cannot reach

MIG-06 does not stop at "a row landed in the declared table". The columns that
CARRY each type are read back off the landed row and compared against what the
push declared: `AggregationTemporality` and `IsMonotonic` for the counter, the
explicit bounds and per-bucket counts for the classic histogram, the
scale/offset/counts triple for the exponential one, and the pre-computed
quantile pairs for the summary. Every probe — including the "landed nowhere
else" one, which waits past the exporter's own flush interval before
concluding, so a later-flushing duplicate cannot hide behind the right-table
row appearing first — is scoped by a per-run resource attribute rather than by
a global row count, so a second run against a live stack cannot go red for a
reason that has nothing to do with the bridge. `cerberus migrate explain` then
supplies the touched-table half from the other side: the set of physical tables
it names for the shapes' read queries must equal the set the rows were actually
located in.

**Stated limit (MIG-06).** That explain comparison covers the four shapes
cerberus's PromQL read path can reach. The summary is excluded: the read path
has no route to `otel_metrics_summary` at all, so no expression could make
explain name that table. The summary's landing and its type are asserted by the
direct ClickHouse probe alone.

MIG-07 enumerates the scrape meta-metric set from the REFERENCE path
(`/api/v1/label/__name__/values` under the shared target's selector) and diffs
it against the collector path's own enumeration, rather than spot-checking
names the harness already knows — what a Prometheus release synthesises for a
target is its own decision, so the reference path is the oracle. The label
difference between the two paths must be exactly the named OTel translation —
`job` → `service.name`, `instance` → `service.instance.id` — values included,
which is why both paths address the target as `prometheus:9090`; any other
reference-path label absent under the collector path is listed and fails. The
surviving `_bucket` layout is compared as parsed bucket edges (a formatting
convention for `1` or `+Inf` is not part of a layout), and the quantile is
parsed — NaN, infinities and non-positive values rejected — then diffed against
the reference Prometheus's OWN `histogram_quantile` over the same target within
an epsilon derived live from that shared layout: the narrowest finite bucket
width, which is the resolution `histogram_quantile` itself has.

**Stated limit (MIG-07).** The collector path additionally carries the
prometheusreceiver's own scrape-target resource attributes (the target's
address and scheme), whose attribute names track the semantic conventions of
the collector release in use. Those are additive context rather than a lost
series, so the assertion pins the loss direction and the translated values, and
does not pin the additive set by name.

### VALIDATE scenarios

| ID     | Tier(s) | CLI                                                                                          | Fixtures                                                                                                                                                | PASS assertion                                                                                                                                                                                                                                                                                         |
| ------ | ------- | -------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| MIG-10 | 0, 1    | `cerberus migrate schema` (render), then diff vs live CH `SHOW CREATE`                       | `CERBERUS_SCHEMA_*` env matrix incl. an override case                                                                                                   | Rendered `CREATE` = the schema cerberus reads; diff vs collector-created tables lists every missing/renamed column/table; each deviation is resolved by a documented `CERBERUS_SCHEMA_*` override or flagged a blocker; schema application stays a deliberate human step.                              |
| MIG-11 | 1       | cerberus `/api/v1/series`, `/label/<n>/values`, `sum by (…)` replays                         | already-otel + kube-prometheus-stack seeds exercising `job`/`instance` ↔ `service.name`/`service.instance.id`, dots→underscores, `_total`/unit suffixes | Resource attributes map to the Prometheus label names/values queries expect; `sum by (namespace)` groups correctly; template vars resolve; alert label-sets (Alertmanager routing keys) are byte-identical pre/post so silences/routes are unchanged.                                                  |
| MIG-12 | 1       | targeted `rate()`/`increase()`/`histogram_quantile()` replays; direct CH table-routing probe | counter/gauge/classic-histogram/native-exp-histogram/summary seeds incl. a delta-temporality case                                                       | Each type routes to the correct `otel_metrics_*` table with the temporality PromQL assumes; cumulative-vs-delta confirmed; classic `le`-bucket layout preserved OR exp-histogram mapping documented with a stated quantile epsilon; exp-histogram path confirmed healthy before quantiles are trusted. |
| MIG-13 | 1, 2    | `cerberus migrate rulegraph` (which names must land) → ruler write-back → cerberus read-back | rulegraph output from MIG-04; a landing-zone row for the Tier-1 read-back half, a Tier-2 ruler for the write-back half                                  | Every recorded series is reproducible in the CH landing zone (ruler→collector→CH) or explicitly marked inline-rewrite/drop with sign-off; dashboards on low-cardinality recorded series don't regress to scanning raw high-cardinality data; no derived name silently disappears.                      |
| MIG-14 | 0, 1    | compute longest lookback from corpus offline; compare to live CH `TTL` per table             | corpus + live CH TTL config                                                                                                                             | Longest lookback across all dashboards/alerts computed and compared to configured CH TTL per table; any table whose TTL < a query's lookback flagged a blocker; retention shown as an explicitly-set value, never assumed from `prometheus.yml`; rollup-MV requirements for long ranges surfaced.      |
| MIG-15 | 1       | per-tenant `X-Scope-OrgID` queries + `/series` + `/label/*/values`                           | mimir/cortex archetype with ≥2 tenants mapped to CH database/row-policy; a per-tenant read budget                                                       | A cross-tenant query returns **exactly zero** foreign-tenant series; an over-budget tenant read is **refused with a specific error**; metadata endpoints return correct per-tenant label sets; any noisy-neighbor gap without CH-side quotas is a documented blocker.                                  |

### VERIFY scenarios

| ID     | Tier(s)          | CLI                                                                                                                                                                                                                                                                                                           | Fixtures                                                                                                                                                                                                                                                                                                                                                                                                                                                                        | PASS assertion                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| ------ | ---------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| MIG-16 | 1                | `cerberus migrate verify --corpus corpus.json --ref http://prometheus:9090 --cerberus http://cerberus:9090 --ref-loki http://loki:3100 --cerberus-loki http://cerberus:9090 --ref-tempo http://tempo:3200 --cerberus-tempo http://cerberus:9090 --start -1h --end now --step 60s --json --report verify.json` | full corpus + dual-write overlap window; VM/SaaS variants add a reference-Prometheus leg fed identical data                                                                                                                                                                                                                                                                                                                                                                     | Same query/`[start,end,step]`, step-aligned, over both backends; first-diff report gives series/timestamp/ref-value/cerberus-value; **diverge count must reach zero — no expected-diff allow-list**; each divergence attributed to cerberus-bug / ingest-artifact / data-window-gap / dialect-semantics; metadata endpoints diffed too.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| MIG-17 | 1                | `cerberus migrate verify` scoped to the hotspot sub-corpus (PromQL lane; the hotspots are PromQL-shaped and so is the attribution)                                                                                                                                                                            | high-churn counters with induced pod-restart counter-resets; target-down transitions; classic + native histograms                                                                                                                                                                                                                                                                                                                                                               | `rate`/`increase`/counter-reset verified across resets and pod-restart edges; staleness/absence (`up==0`, `absent()`, `absent_over_time()`, resolve edge) verified against the documented stale-marker-vs-gap expected answer (zero diverge, not tolerated); `histogram_quantile` verified within the stated estimator epsilon; per-query max/median divergence reported.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| MIG-18 | 2                | run the SAME multi-window multi-burn-rate rule set on both rulers over one fixture; diff the two notification streams                                                                                                                                                                                         | Tier-2 dual rulers, sharing no code: the shadow (Grafana-managed alerting over cerberus) and the incumbent (reference Prometheus over its own TSDB, dispatching through its own Alertmanager), each into its OWN dead-end receiver. One in-memory fixture is rendered into both backends, driving one burning and one intact `slo` identity through an MWMBR rule pair (fast page + slow ticket, each with a long and a short window). Plus the shadow-only fire/resolve probe. | The shadow ruler's own lifecycle: hold-down honored (pending observed, and firing not reported before the provisioned pending window elapsed); firing edge captured at the one receiver with the rule's provisioned labels and an annotation rendered from the alert's own label set; resolve edge captured for the same identity, ordered after the fire edge. AND the incumbent-vs-shadow diff: matched on the alert identity both rulers declare (name plus the routing labels — substrate labels naming which ruler emitted an edge are projected away, and a MISSING routing key fails rather than shrinking the identity), **false positives and false negatives must both be zero**, the intact `slo` is asserted by name to have been paged by neither ruler, and the diff must have compared something. Timing skew is quantized to the shared evaluation interval and its MAGNITUDE bounded by a derived value — one evaluation interval plus the harness's own measured write span — rather than asserted zero: the two schedulers cannot be phase-locked (Prometheus offsets a group by hash(group, file) % interval; prom v3.11.3 has no `align_evaluation_time_on_interval`), and section 5 names that sub-interval skew as not a cerberus artifact. The MWMBR burn rate both rulers page off must hold equal across the FULL bake window under the exact-parity epsilon, not at a spot instant. |
| MIG-19 | 2                | diff CH-landed recorded series value-for-value against the incumbent ruler's own engine at the same instants                                                                                                                                                                                                  | Tier-2 ruler write-back plus the incumbent ruler recording the SAME rule over its own copy of the source series, remote-written from the one fixture that produced the ClickHouse rows                                                                                                                                                                                                                                                                                          | Each recorded series compared sample-by-sample under the exact-parity epsilon; divergences attributed (rule translation / input parity / write-back timing-lag); any diverging recorded output is a blocker until reconciled; comparison window = what dashboards/alerts actually query. Every landed sample reproduces a live re-evaluation of its source expression at the instant it was recorded, the landed samples hold the ruler's cadence with no dropped or duplicated tick, and are not all one value. AND, with cerberus on exactly ONE side: every landed sample equals what the INCUMBENT's engine computes for the same expression at the same instant, so a cerberus evaluation bug no longer cancels out against itself; and the incumbent's OWN recorded series is held to that same engine on the incumbent's own evaluation grid, so deleting its `record:` rule fails the scenario rather than leaving the incumbent leg an ad-hoc query endpoint. The two recorded series are not compared point-for-point to each other because the two rulers never record at the same instants and cannot be made to, and over a ramped source only a tolerance wide enough to swallow the ramp would let such a comparison pass.                                                                                                                                                                      |
| MIG-20 | 1                | dedicated tolerant comparator (**not** `cerberus migrate verify`) comparing cerberus raw/MV-rollup vs the incumbent's 5m/1h downsampled counter-aware aggregates                                                                                                                                              | Thanos archetype with downsampled blocks + a delta band declared up front                                                                                                                                                                                                                                                                                                                                                                                                       | Long-range panels served cheaply verified against the incumbent's downsampled aggregates within the **declared, counter-aware** band; the band is stated before the run from the aggregation math; any excursion beyond it fails.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| MIG-21 | 1 (three-signal) | Grafana-driven correlation hops (Playwright, reusing the Layer-9 crawl engine) + direct CH `trace_id` index probe                                                                                                                                                                                             | three-signal seed: metrics + logs + traces with exemplars, span-metrics/service-graph; Loki + Tempo + Grafana added to the stack                                                                                                                                                                                                                                                                                                                                                | `trace_id` validated as an indexed first-class column in both logs and traces CH tables; each hop (exemplar→trace, trace→logs, logs→trace) resolves in Grafana against cerberus datasources; span-metrics + service-graph reproduced and verified equivalent; trace assembly regroups spans by `trace_id` honoring sampling/late spans.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |

#### MIG-18: how alert-firing parity is proven

MIG-18 runs two rulers that share no code and diffs what they page.

The **shadow** leg is Grafana-managed alerting querying cerberus over
ClickHouse, notifying its own dead-end receiver. The **incumbent** leg is
reference Prometheus evaluating over its own TSDB and dispatching through its
own Alertmanager into a *second* dead-end receiver. One receiver holding both
streams would interleave them with nothing in the payload naming which ruler
emitted an edge, so the diff would be a stream compared against itself.

Both rulers are handed ONE in-memory fixture, rendered into each backend's wire
shape — never regenerated per side, because two sample paths cannot land
identical timestamps and a diff over inputs that differ at the sample level
measures the seeder rather than the rulers. Both evaluate the same **MWMBR**
rule pair: a fast-burn page and a slow-burn ticket off one 0.1% error budget,
each requiring its long AND its short window over threshold at once. The
fixture drives two `slo` identities through them — one burning, one intact — so
the diff is two-sided: a shadow that misses the burn is a false negative, and
one that pages the healthy SLO is a false positive. A single burning identity
would only ever exercise one arm, and a ruler that paged indiscriminately would
sail through it.

What the diff compares is the identity both rulers *declare*: the alert name
plus the routing labels an operator's silences and routes are written against.
Grafana additionally stamps labels naming itself (`grafana_folder`,
`datasource_uid`, `ref_id`); those are projected away, because they are the one
thing the two legs are guaranteed to differ on and the one thing the diff is
not about — comparing them would make every identity mismatch on both sides and
the diff would be uniformly dirty. The projection is a closed, positive list and
cannot hide a divergence: an edge MISSING a routing key fails outright rather
than projecting to a smaller identity that happens to match, and a wrong value
lands as a false positive plus a false negative.

**Timing skew is bounded and measured, not asserted zero.** The two schedulers
cannot be phase-locked: Prometheus offsets a group's evaluation by
`hash(group, file) % interval`, Grafana ticks epoch-aligned, and prom v3.11.3
has no `align_evaluation_time_on_interval` to turn that off. Observed live, the
two rulers fire the same correct alert about three seconds apart, which
straddles a 10s quantization boundary roughly as often as not — so demanding
zero quantized skew would fail on a healthy substrate. Section 5 names that
sub-interval difference as not a cerberus artifact. `DiffAlertStreams`
therefore owns the verdict that cannot be blamed on scheduling (false positives
and false negatives, both zero), and `SkewBoundHolds` owns the magnitude
quantization throws away — bounded by one evaluation interval plus the
harness's own measured write span, every term a property of the substrate
rather than a slack allowance. A shadow ruler firing four minutes late is a
real defect that quantization alone would file indistinguishably from a 3s
phase difference.

Finally, the burn rate both rulers page off must hold equal across the FULL
bake window under the exact-parity epsilon. Two rulers can agree on "fired"
while disagreeing about the value that made them fire: a shadow computing
twice the incumbent's burn rate still pages, and the stream diff comes back
clean.

#### MIG-19: how recorded-series parity is proven

MIG-19's older assertion — every landed sample reproduces a live re-evaluation
of its source expression at the instant it was recorded — covers the write-back
path end to end, but has cerberus on BOTH sides: a cerberus evaluation bug moves
the recorded value and the re-evaluation identically and cancels out.

The oracle that closes it is the incumbent's engine. Reference Prometheus holds
its own copy of the same source samples, remote-written from the one fixture
that produced the ClickHouse rows, and every landed sample must equal what THAT
engine computes for the same expression at the same instant, under the same
exact-parity epsilon. cerberus now stands on exactly one side.

The incumbent's own recorded series is held to that same engine on the
incumbent's own evaluation grid. Without it the incumbent's `record:` rule could
be deleted entirely and the scenario would still pass, since the diff above only
ever asks the incumbent ad-hoc questions — which would leave "the incumbent
records the same rule set" a claim about a config file nothing executes.

The two recorded series are deliberately NOT compared point-for-point to each
other. The two rulers never record at the same instants and cannot be made to,
and the source series is a ramp, so a cross-grid comparison would diff two
correct answers to two different questions; the only way to make it pass would
be a tolerance wide enough to swallow the ramp, which would swallow real
divergence with it. Routing both through one oracle at each ruler's own instants
keeps every comparison exact.

Recovering the incumbent's own recording instants needs `timestamp()`, not a
range-query grid: Prometheus carries a sample forward across its staleness
window, so a grid reports a value at every point whether or not an evaluation
landed there — the same trap the shadow side avoids by reading raw ClickHouse
rows. The recovered instant is rounded to the nearest millisecond, which is
exact rather than a concession: Prometheus stores every sample timestamp as an
int64 count of milliseconds, so anything finer is float noise from the wire
encoding. Skipping that rounding made a re-evaluation land 77ns off the ruler's
own instant and, over the ramp, showed up as a 2.7e-06 divergence against a
1e-09 epsilon.

Two further assertions exist so the verdict cannot hold by construction,
because the value comparison alone has two silent degenerate modes:

1. **A dropped evaluation is invisible to a value-only check**, because a
   sample that never landed is never compared. So the scenario also asserts a
   count oracle: across the window the landed samples span, their number is
   exactly what a ruler evaluating at the provisioned interval produces there,
   with strictly increasing timestamps — a dropped tick or a duplicated write
   fails it.
2. **A flat series makes every comparison zero against zero.** The earlier
   fixture accrued exactly one idle-second per wall-clock second, so
   `1 - rate(...)` was 0.0 everywhere and "landed equals re-evaluated" held
   whether or not the write-back path preserved anything. The source series is
   now seeded with a **ramped** counter rate, and the scenario asserts the
   landed samples are not all one value.

The Tier-2 scenarios that seed this rule's source series each write it under
their own `seed_scope` label. They share one long-lived ClickHouse and one
metric name, so without distinct identities MIG-09's seed and MIG-13/MIG-19's
seed interleave into a single non-monotonic series, every interleaving reads as
a counter reset, and the recording rule's output stops meaning anything —
observed on the first live Tier-2 run as a landed sample of `0.05` against a
re-evaluation of `-27.8`, a negative CPU utilisation. Overlapping windows are
fine; colliding identities are not. The MWMBR fixture carries the same scope for
the same reason, and both rules keep it as a grouping key so each run gets its
own series and its own alert identity.

### CUTOVER scenarios

| ID     | Tier(s) | CLI                                                                                                                                           | Fixtures                                                                                                                                                             | PASS assertion                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| ------ | ------- | --------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| MIG-22 | 1       | script Grafana datasource/ruler URL flip + one-line rollback; re-run a probe query after each move                                            | Grafana provisioned with both incumbent + cerberus datasources                                                                                                       | Cerberus stood up as an **additional** (shadow) datasource before any flip; read traffic moved and reverted per datasource/dashboard/alert-group by URL change alone; every step has a documented one-line rollback; no big-bang swap path exists in the runbook the scenario executes.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                         |
| MIG-23 | 1       | no CLI — direct instant probes either side of the boundary against both backends, plus a one-pass ClickHouse row census over the probe metric | three-signal seed; reference Prometheus additionally holds `seed.PreIngestWindow` of history strictly BELOW ClickHouse's ingest-start, written to the incumbent only | The split-router itself is operator-owned infrastructure — an ingress rule, a proxy route, or a per-datasource pattern in Grafana — and lives OUTSIDE this repository: cerberus ships no boundary configuration and no backend-routing layer, and this scenario asserts against none. What it asserts is the substrate such a router is built on. ClickHouse's ingest-start is MEASURED off the live table (`min(TimeUnix)`) and must equal the boundary the seeder declared, so the split point is observable rather than a deployment note; ClickHouse holds zero rows below it and exactly the seeder's declared row count at or above it; cerberus answers a post-boundary instant with the declared series cardinality and a pre-boundary instant with nothing at all, while the incumbent answers that SAME instant with the declared pre-ingest cardinality — which is what makes "the old backend has to stay" falsifiable; and the live CH TTL still reaches back past the pre-boundary instant without having aged to ingest-start, which both attributes cerberus's empty answer to ingest-start rather than to expiry and keeps the split non-removable (asserted as a gate that fails closed when no live TTL can be read at all). |
| MIG-24 | 2       | stage the cutover order; block each paging repoint on MIG-18 parity evidence via `cerberus migrate gate`                                      | Tier-2 ruler + staged rule inventory (informational → dashboards → recording → paging)                                                                               | Cutover order documented + enforced; **no** alerting rule repointed until the external evaluator has proven fire/resolve parity live; each stage has an explicit go/no-go gate tied to parity evidence; recording-rule write-back confirmed live before dashboards on recorded series flip.                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                     |

### DECOMMISSION scenarios

| ID     | Tier(s) | CLI                                                                                                      | Fixtures                                                                   | PASS assertion                                                                                                                                                                                                                                                                                                                                                        |
| ------ | ------- | -------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| MIG-25 | 1       | audit incumbent read traffic (Grafana datasource refs + ruler + query logs); emit authorization artifact | Tier-1 with a residual reader planted to prove the block fires             | Teardown blocked while any non-zero read traffic remains on the incumbent (the planted reader must make the audit refuse); staged teardown order enforced — old read path first, then ruler/Alertmanager, then ingest/write leg last; the audit result captured as a decommission-authorization artifact.                                                             |
| MIG-26 | 0, 1    | `cerberus migrate gate` folding retention + inventory + verify artifacts into a go/no-go                 | compliance-retention mandate + live CH TTL (regulated-airgapped archetype) | Gate compares configured CH TTL against the incumbent's retention/compliance window and **refuses teardown** until CH ≥ both; old-backend data allowed to age out under its own retention; object-store lifecycle expiry only triggers after the gate passes; `cerberus migrate gate` exits non-zero on the blocking stage (assert the non-zero path, not just PASS). |

The `gate` fold (MIG-24, MIG-26, and a whole-run roll-up) consumes the JSON
artifacts every other scenario emits (`--json` / `--report`, plus `inventory`'s
redirected stdout), so the aggregator proves
`cerberus migrate gate --verify … --classify … --inventory … --rulegraph …` returns PASS
only when every blocking stage is clean, and non-zero on any
divergence/unsupported/orphan/missing-artifact.

### 6.1. Harness shape

Directory layout:

```text
test/e2e/migration/
  archetypes/                # one directory per archetype, named for it
    kube-prometheus-stack/   { rules/  dashboards/  seed/  expected/ }
    prometheus-thanos/       { … }
    mimir-cortex/            { … }
    victoriametrics/         { … }
    already-otel/            { … }
    saas-repatriation/       { … }
    three-signal/            { … }
    regulated-airgapped/     { … }
  tiers/
    tier0-offline/           # runner over fixtures; no Docker
    tier1-dual/              # docker-compose.dual.yml + prometheus.yml + otel-collector-config.yaml + seeders
    tier2-ruler/             # docker-compose.ruler.yml (extends dual): shadow ruler + dead-end alertmanager
  features/                  # MIG-01..MIG-26, one Gherkin .feature file each
  steps/                     # godog step definitions — the assertion library
  lib/                       # assertion + artifact-collection helpers (JSON diff, first-blocker extract)
  tolerances/                # declared epsilons + their derivations (see 6.2)
  coverage-baseline.json     # the raise-only story <-> scenario floor
  cmd/
    scenarios/               # enumerator: features -> {id, tiers, archetypes} JSON
```

The `seed/` generators and the `tolerances/` registry belong to the tiers
that need them: a seed profile drives a live backend, and the first
epsilon is derived from a measured margin on one, so both arrive with
Tier 1 ([section 8](#8-phased-build-order)). The offline tier reads
committed fixtures and asserts exact equality, so it needs neither.

Scenarios are **Gherkin feature files driven by `godog`** (the Cucumber
implementation for Go, MIT; it is never reachable from `cmd/cerberus`, so the
`agpl-clean` gate is unaffected). Go is the host language
because the assertions read the emitted artifacts as *typed structs* —
`internal/migrate`'s corpus / classify / rulegraph shapes and
`internal/migrateverify.Report` — rather than re-declaring those schemas in a
second language where they would drift.

Every command a scenario drives is a real child process whose environment is
replaced wholesale (`lib.OfflineEnv` / `lib.LiveEnv`), so a result cannot depend
on a `CERBERUS_*` variable the developer happens to have exported. Where a
setting exists for a value — the `verify` corpus, backends and window, the
`inventory` source — the harness writes it into a `cerberus.yaml` in the
scenario's workspace and runs the command from there, because that is the shape
[`migration.md`](migration.md) puts in front of an operator. Only per-run output
choices (`--json`, `--out`, `--top`) stay on the command line, alongside the
one-off `--source` MIG-02's fault case aims at a probe server minted on an
ephemeral port. `lib.RequireSettingsFromFile` reads the flag-to-setting mapping
back off the binary's own `--help` and rejects a command line carrying a value
the file should have supplied, so the covered path cannot quietly revert to the
one almost nobody takes.

The gateway those scenarios query is configured the same way. Tier-1's
`cerberus` service mounts
[`tiers/tier1-dual/cerberus.yaml`](../test/e2e/migration/tiers/tier1-dual/cerberus.yaml)
read-only at `/etc/cerberus/cerberus.yaml` — one of the two paths cerberus
discovers a config file on — and its compose `environment:` carries exactly one
entry, `CERBERUS_CH_PASSWORD`. The credential stays out of a checked-in file,
and keeping it there also proves the two sources compose rather than one
shadowing the other: the gateway only reaches ClickHouse if it read the address
from the file and the password from the environment. This is also the only
place in the tree where a real cerberus process boots from a file, so the
unquoted `false` and bare integer that file carries exercise typed YAML
decoding end to end rather than only in unit tests.
[`migration_tier1_test.go`](../test/regression/migration_tier1_test.go) pins the
mount, runs the file through the gateway's own loader and asserts every value
lands where the stack expects; without that, dropping the mount would leave
cerberus on built-in defaults while every pin that reads the file still passed.

The feature files ARE the manifest: there is no separate scenario registry to
keep in step with them. Metadata rides on tags — `@MIG-16` binds the story,
`@tier0`/`@tier1`/`@tier2` the tier(s), `@archetype:<name>` the archetypes. A
split-tier story (MIG-10, MIG-13, MIG-14, MIG-26) carries two `Scenario`s, one per tier
tag, instead of a tier list. `test/e2e/migration/cmd/scenarios/` walks the
features with godog's own parser and emits one record per `Scenario` node —
`{feature, line, keyword, name, stories, tiers, archetypes, unknown_tags,
steps}` — so exactly one Gherkin parser exists in the tree. It carries no
policy: a tag matching none of the three vocabularies is preserved verbatim in
`unknown_tags` rather than judged, and each step's Gherkin keyword type
(`Context` / `Action` / `Outcome` / `Conjunction`) is reported rather than
interpreted. Every verdict belongs to the ratchet that reads the JSON.

Non-trivial step logic lives in `.github/scripts/migration-e2e.mjs` (per the
CLAUDE.md "step logic in `.mjs`, not inline YAML" rule), mirroring
`compose-smoke-matrix.mjs`. It consumes the enumerator's JSON rather than
parsing Gherkin itself:

- `MODE=verify` is the ratchet. It derives its anchors live from this
  document — the story table in [section 4](#4-the-26-migration-user-stories),
  the **Tier(s)** column in [section 6](#6-story--scenario-map), and the
  archetype table in [section 7](#7-archetype-seed-profiles), cross-checked
  against the directories that actually ship fixtures — and reports every
  violation as `::error::` before exiting 1. It rejects a scenario carrying
  anything other than exactly one story tag and one tier tag, a story id or
  archetype the document does not list, a tier tag contradicting the story's
  declared tiers, two scenarios covering the same story and tier, a story
  spread across more than one feature file or living in a file not named for
  it, a feature file contributing no scenario, an unrecognised tag, a
  `Scenario` with no `Then`, and a number or an operator in step text. So the
  ratchet detects a *wrong* story, not merely a wrong count. The aggregate
  floor lives in `coverage-baseline.json` and is raise-only: coverage
  dropping below it fails, and coverage growing past it fails until the
  baseline is raised in the same reviewed diff, so the ratchet tightens
  instead of ossifying at the number first written down.
- `MODE=emit` writes the `strategy.matrix` JSON for the tier job(s), one entry
  per tier that has scenarios, each carrying the stories it drives and its own
  `timeoutMinutes` ceiling so the bound cannot drift from the shard it bounds.
  It re-runs every `verify` assertion first, so removing the verify step can
  never let a silently-incomplete matrix ship, and it refuses to emit an entry
  for a tier the workflow has no job for — a tagged scenario is never one that
  silently never runs.
- `MODE=run` drives the suite — `godog` filtered to the tier's tag, narrowed
  to one story's tag when asked — and reports. `go test`'s exit status is the
  verdict; the script parses no logs and re-derives no result. It also owns the
  path the suite writes its cucumber-JSON run report to, so a tier that runs
  always leaves an execution record behind rather than depending on a workflow
  author remembering a step.
- `MODE=attest` closes the gap between *enumerated* and *executed*. `verify`
  walks feature files, so a scenario that never ran counts exactly the same as
  one that passed — the shape that let a branch report "30/30 across 26/26
  stories; 0 violations" while its five Tier-2 scenarios had never executed
  once, their job skipped by a `needs:` cascade. `attest` reads every tier's
  run report back and holds each counted scenario to *appeared in a report,
  with every step passed*. It attests only the tiers this run **selected**
  (`emit`'s `tiers` output, the same value the roll-up reads), so a dispatch
  narrowed to one tier is not failed by the tiers it deliberately skipped, and
  `verify`'s own notice reports enumerated and attested coverage as two
  different numbers so the caveat lives in the gate's output rather than in
  prose beside it.
- `MODE=rollup` is the lane's final verdict, folding the tier jobs' own
  results rather than re-deriving one from logs. Every tier `emit` selected
  must come back `success` — `skipped` included counts as a failure — and a
  tier that ran without being selected is rejected too, so the fold cannot
  silently pass a cancelled or unexpectedly-run tier.

The `verify` mode and a `migration-e2e.test.mjs` unit guard both run on the
required `lint` job. `verify` is pure file walking, so a story/scenario drift
is a blocking pull-request failure rather than a scheduled-lane surprise; the
guard pins the document parsers against the live document and proves each
detector still fires, because a ratchet whose detectors have rotted into
no-ops reports zero violations forever and looks exactly like a healthy one.

Scheduled workflow skeleton (`migration-e2e.yml`):

```yaml
name: migration-e2e

on:
  push:
    branches: [main]          # informational on merge — not a PR gate
  schedule:
    - cron: '37 4 * * *'      # nightly, offset from e2e and the compat lanes
  workflow_dispatch:
    inputs:
      tier:  { type: choice, options: [all, tier0, tier1, tier2], default: all }   # each tier's option lands with its job
      story: { type: string, required: false }                        # a single MIG id, e.g. MIG-04

permissions:
  contents: read

# NOTE: no `pull_request:` trigger — so it is never a branch-protection check.

jobs:
  migration-setup:                     # enumerate + coverage ratchet + emit the tier list
    runs-on: ubuntu-latest
    outputs: { tiers: ${{ steps.emit.outputs.tiers }} }
    steps:
      - uses: actions/checkout@v7
      - uses: ./.github/actions/setup-go
      - run: go run ./test/e2e/migration/cmd/scenarios --out build/migration-scenarios.json
      - run: node .github/scripts/migration-e2e.mjs   # story <-> scenario cover
        env: { MODE: verify, SCENARIOS_JSON: build/migration-scenarios.json }
      - id: emit
        run: node .github/scripts/migration-e2e.mjs
        env: { MODE: emit, SCENARIOS_JSON: build/migration-scenarios.json, TIER: ${{ inputs.tier || 'all' }} }

  migration-tier0:                     # offline — fast, no Docker; one fixed job, not matrix-driven
    needs: migration-setup
    if: contains(fromJSON(needs.migration-setup.outputs.tiers), 'tier0')
    runs-on: ubuntu-latest
    timeout-minutes: 15
    steps:
      - uses: actions/checkout@v7
      - uses: ./.github/actions/setup-go
      - run: node .github/scripts/migration-artifact.mjs   # resolve the CLI under test
      - run: node .github/scripts/migration-e2e.mjs
        env: { MODE: run, SCENARIOS_JSON: build/migration-scenarios.json, TIER: tier0, STORY: ${{ inputs.story }} }

  migration-tier1:                     # dual-backend — one fixed job, seeds every @tier1 archetype
    needs: migration-setup
    if: contains(fromJSON(needs.migration-setup.outputs.tiers), 'tier1')
    runs-on: ubuntu-latest
    timeout-minutes: 45
    steps:
      - uses: actions/checkout@v7
      - uses: jlumbroso/free-disk-space@…            # same infra-flake fix as e2e.yml
      # GHCR + Docker Hub login, then resolve the CLI/image under test (migration-artifact.mjs)
      - run: just migration-tier1-up
      - run: just migration-tier1-seed
      - run: node .github/scripts/migration-e2e.mjs   # MODE=run TIER=tier1
      # collect artifacts on failure: corpus/explain/classify/rulegraph/verify/gate JSON + logs
      - name: teardown (always)
        run: just migration-tier1-down

  migration-tier2:                     # ruler tier
    if: contains(fromJSON(needs.migration-setup.outputs.tiers), 'tier2')
    needs: [migration-setup, migration-tier1]        # firing parity requires query parity green first
    # extends tier1 compose with shadow ruler + dead-end alertmanager; runs MIG-09/13/18/19/24

  migration-e2e:                       # aggregator — informational roll-up
    needs: [migration-setup, migration-tier0, migration-tier1, migration-tier2]
    if: always()
    runs-on: ubuntu-latest
    steps:
      - run: node .github/scripts/migration-e2e.mjs
        env: { MODE: rollup, SETUP_RESULT: ${{ needs.migration-setup.result }},
               EXPECTED_TIERS: ${{ needs.migration-setup.outputs.tiers }},
               RESULT_TIER0: ${{ needs.migration-tier0.result }}, RESULT_TIER1: ${{ needs.migration-tier1.result }},
               RESULT_TIER2: ${{ needs.migration-tier2.result }} }
        # `MODE=rollup` holds every tier `emit` SELECTED to `success` — `skipped`
        # included counts as a failure — and separately rejects a tier that ran
        # without being selected, so a cancelled or unexpectedly-run tier cannot
        # pass the fold silently.
```

Tier selection is driven by the `tier` dispatch input (default `all`, every
tier that has scenarios), not by which event triggered the run — a
push-to-main, the nightly `schedule` and a `workflow_dispatch` all run every
tier by default, including the heavy Tier-2 ruler tier. A tier a dispatch
deliberately narrowed away reports `skipped` and is passed through **by
name** in the fold, so every tier that *did* run is still held to `success` —
a cancelled matrix must not slip through as a non-failure. `fail-fast: false`
and `cancel-in-progress: false` mirror the existing e2e lanes (a half-killed
compose teardown leaks volumes).

Each tier's `workflow_dispatch` option, its job stanza and its first scenario
land together: a tier tagged in a feature but absent from the workflow is a
scenario that silently never runs, so `MODE=emit` refuses to produce a matrix
entry for a tier that has no job. Goldens are likewise never regenerated by
the lane — `just migration-golden` rewrites them locally and refuses to run
under CI, because a golden is a reviewed artifact rather than a workflow side
effect.

The Tier-0 explain reports record emitted SQL verbatim, so any change that
moves a plan shape drifts them even when nothing under `test/e2e/migration/`
was touched. `just update-golden`'s `migration` shard therefore runs
`migration-golden`, ahead of the fixture-golden body so its output lands inside
the closing diff-stat, whose scope covers `test/e2e/migration/archetypes/`
alongside `test/spec/`. The shard does not have to be remembered: the recipe's
coverage check derives the reports' inputs from the package closure of the
Tier-0 harness *and the binary it spawns*, so a change to SQL emission is
refused until `migration` is named. Run `just migration-golden` directly when
the migration corpus is the only thing that moved.
`test/regression/update_golden_migration_chain_test.go` pins all four: that the
shard reaches the recipe, that a plan-shape change pulls the shard in, its
position relative to the body, and the diff-stat's scope.

Every scenario writes its `migrate` JSON outputs into a per-scenario evidence
dir, uploaded via `actions/upload-artifact` under a per-archetype name (a static
name collides across a matrix). On failure the runner also dumps
`docker compose ps` + last-200 logs for prometheus / otel-collector /
clickhouse / cerberus / ruler / alertmanager (the same shape as e2e.yml's dump
step). A failing scenario reports `::error::MIG-NN <story>: <first blocker>` —
for `verify` that is the first-diff point plus the copy-pasteable
`cerberus migrate verify …` repro the CLI already emits; for `gate` it is the blocking
stage; for offline scenarios it is the diffed golden line.

### 6.2. Scenario language — what a step may assert

The 26 anchors are already written in operator voice ("As an operator I …"), and
this document doubles as the migration runbook. A feature file is therefore both
the executable scenario and the runbook step-list an operator reads, which is
what earns Gherkin its place here.

A feature carries the story verbatim as its narrative, so the story text and the
scenario cannot drift apart:

```gherkin
@MIG-16 @tier1 @archetype:kube-prometheus-stack @archetype:victoriametrics
Feature: MIG-16 — differential query parity over the full corpus
  As an operator I run the differential query-parity harness over my full
  corpus until the diverge count reaches zero.

  Scenario: every corpus query agrees between the reference and cerberus
    Given the archetype is seeded into both backends over a 1h dual-write overlap
    And a harvested corpus of at least 50 queries
    When I run `cerberus migrate verify` over [now-1h, now] at step 60s
    Then the report replayed more than 0 queries
    And both backends returned at least one series
    And the diverge count is exactly 0
    And the metadata endpoint diff is empty
```

#### The assertion taxonomy

A `Then` is a **predicate over typed artifact fields** — equality is only its
degenerate case. Several PASS assertions in
[section 6](#6-story--scenario-map) are inherently relational (MIG-14 and MIG-26
compare a TTL against a lookback, MIG-02 ranks by realized cardinality, MIG-18
quantizes timestamps). There are exactly three predicate kinds, and each is
guarded differently because each carries a different risk of becoming a
tolerance in disguise:

1. **Exact** — `== 0`, `== expected`. The assertion form of comparison mode 1 in
   [section 5](#5-comparison-modes--the-honesty-contract). A scenario tagged
   `@exact-parity` may not reference a tolerance at all; the lint rejects one.
2. **Relational over two measured quantities** — `ttl >= max_lookback`,
   `ch_retention >= compliance_window`. Both operands come from artifacts, so
   there is no constant to tune and nothing to corrupt. This kind needs no extra
   machinery.
3. **Bounded by a declared constant** — `|a − b| <= ε`. The assertion form of
   comparison modes 2 and 3, and the only kind that can decay into a per-case
   allow-list. It is confined by `tolerances/` (below).

#### Where the arithmetic lives

The *relation* is named in prose; the *arithmetic* is Go. Feature files carry no
operators — `Then every table's TTL covers its longest corpus lookback` is a
step function computing the comparison over typed artifacts. This keeps the
runbook readable for its operator audience and puts the math somewhere
unit-testable. The one place numbers are legitimately feature data is a
`Scenario Outline` `Examples` table, where the parameter is the thing varying.

A step may not assert "the command exited 0" or "a file was produced" as its
only claim — that is the existence check
[section 6.3](#63-honesty-guardrails) forbids, and it is the failure mode
Gherkin invites most.

#### The tolerances registry

Every ε of kind 3 lives in `tolerances/`, never as an inline number in a feature
file, and carries a **derivation** — MIG-20's band is "stated before the run
from the aggregation math", and the registry is where that statement becomes
structural. An empty derivation fails the lint.

The registry is **shrink-only**: an ε may be lowered freely, but raising one
fails CI without an explicit reviewed override. That is section 6.3's "no
per-case tolerance inflation" made mechanical rather than aspirational. Each run
reports the observed headroom (`ε_observed / ε_declared`) alongside MIG-17's
per-query max/median divergence, so the ratchet has the evidence to tighten
instead of ossifying at whatever number was first written down.

### 6.3. Honesty guardrails

- **Real assertions from acceptance criteria — never existence checks.** A
  scenario asserts the story's PASS bullet (a landed CH *type*, a diverge count
  of *zero*, a fire/resolve *timestamp* diff), not "the command exited 0" or "a
  file was produced".
- **No expected-diff allow-list.** Exact-parity `verify` scenarios pass only at
  diverge count zero. There is no `EXPECTED_EMPTY`, no per-case tolerance
  inflation, no `should_skip` overlay. A real divergence routes to
  cerberus-bug / ingest-artifact / data-window-gap / dialect-semantics and, if
  it is a cerberus bug, the scenario stays **RED** and a fix PR is spun out.
- **A live-backend scenario must stand up the backend.** Tier is chosen by what
  the assertion needs; a Tier-1/2 scenario may not be downgraded to an offline
  fixture or a stubbed responder to make it green. `verify` must actually query
  a real reference Prometheus **and** a real cerberus over real CH — a
  canned-response stub is a hollow pass and is forbidden.
- **No hollow green.** Assert the machinery actually ran: `verify` must report
  `Total > 0` and both backends must have returned series for the corpus (an
  all-empty or zero-query run **fails**).
- **No `t.Skip` / silent no-run.** A capability genuinely unavailable on the CI
  substrate is recorded **not-applicable with `::notice::`** and covered by an
  alternate scenario — the same posture as the Layer-13 `ch-network-partition`
  gate — never a vacuous pass. The story ↔ scenario coverage ratchet is
  **raise-only**.
- **No pending step.** A Cucumber runner's default is to report an
  unimplemented step as *pending* and carry on, which is `t.Skip` wearing a hat.
  Three mechanisms close it, each failing something concrete. `godog` runs
  under `--strict`, so an undefined, pending **or** ambiguous step fails the
  run. The `forbid-skip` discipline extends to `.feature` files — `@wip` /
  `@skip` / `@ignore` / `@manual` / `@todo` / `@pending` tags are banned, as
  are godog's Go-side skip routes (`godog.ErrSkip`, `godog.ErrPending`, a
  `Skip` call on the `TestingT` a step is handed), which live in non-test `.go`
  files the `t.Skip` scan structurally cannot see. And the coverage ratchet
  fails a `Scenario` with no `Then`, because a scenario that asserts nothing is
  the same vacuous pass by another route. The tag ban and the ratchet both run
  on required pull-request checks, so a suppressed scenario cannot merge and
  wait for the next scheduled run to notice.
- **No inline tolerance.** A numeric epsilon may not appear in feature text; it
  lives in `tolerances/` with a derivation, under the shrink-only ratchet
  ([section 6.2](#62-scenario-language--what-a-step-may-assert)). The ban is
  structural rather than aspirational: the coverage ratchet rejects any digit
  and any comparison or arithmetic operator in step text, so the first epsilon
  cannot land inline. A `Scenario Outline` `Examples` row is data rather than a
  step, so the one legitimate place for a number needs no exemption.
- **Read-only where the CLI is read-only.** Scenarios never auto-provision
  schema (MIG-10 keeps DDL application a deliberate human step) and never mutate
  a real Grafana; the synthetic Grafana in the stack is the only thing driven.

### 6.4. The PASS-assertion pin, and what it is compensating for

The ratchet derives its anchors *live* from this document, which means the
anchor is editable in the same commit as the code it anchors. It checks a
scenario's tier tag against the **Tier(s)** column but never looked at the PASS
assertion's *text*, so narrowing a PASS cell was always a valid route to "full
coverage": weaken what the document demands, implement the weaker thing, stay
green. That happened twice in one session — MIG-23's "the old backend is kept
read-only as a historical tier" clause was deleted, and MIG-18's PASS assertion
was narrowed from an incumbent-vs-shadow notification-stream diff to a
single-ruler lifecycle in the same commit that implemented the narrower thing.

`test/e2e/migration/pass-assertions.pin.json` records the SHA-256 of every
section-6 PASS cell (and its **Tier(s)** cell), and `MODE=verify` fails on any
mismatch. This deliberately does **not** forbid narrowing: a spec legitimately
evolves, and a gate that froze section 6 would be a gate people route around.
What it forbids is narrowing *silently*. Changing a cell fails until the pin is
updated in the **same diff** — one reviewed line saying "this story now demands
less", sitting next to the commit that implements less. There is no
regeneration command, on purpose: the failure prints the new hash to paste,
because a one-command re-pin is a re-pin nobody reads. The pin is hashed over
whitespace-normalised text, so a markdownlint reflow or a column realignment is
not a failure while a wording change is.

One known gap between a PASS cell and what the lane executes today, recorded
here so it is legible rather than buried. (MIG-18's dual-ruler gap was the
other; it closed when the Tier-2 substrate grew its incumbent leg — a second
ruler, its own Alertmanager and its own receiver — and the pin moved in the same
diff as the scenarios that earned it.)

- **MIG-23.** The PASS cell says queries reaching back before ClickHouse's
  ingest-start "transparently route to the incumbent read path". That
  split-router is **operator-owned infrastructure outside this repository** —
  cerberus is one of the two backends it fans out to, not the router. What the
  Tier-1 scenario can and does assert is everything on cerberus's side of that
  boundary: the ingest-start instant is real and measured from ClickHouse's own
  rows, cerberus returns nothing before it, the incumbent still answers there,
  and the retention gate refuses teardown until ClickHouse has aged past the
  boundary. The routing hop itself is out of scope by construction, not by
  omission.

### 6.4. Declared scope limits

A PASS cell in [section 6](#6-story--scenario-map) is the contract; where a
scenario cannot reach a clause of it on the substrate it runs on, the gap is
written down HERE rather than left for a reader to infer from the step list. A
clause absent from both the scenario and this section is a bug, not a decision.

**MIG-10, tier 1 — projection bodies are not diffed.** The render emits, per
metrics catalog table, two idempotent `ALTER TABLE … ADD PROJECTION IF NOT
EXISTS` statements. The tier-1 stack makes the collector's `clickhouseexporter`
the sole schema authority, and those projections are a cerberus-side read
accelerator the exporter never creates — so they are genuinely absent from the
live database, and demanding their presence would assert that a human had
already applied the render, the exact step MIG-10 exists to keep deliberate.
What IS diffed is each ALTER's target: it must be qualified with the live
tenant database and name a table that really exists there, so an operator
piping the render into a client cannot hit a missing target. The parse is held
to the ALTERs' exact set (`TestParseRenderedSchemaReadsEveryAddProjection`), so
"not diffed" cannot quietly become "not read".

**MIG-10, tier 1 — what "the schema cerberus reads" means.** The read-side leg
covers every `*Table` / `*Column` field of `schema.Metrics`, `schema.Logs` and
`schema.Traces`, mapped onto the live table that carries it.
`TestReadSurfaceCoversEveryReadSideSchemaField` holds that mapping TOTAL
against the three config structs, so the claim stays the whole read surface
rather than a hand-picked subset of it. The two fields that resolve to the
empty string on the OTel-CH schema — `Metrics.ZeroThresholdColumn` and
`Traces.ScopeAttributesColumn`, both of which the emitters branch on as "this
deployment has no such column" — address nothing and so are checked by nobody;
every field that DOES resolve to a name is checked, and a field that resolves
to nothing while being listed is a failure, never a skip.

**MIG-15 — tenancy is a ClickHouse database boundary, not a header.** The PASS
cell names `X-Scope-OrgID` queries and a mimir/cortex row-policy fixture.
Cerberus has no tenant header: its deployment model for a mimir-cortex
migration is one cerberus per tenant, each bound to that tenant's own CH
database via `CERBERUS_CH_DATABASE`, which is the boundary the scenario plants
across and reads against. For the same reason the per-tenant read budget under
test is the per-process `CERBERUS_QUERY_MAX_SAMPLES`
(`TestMigrationTier1SampleBudgetIsPinned` holds the scenario's derivation equal
to the compose stack's setting). The metadata half is carried by
`/api/v1/label/__name__/values` and `/api/v1/labels` rather than `/series`;
both absence claims are gated on their positive control having observed a
populated surface in the same run, so an empty answer fails instead of reading
as perfect isolation.

## 7. Archetype seed profiles

Eight archetypes, each a directory named for the archetype under
`test/e2e/migration/archetypes`, contributing `rules/` (recording+alerting),
`dashboards/` (exported Grafana JSON), `seed/` (a **data declaration** — the
service list, log-format list and metric table the shared seeder builds its
fixture from), and `expected/` (golden offline assertions).

`seed/` declares data, never a generator per backend. One declaration produces
one in-memory fixture, and the seeder writes that fixture to both sides. Two
independent sample paths — say a Prometheus scrape config alongside an OTLP
generator — cannot land identical timestamps, and the comparator keys samples
by exact timestamp, so a second path injects sample-level skew by construction
into the one place ground truth exists.

| Archetype             | Representative seed                                                                                                                                                            | Hotspots it forces                                                                                                                                   | Feeds                                |
| --------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------ |
| kube-prometheus-stack | node-exporter + kube-state-metrics + cadvisor; kubernetes-mixin + node-mixin rules; a seeder that restarts synthetic pods so cadvisor counters reset and `container_id` churns | rate() across counter-reset at pod restart; classic histograms; colon-named recorded series; kube-state-metrics staleness/absence; head-series churn | MIG-01/04/08/11/12/16/17, MIG-02     |
| prometheus-thanos     | long-retention + 5m/1h downsampled blocks in object store; recording rules; ranges reaching before CH ingest-start                                                             | downsampled-range delta; historical-read split; retention runway                                                                                     | MIG-20/23/26, MIG-16                 |
| mimir-cortex          | multi-tenant `X-Scope-OrgID`; per-tenant limits; remote_write→OTLP ingest re-plumb; heavy recording rules                                                                      | tenant isolation + per-tenant read quota; ingest-bridge type reconstruction under multi-tenancy                                                      | MIG-06/15, MIG-13                    |
| victoriametrics       | MetricsQL-tainted corpus: `keep_metric_names`, `rollup()`, `WITH`, `range_*`, `label_*`, `default/if/ifnot`, `*_prometheus`                                                    | dialect no-equivalent bucket; two-way `verify` of the rewritten subset vs a reference Prometheus fed identical data                                  | MIG-03/16                            |
| already-otel          | OTLP-native: `service.name`/`service.instance.id`, exponential histograms, dotted metric names                                                                                 | label mapping where dots→underscores is the whole game; exp-histogram quantile fidelity; near-pure read-path swap                                    | MIG-11/12/21                         |
| saas-repatriation     | Datadog/New Relic corpus: DDSketch percentiles, `.as_rate()`, NRQL clauses, `forecast`/`anomaly`, `week_before`                                                                | no-PromQL-equivalent constructs → hand-rewrite + owner; two-way `verify` of the rewritten subset vs reference Prometheus                             | MIG-03/16                            |
| three-signal          | metrics + logs + traces; exemplars; span-metrics + service-graph; `trace_id` across logs and traces                                                                            | cross-signal correlation hops; `trace_id` as an indexed first-class column; trace assembly with late/sampled spans                                   | MIG-21                               |
| regulated-airgapped   | no live backend permitted for assess; explicit compliance-retention mandate                                                                                                    | offline-only harvest/explain/classify/rulegraph/gate must run air-gapped; retention-runway compliance gate; decommission authorization artifact      | MIG-01/03/04/05/25/26 (offline half) |

## 8. Phased build order

Cheapest-first, so value lands before the heavy infra, and each phase's
assertions become the trust anchor the next depends on.

**Phase 1 — Tier-0 offline (build first).** The `godog` runner + the step
library + `test/e2e/migration/cmd/scenarios/` + the coverage ratchet +
`migration-e2e.mjs` + the
`migration-e2e.yml` skeleton running Tier-0 only. Scenarios MIG-01, MIG-03,
MIG-04, MIG-05, MIG-10 (render half), MIG-14 (lookback compute), MIG-26 (gate
compute), plus the `gate` fold. Lands the eight archetype `rules/` +
`dashboards/` + `expected/` fixtures (the seed telemetry generators come in
Phase 2). Dependencies: none beyond the merged CLI — no Docker, seconds to run,
so it ships and starts catching regressions immediately.

These seven scenarios are entirely predicate kinds 1 and 2, so Phase 1 also
fixes the scenario language cheaply: it proves the tag vocabulary, the
strict-mode + `.feature` lint discipline, and the "relation named in prose,
arithmetic in Go" split against real scenarios before the heavier tiers commit
to them. `tolerances/` stays empty until Phase 2 — the first ε is derived from a
measured margin on a live backend, never guessed ahead of one.

**Phase 2 — Tier-1 dual-backend.** `docker-compose.dual.yml` (Prometheus,
Loki, Tempo, OTel collector, ClickHouse, cerberus) + collector config + the
per-archetype `seed/` declarations (incl. the pod-restart counter-reset +
`container_id`-churn shape). Scenarios MIG-02, MIG-06, MIG-07, MIG-08, MIG-10
(diff half), MIG-11, MIG-12, MIG-13 (read-back half), MIG-14 (live TTL),
MIG-15, MIG-16, MIG-17, MIG-20, MIG-21, MIG-22, MIG-23, MIG-25, MIG-26 (live
TTL). Dependencies: Phase-1 corpora
feed `verify --corpus`; reuses e2e.yml's free-disk-space + docker-hub-login +
log-dump patterns. MIG-08's faults are `docker compose kill/pause/stop` on the
compose stack — the Layer-13 `chaos-run.mjs` primitives are k3d/NetworkPolicy
and do not apply to a compose substrate.

**Phase 3 — Tier-2 ruler.** `docker-compose.ruler.yml` extending the dual stack
with a query-only external ruler → cerberus and a dead-end Alertmanager.
Scenarios MIG-09, MIG-13 (write-back half), MIG-18, MIG-19 (write-back timing),
MIG-24. Dependencies: the recording-rule landing zone from Phase 2, and —
critically — firing parity cannot be proven before query parity: MIG-18/24 gate
on MIG-16/17 being green (`migration-tier2 needs migration-tier1`), the same
"ruler-first only after result parity" ordering the stories themselves demand.
