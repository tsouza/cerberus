# Compatibility harnesses

Cerberus's correctness is verified by three differential-parity
harnesses, one per upstream API. Each diffs query results between a
reference backend and cerberus, both seeded with the same deterministic
fixture over the same time window.

| Harness | Location                    | Reference backend                  | Corpus source                                                                                |
| ------- | --------------------------- | ---------------------------------- | -------------------------------------------------------------------------------------------- |
| PromQL  | `compatibility/prometheus/` | Reference Prometheus on `:29090`   | [`prometheus/compliance`](https://github.com/prometheus/compliance) submodule under upstream |
| LogQL   | `compatibility/loki/`       | Reference Loki on `:23100`         | Vendored `grafana/loki/pkg/logql/bench` snapshot at `upstream/loki-bench/`                   |
| TraceQL | `compatibility/tempo/`      | Reference Tempo on `:23200`        | Cerberus-owned TXTAR corpus under `compatibility/tempo/driver/corpus/`                       |

Scores are published to the orphan
[`compat-scores`](https://github.com/tsouza/cerberus/tree/compat-scores)
branch as shields.io badge JSON; the README shows them live.

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

A passing run has no `unexpectedFailure` entries; `diffs` reflects only
allowlisted cases in `expected-failures.json`.

The LogQL and TraceQL reports follow the same shape with one extra
field — `should_skip` overlay matches surface as a `skip` outcome
rather than a diff.

## Expected-failures allowlist

`compatibility/prometheus/expected-failures.json` and the equivalent
overlay for LogQL at `compatibility/loki/cerberus-test-queries.yml`
record queries where cerberus is **knowingly** different from the
reference. Every entry carries:

- `query` — the exact source string.
- `reason` — non-empty justification. Acceptable shapes:
  - Upstream quirk that ClickHouse-side execution can't sensibly
    reproduce (e.g. NaN ordering in `topk` ties, float-mod sign drift).
  - Documented OTel-CH schema gap (e.g. a label that reference Prom
    scrapes but the OTel exporter doesn't carry).
  - Harness-side seed limitation (a label the seeder doesn't emit; the
    reference returns empty).
- `jira` — link to the tracking issue or PR.

The `forbid-skip` CI gate enforces that every net-new `should_skip`
addition cites an `jira:` URL or inline `#NNN` ref — see
`scripts/check-skip-additions.sh`. An empty `reason` fails review.

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
