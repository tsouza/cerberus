# PromQL compliance harness

cerberus's correctness for PromQL is gated by the upstream [`prometheus/compliance`](https://github.com/prometheus/compliance) suite. The corpus diffs query results between a **reference Prometheus** and **cerberus**, both seeded with the same deterministic fixture, over the same time window.

Today (M0.6) the harness lands as **informational** — the workflow runs but doesn't block merges. It becomes a merge gate at **M6 (RC1 release)**, by which point M1.x has lowered enough of PromQL that the pass rate is meaningfully high (target: ≥ 538/539, matching [promshim-clickhouse](https://github.com/BadLiveware/promshim-clickhouse)).

## Local run

```sh
just compliance
```

This:

1. Brings up `harness/compliance/docker-compose.yml` (reference Prometheus on `localhost:29090`, cerberus on `localhost:29091`, ClickHouse on `localhost:29000`, plus a one-shot seeder).
2. Builds the upstream `promql-compliance-tester` binary from the submodule.
3. Runs it pointed at the two endpoints, with `test-cerberus.yml` as config and `2026-05-11T00:00:00Z..01:00:00Z` (a 1-hour seed window) as the eval range.
4. Writes `harness/compliance/report.json`.
5. Tears the stack down.

Set `COMPOSE_KEEP=1` to leave the stack running for poking around:

```sh
just compliance-keep
# inspect things; then
just compliance-down
```

## Reading the report

```sh
jq '{
  total: ([.results[]?] | length),
  passed: ([.results[]? | select(.unexpectedFailure == null and .diff == null)] | length),
  diffs: ([.results[]? | select(.diff != null)] | length),
  unexpected_failures: ([.results[]? | select(.unexpectedFailure != null)] | length)
}' harness/compliance/report.json
```

A passing run has no `unexpectedFailure` entries and the `diffs` field reflects only the allowlist in `expected-failures.json`.

## Allowlist (`expected-failures.json`)

`harness/compliance/expected-failures.json` documents queries where cerberus is **knowingly** different from reference Prometheus. Every entry must include:

- `query` — the exact PromQL string from `promql-test-queries.yml`.
- `reason` — why the result differs. Acceptable reasons:
  - upstream Prometheus quirk that ClickHouse-side execution can't sensibly reproduce (e.g. NaN ordering in `topk` ties, float-mod sign drift);
  - documented OTel-CH schema difference (e.g. a label that reference Prom adds via scrape config but the OTel exporter doesn't carry);
  - explicit deferral to a future RC (with a link to `docs/<area>.md` or the RC2/RC3 plan section).
- `tracking` — link to the PR that will close the entry, or `"will-not-fix"` with justification.

Reviewers gate every addition. **Never an empty `reason`.**

## CI

`.github/workflows/compliance.yml` runs the harness:

- on **push to `main`** with paths under `internal/promql/`, `internal/chsql/`, `internal/optimizer/`, `internal/chplan/`, `harness/compliance/`, or the workflow file itself
- on **PRs** touching the same paths
- **nightly at 04:11 UTC**
- on **manual `workflow_dispatch`**

The workflow is currently `continue-on-error: true` so a failing run reports but doesn't block. M6 flips it to `false` and adds the `compliance / prometheus-compliance` check to the required-status-checks list.

## Adding new test cases

The upstream corpus already covers a generous slice of PromQL. If you discover a real-world query that cerberus mishandles but the corpus doesn't cover, the right move is:

1. Open a PR to [`prometheus/compliance`](https://github.com/prometheus/compliance) adding the query (so every adapter benefits, not just cerberus).
2. Once it lands, bump the submodule SHA in `harness/compliance/upstream`.

If the case is cerberus-specific (e.g. OTel-CH schema quirk), add it as a TXTAR fixture under `test/spec/promql/` instead — that's where cerberus-only tests live.

## Why we don't gate at M0.6

Most of PromQL isn't lowered yet at the seed stage. Gating now would make every PR red. The harness lands now so:

- Each subsequent M1.x PR can run `just compliance` locally and report the pass-rate delta in the PR body (per the [CONTRIBUTING](../CONTRIBUTING.md) test-plan template).
- The CI run produces an artifact (`compliance-report` for 30 days) so we can chart progress.
- When M1.7 closes, flipping the gate is a one-line `continue-on-error: false` + adding the check to branch protection.
