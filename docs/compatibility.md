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
[Per-head confidence](#per-head-confidence) below).

> **What gates vs. what scores.** All three `compatibility/<head>` checks
> run on every PR and are required. The *harness* is report-only on
> per-case parity drift by design
> ([#503](https://github.com/tsouza/cerberus/pull/503)) — drift is
> captured in the report + the live badge score, not in the harness exit
> code — but the required check also runs a **parity-regression ratchet**
> that fails the job if the score drops below a committed per-head floor.
> So the badges are a continuously re-measured conformance score, *and* a
> drop below baseline is a merge gate: noise within the baseline is
> tolerated, a real regression is not. The
> `compatibility/prometheus-forced-route` lane additionally hard-fails on
> *any* numeric parity diff (`FAIL_ON_DIFF=1`) but is informational, not a
> required check. See [CI integration](#ci-integration).

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
  template-expanded to 574 concrete cases.
- **Today**: **574/574** cases pass; no allow-list exists. This is the
  highest-confidence leg — an industry-standard conformance suite against
  a real reference. (Parity drift is report-only in CI; the 574/574 is a
  measured score, not a merge gate — see the note at the top of this
  page.)

### LogQL — `grafana/loki:pkg/logql/bench`

- **Driver**: cerberus-owned `loki-compliance-tester`, shape-compatible
  JSON report with the Prom driver so both feed a single downstream
  analyser.
- **Corpus**: vendored
  [`grafana/loki:pkg/logql/bench/queries/{fast,regression,exhaustive}`](https://github.com/grafana/loki/tree/main/pkg/logql/bench/queries);
  the widened corpus's `${SELECTOR}` / `${LABEL_*}` templates resolve
  off `dataset_metadata.json`.
- **Reference**: a real Loki container on `:23100`, seeded from the same
  in-memory fixture as cerberus.
- **Today**: shipped and running as the required `compatibility/loki` PR
  check; no allow-list exists. Solid confidence — a real backend on a
  real corpus — but Grafana's `bench` set is a benchmark corpus, not a
  standardised conformance suite like PromQL's. Parity drift is
  report-only.

### TraceQL — cerberus-owned driver

- **Driver**: cerberus-owned binary with `seed` + `diff` subcommands
  (OTLP push to Tempo + direct CH `INSERT` to cerberus, both from one
  in-memory fixture so per-span fields stay 1:1 across both read paths),
  patterned on `cmd/tempo-vulture`.
- **Corpus**: cerberus-owned TXTAR corpus. **There is no third-party
  TraceQL conformance suite** (no TraceQL analogue of
  `prometheus/compliance`), so this corpus is author-written rather than
  derived from an external standard — the lightest of the three legs.
- **Today**: shipped and running. `/api/search`, `/api/traces/<id>`, the
  four tag / tag-values endpoints (V1 + V2), and the metrics endpoints
  (`/api/metrics/query_range` + `/api/metrics/query`) all run under the
  required `compatibility/tempo` PR check; no allow-list exists. Parity
  drift is report-only, like the other two heads.

## Per-head confidence

The three legs are *not* equally strong, and the docs should not imply
they are:

| Head    | Reference          | Corpus origin                                  | Numerical confidence                                                                        |
| ------- | ------------------ | ---------------------------------------------- | ------------------------------------------------------------------------------------------- |
| PromQL  | real Prometheus    | third-party `prometheus/compliance` (CNCF)     | **Highest** — industry-standard conformance suite, 574/574, no allow-list                   |
| LogQL   | real Loki          | Grafana's own `pkg/logql/bench` corpus         | **Solid** — real backend + real corpus, but a Grafana bench set, not a conformance standard |
| TraceQL | real Tempo         | cerberus-owned author-written TXTAR            | **Lowest** — real backend, but no third-party suite; corpus breadth is author-bounded       |

All three run against a real reference backend on identical seeded data,
so each catches genuine semantic divergence. The difference is *corpus
provenance and breadth*: PromQL inherits an externally-curated standard;
TraceQL's coverage is only as wide as the author wrote it. Raising
TraceQL's confidence is the top improvement item (see the project
roadmap / the PR that introduced this section).

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

After a corpus re-snapshot, audit the skip-set diff, then regenerate
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

1. **Catalogue** — `test/rejection-parity/catalogue.json` is the
   machine-readable inventory of every prefixed error-construction
   site in the three lowerings, derived by a go/ast scan
   (`test/rejection-parity`). Every site is classified either
   `rejection` (reachable from a parseable query; carries a minimal
   trigger query) or `internal` (parser-enforced shape, invariant, or
   `%w` wrapper; carries a rationale).
2. **Meta-tests** — `go test ./test/rejection-parity/` pins the
   ratchet: the scanned-site set must equal the catalogue
   (regenerable via `CERBERUS_UPDATE_INVENTORY=1`, mirroring
   `test/inventory`), every entry must be classified, every
   `rejection` trigger must parse with the head's reference parser
   AND fail the head's lowering with the catalogued message, and the
   parity corpus is derived 1:1 from the rejection entries. Adding a
   new rejection to a lowering therefore *requires* a catalogue entry,
   a trigger query, and — by construction — a parity case.
3. **Parity driver** — `compatibility/cmd/rejection-parity` runs
   inside each harness (wired into the three run scripts, after the
   main tester) and sends every trigger query to both backends. It
   compares the rejection **status class** only (both 4xx = parity);
   message text is never compared. Verdicts:
   - `parity` — both backends reject; the claim holds.
   - `wrong_rejection` — the reference backend accepts a query
     cerberus rejects: a real bug to fix at the source (the
     `kind != nil` class). There is no allow-list for these.
   - `stale_catalogue` — cerberus accepted a query the catalogue says
     it rejects; regenerate + re-curate the catalogue.
   - `hard_error` — 5xx / transport failure (infrastructure).

   Reports land at `compatibility/prometheus/rejection-parity.json`,
   `compatibility/loki/reports/rejection-parity.json`, and
   `compatibility/tempo/reports/rejection-parity.json`. Like the main
   testers, the driver is report-only: verdicts never change the exit
   code; only infrastructure failures do.

## CI integration

`.github/workflows/compatibility.yml` runs all three harnesses:

- on **PRs** touching `internal/{promql,logql,traceql,chsql,optimizer,chplan}/`,
  `internal/api/{prom,loki,tempo}/`, or `compatibility/*`;
- on **push to `main`**;
- **nightly** at 04:11 UTC;
- on **manual `workflow_dispatch`**.

Each harness job uploads its report as a workflow artifact (30-day
retention). On push-to-main, the per-head pass-rate is appended to the
orphan `compat-scores` branch so the README badges refresh.

**Required: scored, plus a regression ratchet.** All three
`compatibility/<head>` checks are required status checks on `main`. The
*harness* step itself is report-only on parity — per
[#503](https://github.com/tsouza/cerberus/pull/503) it captures per-case
numeric drift in `report.json` + the badge and exits 0, failing only on
**infrastructure** errors (compose-up, seed, build, unparseable report).
But the required job no longer passes on a numeric regression: a
**parity-regression ratchet** step (next section) runs after the harness
and fails the job when the run's score drops below the committed
per-head floor. So the required check gates **both** infrastructure
breakage **and** a parity *regression* — while still tolerating noise
within the baseline, which is what #503 was protecting against.

The `compatibility/prometheus-forced-route` lane additionally
**hard-fails on any parity diff** (`FAIL_ON_DIFF=1` in
`run-compatibility.sh`) as the corpus-wide proof that the sharded solver
route is byte-identical to reference Prometheus; it is intentionally an
*informational* job, not a required check, so it doesn't gate every
unrelated PR on the full forced-route corpus run.

### Parity-regression ratchet (the gate)

The three differs are **scored** — they accumulate per-case results,
write `compat-score.json`, and exit 0 even when a case diverges, so the
harness step turns the job red only on infrastructure breakage (corpus
load, compose-up, missing report). On its own that makes the score an
informational badge, not a gate: a real numeric regression on the main
route would merge green.

The **parity-regression ratchet** closes that hole and makes
"compatibility is the source of truth" a real gate. After each harness
runs, `.github/scripts/compat-ratchet.mjs` reads the run's
`compat-score.json` and the committed floor in
`compatibility/parity-baseline.json`, and **fails the required job when
the run drops below baseline** — either fewer passing cases (a real
regression) or a corpus smaller than baseline (which could otherwise
mask a regression by dropping a failing case). A run that matches or
exceeds the floor passes; noise *within* the baseline is tolerated.

The floors today (`heads.<name>.{passed,total}`):

| head       | passed/total |
| ---------- | ------------ |
| prometheus | 574 / 574    |
| loki       | 116 / 116    |
| tempo      | 48 / 48      |

This is **not** an allow-list and does not resurrect the deleted
`expected-failures.json`: it never names an individual case or excuses a
specific failure — it pins only the aggregate floor and rejects any drop
below it. It cannot flake because the differs compare with
absolute + relative epsilon tolerance over canonical-key-sorted result
sets against a deterministic seed, so `passed`/`total` are stable run to
run (verified: three consecutive green main runs produced byte-identical
574/574, 116/116, 48/48); the ratchet then compares **integers**, with
no float/timing/ordering surface left to jitter.

When the harness legitimately gains passing cases or the corpus grows,
**raise the matching floor** in `compatibility/parity-baseline.json` in
the same PR — the ratchet's pass log prints the exact new numbers. Never
*lower* a floor to make a real parity bug merge; fix the bug at the
source instead.

## Adding new test cases

The upstream corpus covers the bulk of each query language. If you
discover a query that cerberus mishandles but the corpus doesn't cover:

- **PromQL**: open a PR to
  [`prometheus/compliance`](https://github.com/prometheus/compliance)
  adding the query (so every adapter benefits), then bump the submodule
  SHA under `compatibility/prometheus/upstream`.
- **LogQL**: same upstream path against `grafana/loki/pkg/logql/bench`.
- **TraceQL**: the corpus is cerberus-owned; add a TXTAR case under
  `compatibility/tempo/driver/corpus/`.

Cerberus-specific cases (OTel-CH schema quirks, ClickHouse-only edge
cases) belong in `test/spec/<head>/` as TXTAR fixtures, not in the
compatibility harness.
