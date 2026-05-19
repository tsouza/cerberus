# PromQL compatibility harness

cerberus's correctness for PromQL is gated by the upstream [`prometheus/compliance`](https://github.com/prometheus/compliance) suite. The corpus diffs query results between a **reference Prometheus** and **cerberus**, both seeded with the same deterministic fixture, over the same time window.

Today (M0.6) the harness lands as **informational** — the workflow runs but doesn't block merges. It becomes a merge gate at **M6 (RC1 release)**, by which point M1.x has lowered enough of PromQL that the pass rate is meaningfully high (target: ≥ 538/539, matching [promshim-clickhouse](https://github.com/BadLiveware/promshim-clickhouse)).

## Local run

```sh
just compat-promql
```

This:

1. Brings up `compatibility/prometheus/docker-compose.yml` (reference Prometheus on `localhost:29090`, cerberus on `localhost:29091`, ClickHouse on `localhost:29000`, plus a one-shot seeder).
2. Builds the upstream `promql-compliance-tester` binary from the submodule.
3. Runs it pointed at the two endpoints, with `test-cerberus.yml` as config and `2026-05-11T00:00:00Z..01:00:00Z` (a 1-hour seed window) as the eval range.
4. Writes `compatibility/prometheus/report.json`.
5. Tears the stack down.

Set `COMPOSE_KEEP=1` to leave the stack running for poking around:

```sh
just compat-promql-keep
# inspect things; then
just compat-promql-down
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

- Each subsequent M1.x PR can run `just compat-promql` locally and report the pass-rate delta in the PR body (per the [CONTRIBUTING](../CONTRIBUTING.md) test-plan template).
- The CI run produces an artifact (`compatibility-prometheus-report` for 30 days) so we can chart progress.
- When M1.7 closes, flipping the gate is a one-line `continue-on-error: false` + adding the check to branch protection.

## Known limitations (v1.0.0 GA)

The three compatibility harnesses (`compatibility/{prometheus,loki,tempo}`) all
pass cleanly on `main`: Prom 536/536 with `expected-failures.failures: []`,
Loki 0 skipped queries, Tempo 0 expected-failures. The items below are not
caught by the harness corpus but are real semantic gaps tracked toward GA.

### PromQL

No outstanding limitations on the PromQL parsing / lowering surface. Previous
entries (nested subqueries, computed-K `topk`/`bottomk`, native-histogram
`histogram_quantile` range mode) all lowered to chplan + chsql by their
respective RC tasks and round-trip cleanly under the chDB suite. The
`compatibility/prometheus` harness is the source of truth — see the
`prometheus-compliance` job on every push to `main`.

### LogQL

All parser stages (`| json`, `| logfmt`, `| regexp`, `| pattern`, `| unpack`,
`| drop`, `| keep`), label filters (string, numeric, duration, bytes), unwrap
forms (raw, `duration_seconds(...)`, `bytes(...)`), and value-based range
aggregations (`sum_over_time`, `avg_over_time`, `min_over_time`,
`max_over_time`, `stddev_over_time`, `stdvar_over_time`,
`quantile_over_time(<phi>, ...)`) lower to SQL and round-trip in the chDB
suite. Range-aggregation `by` / `without` grouping is wired. Loki's
stream-label-wins-on-conflict contract is enforced at SQL emit time for
`| logfmt`: bare extraction wraps the extracted map in a `mapApply` that
suffixes any colliding key with `_extracted`; typed `| logfmt foo="..."`
wraps each destination identifier in an `if(mapContains(ResourceAttributes,
'<id>'), '<id>_extracted', '<id>')` (`internal/logql/lower.go`).

### TraceQL

- **TraceQL chained-structural `R.*` projection (compat-harness blind spot)** —
  certain multi-hop structural-join lowerings emit `SELECT R.* FROM (SELECT R.*
  FROM <left> AS L INNER JOIN <right> AS R ON ...) AS L INNER JOIN <right2> AS R
  ON L.TraceId = R.TraceId`. CH's analyzer rejects the outer reference because
  the inner `R.*` keeps its qualifier when re-aliased to outer `L`. The
  emitter must project explicit columns (or re-qualify on wrap) when wrapping a
  structural join in an outer subquery. Affects `multi_hop_chain`,
  `recursive_mixed`, `edge_chain_ancestor_3` / `_descendant_3` / `_mixed_5` /
  `_child_5`. Tracked in the GA punch list.
- **Map-valued aggregate inputs missing `toFloat64` cast** — `max(SpanAttributes[k])`
  returns `String` (Map values are stringly typed); the outer `WHERE Value > 100`
  then fails with `NO_COMMON_TYPE: no supertype for String, UInt8`. The
  emitter needs an explicit `toFloat64` cast on Map-valued aggregate inputs,
  mirroring the existing wrap on `parseReadableSize` (the unwrap-bytes path).
  Affects `edge_inner_max_attr` and likely event/link numeric comparisons.
  Tracked in the GA punch list.
- **`?scope=` filter on `/api/v1/search/tags` (v1)** — partially honoured;
  the v1 endpoint always includes resource and span tags regardless of the
  requested scope, narrowing only intrinsic tags
  (`internal/api/tempo/search_tags.go:158-164`). The v2 endpoint
  (`/api/v2/search/tags`) honours the filter for all three scopes
  (`search_tags.go:143-156`). The v1 partial behaviour matches upstream Tempo's
  documented contract and may stay as-is post-GA pending an explicit upstream
  clarification.

### Placeholder endpoints

These two endpoints accept the request, validate inputs, and return a
well-formed empty envelope. Real implementations are tracked in
[`docs/roadmap.md`](roadmap.md):

- **`/loki/api/v1/patterns`** — empty `patterns` array. Real implementation
  will vendor `grafana/loki/pkg/pattern/drain` (faceair drain3 port) and emit
  one cluster per pattern over the query range.
- **`/api/v1/query_exemplars`** (PromQL) — empty `data` array. Real
  implementation will read exemplar columns from `otel_traces_*` via the
  schema abstraction.
