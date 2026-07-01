# Histogram-quantile benchmark: cerberus vs Prometheus vs Mimir

An honest, reproducible head-to-head latency harness for `histogram_quantile`
queries against three backends over a **byte-identical** synthetic dataset:

- **cerberus** — reading OTel-native histograms from ClickHouse,
- **Prometheus** — classic in-memory TSDB,
- **Grafana Mimir** — monolithic single-binary mode (the "stretch" backend).

The whole point is apples-to-apples: the same series, the same values, the same
quantiles. The runner **verifies the three backends return numerically
equivalent results** before trusting any latency number — a fast query over
wrong answers is worthless.

## One-command run

```bash
cd bench/histogram
./run.sh smoke     # small + fast (1h of data, low cardinality) — proves the harness
./run.sh bench     # larger (24h of data, high cardinality)
./run.sh down      # tear the stack down (removes volumes)
```

Requirements: Docker (+ compose v2) and the Go toolchain. `run.sh` builds the
cerberus image from the repo root (`Dockerfile.local`), brings up the stack,
seeds the fixture, times the query battery, and writes [`RESULTS.md`](RESULTS.md).

All host ports live in the **51xxx** range (ClickHouse `51000/51123`, cerberus
`51091`, Prometheus `51090`, Mimir `51009`) and the compose project is
`histbench-a29`, so the stack dodges other services (and any other bench stack)
that may be running on the box.

## How it works

```text
                 ┌──────────────┐  INSERT (arrays)   ┌────────────┐
                 │              │ ─────────────────▶ │ ClickHouse │ ◀── cerberus /api/v1/query
   cmd/gen  ─────┤  one fixture │                    └────────────┘
 (deterministic) │              │  remote_write      ┌────────────┐
                 │              │ ─────────────────▶ │ Prometheus │ ◀── /api/v1/query
                 └──────────────┘ ────────────┐      └────────────┘
                                              └────▶ ┌────────────┐
                                                     │   Mimir    │ ◀── /prometheus/api/v1/query
                                                     └────────────┘
   cmd/bench: fire each query × each backend × N iters → p50/p95/p99 + docker stats
              + cross-backend equivalence check → RESULTS.md
```

### The histogram schema mapping (the crux)

Cerberus does **not** store classic `_bucket` counter series. It stores
**OTel-native histograms**: one ClickHouse row per (series, timestamp) in
`otel_metrics_histogram`, under the **bare** metric name (no `_bucket` suffix),
with parallel arrays:

| Column           | Meaning                                                                                                                                      |
| ---------------- | -------------------------------------------------------------------------------------------------------------------------------------------- |
| `MetricName`     | `http_request_duration_seconds` (bare — the `_bucket` suffix is stripped at query time)                                                      |
| `Attributes`     | `Map(String,String)` label set — **without** `le`                                                                                            |
| `TimeUnix`       | `DateTime64(9)` sample time                                                                                                                  |
| `Count` / `Sum`  | total observations / sum                                                                                                                     |
| `BucketCounts`   | `Array(UInt64)` — **per-bucket** counts (NOT cumulative across buckets); `len == len(ExplicitBounds)+1`, last element is the `+Inf` overflow |
| `ExplicitBounds` | `Array(Float64)` — the finite `le` upper bounds                                                                                              |

At query time cerberus reconstructs the classic `le` series with
`arraySum(arraySlice(BucketCounts, 1, le_idx))` — i.e. it cumulates across
buckets itself, and synthesizes `le = ExplicitBounds[le_idx]` (or `+Inf`).

The generator writes the **same** distribution to Prometheus/Mimir in the
classic exploded form — one `<metric>_bucket{le=…}` series per bound
(**cumulative** across buckets), plus `<metric>_sum` and `<metric>_count`.
Because both representations come from one per-series bucket-weight vector,
`histogram_quantile` over either yields identical numbers.

### The synthetic data

- Metric `http_request_duration_seconds` with labels `job`, `instance`, `route`.
- Each series gets a fixed integer **triangular** bucket-weight vector peaked at
  a `route`/`instance`-dependent bucket, so `by (le, route)` returns genuinely
  different quantiles per route (not a degenerate all-identical result).
- Counts are **cumulative over time** (monotonic counters) so `rate()`/
  `increase()` behave; the per-step increment is constant, so quantiles are
  stable and cross-backend equality is exact.
- Data ends at "now" (aligned to the scrape interval) so samples are fresh —
  reference Prometheus rejects far-past remote-writes, and cerberus's
  instant-query staleness lookback wants a recent last sample.

Profiles (set in `run.sh`):

