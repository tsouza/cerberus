# Histogram-quantile benchmark — cerberus vs Prometheus vs Mimir

Two profiles, one harness. Every latency number below is gated by a
**cross-backend equivalence check**: each query shape is fired at cerberus,
Prometheus, and Mimir over a byte-identical fixture and the results are compared
series-by-series, value-by-value within a `1e-4` relative tolerance *before* any
timing is trusted. Both profiles passed that gate with **zero mismatches**, so
the comparisons are apples-to-apples.

- **`bench`** — 24h window × high cardinality (500 OTel series / 7000 exploded
  Prometheus series). This is the regime that makes cerberus's case; it is the
  headline table.
- **`smoke`** — 1h window × low cardinality (10 OTel series / 140 exploded). It
  proves the harness end-to-end and shows the small/recent corner where an
  in-memory TSDB is unbeatable.

All numbers are single-host, single-concurrency wall-clock over 50 (`bench`) /
30 (`smoke`) timed iterations per query after 3 warmups.

---

## Profile: `bench` — 24h × high cardinality

*Generated 2026-07-01T14:12:46Z • profile `bench` • 50 iterations/query (3 warmup)*

### Dataset

| Property                           | Value                                                                               |
| ---------------------------------- | ----------------------------------------------------------------------------------- |
| Metric                             | `http_request_duration_seconds` (queried as `http_request_duration_seconds_bucket`) |
| OTel series (ClickHouse rows/step) | 500 (100 routes × 5 instances)                                                      |
| Buckets (le)                       | 11 finite + `+Inf`                                                                  |
| Samples/series                     | 1440 @ 60s                                                                          |
| Time window                        | 2026-06-30T13:49:00Z → 2026-07-01T13:48:00Z (24h)                                   |
| Prometheus/Mimir series (exploded) | 7000                                                                                |

### Latency (lower is better)

| Query shape                              | Type    | Backend    | p50 ms | p95 ms | p99 ms | mean ms | Equivalence |
| ---------------------------------------- | ------- | ---------- | ------ | ------ | ------ | ------- | ----------- |
| p50 instant · by(le)                     | instant | cerberus   | 49.7   | 75.0   | 77.0   | 52.3    | match       |
|                                          |         | prometheus | 43.0   | 80.0   | 95.7   | 48.8    |             |
|                                          |         | mimir      | 108.8  | 176.2  | 256.0  | 118.9   |             |
| p90 instant · by(le)                     | instant | cerberus   | 63.6   | 86.5   | 113.7  | 65.0    | match       |
|                                          |         | prometheus | 84.8   | 137.1  | 192.1  | 91.9    |             |
|                                          |         | mimir      | 147.6  | 352.6  | 370.2  | 177.9   |             |
| p99 instant · by(le)                     | instant | cerberus   | 78.5   | 237.9  | 266.2  | 101.1   | match       |
|                                          |         | prometheus | 85.7   | 122.5  | 152.0  | 86.4    |             |
|                                          |         | mimir      | 174.4  | 315.2  | 448.5  | 195.0   |             |
| p99 instant · by(le,route)               | instant | cerberus   | 82.5   | 107.7  | 119.7  | 85.7    | match       |
|                                          |         | prometheus | 79.9   | 125.8  | 155.2  | 84.4    |             |
|                                          |         | mimir      | 187.5  | 261.8  | 273.7  | 192.0   |             |
| p50/p90/p99 union instant · by(le,route) | instant | cerberus   | 250.4  | 344.1  | 469.2  | 243.4   | match       |
|                                          |         | prometheus | 213.4  | 308.3  | 403.0  | 219.3   |             |
|                                          |         | mimir      | 525.8  | 807.2  | 1135.9 | 549.1   |             |
| p99 range 1h/60s · by(le)                | range   | cerberus   | 121.0  | 148.1  | 169.4  | 122.1   | match       |
|                                          |         | prometheus | 289.9  | 403.6  | 509.0  | 300.0   |             |
|                                          |         | mimir      | 435.4  | 797.2  | 843.5  | 456.7   |             |
| p99 range 24h/300s · by(le,route)        | range   | cerberus   | 1017.7 | 1652.0 | 2081.2 | 1045.0  | match       |
|                                          |         | prometheus | 3181.5 | 3937.0 | 4329.0 | 3232.8  |             |
|                                          |         | mimir      | 2114.0 | 3382.7 | 4159.7 | 2164.3  |             |

### Peak container resources (sampled ~1 Hz during the run)

| Backend    | Peak CPU % | Peak mem (MiB) |
| ---------- | ---------- | -------------- |
| cerberus   | 51         | 31             |
| prometheus | 107        | 209            |
| mimir      | 167        | 197            |

