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
> that fails the job if any case moves against the committed per-head
> roster. So the badges are a continuously re-measured conformance score,
> *and* every individual case is a merge gate: the ratchet names the cases
> that must pass, so a regression cannot be offset by an unrelated win. The
> `compatibility/prometheus-forced-route` lane additionally hard-fails on
> *any* numeric parity diff (`FAIL_ON_DIFF=1`) and is itself a required
> check. See [CI integration](#ci-integration).

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
  histograms, whose data upstream's float-only demo fixture never carries
  — 768 in all.
- **Today**: **768/768** cases pass; no allow-list exists. This is the
  highest-confidence leg — an industry-standard conformance suite against
  a real reference. (Parity drift is report-only in CI; the 768/768 is a
  measured score, not a merge gate — see the note at the top of this
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
- **Today**: shipped and running as the required `compatibility/loki` PR
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
- **Today**: shipped and running. `/api/search`, `/api/traces/<id>`, the
  four tag / tag-values endpoints (V1 + V2), and the metrics endpoints
  (`/api/metrics/query_range` + `/api/metrics/query`) all run under the
  required `compatibility/tempo` PR check; no allow-list exists. Parity
  drift is report-only, like the other two heads.
- **Two transport arms, one required check**: `diff` drives the corpus
  over HTTP; `diff-grpc` (#1453) drives the SAME corpus over cerberus's
  and reference Tempo's `tempopb.StreamingQuerier` gRPC/h2c service —
  the transport Grafana's Tempo datasource actually opens when
  "Streaming" is enabled, which the HTTP-only harness could not exercise
  at all. Both arms run inside the one `compatibility/tempo` job and gate
  independently (`compatibility/parity-baseline.json`'s `heads.tempo` /
  `heads.tempo-grpc`), so a regression on either transport fails the
  check. The gRPC arm's roster is 59 cases, not 61: `traces` /
  `traces_v2` have no `StreamingQuerier` RPC (trace-by-id is
  HTTP/proto-only on both backends) and are reported as skipped rather
  than run — see `compatibility/tempo/driver/grpc_diff.go`'s file-level
  comment for the full wire-contract trace, including where reference
  Tempo's own gRPC listener lives (the query-frontend module's dedicated
  `:9095`, not multiplexed onto its HTTP port the way cerberus's h2c
  listener is).

## Per-head confidence

The three legs are *not* equally strong, and the docs should not imply
they are:

| Head    | Reference          | Corpus origin                                  | Numerical confidence                                                                        |
| ------- | ------------------ | ---------------------------------------------- | ------------------------------------------------------------------------------------------- |
| PromQL  | real Prometheus    | third-party `prometheus/compliance` (CNCF)     | **Highest** — industry-standard conformance suite, 768/768, no allow-list                   |
| LogQL   | real Loki          | Grafana's own `pkg/logql/bench` corpus         | **Solid** — real backend + real corpus, but a Grafana bench set, not a conformance standard |
| TraceQL | real Tempo         | cerberus-owned author-written TXTAR            | **Lowest** — real backend, but no third-party suite; corpus breadth is author-bounded       |

All three run against a real reference backend on identical seeded data,
so each catches genuine semantic divergence. The difference is *corpus
provenance and breadth*: PromQL inherits an externally-curated standard;
TraceQL's coverage is only as wide as the author wrote it. Raising
TraceQL's confidence is the top improvement item.

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

1. **Catalogue** — `test/rejection-parity/catalogue/` is the
   machine-readable inventory of every prefixed error-construction
   site in the three lowerings, derived by a go/ast scan
   (`test/rejection-parity`). Every site is classified either
   `rejection` (reachable from a parseable query; carries a minimal
   trigger query) or `internal` (parser-enforced shape, invariant, or
   `%w` wrapper; carries a rationale). It is stored as one JSON shard
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
The required job does not pass on a parity regression, though: a
**parity-regression ratchet** step (next section) runs after the harness
and fails the job when any case moves against the committed per-head
roster. So the required check gates **both** infrastructure breakage
**and** every individual parity case, while keeping the harness's own
exit code reserved for infrastructure — which is what #503 was
protecting.

The `compatibility/prometheus-forced-route` lane additionally
**hard-fails on any parity diff** (`FAIL_ON_DIFF=1` in
`run-prometheus-compatibility.sh`) as the corpus-wide proof that the sharded solver
route is byte-identical to reference Prometheus; it is a required check, so
every non-docs-only PR is gated on the full forced-route corpus run.

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
`compatibility/parity-baseline.json`, and **fails the required job on any
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

`VANISHED` and `UNRECORDED` are loud rather than silent on purpose. A
divergence must never be retired by deleting or renaming its case, and
corpus coverage must never shrink unnoticed — so a disappearance is a
failure that names the missing IDs. Likewise, a newly-passing case that
nobody records is a case no future run is gated on, which means the
ratchet has not actually ratcheted. `ARRIVED-FAILING` is fatal for the
same reason the project has no allow-lists: "it wasn't passing before" is
exactly the reasoning an allow-list encodes, and accepting it would let a
corpus refresh import known-bad behaviour under a green check.

The rosters today (`heads.<name>.{passed,total,cases}`):

| head       | passed/total |
| ---------- | ------------ |
| prometheus | 768 / 768    |
| loki       | 130 / 130    |
| tempo      | 68 / 68      |
| tempo-grpc | 65 / 65      |

The baseline records **full parity** for every head — the ratchet asserts
`passed == total == cases.length`, so the file has no shape in which a
divergence can be recorded as acceptable. That is what keeps it the
opposite of the deleted `expected-failures.json`: an allow-list names the
cases you are permitted to fail, whereas this roster names the cases that
must pass, and every entry on it is an obligation.

It cannot flake. Each case ID is built from that case's static corpus
identity (query text, endpoint, suite, lane) and never from
wall-clock-derived values, so an unchanged corpus yields a byte-identical
roster; pass/fail per case comes from the same success predicate that
feeds `compat-score.json`, comparing with absolute + relative epsilon
tolerance over canonical-key-sorted result sets against a deterministic
seed. There is no float, timing or ordering surface left to jitter.

When the corpus legitimately grows, or a case is deliberately renamed or
retired, **move the head's entry** in
`compatibility/parity-baseline.json` in the same PR. Take the roster from
the run's `compat-cases.json` (uploaded as a job artefact) rather than
hand-editing it:

```sh
node .github/scripts/compat-baseline-sync.mjs path/to/compat-cases.json
```

That writes the entry sorted and with counts derived from the roster, so
the committed list cannot drift from what the harness actually ran and
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
