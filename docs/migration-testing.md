# Migration-scenario end-to-end testing (Layer 14)

This is the design for the scheduled end-to-end lane that turns every one of
the 26 migration user-stories into an executable scenario, `MIG-01 … MIG-26`.
It sits above [Layer 13](test-strategy.md#layer-13--live-stack-chaos-real-faults)
in the [13-layer test map](test-strategy.md) as the most-realistic, slowest
layer — it asserts an **operator-workflow** contract (the journey a team takes
to move onto cerberus) rather than a code contract.

The lane drives the merged `migrate` CLI documented in
[`migration.md`](migration.md) against real ClickHouse and a real reference
Prometheus, over eight migration archetypes. The alert-firing and
recording-rule-write-back scenarios go **beyond** the `migrate` tool's
documented v1 scope (which is read-only and does query-result parity, not
alert-firing parity): they stand up a real external ruler as lab
infrastructure. Layer 14 is a harness around the whole operator journey, not
a claim that the `migrate` tool itself grew those capabilities.

The harness code, compose files, seeders, and workflow land in follow-up build
PRs in the [phase order](#8-phased-build-order) below. This document is the
canonical anchor: the 26 stories in [section 4](#4-the-26-migration-user-stories)
are the source of truth the coverage ratchet diffs against.

## 1. Goal and placement

**Goal.** Each migration user-story becomes one executable scenario driving the
merged `migrate` CLI plus the real dual-backend / ruler stacks the parity
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

## 2. The `migrate` CLI surface this lane drives

Every scenario composes only the merged CLI. There are **seven subcommands**
(`harvest`, `explain`, `classify`, `rulegraph`, `verify`, `inventory`, `gate`)
plus a root `--schema` flag. The exact flags each scenario relies on:

| Command             | Flags this lane uses                                                                                                                 | Output                                                      | Network               |
| ------------------- | ------------------------------------------------------------------------------------------------------------------------------------ | ----------------------------------------------------------- | --------------------- |
| `migrate harvest`   | `--rules <paths/globs>`, `--dashboards <dir>`, `--out <file>`                                                                        | corpus JSON (stdout or `--out`)                             | offline               |
| `migrate explain`   | `--corpus <file>` (or `--rules`/`--dashboards`), `--out <file>`                                                                      | text explain report; no `--json`                            | offline               |
| `migrate classify`  | `--corpus <file>` (or `--dashboards`), `--json`, `--out <file>`                                                                      | classification ledger                                       | offline               |
| `migrate rulegraph` | `--rules <paths/globs>`, `--corpus <file>`, `--json`, `--out <file>`                                                                 | dependency graph                                            | offline               |
| `migrate verify`    | `--corpus <file>`, `--ref <url>`, `--cerberus <url>`, `--start`, `--end`, `--step`, `--tolerance <eps>`, `--json`, `--report <file>` | parity report; non-zero exit on divergence                  | live (two backends)   |
| `migrate inventory` | `--source <url>`, `--top <n>`, `--window <dur>`, `--json`                                                                            | inventory (**stdout only — no `--out`**; redirect with `>`) | live (one Prometheus) |
| `migrate gate`      | `--verify`, `--classify`, `--inventory`, `--rulegraph`                                                                               | fold decision; non-zero exit on a blocking stage            | offline               |
| `migrate --schema`  | root flag                                                                                                                            | `CREATE` statements from `CERBERUS_*` env                   | offline               |

Two capability facts shape the scenarios below and are the reason several cells
differ from a naive reading:

- **`verify` is strictly two-way.** It takes exactly `--ref` and `--cerberus`
  and diffs their results. There is no third-backend / oracle leg. Any scenario
  that wants a semantic oracle for a non-Prometheus source composes **two
  two-way runs** feeding both backends identical synthetic data (see
  [section 5](#5-comparison-modes--the-honesty-contract)).
- **`verify --tolerance` is a single flat absolute epsilon.** It is the
  definition of "the same float", not a counter-aware or downsample-aware mode.
  A structural, counter-aware long-range delta (downsampling) therefore cannot
  live inside `verify`; it needs a dedicated harness comparator outside the
  zero-diverge gate.

## 3. Three infrastructure tiers

**Tier 0 — offline fixtures (no backend).** Pure `migrate` CLI over checked-in
rule files, exported Grafana dashboard JSON, and canned corpus/schema fixtures.
No Docker, no network, no ClickHouse — air-gap-faithful, seconds to run. Drives
`harvest`, `explain`, `classify`, `rulegraph`, `--schema` (render), and `gate`
(the pure aggregator). Cheap enough that it *may* also run per-PR later, but it
ships informational-first.

**Tier 1 — dual-backend compose.** `docker-compose.dual.yml`: reference
**Prometheus** (scrapes synthetic exporters) **+ OTel collector**
(clickhouseexporter → the OTel-shaped tables cerberus reads) **+ ClickHouse +
cerberus**, both fed the *same* synthetic sources over an overlapping dual-write
window. This is the only place ground truth exists for a differential diff.
Drives `inventory` (probe live Prometheus), `verify` (diff both backends), and
the live half of schema/label/histogram/retention validation.

**Tier 2 — ruler tier.** The Tier-1 stack **+ a real query-only external
ruler** (a Prometheus/Thanos ruler in rule-eval-only mode, or Grafana-managed
alerting) pointed at cerberus's HTTP API, writing recording-rule output back
through the OTLP collector into ClickHouse, plus a **dead-end Alertmanager
receiver** (a null/webhook sink that computes but never pages). This is the only
place alert-firing parity (`for:` / `keep_firing_for` hold-down, staleness
resolve edges, the recording-rule write-back loop) can be proven, because
result-diffing does not model it. The ruler is real infrastructure, not the
`migrate` tool.

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
three comparison modes, and no scenario mixes them silently:

1. **Exact parity — `migrate verify`, diverge count zero.** The default
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
   (MIG-12). This is `migrate verify` with a larger-but-declared epsilon, and the
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

**Alert-firing parity is eval-interval-quantized (MIG-18).** Two independent
rulers on independent evaluation schedules produce sub-interval fire/resolve
skew that is not a cerberus artifact. The comparator quantizes fire/resolve
timestamps to the shared evaluation interval; that quantization is the correct
definition of "fired at the same evaluation", not an allow-list. Under it, the
multi-window multi-burn-rate SLO deltas must hold zero across the **full bake
window**, not a spot check.

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
scenario's PASS assertion needs; a split-tier scenario (MIG-10, MIG-14, MIG-26)
declares two, and the manifest schema in [section 6.1](#61-harness-shape)
carries a tier **list**, not a single tier. A scenario is never downgraded to a
cheaper tier to make it pass ([section 6.2](#62-honesty-guardrails)). Every CLI
cell uses only the verified flags from [section 2](#2-the-migrate-cli-surface-this-lane-drives).

### ASSESS scenarios

| ID     | Tier(s) | CLI                                                                                                     | Fixtures                                                                                                                                              | PASS assertion                                                                                                                                                                                                                   |
| ------ | ------- | ------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| MIG-01 | 0       | `migrate harvest --rules archetypes/<a>/rules --dashboards archetypes/<a>/dashboards --out corpus.json` | rule files + exported dashboard JSON per archetype; a golden expected-corpus                                                                          | Corpus lists every distinct expression with provenance (`grafana:<dash>`, `grafana-alert`, `rules-file`); refs expanded or listed as an explicit **dropped** count with reason; byte-stable across two runs; zero network.       |
| MIG-02 | 1       | `migrate inventory --source http://prometheus:9090 --top 50 --json > inventory.json`                    | Tier-1 Prometheus seeded so `/api/v1/status/tsdb` + `/api/v1/metadata` return real head-series/label cardinality; a high-churn label (`container_id`) | Top-N metrics/labels ranked by realized cardinality; high-churn dimensions flagged; a `--source` that 404s `/status/tsdb` exits non-zero (assert the hard-error path too).                                                       |
| MIG-03 | 0       | `migrate classify --corpus corpus.json --json --out classify.json`                                      | VictoriaMetrics + SaaS corpora carrying MetricsQL/DDSketch/NRQL constructs                                                                            | Every query bucketed PromQL-pure / rewritable / no-equivalent with the offending construct quoted; no dialect-only query silently dropped — each carries a decision; hard parse-error distinguished from translatable-but-lossy. |
| MIG-04 | 0       | `migrate rulegraph --rules archetypes/<a>/rules --corpus corpus.json --json --out rulegraph.json`       | kube-prometheus-stack mixin rules with colon-named recorded series + consuming dashboards/alerts                                                      | Each recorded series marked consumed or orphan; consumers that must stay materialized listed; unparseable exprs counted, never dropped; deterministic across runs.                                                               |
| MIG-05 | 0       | `migrate explain --corpus corpus.json --out explain.txt`                                                | full multi-archetype corpus                                                                                                                           | Each query yields byte-exact SQL + touched tables **or** an `UNSUPPORTED` entry naming the unsupported symbol; no network; every `UNSUPPORTED` maps back to its source; identical bytes on re-run.                               |

### TEST scenarios

| ID     | Tier(s) | CLI                                                                                                                                 | Fixtures                                                                                               | PASS assertion                                                                                                                                                                                                                                                                                                    |
| ------ | ------- | ----------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| MIG-06 | 1       | drive OTLP through the collector; assert via cerberus `/api/v1/query` + direct CH type probe; `migrate explain` for touched tables  | synthetic counter/gauge/classic-histogram/native-exp-histogram/summary pushed through the bridge       | Each metric's landed CH table + type asserted against expectation; name-dotting + resource-attribute placement asserted on landed rows; a mis-reconstructed type fails loudly — no pass on "a row landed somewhere".                                                                                              |
| MIG-07 | 1       | curl cerberus for `up`, `scrape_duration_seconds`, `scrape_samples_scraped`; classic `_bucket` presence                             | Prometheus scrape config + equivalent collector prometheusreceiver over the same targets               | Series present under one path but absent under the other listed with the relabel/`honor_labels` reason; scrape meta-metrics produced under the collector path; classic `_bucket` histograms survive in a `histogram_quantile`-consumable form.                                                                    |
| MIG-08 | 1       | replay heaviest harvested queries at production QPS; fault-inject via `docker compose kill/pause/stop` on CH / cerberus / collector | widest-window × highest-cardinality queries from the kube-prometheus-stack + Thanos corpora; loaded CH | Any query tripping a resource-bound guard or Go-side result-buffering OOM is listed; `query.maxSamples` + result-buffering bound proven to stop one heavy range query OOMing the gateway; p50/p95/p99 + memory captured; a `docker compose kill` shows graceful degradation + a working datasource-flip rollback. |
| MIG-09 | 2       | stand up query-only ruler → cerberus HTTP → CH; assert recorded series selectable via cerberus                                      | Tier-2 shadow ruler + dead-end Alertmanager; a small recording+alerting rule set                       | Ruler evaluates rules against cerberus and lands recording-rule output back into CH; those recorded series become selectable through cerberus; the shadow ruler fires into a null receiver (computes, never pages); the full loop validated in-lab.                                                               |

### VALIDATE scenarios

| ID     | Tier(s) | CLI                                                                                          | Fixtures                                                                                                                                                | PASS assertion                                                                                                                                                                                                                                                                                         |
| ------ | ------- | -------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| MIG-10 | 0, 1    | `migrate --schema` (render), then diff vs live CH `SHOW CREATE`                              | `CERBERUS_SCHEMA_*` env matrix incl. an override case                                                                                                   | Rendered `CREATE` = the schema cerberus reads; diff vs collector-created tables lists every missing/renamed column/table; each deviation is resolved by a documented `CERBERUS_SCHEMA_*` override or flagged a blocker; schema application stays a deliberate human step.                              |
| MIG-11 | 1       | cerberus `/api/v1/series`, `/label/<n>/values`, `sum by (…)` replays                         | already-otel + kube-prometheus-stack seeds exercising `job`/`instance` ↔ `service.name`/`service.instance.id`, dots→underscores, `_total`/unit suffixes | Resource attributes map to the Prometheus label names/values queries expect; `sum by (namespace)` groups correctly; template vars resolve; alert label-sets (Alertmanager routing keys) are byte-identical pre/post so silences/routes are unchanged.                                                  |
| MIG-12 | 1       | targeted `rate()`/`increase()`/`histogram_quantile()` replays; direct CH table-routing probe | counter/gauge/classic-histogram/native-exp-histogram/summary seeds incl. a delta-temporality case                                                       | Each type routes to the correct `otel_metrics_*` table with the temporality PromQL assumes; cumulative-vs-delta confirmed; classic `le`-bucket layout preserved OR exp-histogram mapping documented with a stated quantile epsilon; exp-histogram path confirmed healthy before quantiles are trusted. |
| MIG-13 | 2       | `migrate rulegraph` (which names must land) → ruler write-back → cerberus read-back          | rulegraph output from MIG-04 + Tier-2 ruler                                                                                                             | Every recorded series is reproducible in the CH landing zone (ruler→collector→CH) or explicitly marked inline-rewrite/drop with sign-off; dashboards on low-cardinality recorded series don't regress to scanning raw high-cardinality data; no derived name silently disappears.                      |
| MIG-14 | 0, 1    | compute longest lookback from corpus offline; compare to live CH `TTL` per table             | corpus + live CH TTL config                                                                                                                             | Longest lookback across all dashboards/alerts computed and compared to configured CH TTL per table; any table whose TTL < a query's lookback flagged a blocker; retention shown as an explicitly-set value, never assumed from `prometheus.yml`; rollup-MV requirements for long ranges surfaced.      |
| MIG-15 | 1       | per-tenant `X-Scope-OrgID` queries + `/series` + `/label/*/values`                           | mimir/cortex archetype with ≥2 tenants mapped to CH database/row-policy; a per-tenant read budget                                                       | A cross-tenant query returns **exactly zero** foreign-tenant series; an over-budget tenant read is **refused with a specific error**; metadata endpoints return correct per-tenant label sets; any noisy-neighbor gap without CH-side quotas is a documented blocker.                                  |

### VERIFY scenarios

| ID     | Tier(s)          | CLI                                                                                                                                                             | Fixtures                                                                                                                                    | PASS assertion                                                                                                                                                                                                                                                                                                                                                            |
| ------ | ---------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| MIG-16 | 1                | `migrate verify --corpus corpus.json --ref http://prometheus:9090 --cerberus http://cerberus:9090 --start -1h --end now --step 60s --json --report verify.json` | full corpus + dual-write overlap window; VM/SaaS variants add a reference-Prometheus leg fed identical data                                 | Same query/`[start,end,step]`, step-aligned, over both backends; first-diff report gives series/timestamp/ref-value/cerberus-value; **diverge count must reach zero — no expected-diff allow-list**; each divergence attributed to cerberus-bug / ingest-artifact / data-window-gap / dialect-semantics; metadata endpoints diffed too.                                   |
| MIG-17 | 1                | `migrate verify` scoped to the hotspot sub-corpus                                                                                                               | high-churn counters with induced pod-restart counter-resets; target-down transitions; classic + native histograms                           | `rate`/`increase`/counter-reset verified across resets and pod-restart edges; staleness/absence (`up==0`, `absent()`, `absent_over_time()`, resolve edge) verified against the documented stale-marker-vs-gap expected answer (zero diverge, not tolerated); `histogram_quantile` verified within the stated estimator epsilon; per-query max/median divergence reported. |
| MIG-18 | 2                | run the rule set on the incumbent **and** the shadow ruler over the same overlap; diff Alertmanager notification streams                                        | Tier-2 dual rulers + dead-end Alertmanagers; rules with `for:` / `keep_firing_for` + a multi-window multi-burn-rate SLO rule near threshold | Fire/resolve timestamps (eval-interval-quantized), active labels, rendered annotations diffed; false-positive / false-negative / timing-skew counts per rule; hold-down + resolve-edge compared, not just final state; MWMBR SLO deltas hold zero across the full bake window.                                                                                            |
| MIG-19 | 2                | diff CH-landed recorded series value-for-value vs the incumbent's own recorded series                                                                           | Tier-2 ruler write-back + incumbent recorded series over overlap                                                                            | Each recorded series compared sample-by-sample under the exact-parity epsilon; divergences attributed (rule translation / input parity / write-back timing-lag); any diverging recorded output is a blocker until reconciled; comparison window = what dashboards/alerts actually query.                                                                                  |
| MIG-20 | 1                | dedicated tolerant comparator (**not** `migrate verify`) comparing cerberus raw/MV-rollup vs the incumbent's 5m/1h downsampled counter-aware aggregates         | Thanos archetype with downsampled blocks + a delta band declared up front                                                                   | Long-range panels served cheaply verified against the incumbent's downsampled aggregates within the **declared, counter-aware** band; the band is stated before the run from the aggregation math; any excursion beyond it fails.                                                                                                                                         |
| MIG-21 | 1 (three-signal) | Grafana-driven correlation hops (Playwright, reusing the Layer-9 crawl engine) + direct CH `trace_id` index probe                                               | three-signal seed: metrics + logs + traces with exemplars, span-metrics/service-graph; Loki + Tempo + Grafana added to the stack            | `trace_id` validated as an indexed first-class column in both logs and traces CH tables; each hop (exemplar→trace, trace→logs, logs→trace) resolves in Grafana against cerberus datasources; span-metrics + service-graph reproduced and verified equivalent; trace assembly regroups spans by `trace_id` honoring sampling/late spans.                                   |

### CUTOVER scenarios

| ID     | Tier(s) | CLI                                                                                                | Fixtures                                                                               | PASS assertion                                                                                                                                                                                                                                                                                                      |
| ------ | ------- | -------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| MIG-22 | 1       | script Grafana datasource/ruler URL flip + one-line rollback; re-run a probe query after each move | Grafana provisioned with both incumbent + cerberus datasources                         | Cerberus stood up as an **additional** (shadow) datasource before any flip; read traffic moved and reverted per datasource/dashboard/alert-group by URL change alone; every step has a documented one-line rollback; no big-bang swap path exists in the runbook the scenario executes.                             |
| MIG-23 | 1       | query spanning before/after the CH ingest-start boundary; assert routing                           | Thanos/SaaS seed where CH ingest-start < the queried range                             | Queries starting earlier than CH ingest-start transparently route to the incumbent read path; the boundary is configurable + observable; the old backend is kept read-only as a historical tier; the split-router is only removable once CH retention has aged past the ingest-start boundary (asserted as a gate). |
| MIG-24 | 2       | stage the cutover order; block each paging repoint on MIG-18 parity evidence via `migrate gate`    | Tier-2 ruler + staged rule inventory (informational → dashboards → recording → paging) | Cutover order documented + enforced; **no** alerting rule repointed until the external evaluator has proven fire/resolve parity live; each stage has an explicit go/no-go gate tied to parity evidence; recording-rule write-back confirmed live before dashboards on recorded series flip.                         |

### DECOMMISSION scenarios

| ID     | Tier(s) | CLI                                                                                                      | Fixtures                                                                   | PASS assertion                                                                                                                                                                                                                                                                                                                                               |
| ------ | ------- | -------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| MIG-25 | 1       | audit incumbent read traffic (Grafana datasource refs + ruler + query logs); emit authorization artifact | Tier-1 with a residual reader planted to prove the block fires             | Teardown blocked while any non-zero read traffic remains on the incumbent (the planted reader must make the audit refuse); staged teardown order enforced — old read path first, then ruler/Alertmanager, then ingest/write leg last; the audit result captured as a decommission-authorization artifact.                                                    |
| MIG-26 | 0, 1    | `migrate gate` folding retention + inventory + verify artifacts into a go/no-go                          | compliance-retention mandate + live CH TTL (regulated-airgapped archetype) | Gate compares configured CH TTL against the incumbent's retention/compliance window and **refuses teardown** until CH ≥ both; old-backend data allowed to age out under its own retention; object-store lifecycle expiry only triggers after the gate passes; `migrate gate` exits non-zero on the blocking stage (assert the non-zero path, not just PASS). |

The `gate` fold (MIG-24, MIG-26, and a whole-run roll-up) consumes the JSON
artifacts every other scenario emits (`--json` / `--report`, plus `inventory`'s
redirected stdout), so the aggregator proves
`migrate gate --verify … --classify … --inventory … --rulegraph …` returns PASS
only when every blocking stage is clean, and non-zero on any
divergence/unsupported/orphan/missing-artifact.

### 6.1. Harness shape

Directory layout:

```text
test/e2e/migration          # created by the Phase-1/2 build PRs
  archetypes/
    kube-prometheus-stack/   { rules/  dashboards/  seed/  expected/ }
    thanos/                  { … }
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
  scenarios/                 # MIG-01..MIG-26, one declarative file each
  lib/                       # assertion + artifact-collection helpers (JSON diff, first-blocker extract)
  scenario-manifest.mjs      # single source of truth: id -> { tiers: [...], archetypes, commands, assertions }
```

The manifest keys each story to a **tier list**, so a split-tier scenario
(MIG-10, MIG-14, MIG-26) declares `tiers: [0, 1]` and maps to two jobs. Its
schema is `id -> { tiers: [...], archetypes: [...], commands: [...],
assertions: [...] }`.

Non-trivial step logic lives in `.github/scripts/migration-e2e.mjs` (per the
CLAUDE.md "step logic in `.mjs`, not inline YAML" rule), mirroring
`compose-smoke-matrix.mjs`:

- `MODE=verify` asserts every one of the 26 stories in
  [section 4](#4-the-26-migration-user-stories) has a scenario in the manifest
  and that no manifest entry references an unlisted story; it emits `::error::`
  - exit 1 on any gap, extra, or stale entry. The 26-story list is the durable
  anchor it diffs against, so the ratchet detects a *wrong* or *missing* story,
  not merely a wrong count.
- `MODE=emit` writes the `strategy.matrix` JSON keyed by archetype (Tier-1/2)
  or a single Tier-0 entry, expanding split-tier stories into one matrix entry
  per declared tier.
- `MODE=run` drives one scenario and reports.

A `migration-e2e.test.mjs` unit guard runs the coverage assertion at PR-cheap
cost so a manifest drift is caught before the heavy lane runs.

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
      tier:        { type: choice, options: [all, tier0, tier1, tier2], default: all }
      archetype:   { type: string, required: false }
      update_goldens: { type: boolean, default: false }

permissions:
  contents: read

# NOTE: no `pull_request:` trigger — so it is never a branch-protection check.

jobs:
  migration-setup:                     # coverage ratchet + emit matrix
    runs-on: ubuntu-latest
    outputs: { matrix: ${{ steps.emit.outputs.matrix }} }
    steps:
      - uses: actions/checkout@v7
      - run: node .github/scripts/migration-e2e.mjs   # MODE=verify (story <-> scenario cover)
        env: { MODE: verify }
      - id: emit
        run: node .github/scripts/migration-e2e.mjs
        env: { MODE: emit, TIER: ${{ inputs.tier || 'all' }} }

  migration-tier0:                     # offline — fast, no Docker
    needs: migration-setup
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
      - uses: actions/setup-go@v7
      - run: node .github/scripts/migration-e2e.mjs   # MODE=run TIER=tier0
        env: { MODE: run, TIER: tier0 }

  migration-tier1:                     # dual-backend — matrix by archetype
    needs: migration-setup
    runs-on: ubuntu-latest
    strategy: { fail-fast: false, matrix: ${{ fromJSON(needs.migration-setup.outputs.matrix) }} }
    timeout-minutes: 60
    concurrency: { group: migration-t1-${{ github.ref }}-${{ matrix.archetype }}, cancel-in-progress: false }
    steps:
      - uses: actions/checkout@v7
      - uses: jlumbroso/free-disk-space@…            # same infra-flake fix as e2e.yml
      - uses: docker/setup-buildx-action@v4
      # docker hub login for a higher pull rate limit — mirrors e2e.yml
      - run: docker compose -f tiers/tier1-dual/docker-compose.dual.yml up --wait --wait-timeout 300
      # seed the dual-write window; wait for the overlap to fill
      - run: node .github/scripts/migration-e2e.mjs   # MODE=run TIER=tier1 ARCHETYPE=${{ matrix.archetype }}
      # collect artifacts on failure: corpus/explain/classify/rulegraph/verify/gate JSON + logs
      - name: teardown (always)
        run: docker compose -f tiers/tier1-dual/docker-compose.dual.yml down -v --remove-orphans

  migration-tier2:                     # ruler tier — nightly/dispatch only
    if: github.event_name == 'schedule' || github.event_name == 'workflow_dispatch'
    needs: [migration-setup, migration-tier1]        # firing parity requires query parity green first
    # extends tier1 compose with shadow ruler + dead-end alertmanager; runs MIG-09/13/18/19/24

  migration-e2e:                       # aggregator — informational roll-up
    needs: [migration-setup, migration-tier0, migration-tier1, migration-tier2]
    if: always()
    runs-on: ubuntu-latest
    steps:
      # skipped tier2 (on push) must NOT fail the roll-up — treat skipped as OK,
      # fail only on a real failure, the idiom compose-smoke's aggregator uses.
      - run: |
          if ${{ contains(needs.*.result, 'failure') }}; then exit 1; fi
```

Push-to-main runs Tier-0 + Tier-1 (fast, informational); the heavy Tier-2 ruler
tier runs on nightly `schedule` + `workflow_dispatch` only. Because Tier-2 is
skipped on push, the aggregator keys on `!contains(needs.*.result, 'failure')`
(the same idiom `compose-smoke` uses) rather than requiring every tier to be
`success` — a *skipped* Tier-2 must not fail the merge roll-up. `fail-fast:
false` and `cancel-in-progress: false` mirror the existing e2e lanes (a
half-killed compose teardown leaks volumes).

Every scenario writes its `migrate` JSON outputs into a per-scenario evidence
dir, uploaded via `actions/upload-artifact` under a per-archetype name (a static
name collides across a matrix). On failure the runner also dumps
`docker compose ps` + last-200 logs for prometheus / otel-collector /
clickhouse / cerberus / ruler / alertmanager (the same shape as e2e.yml's dump
step). A failing scenario reports `::error::MIG-NN <story>: <first blocker>` —
for `verify` that is the first-diff point plus the copy-pasteable
`migrate verify …` repro the CLI already emits; for `gate` it is the blocking
stage; for offline scenarios it is the diffed golden line.

### 6.2. Honesty guardrails

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
  gate — never a vacuous pass. The story ↔ scenario coverage manifest is a
  **raise-only ratchet**.
- **Read-only where the CLI is read-only.** Scenarios never auto-provision
  schema (MIG-10 keeps DDL application a deliberate human step) and never mutate
  a real Grafana; the synthetic Grafana in the stack is the only thing driven.

## 7. Archetype seed profiles

Eight archetypes, each a directory named for the archetype under
`test/e2e/migration/archetypes`, contributing `rules/` (recording+alerting),
`dashboards/` (exported Grafana
JSON), `seed/` (an OTLP metric generator config **and** the Prometheus-side
synthetic exposition/scrape config, so the dual-write is genuinely
dual-sourced), and `expected/` (golden offline assertions).

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

**Phase 1 — Tier-0 offline (build first).** Scenario manifest + coverage
ratchet + `migration-e2e.mjs` + the `migration-e2e.yml` skeleton running Tier-0
only. Scenarios MIG-01, MIG-03, MIG-04, MIG-05, MIG-10 (render half), MIG-14
(lookback compute), MIG-26 (gate compute), plus the `gate` fold. Lands the eight
archetype `rules/` + `dashboards/` + `expected/` fixtures (the seed telemetry
generators come in Phase 2). Dependencies: none beyond the merged CLI — no
Docker, seconds to run, so it ships and starts catching regressions immediately.

**Phase 2 — Tier-1 dual-backend.** `docker-compose.dual.yml` (Prometheus + OTel
collector + ClickHouse + cerberus) + collector config + the per-archetype
`seed/` generators (incl. the pod-restart counter-reset + `container_id`-churn
seeder). Scenarios MIG-02, MIG-06, MIG-07, MIG-08, MIG-10 (diff half), MIG-11,
MIG-12, MIG-13 (read-back half), MIG-14 (live TTL), MIG-15, MIG-16, MIG-17,
MIG-20, MIG-22, MIG-23, MIG-25, MIG-26 (live TTL). A **Phase 2b** three-signal
variant adds Loki + Tempo + Grafana for MIG-21. Dependencies: Phase-1 corpora
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
