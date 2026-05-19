# PromQL compatibility harness

cerberus's correctness for PromQL is gated by the upstream [`prometheus/compliance`](https://github.com/prometheus/compliance) suite. The corpus diffs query results between a **reference Prometheus** and **cerberus**, both seeded with the same deterministic fixture, over the same time window.

Today (M0.6) the harness lands as **informational** — the workflow runs but doesn't block merges. It becomes a merge gate at **M6 (RC1 release)**, by which point M1.x has lowered enough of PromQL that the pass rate is meaningfully high (target: ≥ 538/539, matching [promshim-clickhouse](https://github.com/BadLiveware/promshim-clickhouse)).

## Local run

```sh
just compatibility
```

This:

1. Brings up `compatibility/prometheus/docker-compose.yml` (reference Prometheus on `localhost:29090`, cerberus on `localhost:29091`, ClickHouse on `localhost:29000`, plus a one-shot seeder).
2. Builds the upstream `promql-compliance-tester` binary from the submodule.
3. Runs it pointed at the two endpoints, with `test-cerberus.yml` as config and `2026-05-11T00:00:00Z..01:00:00Z` (a 1-hour seed window) as the eval range.
4. Writes `compatibility/prometheus/report.json`.
5. Tears the stack down.

Set `COMPOSE_KEEP=1` to leave the stack running for poking around:

```sh
just compatibility-keep
# inspect things; then
just compatibility-down
```

## Reading the report

```sh
jq '{
  total: ([.results[]?] | length),
  passed: ([.results[]? | select(.unexpectedFailure == null and .diff == null)] | length),
  diffs: ([.results[]? | select(.diff != null)] | length),
  unexpected_failures: ([.results[]? | select(.unexpectedFailure != null)] | length)
}' compatibility/prometheus/report.json
```

A passing run has no `unexpectedFailure` entries and the `diffs` field reflects only the allowlist in `expected-failures.json`.

## Allowlist (`expected-failures.json`)

`compatibility/prometheus/expected-failures.json` documents queries where cerberus is **knowingly** different from reference Prometheus. Every entry must include:

