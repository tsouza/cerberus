# Performance & optimization — background

This document collects the design rationale, rejected alternatives, incident
history, and measurements behind [`performance.md`](performance.md). It answers
"why is it built this way" rather than "what does it do" — nothing here is
required to operate cerberus or to extend its performance lanes correctly;
`performance.md` is self-sufficient for that.

Every wall-clock and memory figure below is a point-in-time measurement from a
benchmark run, recorded to explain a decision. The values a gate actually
enforces — the pinned thresholds and the committed baselines — live in
`performance.md` and in the baseline files it names; a threshold moves only on
a legitimate re-cut, never because a number here was re-measured.

## Why route A is not a scatter-gather frontend

Loki's query-frontend exists because object storage has no parallel scan — that
constraint doesn't transfer to CH, so route A stays the default rather than a
scatter-gather frontend.

## Why the real-ClickHouse memory sentinels (assurance layer 5) exist

Assurance layers 1-4 all profile structural proxies for cost — fan factor,
cardinality, scaling exponents — measured against chDB or in-process. None of
them runs a real ClickHouse server and reads back real per-query memory, which
is exactly the blind spot that let issue #2358 ship: a fix validated only by a
manual timing comparison on CI-fixture-scale data silently removed a CSE fold
the emitter relied on, and 6h8m later prod hit `MEMORY_LIMIT_EXCEEDED`
(issue #2364) — invisible to every layer above because the failure mode was
memory, not fan-out shape, and only appeared at real cardinality on a real
server. That is why layer 5 seeds each sentinel at the scale #2364's own
root-cause mechanisms need in order to engage, and reads peak per-query memory
back from a real server's `system.query_log`.

## Why `fan_factor` is nullable

Before issue #1519, the stages the profiler's per-subquery `count()`
decomposition cannot descend into silently flattened to `fan_factor = 1.00` —
the profiler reported "no fan-out" on exactly the constructs it exists to
catch. Making `profile.Record.FanFactor` a `*float64` that is nil whenever
`UncountableLevels` is nonzero replaces that fabricated `1.00` with an honest
"unmeasured", which the ratchet can then treat as a state to hold rather than
as a passing measurement.

## Why the rate-range fan-out ships as the default

The `arrayJoin` fan-out that [`performance.md`](performance.md) describes for
`sum(rate(metric[5m]))` as a `query_range` (e.g. 1h @ 15s = **240 anchors**)
over the OTel-CH counter table is the one metrics shape route A cannot fold
flat — `rate` needs the per-window sample pairs, so the RangeLWR collapse
doesn't apply (see the
[`range query (240 steps)` note in benchmarks.md](benchmarks.md#end-to-end-the-query_range-path)).
That fan-out *looks* like the expensive part, and every instinct says to
attack the data movement. The alternatives don't win at realistic scale; this
section is the rationale for why the fan-out is the right default.

The numbers below come from real ClickHouse 24.8, 8-core (a bench host at the
supported deployment floor of CH 24.8) — not chDB; the benchmarks.md curves run
in-process chDB, these are prod CH.

### The bottleneck is the extrapolation arithmetic, not the scan

The single load-bearing fact: the bare table scan is **14 ms**, fully
page-cached. The wall is **~98% per-anchor Prometheus-extrapolation
arithmetic** — `extrapolatedRate` evaluated once per `(series, anchor)` window.
It is compute, not data movement. The route-A fan-out scale curve:

| samples | wall  | peak mem |
| ------- | ----- | -------- |
| 100k    | 0.45s | —        |
| 300k    | 0.57s | 0.76 GiB |
| 500k    | 0.79s | —        |
| 1M      | 1.5s  | —        |
| 5M      | 7.6s  | 5.47 GiB |

The realistic-scale reading is the decision: **a normal 1h panel
(~1000 series × 15s ≈ 200–500k samples) is already sub-second on what we
ship.** 5M samples is **5000 fully-sampled series** — a high-cardinality stress
case, not a panel anyone draws. At realistic scale the fan-out is already
Prometheus-class, and the extrapolation-arithmetic floor (~1.7–2s at high
cardinality) — *not* the data movement — is what every alternative has to beat.
None do.

### Why the alternatives don't win on wall time

Each row is an alternative to the fan-out, with the numbers that decide it. The
common thread: every one of them optimizes data movement (memory or
cardinality), while the irreducible cost at realistic scale is the per-anchor
extrapolation arithmetic — so none of them moves the wall-time floor.

| alternative                            | what it does                                                        | result                                                                             | why it doesn't win on wall                                                                                                                                                                                                                                                                                      |
| -------------------------------------- | ------------------------------------------------------------------- | ---------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Anchor-grid **sharding** (route B)     | K parallel shards over disjoint anchor sub-grids                    | **+8% slower** AND **8× scan amplification** (40M vs 5M `read_rows`)               | the 5m lookback straddles shards, so every shard re-scans the whole table. Sharding is a MEMORY mechanism — it divides the per-statement peak so the unbounded class clears the cap; it is not a wall-time optimization, which is exactly why route B is reserved for that class and route A stays the default. |
| **ASOF JOIN** boundary lookup          | window-function enrichment instead of `arrayJoin`                   | cardinality down (fan_factor **5.83 → 1.0**) but wall **6.31s → 36.90s** worse     | ASOF + window enrichment is far slower than `arrayJoin` + `GROUP BY`, and the cardinality ratchet is blind to wall so it wouldn't catch the regression.                                                                                                                                                         |
| **MV substitution / downsampling**     | rollup the 14 ms scan                                               | attacks the wrong axis; also lossy                                                 | breaks exact parity; the default schema ships no rollups, so a `MVSubstitution` rule would be a guaranteed no-op (the optimizer carries no such rule).                                                                                                                                                          |
| **Naive single-pass array**            | per-anchor `arrayCount` / `arrayFirstIndex` over a per-series array | **6.9s**                                                                           | per-anchor LINEAR rescans — same `O(n × windows)` class as the fan-out; cuts memory ~5× but not wall.                                                                                                                                                                                                           |
| **`arrayReduceInRanges`** segment-tree | range-aggregate the per-series sample array                         | dominated                                                                          | cannot produce the reset-adjusted increase — counter resets are a global per-series property needing the cumulative prefix-sum, which a segment-tree doesn't carry.                                                                                                                                             |
| **B2 prefix-sum / two-pointer**        | the asymptotically-optimal single pass, byte-exact parity           | loses at realistic scale, wins only past ~1M (crossover **~1M**)                   | the optimal algorithm is the wrong *default* because the extrapolation arithmetic is the floor, not the data movement it optimizes. See below.                                                                                                                                                                  |

The B2 prefix-sum / two-pointer deserves the detail, because it is the
*asymptotically optimal* answer and it still loses as a default:

| samples | B2 (optimal single-pass) | fan-out (shipped) | verdict                         |
| ------- | ------------------------ | ----------------- | ------------------------------- |
| 300k    | 1.69s / 2.2 GiB          | 0.57s / 0.76 GiB  | fan-out **3× faster**           |
| 5M      | 3.75s / 1.23 GiB         | 7.58s / 5.47 GiB  | B2 **2× faster, 4.4× less mem** |

The crossover sits near **1M samples** — above realistic panel scale. B2 wins
only in the high-cardinality stress regime, and it wins on *memory* and *wall*
there because it stops materializing the pair set. But at the scale real panels
run, the extrapolation-arithmetic floor dominates the data movement B2
optimizes, so the optimal algorithm is **3× slower** than the fan-out it was
meant to replace.

### Why the fan-out is the default

At realistic scale it is already Prometheus-class, and every alternative
**loses at realistic scale** because they all optimize data movement while the
irreducible cost is the per-anchor extrapolation arithmetic. Sharding and ASOF
and single-pass each cut memory or cardinality — the axes the cardinality
ratchet watches — but pay for it in wall, and wall is the axis the user feels.
(Sharding's memory win is still the right tool for the unbounded class, which is
why route B exists for it — but it is a memory mechanism, not a wall-time one,
so it is not the default for the bounded majority.) The fan-out is the right
default until the arithmetic floor itself moves.

> Every wall/memory number in this section is **prose** — a real benchmark
> run, hand-transcribed into this file, with no CI gate keeping it honest.
> That gap is exactly what let #2358 ship unnoticed: a real-CH fix validated
> only by a manual timing comparison at CI-fixture scale, with nothing
> re-measuring memory at realistic cardinality, silently regressed peak
> memory 6h8m later (#2364). `test/perf/smoke` (the assurance framework's
> [layer 5](performance.md#how-fast-is-kept-fast--the-assurance-framework),
> in the required `strict-scan` job) closes that specific gap for the
> incident's own three
> mechanisms: it is a live, per-PR ClickHouse memory measurement, not a
> point-in-time table like the ones above.

## The lesson: confirm which axis dominates before building the alternative

The bottleneck for rate-range is the **per-anchor extrapolation arithmetic
(irreducible)**, not data movement. So data-movement optimizations
(sharding / ASOF / MV / single-pass) don't help at realistic scale — and
realistic scale is already fast. When an optimization targets memory or
cardinality but the user-felt cost is wall, confirm which axis actually
dominates *before* building the alternative: here, four of them were built
before the 14 ms scan vs ~98%-arithmetic split was measured.
