# Compatibility harnesses

Cerberus's correctness is measured by three differential-parity
harnesses, one per upstream API. Each diffs query results between a
reference backend and cerberus, both seeded with the same deterministic
fixture over the same time window.

The strongest of the three is PromQL: it runs the **third-party PromQL
Compliance Tester** (`prometheus/compliance`, the PromLabs / CNCF
Prometheus Conformance Program tooling) against a real `prom/prometheus`
— not a home-grown diff. LogQL and TraceQL use cerberus-owned drivers
against real Loki / Tempo; TraceQL additionally has no third-party
conformance suite to draw on, so its corpus is author-written and its
numerical confidence is honestly lower (see
[Per-head confidence](compatibility.background.md#per-head-confidence)
below).

> **What gates vs. what scores.** `.github/workflows/compatibility.yml` posts
> six `compatibility/<lane>` checks: `compatibility/prometheus`,
> `compatibility/loki`, `compatibility/tempo`,
> `compatibility/prometheus-forced-route`, `compatibility/prometheus-floor`
> (the PromQL differential against the minimum supported ClickHouse) and
> `compatibility/promql-surface` (the live function-surface probe). The first
> four are named in `release.yml`'s `RELEASE_REQUIRED_CHECKS`; the last two
> carry `release_posture: advisory` in `.github/ci-lanes.json`. All six are
> **release-gate** lanes, not PR-blocking ones (#2230): they short-circuit
> to a fast no-op on an ordinary PR or merge-group entry, and run for real on
> push to `main`, on schedule/dispatch, and on a `release/*` head-branch PR —
> where `release.yml`'s preflight requires each to post a green check-run
> before anything publishes. The *harness* is report-only on per-case parity
> drift by design
> ([#503](https://github.com/tsouza/cerberus/pull/503)) — drift is
> captured in the report + the live badge score, not in the harness exit
> code — but the release-gate run also runs a **parity-regression ratchet**
> that fails the job if any case moves against the committed per-head
> roster. So the badges are a continuously re-measured conformance score,
> *and* every individual case is a release gate: the ratchet names the cases
> that must pass, so a regression cannot be offset by an unrelated win. The
> `compatibility/prometheus-forced-route` lane additionally hard-fails on
> *any* numeric parity diff (`FAIL_ON_DIFF=1`) under the same release-gate
> posture. See [CI integration](#ci-integration).

| Harness | Location                    | Reference backend                  | Corpus source                                                                                |
| ------- | --------------------------- | ---------------------------------- | -------------------------------------------------------------------------------------------- |
| PromQL  | `compatibility/prometheus/` | Reference Prometheus on `:29090`   | [`prometheus/compliance`](https://github.com/prometheus/compliance) submodule under upstream |
| LogQL   | `compatibility/loki/`       | Reference Loki on `:23100`         | Vendored `grafana/loki/pkg/logql/bench` snapshot at `upstream/loki-bench/`                   |
| TraceQL | `compatibility/tempo/`      | Reference Tempo on `:23200`        | Cerberus-owned TXTAR corpus under `compatibility/tempo/driver/corpus/`                       |

Scores are published to the orphan
[`compat-scores`](https://github.com/tsouza/cerberus/tree/compat-scores)
branch as shields.io badge JSON; the README shows them live. On
`push: main` the workflow commits a fresh `compat-score.json` under
`badges/<head>.json`, which the shields.io endpoint badges read from.

## Per-head detail

### PromQL — `prometheus/compliance`

- **Driver**: the upstream third-party `promql-compliance-tester` — the
  PromLabs / CNCF Prometheus Conformance Program tool, vendored as the
  `prometheus/compliance` submodule under
  `compatibility/prometheus/upstream/`. Not a cerberus-authored diff.
- **Reference**: a real `prom/prometheus` container seeded with the
  *same* fixture as cerberus's ClickHouse (the seeder reads the CH rows
  back and mirrors them into Prometheus over remote-write — see
  `compatibility/prometheus/cmd/seed/prom_remote.go` — so both backends answer from byte-identical
  data).
- **Corpus**: vendored
  [`prometheus/compliance/promql/promql-test-queries.yml`](https://github.com/prometheus/compliance),
  template-expanded to concrete cases, plus a small cerberus-owned tail for
  shapes upstream cannot express — resource-attribute grouping, and native
  histograms, whose data upstream's float-only demo fixture never carries.
  The case count is reconstructed as `heads.prometheus.total` from
  [`compatibility/parity-baseline/`](../compatibility/parity-baseline/manifest.json).
- **Native-histogram merge width**: a merge of exponential histograms —
  `sum()` / `avg()` across series, `histogram_quantile()` over an
  aggregate, `histogram + histogram` — whose natural bucket range at the
  rows' minimum scale exceeds 160 buckets (the OTel SDK default bucket
  budget, `chplan.OTelExpoHistogramDefaultMaxSize`) is answered at a
  coarser scale than Prometheus computes: the scale is lowered by
  `ceil(log2(width / 160))` so the merged ladder holds at most 161
  buckets. Count, sum and zero-count are exact at any scale; only the
  bucket ladder, and any quantile read from it, is coarser. Prometheus
  merges at the minimum scale with sparse spans and applies no budget.
- **Native-grid duplicate-timestamp NaN survivor**: when a query lowers
  onto a `timeSeries*ToGrid` aggregate (the auto-enabled `ts_grid_*`
  features on a server >= 25.9 — see
  [`clickhouse-optimizations.md`](clickhouse-optimizations.md)) and one
  series carries two samples at the SAME timestamp, one of them `NaN`, the
  sample that survives depends on the order the rows reach the aggregate,
  not on the sample multiset. Prometheus, and cerberus's own array-fold
  fan-out, always answer from the same survivor (`arraySort` ranks `NaN`
  greatest). Reproduced on chDB 26.5.1.1 directly against the aggregate,
  isolated from cerberus's lowering:

  ```sql
  SELECT gv FROM (
    SELECT
      timeSeriesDeltaToGrid(toDateTime(60), toDateTime(60), 60, 60)(ts, val) AS grid,
      timeSeriesRange(toDateTime(60), toDateTime(60), 60) AS grid_ts
    FROM (
      SELECT toDateTime(20) AS ts, nan AS val
      UNION ALL SELECT toDateTime(20), 25.0
      UNION ALL SELECT toDateTime(50), 40.0
    )
  ) ARRAY JOIN grid AS gv, grid_ts AS gt
  SETTINGS allow_experimental_time_series_aggregate_functions = 1
  -- NaN inserted first: returns nan. Swap the first two rows: returns 30.
  ```

  `timeSeriesRateToGrid` over the same rows answers `nan` / `0.5` the same
  way; the two instant members (`timeSeriesInstantRateToGrid`,
  `timeSeriesInstantDeltaToGrid`) invert which order keeps the finite sample.
  The shape needs two samples at one series' exact timestamp with one of
  them `NaN`; a window without such a duplicate is unaffected, and
  `CERBERUS_CH_OPTIMIZATIONS=off` (or a list omitting the `ts_grid_*` ids)
  keeps every query on the order-independent fan-out.
- **Posture**: every case passes; no allow-list exists. This is the
  highest-confidence leg — an industry-standard conformance suite against
  a real reference. (Parity drift is report-only in CI; the score is a
  measurement, not a merge gate — see the note at the top of this
  page.)

### LogQL — `grafana/loki:pkg/logql/bench`

- **Driver**: cerberus-owned `loki-compliance-tester`, shape-compatible
  JSON report with the Prom driver so both feed a single downstream
  analyser.
- **Corpus**: vendored
  [`grafana/loki:pkg/logql/bench/queries/{fast,regression,exhaustive}`](https://github.com/grafana/loki/tree/main/pkg/logql/bench/queries);
  the widened corpus's `${SELECTOR}` / `${LABEL_*}` templates resolve
  off `dataset_metadata.json`. Plus a small cerberus-owned additive
  corpus under `compatibility/loki/cerberus-queries/` (same suite/file
  layout, merged in by the driver) for behaviour the vendored corpus
  has no coverage for at all.
- **Reference**: a real Loki container on `:23100`, seeded from the same
  in-memory fixture as cerberus.
- **Beyond the corpus**: the range lane also diffs the routes Grafana's
  UI is driven off — `/detected_fields`, `/detected_field/{name}/values`,
  `/labels`, `/label/{name}/values`, `/series`, `/index/stats`,
  `/index/volume`, `/detected_labels` and `/patterns` — status plus data
  set, scored like corpus cases. `/index/stats` grades `streams` and
  `entries`, `/index/volume` grades the label sets and the tie-break
  ranking, and `/patterns` grades template text exactly against the
  seeder's constant fixture line and nowhere else
  (`compatibility/loki/README.md` names each route's graded fields).
- **Posture**: runs as the release-gate `compatibility/loki`
  check; no allow-list exists. Solid confidence — a real backend on a
  real corpus — but Grafana's `bench` set is a benchmark corpus, not a
  standardised conformance suite like PromQL's. Parity drift is
  report-only.

### TraceQL — cerberus-owned driver

- **Driver**: cerberus-owned binary with `seed` + `diff` + `diff-grpc`
  subcommands (OTLP push to Tempo + direct CH `INSERT` to cerberus, both
  from one in-memory fixture so per-span fields stay 1:1 across both read
  paths), patterned on `cmd/tempo-vulture`.
- **Corpus**: cerberus-owned TXTAR corpus. **There is no third-party
  TraceQL conformance suite** (no TraceQL analogue of
  `prometheus/compliance`), so this corpus is author-written rather than
  derived from an external standard — the lightest of the three legs.
- **Posture**: `/api/search`, `/api/traces/<id>`, the
  four tag / tag-values endpoints (V1 + V2), and the metrics endpoints
  (`/api/metrics/query_range` + `/api/metrics/query`) all run under the
  release-gate `compatibility/tempo` check; no allow-list exists. Parity
  drift is report-only, like the other two heads.
- **Two transport arms, one release-gate check**: `diff` drives the corpus
  over HTTP; `diff-grpc` (#1453) drives the SAME corpus over cerberus's
  and reference Tempo's `tempopb.StreamingQuerier` gRPC/h2c service —
  the transport Grafana's Tempo datasource actually opens when
  "Streaming" is enabled, which the HTTP-only harness could not exercise
  at all. Both arms run inside the one `compatibility/tempo` job and gate
  independently (`compatibility/parity-baseline/`'s reconstructed `heads.tempo` /
  `heads.tempo-grpc`), so a regression on either transport fails the
  check. The gRPC arm's roster (`heads.tempo-grpc`) is two cases smaller
  than the HTTP arm's (`heads.tempo`): `traces` /
  `traces_v2` have no `StreamingQuerier` RPC (trace-by-id is
  HTTP/proto-only on both backends) and are reported as skipped rather
  than run — see `compatibility/tempo/driver/grpc_diff.go`'s file-level
  comment for the full wire-contract trace, including where reference
  Tempo's own gRPC listener lives (the query-frontend module's dedicated
  `:9095`, not multiplexed onto its HTTP port the way cerberus's h2c
  listener is).

## Scope: single ClickHouse data shard

All three harnesses target a single-node/single-data-shard ClickHouse — a
permanent, stated scope boundary (cerberus issue #3079), not an open-ended
deferral. Multi-data-shard `Distributed`-table correctness is a ClickHouse
distributed-query-execution concern, not a query-LANGUAGE-fidelity one (none
of Prometheus, Loki, or Tempo has any concept of a ClickHouse data shard to
diff against), so it is validated directly — with real `system.query_log`
evidence these reference-diff harnesses have no way to produce — by the
dedicated `datashard` e2e leg instead. See
[`operations.md`](operations.md#compat-and-migration-lane-scope-single-clickhouse-data-shard)
for the full reasoning.

## Local run

```sh
just compat-promql   # PromQL harness
just compat-logql    # LogQL harness
just compat-traceql  # TraceQL harness
just compat-all      # all three sequentially
```

Each recipe:

1. Brings up the harness's docker-compose stack (reference backend +
   cerberus + ClickHouse + a one-shot seeder).
2. Builds the upstream compliance-tester (or runs cerberus's driver,
   for Loki / Tempo).
3. Diffs the two endpoints over the seeded window and writes a report
   to `compatibility/<head>/reports/`.
4. Tears the stack down.

Each harness's compose project is named per checkout —
`cerberus-compatibility` and its two siblings, plus the suffix
`scripts/compose-project-suffix.sh` derives from the checkout's path (empty in a
primary checkout and in CI). Two checkouts run two independent stacks instead of
adopting and tearing down each other's containers; the recipes and the harness
scripts both derive it, so it applies whichever way a run is launched.

Set `COMPOSE_KEEP=1` to leave the stack running for inspection:

```sh
COMPOSE_KEEP=1 just compat-promql
# poke around; then
just compat-promql-down
```

## Reading the PromQL report

```sh
jq '{
  total: ([.results[]?] | length),
  passed: ([.results[]? | select(.unexpectedFailure == null and .diff == null)] | length),
  diffs: ([.results[]? | select(.diff != null)] | length),
  unexpected_failures: ([.results[]? | select(.unexpectedFailure != null)] | length)
}' compatibility/prometheus/reports/report.json
```

A passing run has no `unexpectedFailure` entries and no `diff`
entries. The LogQL and TraceQL reports follow the same shape.

## No allow-lists

There is no `expected-failures.json` / `should_skip` allow-list for
any of the three heads. Every diff against the reference backend is
a real bug to fix at the source (cerberus code, seed, or upstream
config). The `forbid-skip` CI gate rejects:

- Any non-empty `should_skip:` block in `compatibility/**/*.{yml,yaml}`.
- Any test-suite escape-hatch primitive (`EXPECTED_EMPTY`,
  `EXPECTED_TOLERATED`, `isKnownTolerated*`, `tolerated404`,
  `expect.soft`, `should_tolerate`, `SkipReason`/`skipReason`).

If a diff surfaces noise that isn't a cerberus bug (e.g. upstream
behaviour change after a Prom/Loki/Tempo bump), the fix is to update
the reference image pin or the seeder — never to add a per-case
exception.

## Floating-point comparison in the parity oracle

Every diff against a reference backend is a real bug — with one
qualification that is about the *comparison*, not about any case: two
`float64` answers are compared with a relative tolerance, not with `==`.

`oracle.EqualValues` (`test/spec/parityoracle/promql/oracle.go`) is the
single comparator, and `summationReorderRelativeTolerance` is the single
number. It is derived, not fitted: the standard backward-error bound for
summing *n* floats gives two different summation orders a relative
separation of at most `2(n-1)u` with `u = 2^-53`, and the constant is
that expression at a stated sample budget of 4096 samples per output
value — about `9.09e-13`.

- **This is not an allow-list.** One tolerance applies to every value on
  every fixture. No fixture can opt into it, widen it, or be excused by
  it; there is no `parity:` key and no `scope:` value that reaches it.
- **It still catches real divergence.** The divergences the bound accepts
  are ULP-scale and sit orders of magnitude inside it, while the smallest
  genuine disagreement the round-trip lane has produced sits orders outside
  it. `TestEqualValuesRejectsRealDivergence` pins that separation with a
  required headroom factor, so the tolerance cannot be widened toward a
  real disagreement without a test going red.
- **Production pushdown is unchanged.** Cerberus still evaluates
  vector-involving `atan2`, `^`, and window sums in ClickHouse SQL
  rather than in Go.

See
[`compatibility.background.md`](compatibility.background.md#why-the-parity-oracle-compares-with-a-relative-tolerance)
for why bit-for-bit agreement is not reachable here, the measured
divergences behind the bound, and why results are not buffered
client-side to chase it.

## Two PromQL reference engines, one configuration

Cerberus grades PromQL against two independent reference surfaces, not
one:

- **The spec lane's oracle** (`test/spec/parityoracle/promql/oracle.go`)
  builds its own `promql.Engine` in-process. Most PromQL fixtures live in
  the spec lane, and a change is graded here first.
- **The compat lane** (`compatibility/prometheus/`, described above)
  diffs against a real, separately started `prom/prometheus:v3.11.3`
  server — the higher-confidence surface, and, per invariant 7
  (`CLAUDE.md`), the one whose behaviour is authoritative for all three
  heads.

Both surfaces run the reference engine's semantics-affecting options
identically: the real server enables only `promql-experimental-functions`
(`compatibility/prometheus/docker-compose.yml`), leaving
`promql-delayed-name-removal` at Prometheus's documented default of
**off**, and the spec oracle builds its `promql.Engine` explicitly with
`EnableDelayedNameRemoval` set to match. The real server is the
authority; when the two disagree, the oracle is aligned to the server.
`test/regression/promql_oracle_engine_parity_test.go` is the mechanical
guard: it reads the compose file's `--enable-feature` line and the
oracle's `EnableDelayedNameRemoval` constant and fails if they disagree,
so a change to either side surfaces as a CI failure naming both sites.

One residual difference is intentional: the oracle's parser options
(`promqltest.TestParserOpts`) accept a broader grammar — experimental
functions, extended range selectors, duration expressions, binop fill
modifiers — than the compat server's single enabled flag. That is a PARSE
acceptance difference, not an ANSWER difference (an upstream parse
rejection on a cerberus-only extension is a fact about the fixture, not a
parity failure), so it is not held to the same rule. See
[`compatibility.background.md`](compatibility.background.md#why-the-spec-oracle-runs-delayed-name-removal-off)
for the shape on which the two settings diverge and why the server's
default won.

## Upstream-skip baseline (LogQL)

The vendored `loki-bench` corpus contains a handful of queries that
*upstream itself* marks `skip: true` in the YAML — cases Loki's own
v2-engine test suite declines to run (quantile / stddev / stdvar unwrap
aggregations, some structured-metadata filters). For those entries the
reference Loki provides no baseline to diff against, so they cannot be
scored: a differential harness needs both sides to answer.

This is **not** an allow-list. The boundary is drawn by the upstream
corpus, not by cerberus, and it never suppresses a diff: the badge
denominator counts the *runnable* corpus — every entry upstream marks
runnable is seeded, executed against both backends, and scored, with
zero cerberus-side exclusions on top.

`compatibility/loki/upstream-skip-baseline.txt` is the trip-wire that
keeps that boundary honest. The driver loads the full corpus
(including skipped entries), partitions it into runnable +
upstream-skipped, and asserts the upstream-skipped set exactly matches
the file — one `<suite>/<file>.yaml#<description>` key per line. Drift
in either direction fails the harness:

- a new upstream `skip: true` would otherwise silently shrink the
  scored denominator;
- an upstream `skip: true` → `skip: false` flip (e.g. the v2 engine
  gaining quantile support) would otherwise silently add a query to
  the scored set without anyone triaging cerberus's parity for it.

After a corpus re-snapshot, inspect the skip-set diff, then regenerate
the baseline with:

```sh
loki-compliance-tester \
    -corpus=compatibility/loki/upstream/loki-bench/queries \
    -skip-baseline=compatibility/loki/upstream-skip-baseline.txt \
    -regen-baseline
```

See `compatibility/loki/README.md` for the full mechanism.

## Rejection parity

Cerberus's deliberate rejections — the HTTP 422 "valid query, but the
lowering refuses it" paths in `internal/{promql,logql,traceql}` — are
claims about reference behaviour: "the reference backend cannot answer
this either". The rejection-parity layer verifies those claims
differentially, so a query cerberus rejects but the reference accepts
(the `kind != nil` class, which reference Tempo answers) surfaces as a
real bug rather than a silent wrong-rejection:

1. **Catalogue** — `test/rejection-parity/catalogue/` is the
   machine-readable inventory of every prefixed error-construction
   site in the three lowerings, derived by a go/ast scan
   (`test/rejection-parity`). Every site is classified into one of
   three classes: `rejection` (reachable from a parseable query, and the
   reference rejects it too; carries a minimal trigger query),
   `internal` (parser-enforced shape, invariant, or `%w` wrapper; carries
   a rationale), or `divergence` (cerberus rejects a query the reference
   answers, on purpose; carries everything `rejection` carries PLUS an
   open tracking issue number and a `since` date). The `divergence` set
   is held under two ratchets pinned by `divergence_ratchet_test.go`: a
   monotonic count ceiling (`divergence-ceiling.json`, which only a
   hand-edited bump in the same diff can raise) and a per-entry age cap
   (`divergenceStaleAfter`) that fails the build once an entry has sat
   open past the threshold. It is stored as one JSON shard
   per lowering SOURCE FILE —
   `catalogue/internal__promql__subquery.go.json` holds exactly the
   entries whose site keys name `internal/promql/subquery.go` — so two
   branches fixing guards in different lowering files write different
   files and never blend. `LoadCatalogue` merges the shards back into
   one site-sorted value, so every consumer sees the same flat
   catalogue it always did.
2. **Meta-tests** — `go test ./test/rejection-parity/` pins the
   ratchet: the scanned-site set must equal the catalogue
   (regenerable via `CERBERUS_UPDATE_INVENTORY=1`, mirroring
   `test/oracle/inventory`), every entry must be classified, every
   `rejection` trigger must parse with the head's reference parser
   AND fail the head's lowering with the catalogued message, and the
   parity corpus is derived 1:1 from the `rejection` and `divergence`
   entries. Adding a new rejection to a lowering therefore *requires* a
   catalogue entry, a trigger query, and — by construction — a parity
   case.
3. **Parity driver** — `compatibility/cmd/rejection-parity` runs
   inside each harness (wired into the three run scripts, after the
   main tester) and sends every trigger query to both backends. It
   compares the rejection **status class** only (both 4xx = parity);
   message text is never compared. Seven verdicts; the first four apply
   to `rejection` entries, the next three to `divergence` entries:
   - `parity` — both backends reject; the claim holds.
   - `wrong_rejection` — the reference backend accepts a query
     cerberus rejects: a real bug to fix at the source (the
     `kind != nil` class). There is no allow-list for these.
   - `stale_catalogue` — cerberus accepted a query the catalogue says
     it rejects; regenerate + re-curate the catalogue.
   - `hard_error` — 5xx / transport failure (infrastructure).
   - `divergence_confirmed` — cerberus rejects, the reference answers:
     the expected, passing state for a live divergence.
   - `divergence_resolved` — cerberus now answers the query; the
     divergence closed from cerberus's side and the entry must be
     deleted.
   - `divergence_closed` — the reference now also rejects; the entry
     must be reclassified to `rejection`.

   Reports land at `compatibility/prometheus/rejection-parity.json`,
   `compatibility/loki/reports/rejection-parity.json`, and
   `compatibility/tempo/reports/rejection-parity.json`. Unlike the main
   testers, this driver is **not** report-only: a `wrong_rejection`,
   `divergence_resolved` or `divergence_closed` verdict — the catalogue's
   own claims turning out false — exits non-zero and fails the harness
   under `set -e`. Only `stale_catalogue` and `hard_error` stay non-fatal.

   Two harness conditions make the PromQL verdicts meaningful, both pinned
   by `test/regression/compat_rejection_parity_reference_test.go`:

   - the reference Prometheus runs with
     `--enable-feature=promql-experimental-functions`, matching cerberus's
     own parser config, so a "both 4xx" verdict can never record agreement
     about a feature flag instead of about the guard under test;
   - the driver is given `-eval-time` inside the seeded fixture window, so
     upstream guards that validate per series (for example
     `double_exponential_smoothing`'s smoothing / trend factors) actually
     run instead of short-circuiting on an empty selector.

## CI integration

`.github/workflows/compatibility.yml` runs all three harnesses:

- on **every PR** — deliberately no `paths:` filter, so the three head checks
  always appear in the status-check rollup, but each job short-circuits to a
  fast no-op unless the PR is a `release/*` head branch (`changes`'
  `run_heavy` output) — an ordinary PR reports green in seconds without
  booting any reference backend;
- on **push to `main`** (and to `release/*.x`, so a maintenance-line hotfix
  also gets a real compatibility run), where the job runs for real;
- **nightly**, offset across the three heads to spread runner load:
  prometheus at 04:11 UTC, tempo at 04:23 UTC, loki at 04:37 UTC;
- on **manual `workflow_dispatch`**.

Each harness job uploads its report as a workflow artifact (30-day
retention). On push-to-main, the per-head pass-rate is appended to the
orphan `compat-scores` branch so the README badges refresh.

**Release-gated: scored, plus a regression ratchet.** All six
`compatibility/<lane>` checks are **release-gate** lanes (#2230,
the merge/release two-tier test fence), not required PR status checks —
`gh api repos/tsouza/cerberus/rules/branches/main --jq '[.[] |
select(.type == "required_status_checks") |
.parameters.required_status_checks[].context] | unique[]'` does not list any
of them. Instead,
`release.yml`'s `RELEASE_REQUIRED_CHECKS` names the three per-language legs
and `compatibility/prometheus-forced-route`, and its preflight blocks a
publish until each has posted a green check-run on the commit being
shipped: the gate moved from the PR to the release, it did not disappear.
(`compatibility/prometheus-floor` and `compatibility/promql-surface` carry
`release_posture: advisory` in `.github/ci-lanes.json`.) On a real
(non-short-circuited) run, the *harness* step itself is report-only on
parity — per
[#503](https://github.com/tsouza/cerberus/pull/503) it captures per-case
numeric drift in `report.json` + the badge and exits 0, failing only on
**infrastructure** errors (compose-up, seed, build, unparseable report). The
job does not pass on a parity regression, though: a **parity-regression
ratchet** step (next section) runs after the harness and fails the job when
any case moves against the committed per-head roster. So a real run gates
**both** infrastructure breakage **and** every individual parity case, while
keeping the harness's own exit code reserved for infrastructure — which is
what #503 was protecting.

The `compatibility/prometheus-forced-route` lane additionally
**hard-fails on any parity diff** (`FAIL_ON_DIFF=1` in
`.github/scripts/run-prometheus-compatibility.mjs`) as the corpus-wide proof that the sharded solver
route is byte-identical to reference Prometheus; under the same release-gate
posture, every push / schedule / dispatch / `release/*` PR run is gated on
the full forced-route corpus.

### Parity-regression ratchet (the gate)

The three differs are **scored** — they accumulate per-case results,
write `compat-score.json` plus a per-case roster in `compat-cases.json`,
and exit 0 even when a case diverges, so the harness step turns the job
red only on infrastructure breakage (corpus load, compose-up, missing
report). On its own that makes the score an informational badge, not a
gate: a real parity regression on the main route would merge green.

The **parity-regression ratchet** closes that hole and makes
"compatibility is the source of truth" a real gate. After each harness
runs, `.github/scripts/compat-ratchet.mjs` reads the run's
`compat-cases.json` and the committed roster in
`compatibility/parity-baseline/`, and **fails the job on any
case that moved**. It gates on case *identity*, not on a count, because a
count cannot tell a swap from a steady state: one case regressing while a
different one starts passing leaves `passed`/`total` untouched, so an
aggregate comparison reports green while parity got worse on a real query.

Four verdicts, all fatal:

| verdict         | meaning                                      | how it is resolved               |
| --------------- | -------------------------------------------- | -------------------------------- |
| REGRESSED       | a recorded case now diverges                 | fix the engine                   |
| VANISHED        | a recorded case did not run at all           | restore it, or move the baseline |
| ARRIVED-FAILING | a case new to the corpus diverges on arrival | fix the engine                   |
| UNRECORDED      | a case new to the corpus passes              | move the baseline so it is gated |

The rosters live in
[`compatibility/parity-baseline/`](../compatibility/parity-baseline/manifest.json).
The shared loader reconstructs `heads.<name>.{passed,total,cases}`, one
entry per head (`prometheus`, `loki`, `tempo`, `tempo-grpc`). Their sizes
are stated there and nowhere else: the selected head's deterministic
buckets are synced from its `compat-cases.json`, and `doc-counts.mjs`
fails the build if a second copy of one of those counts comes back into
this page.

The baseline records **full parity** for every head — the ratchet asserts
`passed == total == cases.length`, so the tree has no shape in which a
divergence can be recorded as acceptable.

It cannot flake. Each case ID is built from that case's static corpus
identity (query text, endpoint, suite, lane) and never from
wall-clock-derived values, so an unchanged corpus yields a byte-identical
roster; pass/fail per case comes from the same success predicate that
feeds `compat-score.json`, comparing with absolute + relative epsilon
tolerance over canonical-key-sorted result sets against a deterministic
seed. There is no float, timing or ordering surface left to jitter.

When the corpus legitimately grows, or a case is deliberately renamed or
retired, **sync that head's buckets** in `compatibility/parity-baseline/`
in the same PR. Take the roster from the run's `compat-cases.json`
(uploaded as a job artefact) rather than hand-editing it:

```sh
node .github/scripts/compat-baseline-sync.mjs path/to/compat-cases.json
```

That writes only the selected head's owning buckets and reconstructs a
globally sorted roster with counts derived from it, so the committed list
cannot drift from what the harness actually ran and
the diff shows exactly which cases moved. It refuses to write a roster
that omits a failing case — never make a real parity bug merge by moving
the baseline around it; fix the bug at the source instead.

## Adding new test cases

The upstream corpus covers the bulk of each query language. If you
discover a query that cerberus mishandles but the corpus doesn't cover:

- **PromQL**: open a PR to
  [`prometheus/compliance`](https://github.com/prometheus/compliance)
  adding the query (so every adapter benefits), then bump the submodule
  SHA under `compatibility/prometheus/upstream`.
- **LogQL**: same upstream path against `grafana/loki/pkg/logql/bench`
  when the case would benefit every consumer of that corpus. For a
  case that needs a real differential run against reference Loki but
  is cerberus-fixture-specific (e.g. it needs a seeded stream shape
  the vendored corpus's generator never produces), add it to
  `compatibility/loki/cerberus-queries/` instead — a cerberus-owned,
  additive query corpus mirroring the vendored suite/file layout that
  the driver merges in alongside the vendored cases (see
  `compatibility/loki/cerberus-queries/README.md`). It carries no
  skip/tolerance mechanism: every case added there runs and is graded
  like a vendored one.
- **TraceQL**: the corpus is cerberus-owned; add a TXTAR case under
  `compatibility/tempo/driver/corpus/`.

Cerberus-specific cases (OTel-CH schema quirks, ClickHouse-only edge
cases) belong in `test/spec/<head>/` as TXTAR fixtures, not in the
compatibility harness.

---

For the rationale behind these choices — alternatives considered, incidents,
measurements — see [compatibility.background.md](compatibility.background.md).