> **Read the memory column carefully — it is not apples-to-apples.** The sampler
> watches only the three query-serving containers. cerberus is a thin gateway:
> the scan + aggregation runs in the **ClickHouse** container, which the harness
> does **not** sample, so cerberus's 31 MiB reflects the gateway alone, not the
> work. Prometheus's 209 MiB is the full picture (its in-process TSDB does
> storage *and* compute). Don't read "31 vs 209" as "cerberus uses ~7× less
> memory for the same work" — the honest comparison is architectural (below), not
> a single number.

---

## Profile: `smoke` — 1h × low cardinality (harness validation)

*Generated 2026-07-01T09:11:02Z • profile `smoke` • 30 iterations/query (3 warmup)*

### Dataset

| Property                           | Value                                                                               |
| ---------------------------------- | ----------------------------------------------------------------------------------- |
| Metric                             | `http_request_duration_seconds` (queried as `http_request_duration_seconds_bucket`) |
| OTel series (ClickHouse rows/step) | 10 (5 routes × 2 instances)                                                         |
| Buckets (le)                       | 11 finite + `+Inf`                                                                  |
| Samples/series                     | 240 @ 15s                                                                           |
| Time window                        | 2026-07-01T08:06:45Z → 2026-07-01T09:06:30Z (1h)                                    |
| Prometheus/Mimir series (exploded) | 140                                                                                 |

### Latency (lower is better)

| Query shape                              | Type    | Backend    | p50 ms | p95 ms | p99 ms | mean ms | Equivalence |
| ---------------------------------------- | ------- | ---------- | ------ | ------ | ------ | ------- | ----------- |
| p50 instant · by(le)                     | instant | cerberus   | 20.8   | 29.0   | 29.0   | 23.1    | match       |
|                                          |         | prometheus | 1.5    | 3.1    | 3.1    | 1.7     |             |
|                                          |         | mimir      | 3.3    | 4.2    | 4.5    | 3.4     |             |
| p90 instant · by(le)                     | instant | cerberus   | 19.8   | 26.7   | 27.0   | 20.5    | match       |
|                                          |         | prometheus | 1.3    | 2.6    | 2.8    | 1.6     |             |
|                                          |         | mimir      | 3.3    | 4.7    | 8.5    | 3.7     |             |
| p99 instant · by(le)                     | instant | cerberus   | 21.3   | 29.8   | 29.9   | 22.9    | match       |
|                                          |         | prometheus | 1.8    | 3.6    | 4.4    | 1.8     |             |
|                                          |         | mimir      | 3.5    | 5.2    | 7.3    | 3.9     |             |
| p99 instant · by(le,route)               | instant | cerberus   | 21.1   | 27.7   | 31.6   | 22.3    | match       |
|                                          |         | prometheus | 1.3    | 2.2    | 2.3    | 1.6     |             |
|                                          |         | mimir      | 3.1    | 4.3    | 4.3    | 3.3     |             |
| p50/p90/p99 union instant · by(le,route) | instant | cerberus   | 66.8   | 92.1   | 97.1   | 70.0    | match       |
|                                          |         | prometheus | 3.3    | 4.8    | 5.7    | 3.5     |             |
|                                          |         | mimir      | 6.7    | 15.0   | 19.0   | 7.7     |             |
| p99 range 1h/60s · by(le)                | range   | cerberus   | 26.0   | 36.3   | 42.8   | 29.2    | match       |
|                                          |         | prometheus | 4.9    | 8.7    | 9.2    | 5.7     |             |
|                                          |         | mimir      | 7.4    | 12.6   | 12.6   | 8.9     |             |
| p99 range 24h/300s · by(le,route)        | range   | cerberus   | 23.1   | 64.3   | 73.4   | 30.8    | match       |
|                                          |         | prometheus | 3.8    | 7.4    | 7.5    | 4.4     |             |
|                                          |         | mimir      | 8.4    | 12.0   | 12.2   | 9.1     |             |

### Peak container resources (sampled ~1 Hz during the run)

| Backend    | Peak CPU % | Peak mem (MiB) |
| ---------- | ---------- | -------------- |
| cerberus   | 12         | 10             |
| prometheus | 20         | 30             |
| mimir      | 45         | 76             |

*(smoke's "24h/300s" range shape is clamped to the 1h data window, so it covers
the whole hour — that row is short-range there, not a real 24h scan.)*

---

## Query shapes

- **p50 / p90 / p99 instant · by(le)** — median / tail / extreme-tail quantile,
  one aggregated output series. The cheapest common shape.

  ```promql
  histogram_quantile(0.99, sum by (le) (rate(http_request_duration_seconds_bucket[5m])))
  ```

- **p99 instant · by(le,route)** — per-route tail quantile, one output series per
  route (high cardinality).

  ```promql
  histogram_quantile(0.99, sum by (le, route) (rate(http_request_duration_seconds_bucket[5m])))
  ```

- **p50/p90/p99 union instant · by(le,route)** — three quantiles fanned per route
  in one evaluation.

  ```promql
  histogram_quantile(0.50, sum by (le, route) (rate(http_request_duration_seconds_bucket[5m])))
    or histogram_quantile(0.90, sum by (le, route) (rate(http_request_duration_seconds_bucket[5m])))
    or histogram_quantile(0.99, sum by (le, route) (rate(http_request_duration_seconds_bucket[5m])))
  ```