- `query` — the exact PromQL string from `promql-test-queries.yml`.
- `reason` — why the result differs. Acceptable reasons:
  - upstream Prometheus quirk that ClickHouse-side execution can't sensibly reproduce (e.g. NaN ordering in `topk` ties, float-mod sign drift);
  - documented OTel-CH schema difference (e.g. a label that reference Prom adds via scrape config but the OTel exporter doesn't carry);
  - explicit deferral to a future RC (with a link to `docs/<area>.md` or the RC2/RC3 plan section).
- `tracking` — link to the PR that will close the entry, or `"will-not-fix"` with justification.

Reviewers gate every addition. **Never an empty `reason`.**

## CI

`.github/workflows/compatibility.yml` runs the harness:

- on **push to `main`** with paths under `internal/promql/`, `internal/chsql/`, `internal/optimizer/`, `internal/chplan/`, `compatibility/prometheus/`, or the workflow file itself
- on **PRs** touching the same paths
- **nightly at 04:11 UTC**
- on **manual `workflow_dispatch`**

The workflow is currently `continue-on-error: true` so a failing run reports but doesn't block. M6 flips it to `false` and adds the `compatibility/prometheus` check to the required-status-checks list.

## Adding new test cases

The upstream corpus already covers a generous slice of PromQL. If you discover a real-world query that cerberus mishandles but the corpus doesn't cover, the right move is:

1. Open a PR to [`prometheus/compliance`](https://github.com/prometheus/compliance) adding the query (so every adapter benefits, not just cerberus).
2. Once it lands, bump the submodule SHA in `compatibility/prometheus/upstream`.

If the case is cerberus-specific (e.g. OTel-CH schema quirk), add it as a TXTAR fixture under `test/spec/promql/` instead — that's where cerberus-only tests live.

## Why we don't gate at M0.6

Most of PromQL isn't lowered yet at the seed stage. Gating now would make every PR red. The harness lands now so:

- Each subsequent M1.x PR can run `just compatibility` locally and report the pass-rate delta in the PR body (per the [CONTRIBUTING](../CONTRIBUTING.md) test-plan template).
- The CI run produces an artifact (`compatibility-prometheus-report` for 30 days) so we can chart progress.
- When M1.7 closes, flipping the gate is a one-line `continue-on-error: false` + adding the check to branch protection.

## Known limitations (v1.0.0 GA)

### PromQL

- **Nested subqueries** — nested subqueries through `Call` / `ParenExpr` / `AggregateExpr` intermediaries — including the canonical Grafana shape `max_over_time(rate(m[5m])[10m:1m])[1h:5m]` and the deeper `min_over_time(avg_over_time(max_over_time(rate(m[1m])[5m:30s])[1h:5m])[2h:10m])` — lower correctly. Subquery over `by(...)` / `without(...)` aggregations (`sum` / `avg` / `count` / `min` / `max` / `quantile` / `topk` / `bottomk` / `count_values`) also lowers. PromQL's parser type system rejects a direct `<subquery>[range:step]` (a SubqueryExpr inside a SubqueryExpr) at parse time with the "subquery is only allowed on instant vector" error; cerberus's `lowerSubqueryOverSubquery` handles the parser-impossible AST shape defensively for any optimizer rewrite that might produce it.
- **Computed-K `topk` / `bottomk`** — `topk(scalar(expr), v)` and `bottomk(scalar(expr), v)` where K is not a literal scalar integer are rejected (`internal/promql/lower.go:1077`). Only literal-K forms are supported.
- **Native histogram `histogram_quantile` range mode** — `histogram_quantile(phi, <metric>_exp_hist)` over `/api/v1/query_range` collapses to instant mode: a single quantile value is computed and repeated at every step. The `now64(9)` timestamp is used for all rows (`internal/promql/histogram_quantile.go:477`). Use `/api/v1/query` for per-instant native-histogram quantiles. Range-mode (Phase 3 StepGrid + per-anchor lookback) is planned for a follow-up release.

### LogQL

- **`| json` and `| regexp` parser stages** — return "not yet supported". Both the bare parsers (`| json`, `| regexp`) and the `| json field="..."` expression-select variant pending chsql `JSONExtract` helpers (`internal/logql/lower.go`).
- **`| pattern`, `| unpack`, `| drop`, `| keep`, and `| logfmt` are supported**. `| pattern`, `| unpack`, `| drop`, `| keep` extract / project labels in Go after the rows return (no SQL impact). `| logfmt` (bare and `| logfmt field="..."`) lowers to `extractKeyValuePairs(Body, '=', ' ', '"')` and merges the parsed keys into the labels map for downstream string-equality / regex label filters. Loki's stream-label-wins-on-conflict contract is enforced at SQL emit time: bare `| logfmt` wraps the extracted map in a `mapApply` that suffixes any colliding key with `_extracted`; typed `| logfmt foo="..."` wraps each destination identifier in an `if(mapContains(ResourceAttributes, '<id>'), '<id>_extracted', '<id>')` (see `internal/logql/lower.go`).
- **Typed label filters (numeric / duration / bytes) are supported** — `| size > 10KB`, `| latency > 500ms`, `| status > 400` lower to `parseReadableSize` / `parseTimeDelta` / `toFloat64OrZero` comparisons against the label string. They share the `labelsExpr` threaded through parser stages so post-parse filters work.
- **`| unwrap` and value-based range aggregations are supported** — `sum_over_time`, `avg_over_time`, `min_over_time`, `max_over_time`, `stddev_over_time`, `stdvar_over_time`, and `quantile_over_time(<phi>, ...)` all read the unwrapped value from the live `labelsExpr` and feed the same chsql RangeWindow emitter the PromQL head uses. Unwrap variants `unwrap foo` (raw), `unwrap duration(foo)` / `unwrap duration_seconds(foo)` (CH `parseTimeDelta` → Float64 seconds), and `unwrap bytes(foo)` (CH `parseReadableSize` → Float64 bytes) are all wired.
- **Range-aggregation `by` / `without` grouping is supported** — `avg_over_time({...} | logfmt | unwrap latency [5m]) by (level)` and friends synthesise a `map(...)` / `mapFilter(...)` group key in the inner Project so the RangeWindow GROUP BY collapses per-group rather than per-stream.

### TraceQL

- **Spanset pipeline expressions** — some `PipelineElement` types return "not yet supported" when cerberus encounters them as a pipeline tail (`internal/traceql/lower.go:114`, `lower.go:186`). Second-stage metrics operators (`| topk`, `| bottomk`, `| > N`) have a landed chplan + chsql IR (`internal/chplan/metrics_second_stage.go`, `internal/chsql/metrics_second_stage.go`) but the TraceQL lowering layer still returns "not yet supported" (`internal/traceql/lower.go:83`) pending tsouza/tempo accessors on the upstream-unexported `TopKBottomK` / `MetricsFilter` fields.
- **Multi-quantile `quantile_over_time` is supported** — `quantile_over_time(<attr>, p1, p2, ...)` lowers into a chplan.MetricsAggregate whose `Quantiles` slice carries every phi (`internal/traceql/metrics_pipeline.go`), and the chsql emitter (`internal/chsql/range_window.go`) renders one output series per phi tagged with a synthetic `__phi__` label. The CH-side aggregator is `quantiles(p1, p2, ...)(<attr>)` (single scan returning Array(Float64)); the wrapping SELECT fans the array out via `arrayJoin` + `tupleElement` so each (group, anchor, phi) tuple becomes one row.
- **`?scope=` filter on `/api/v2/search/tags`** — not honoured; the handler returns all scopes (resource, span, intrinsic) regardless of the requested scope (`internal/api/tempo/search_tags.go:109`).