| Profile | routes × instances | buckets     | samples/series | window |
| ------- | ------------------ | ----------- | -------------- | ------ |
| `smoke` | 5 × 2 = 10         | 11 + `+Inf` | 240 @ 15s      | 1h     |
| `bench` | 50 × 4 = 200       | 11 + `+Inf` | 2880 @ 30s     | 24h    |

Every knob is a flag/env on `cmd/gen` (`-routes`, `-instances`, `-bounds`,
`-steps`, `-interval`) if you want to sweep cardinality or range yourself.

## What each query shape probes

See [`queries.yaml`](queries.yaml). In short:

- **p50 / p90 / p99 instant, `by (le)`** — the cheapest common shape: one
  aggregated output series, varying quantile.
- **p99 instant, `by (le, route)`** — high cardinality: one output series per
  route.
- **p50/p90/p99 union, `by (le, route)`** — three quantiles fanned per route in
  one evaluation (multi-quantile).
- **p99 range 1h/60s, `by (le)`** — short-lookback range evaluation.
- **p99 range 24h/300s, `by (le, route)`** — long range × high cardinality (the
  shape where a columnar store should shine; clamped to the data window).

## Reading the results honestly

Set expectations before you look at the table:

- **Prometheus should win on small/recent instant queries.** Its data is already
  in memory in the exact shape PromQL wants. cerberus pays for a round trip to
  ClickHouse plus SQL planning/execution — for a p99 over the last 5 minutes of
  a handful of series, that overhead dominates and Prometheus will be faster.
  That is expected and fine; don't spin it.
- **cerberus should get competitive — and can win — on long-range × high
  cardinality.** Scanning 24h across many `route`s is exactly what a columnar
  engine with vectorized aggregation is built for; the gap narrows or flips as
  the window and cardinality grow.
- **The real story isn't just latency.** cerberus stores this data in ClickHouse
  (cheap, S3-backed, already your logs/traces store) with **no separate TSDB to
  run, scale, or pay for**. Mimir buys horizontal scale and long retention but
  is a whole distributed system to operate. A "Prometheus is 2× faster on this
  tiny instant query" line has to be read next to "…and it's a second database
  you have to keep alive." The benchmark measures latency; the architecture
  argument is about total cost of ownership.
- **Equivalence gates everything.** If the equivalence check flags a mismatch,
  the latency numbers for that row compare *different answers* and must not be
  published until the discrepancy is understood.

## Layout

| Path                                         | What                                                                 |
| -------------------------------------------- | -------------------------------------------------------------------- |
| `docker-compose.yml`                         | ClickHouse + cerberus + Prometheus + Mimir, one network, 51xxx ports |
| `config/prometheus.yml`, `config/mimir.yaml` | backend configs                                                      |
| `cmd/gen/`                                   | deterministic fixture generator → CH insert + remote_write fan-out   |
| `cmd/bench/`                                 | query timer + cross-backend equivalence + docker-stats + RESULTS.md  |
| `queries.yaml`                               | the query battery                                                    |
| `run.sh`                                     | one-command driver (`smoke` / `bench` / `down`)                      |
| `data-window.json`                           | written by `cmd/gen`, read by `cmd/bench` (metric + time window)     |
| `RESULTS.md`                                 | generated report                                                     |

This directory is an **isolated nested Go module** (`go.mod` present) so it never
touches cerberus's own build/lint/dependency graph.

## Caveats / what's honest about the harness

- Latencies are wall-clock of the HTTP round trip from the same host, single
  concurrency. This measures per-query serving latency, not throughput under
  load. A concurrency sweep would be the natural next step.
- Peak CPU/mem come from `docker stats` sampled ~1 Hz during the run — indicative
  peaks, not precise accounting.
- Mimir is monolithic single-binary with filesystem storage: a fair *functional*
  comparison, but not a tuned production Mimir cluster.
- Constant per-step increments make `rate()` flat, which keeps cross-backend
  equality exact; it also means the range queries do uniform work per step
  (good for a stable latency signal, not a stress of pathological data).
- Range-query grid phase: on a **non-step-aligned** window cerberus anchors its
  `query_range` sample grid to `end` while Prometheus anchors to `start`, so
  their sample timestamps are phase-shifted and never coincide — the values at
  each grid point are still identical (verified: max relative diff `0.00e+00`
  over a 60-point window), only the timestamps differ. `cmd/bench` therefore
  snaps `start`/`end` onto the step grid before firing, exactly as Grafana's
  Prometheus datasource does in production, so the value-by-timestamp
  equivalence check compares like-for-like points. This is a benchmark-harness
  concern, not a value-correctness one.