- **p99 range 1h/60s · by(le)** — short-lookback range evaluation, aggregated.
- **p99 range 24h/300s · by(le,route)** — long range × high cardinality — the
  shape where a columnar store should shine.

  ```promql
  histogram_quantile(0.99, sum by (le, route) (rate(http_request_duration_seconds_bucket[5m])))
  ```

---

## Interpretation (straight, not spun)

**Where Prometheus wins: small, recent, low-cardinality instant queries.** In the
`smoke` profile (10 series, 1h) Prometheus answers an instant p99 in ~1.8 ms
against cerberus's ~21 ms — a ~10× gap in Prometheus's favour. The data already
lives in memory in exactly the shape PromQL wants; cerberus pays a fixed cost per
query (HTTP round-trip + SQL planning + a ClickHouse execution hop) that
completely dominates when there is almost no work to do. This is expected and
correct, and it does not go away — if your dashboards only ever ask for the last
5 minutes of a handful of series, Prometheus is the right tool.

**Where the gap closes: as cardinality rises, that fixed hop amortizes.** In the
`bench` profile (500 CH series / 7000 exploded, 24h) the instant shapes are a
near-tie that trades places by query: Prometheus edges the cheap p50 `by(le)`
(43 ms vs 49.7 ms) and the high-cardinality `by(le,route)` (79.9 ms vs 82.5 ms),
while cerberus is *faster* on the tail-quantile `by(le)` shapes (p90: 63.6 ms vs
84.8 ms). The multi-quantile union favours Prometheus (213 ms vs 250 ms). Net:
for one-shot instant reads the two are within a coin-flip of each other once the
dataset is non-trivial, and cerberus has fully absorbed the per-query overhead
that dominated the smoke corner.

**Where cerberus wins: range queries, and it wins big on long-range ×
high-cardinality.** This is the whole reason to sit histograms on a columnar
store:

| Shape                             | cerberus p50 | Prometheus p50 | Mimir p50 | cerberus advantage                               |
| --------------------------------- | ------------ | -------------- | --------- | ------------------------------------------------ |
| p99 range 1h/60s · by(le)         | 121.0 ms     | 289.9 ms       | 435.4 ms  | **2.4× faster** than Prometheus                  |
| p99 range 24h/300s · by(le,route) | 1017.7 ms    | 3181.5 ms      | 2114.0 ms | **3.1× faster** than Prometheus, 2.1× than Mimir |

Evaluating a 24h range at a 300 s step means 288 output points, each a
`rate(...[5m])` over thousands of bucket series, then a `histogram_quantile`
fan-out across 100 routes. Prometheus grinds that through its engine step by step
(pinning a CPU at 107% and climbing to a 4.3 s p99); cerberus pushes it into
ClickHouse as one vectorized columnar scan + aggregation and returns in ~1.0 s.
The longer the window and the wider the cardinality, the wider that gap opens —
and this is the exact query pattern behind SLO/latency dashboards over multi-day
windows.

**The real argument isn't the latency — it's the architecture.** cerberus reads
its histograms straight out of a ClickHouse that (in a real deployment) already
stores the org's logs and traces, is S3-backed, and needs **no separate TSDB to
run, scale, back up, or operate**. Prometheus's 209 MiB in this run is a
dedicated in-memory time-series database that exists only to serve these metrics;
cerberus's gateway is 31 MiB in front of storage the org is already paying for.
That total-cost-of-ownership angle — one storage engine for all three signals,
no metrics-specific TSDB fleet — is cerberus's strongest claim, and it holds even
in the corners where Prometheus is milliseconds faster.

### Caveats (what this harness does and does not measure)

- Latencies are single-concurrency wall-clock HTTP round-trips on one host —
  per-query serving latency, not throughput under concurrent load. A concurrency
  sweep is the natural next step.
- CPU/mem are `docker stats` peaks sampled ~1 Hz on the **query-serving**
  containers only; the ClickHouse container that does cerberus's actual work is
  not sampled, so cerberus's resource rows understate its true footprint.
- Mimir runs monolithic single-binary on filesystem storage — a fair *functional*
  reference, not a tuned production cluster.
- The fixture uses constant per-step increments, so `rate()` is flat: this keeps
  cross-backend equality exact and gives a stable latency signal, but it is not a
  pathological-data stress test. Absolute millisecond figures also carry run-to-
  run variance from shared-host load; the *shape* of the result (instant ≈ tie,
  range → cerberus) is the stable signal, not any single number.

## Equivalence verdict

✅ **Both profiles, all 7 query shapes, all live backends returned equivalent
results within a 1e-4 relative tolerance — zero mismatches.** The latency
comparison above is apples-to-apples: every row compares the same answer computed
three ways.
