# Evaluation architecture: push-down-everything vs a streaming eval tier

**Status:** assessment / decision pending (pre-RC1). **Date:** 2026-06-12.

This document exists because `rate()` over a realistic step grid kept hitting
hard problems (`#75`, `#76`, `#90`, and `#92` — a 240-step `rate()` over 5M
rows that **OOMs** real ClickHouse at the 1 GiB per-query cap). Those are not
five unrelated bugs; they are one architectural seam showing through. Per the
RC1 perf gate ("performance can force architecture changes, so it must be
settled *before* RC1 is tagged"), this is the call to make now.

## The seam

Cerberus does two jobs through one mechanism:

1. **Storage access** — selectors, label filters, groupings, simple
   aggregates. This is a *query against a store*, and ClickHouse is excellent
   at it. This is ~80% of real traffic and the whole drop-in value
   proposition.
2. **Time-series evaluation** — `rate()`/`increase()` with counter-reset
   correction, subqueries, `quantile_over_time`, overlapping step-grid
   windows. This is *not* a query against a store; it is a re-implementation
   of Prometheus's / Loki's streaming sample-evaluator, expressed in SQL.

Job 1 fits the "everything is one ClickHouse SQL statement" model perfectly.
Job 2 fights it. Prometheus computes `rate()` trivially because it walks
samples in memory with a purpose-built iterator. Expressing the same thing as
a single relational query forces one of two corners:

- **materialise the per-window sample arrays** — and because each sample at
  step `s` with lookback `L` lands in `L/s` overlapping windows, the
  intermediate is `O(rows × L/s)` → the `#92` OOM; or
- **heroic window-function chains** — what `#92`'s fix is.

The step-grid × overlapping-window shape is *intrinsically* a fan-out in the
relational model that a sample-iterator does not have. That is the seam.

## Spike — measured, not assumed

Reproduced on the compose `clickhouse-server` (1 GiB per-query cap), a counter
metric of **5,000,000 samples / 500 series over 1 h**, query
`sum(rate(metric[5m]))` as a `query_range` at 1 h / 15 s step (240 anchors,
`L/s = 20×` overlap).

- **A — all in SQL (current).** Full eval pushed into one CH query. CH peak
  memory **> 1 GiB**, ~0 wire → **OOM 422 (Code 241)**. Fails.
- **A′ — `#92` cumulative-counter.** Same, but the reset-corrected cumulative
  is computed once then `argMin`/`argMax` per anchor — no per-anchor array, so
  CH memory is bounded, ~0 wire. Succeeds (in progress).
- **B — streaming eval tier.** CH streams the raw ordered samples; cerberus
  runs the reference evaluator per series. CH peak memory **180 MiB** (bounded
  scan + sort), **89 MiB** shipped over the wire (4.99 M rows), **0.68 s**
  scan. Succeeds, bounded.

Key derived facts:

- **A** trades *zero wire* for *`O(rows × L/s)` CH memory* → OOM-prone, and
  parity-by-re-implementation (every operator is a fresh SQL puzzle with a
  fresh sharp edge).
- **B** trades *bounded memory on both sides* and *parity-for-free* (reuse
  upstream's evaluator, so `rate()` is exact by construction) for a **wire
  cost that scales with `range × cardinality`**. Here it is 89 MiB; a 24 h
  query over 50k series would be multiple GB shipped to cerberus per request.
  cerberus-side memory stays bounded only because it evaluates **one series at
  a time** (~largest-series ≈ 10k samples ≈ 160 KB), not the whole result.

Neither is free. A pays in CH memory + emitter complexity; B pays in wire +
a re-introduced compute tier.

## The three points on the spectrum

- **A — push everything into SQL** *(today + `#92`)*. Maximally stateless: one
  query, one round-trip, no compute tier, no caching. Works for every operator
  that has a **bounded-memory SQL reduction**. The risk is the operators that
  do *not* — each becomes an OOM waiting to happen, and the emitter accretes
  ever-more-intricate window-function gymnastics.
- **B — minimal SQL + in-process evaluator**. CH does scan + cheap reductions;
  cerberus streams ordered samples and runs the actual
  `prometheus/promql` / `loki/logql` evaluator over them. Parity becomes a
  property of the build, not a per-operator project; memory is bounded by
  streaming. Cost: a compute tier returns, the wire grows with
  `range × cardinality`, and it is in direct tension with the ratified
  "single-query, no scatter-gather, no caching" lock.
- **B′ — hybrid (push the bounded reduction, finalise the edge)**. Push the
  *per-series bounded reduction* to CH (exactly `#92`'s cumulative-counter, or
  per-`(series, anchor)` first/last), stream the **small** reduced result
  (~120k rows ≈ low single-digit MB here), and finalise only the cheap,
  parity-sensitive edge logic (extrapolation to window boundaries, exemplar
  selection) in cerberus. Best of both **for any operator whose reduction is
  expressible bounded in SQL**. It does not help operators whose reduction is
  *not* so expressible (nested subqueries, some `_over_time` shapes at scale) —
  those still force a choice between A's gymnastics and B's streaming.

## Recommendation

1. **Land `#92` now.** The cumulative-counter rewrite is the A′ fix; it
   removes the immediate OOM in pure SQL and unblocks the live failure. Do not
   block it on this decision.
2. **Make the architecture call a gating, enumerated decision — before RC1.**
   The push-down model (A) is *sound as a default* iff every time-series-eval
   operator has a **bounded-memory** lowering. So enumerate them and classify:
   - **Bounded-in-SQL** → keep in A, with a memory-axis perf guard
     (the `#92` scaling construct generalised). e.g. `rate`/`increase`/`delta`
     (via cumulative), the incremental `_over_time` (avg/min/max/sum/count).
   - **Not obviously bounded** → `quantile_over_time` / `stddev_over_time`
     (need all samples), nested subqueries (window-of-windows), high-overlap
     `_over_time` at scale. For each: either find a bounded SQL reduction (B′)
     or accept it needs streaming eval (B).
   If the unbounded set is **small and rare**, stay with A + per-operator
   bounded reductions and a memory guard — *do not* add a compute tier. If it
   is **large or common**, build the B′ reduction tier (and only fall back to
   full B for the genuinely irreducible operators).
3. **RC1's architecture is "final" only once that table is filled in** — every
   remaining eval operator is either proven bounded-in-SQL or has a decided
   B′/B plan. That is the literal meaning of the perf-RC1 gate; `#92` grinding
   is the gate working.

## What would actually change cerberus, and what would not

- A / A′ / B′ **preserve** the public contract (three drop-in datasources,
  one store, no caching) and the single-query/no-merge lock. B′ adds a thin
  in-process finalisation step but keeps the heavy lifting in CH.
- Full **B** reintroduces a compute tier and would reopen the no-merge /
  wire-budget questions — it is the only option that materially changes the
  execution architecture, and should be reserved for operators that provably
  cannot be reduced bounded in SQL.

## Pointers

- [`performance.md`](performance.md) — the compute-fan-out lens these problems
  live under; the four-layer assurance framework (its `range_lwr` scaling
  construct covers *latest-per-series*, **not** range-aggregations — the gap
  `#92` exposed, now being closed with a memory-axis construct).
- [`engine.md`](engine.md) — the parse → lower → optimize → emit → execute
  pipeline this assessment is about.
