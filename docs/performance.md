# Performance & optimization

How cerberus keeps queries fast, and how that speed is held in place so
it cannot silently regress.

This is the detailed companion to the one-line claim in the README ("queries
are translated into optimised ClickHouse SQL"). It describes the *strategy*;
for the mechanical reference — the optimizer rule table and the CH-native
emitter features — see [`engine.md`](engine.md).

## The thing we optimize for: compute fan-out

Every real performance bug cerberus has shipped had the same shape, and it is
**not** the one a "rows read" lens catches. The leaf table scan was a normal
size and the storage layer pruned it perfectly; the cost was an **intermediate
row set that exploded** — a CROSS JOIN of an N-anchor step grid against raw
scan rows, a set-op chain re-executing an arm per level, a recursive closure
without a depth cap. Read-side cost (granules pruned, round-trips) and
**compute fan-out** (peak intermediate cardinality, wall-time vs a scaling
parameter) are *orthogonal* axes. A query can prune to one granule and still
re-evaluate those rows N times.

So the single number cerberus watches is the **fan factor**: peak intermediate
rows ÷ leaf scan rows. A bounded shape keeps it a small constant; a fan-out
makes it track a query parameter (step count, chain depth, recursion depth).
Everything below is in service of keeping that number flat.

Two architectural invariants frame the whole approach:

- **Route A is the default: one optimized plan → one CH statement.** The
  overwhelming majority of traffic is solved by lowering, optimizing, and
  emitting a *single* parameterised ClickHouse statement, with all reduction
  pushed into ClickHouse — the leaf scan, the windowing, the aggregation all
  happen server-side, where CH parallelises a single MergeTree scan across
  cores. Route A bounds memory with `max_memory_usage`, the sample budget, an
  11k-point resolution cap, and a streaming cursor; it is byte-identical to the
  pipeline cerberus has always shipped. (Loki's query-frontend exists because
  object storage has no parallel scan — that constraint doesn't transfer to CH,
  so route A stays the default rather than a scatter-gather frontend.)
- **The sharded-pushdown solver is the exception — ON by default (`auto`),
  narrow by construction.** Route A holds one CH statement per request for the
  overwhelming majority of traffic; the solver handles the single class route A
  cannot solve bounded: high **anchor fan-out** (`F = Range/Step`, e.g.
  `sum(rate(m[5m]))` at a fine step over a wide range), where one statement's
  peak intermediate cardinality exceeds the CH memory cap. For *that class
  only*, the `internal/solver` orchestrator ([`solver.md`](solver.md))
  re-anchors `K` deep copies of the **same already-optimized plan** onto
  disjoint slices of the anchor grid, emits each via the existing `chsql.Emit`,
  and concatenates the result streams behind the existing cursor. There is **no
  new evaluator and no new SQL template** — every shard runs the same
  compat-gated route-A SQL, restricted to its anchor sub-grid. The solver routes
  by default (`CERBERUS_EVAL_ROUTE=auto`), gated on the
  `compatibility/prometheus-forced-route` CI job, which forces every eligible
  plan onto route B (`CERBERUS_EVAL_ROUTE=sharded`) over the WHOLE upstream
  PromQL corpus and fails on any diff vs reference Prometheus — the corpus-wide
  proof that route B is byte-identical to route A. Routing is fail-toward-A:
  only ELIGIBLE, above-threshold plans take route B; ineligible queries
  (instant / `now64` / un-sliceable / grid-mismatch) always stay on route A.
  Operators pin `CERBERUS_EVAL_ROUTE=single` to disable routing entirely. The
  additive `X-Cerberus-Route-Decision` response header reports the per-request
  classification in every mode (`routed` / `below-threshold` / `instant` /
  `not-sliceable` / …). See [`solver.md`](solver.md) for the eligibility
  signals, slicing geometry, execution/cursor model, and failure contract, and
  [operations.md](operations.md#sharded-pushdown-solver) for the runtime knobs.
- **All-or-nothing wire contract.** Whether a request is solved by route A or
  fanned out across `K` shards, the client sees a single response: a shard
  failure surfaces as one typed error (first-error-wins, cause-threaded), never
  as a partial body. Sharding is an internal execution strategy, invisible at
  the wire format.
- **No caching.** Cerberus is a stateless query gateway, not a result cache.
  This invariant is **unchanged** by the solver relaxation — the solver
  re-emits and re-executes per request, it never memoises a previous result.
  The only TTL anywhere is the `/readyz` health probe. Speed comes from
  emitting a better query (and, for the unbounded class, from dividing it),
  never from caching a previous one.

## Where the speed comes from, layer by layer

The pipeline is `parse → lower → optimize → emit → execute`. Each stage has a
performance job.

### Lowering (`internal/{promql,logql,traceql}`)

The lowering is where the largest wins live, because it decides the *shape* of
the plan before any rule runs.

- **Single-pass range queries (RangeLWR).** A PromQL `query_range` over a bare
  selector needs the latest sample per series at each step anchor. The naive
  shape cross-joins an N-anchor step grid against the raw scan —
  `O(rows × anchors)`. Cerberus lowers it to a single windowed pass
  (`RangeLWR`): collapse to latest-per-series once, then broadcast across the
  grid. At 241 anchors this is the difference between materialising millions
  of sample×anchor pairs and a single scan.
- **Single-pass set-op chains.** `a or b or c …` lowers so each binary set-op
  scans both arms exactly once (a tagged `UNION ALL` + a window-partition
  anti-join) instead of re-materialising the left arm per level. The optimizer's
  `FlattenVectorSetOp` rule then collapses the left-associative *nesting* of N
  arms into one N-ary `NaryVectorSetOp`, which the emitter renders as a single
  `UNION ALL` over all K arms under one window pass — so a K-arm chain is one
  scan, not K nested window passes. (`unless` is not associative, so an `unless`
  chain keeps its binary nesting.)
- **Bounded recursion.** TraceQL structural operators (`>>`, `<<`) and
  nested-set traversals lower to `WITH RECURSIVE` carrying an explicit
  `_depth` column and a hard cap, so a cyclic trace terminates instead of
  running away.

### Optimizer (`internal/optimizer`)

A Catalyst-/DataFusion-style rule engine (Analyzer → Once → FixedPoint
batches) over the shared IR. The performance-relevant rules push filters and
projections toward the scan so ClickHouse does less work:

- **Predicate pushdown** moves filters below aggregates / range windows so the
  emitter can promote them into `PREWHERE` / skip-index territory.
- **Projection pushdown / late materialisation** resolves wide columns only
  after `LIMIT` has cut the row set.
- **Constant folding** collapses pure-literal subtrees so downstream rules and
  the emitter see simplified predicates.

The optimizer carries **no cost model** — it is a deterministic rewrite engine,
and every rule must earn its place: it carries only rules that fire.
The exhaustive, current rule table lives in
[`engine.md`](engine.md#a-real-rule-based-optimiser--internaloptimizer); it is
the source of truth and is not duplicated here precisely so it cannot drift.

### Emission (`internal/chsql`)

The typed emitter is CH-native, not ANSI-ish:

- **`PREWHERE` promotion** fuses `Filter(Scan)` and partitions conjuncts into a
  sort-prefix bucket / skip-index bucket / rest, promoting cheap predicates
  that touch no wide column so CH evaluates them before reading wide columns.
- **Streaming `clickhouse-go/v2` cursor** — bounded RSS, no full row buffer on
  the hot path.

See [`engine.md`](engine.md#typed-sql--internalchsql) for the full emitter
surface.

### Schema (`internal/schema`)

Cerberus defaults to the OpenTelemetry ClickHouse Exporter layout, but the
metrics tables are sorted **`MetricName`-first** (via the
`tsouza/opentelemetry-collector-contrib:cerberus-ddl` fork) rather than
`ServiceName`-first. A metric-only query (no service matcher) then binary-
searches the primary key instead of scanning most of the part — measured at
**8–17× fewer granules read** on representative queries, while a
service-pinned query costs only a couple of extra granules. See
[`operations.md`](operations.md) for the runtime memory/scaling contract.

## How "fast" is kept fast — the assurance framework

A one-time optimization is worthless if the next refactor quietly undoes it.
A human manually sweeping Grafana for the bugs above is not a control. Cerberus
holds the speed in place with four automated layers, spanning *static* (cheap,
every PR) to *broad* (corpus-wide, nightly).

1. **Static fan-out lint** — `internal/perf/fanout`, always-on in the
   regression suite. Flags the structurally-unbounded shapes — a `CrossJoin`
   with neither side bounded, an `arrayJoin` feeding a `JOIN`, an uncapped
   `WITH RECURSIVE`, a correlated subquery — on the lowered plan *and* emitted
   SQL of **every** corpus fixture. Cheap, pre-execution, no chDB needed.
2. **Per-construct scaling harness** — `test/perf/scaling`, in the
   `perf-guards` chDB job (runs on every PR; informational). For a known-hot
   construct it sweeps a parameter (step count, chain depth, recursion depth)
   and asserts wall-time stays **sub-linear** in it *and* peak intermediate
   cardinality stays **bounded**. This is the compute-fan-out axis the original
   read-side harness was blind to.
3. **Corpus-wide fan-out profiler** — `test/perf/profile`, nightly +
   push-to-main, informational. Profiles all ~636 fixtures via in-process chDB
   `EXPLAIN` + per-subquery `count()`, ranks them by fan factor, and surfaces
   the worst as a job step-summary. The wide net for a fan-out in a construct
   nobody thought to write a guard for.
4. **Cardinality ratchet** — `test/perf/cardinality_ratchet_test.go`, in the
   `perf-guards` chDB job (runs on every PR; informational). Pins every
   fixture's fan factor + structural flags + recursion depth in
   `test/perf/cardinality-baseline.json` and fails the run on an **upward**
   fan-factor regression, a new CROSS JOIN / `WITH RECURSIVE` where the baseline
   had none, or a deeper recursion. A new fixture must add a baseline row, so a
   new construct's absolute fan factor lands in the diff as a built-in cost
   review.

The static fan-out lint is the per-PR gate (in the required `check` job); the
scaling harness and cardinality ratchet run on every PR through the
informational `perf-guards` chDB lane; the profiler is the nightly wide net for
the unknown shapes.
Improvements are always allowed (a fan-factor *decrease* never blocks); the
ceiling only tightens when a maintainer re-runs
`just update-cardinality-baseline`.

### Set-op chains: N-ary linearisation

An associative set-op chain (`a or b or c …`, `and`) is fully flat on both
axes. Each binary operator already scans both arms exactly once, and the
`FlattenVectorSetOp` optimizer rule collapses the left-associative nesting of N
arms into one N-ary `NaryVectorSetOp` — a single `UNION ALL` over all K arms
under one window pass — so wall-time is sub-linear in chain depth and the peak
intermediate stays a small bounded constant. The `setop_chain` scaling harness
hard-gates **both** axes on this shape. `unless` is not associative
(`a unless (b unless c) ≠ (a unless b) unless c`), so an `unless` chain keeps
its binary nesting by construction — the one set-op shape that does not
linearise, because flattening it would change results.

## Rate-range windowing: why the fan-out ships as the default

`sum(rate(metric[5m]))` as a `query_range` (e.g. 1h @ 15s = **240 anchors**)
over the OTel-CH counter table is the one metrics shape route A cannot fold
flat — `rate` needs the per-window sample pairs, so the RangeLWR collapse
doesn't apply (see the
[`range query (240 steps)` note in benchmarks.md](benchmarks.md#end-to-end-the-query_range-path)).
The emit fans each sample into the `~Range/Step` overlapping windows it belongs
to (`arrayJoin`), groups + sorts per `(series, anchor)`, then applies
Prometheus's `extrapolatedRate`. That fan-out *looks* like the expensive part,
and every instinct says to attack the data movement. The alternatives don't win
at realistic scale; this section is the rationale for why the fan-out is the
right default.

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

## Native rate: exactness vs. scale (should I enable it?)

There is one optional knob in the rate-range story —
`CERBERUS_EXPERIMENTAL_TS_GRID_RANGE` — and it asks you a single, honest
question: **do you want results that are identical to Prometheus down to the
last bit, or results that scale to millions of rows on flat memory?** You only
have to think about it for `rate(...)` range queries (the `sum(rate(...[5m]))`
panel shape); everything else is unaffected. For almost every deployment the
default — *off* — is the right answer, and you can stop reading here. The rest
of this section is for the case where a query is large enough that "scales to
millions of rows" starts to matter.

### Default (off): exact, Prometheus-identical, sub-second at realistic scale

With the flag off, cerberus computes the rate the way it always has: the
`arrayJoin` fan-out described above, applying Prometheus's own
`extrapolatedRate` to each `(series, anchor)` window. This is the path the
differential compatibility suite proves against a *reference Prometheus engine*
on the same seeded data — the `compatibility/prometheus` gate, a required check
on every merge. **The default path is the one that gate signs off on, so its
results match Prometheus exactly.**

It is also fast at the scale real dashboards run. A normal 1h panel
(~1000 series × 15s ≈ 200–500k samples) is comfortably **sub-second** on the
shipped fan-out (the scale curve above tops out at 0.79s for 500k samples). The
one place it strains is memory: the fan-out materializes one intermediate row
per `(sample, anchor)` pair, so its peak memory grows with
**series × anchors**. Push that high enough — millions of samples over a wide,
fine-grained grid — and a single statement's peak crosses the per-query memory
cap (`CERBERUS_CH_QUERY_MAX_MEMORY`, **1 GiB** by default), and the query is
rejected rather than served. That is exactly the wall this flag exists to move.

> The [sharded-pushdown solver](solver.md) is cerberus's *other* answer to that
> wall, and it is on by default (`auto`): it slices the same fan-out across `K`
> statements so no single one exceeds the cap. Sharding makes the fan-out
> *fit*; the native path below makes it *vanish*. They are independent levers —
> see the [route × native-rate numbers in benchmarks.md](benchmarks.md#execution-routes--native-rate-the-matrix)
> for how they compose.

### The durable answer

Enabling the flag is the path that moves the arithmetic floor *down* instead of
working around it: ClickHouse ≥ 25.6 ships **`timeSeriesRateToGrid`**, which
ClickHouse ported from Prometheus's rate code essentially verbatim, so it
computes the *same* `extrapolatedRate`, but *inside the engine* in a single
pass. There is no
`(sample, anchor)` matrix to build, so the intermediate row set — and therefore
the memory — stays **flat** instead of growing with the grid. On the canonical
500k-row rate-range query the difference is stark:

| flag                  | how the rate is computed         | wall    | modeled peak memory |
| --------------------- | -------------------------------- | ------- | ------------------- |
| **off** (default)     | `arrayJoin` fan-out (Prom-exact) | ~658 ms | ~216 MiB            |
| **on** (experimental) | native `timeSeriesRateToGrid`    | ~87 ms  | ~11 MiB             |

(Measured on the 500k-row seed; full methodology and the route × native-rate
numbers are in [benchmarks.md](benchmarks.md#execution-routes--native-rate-the-matrix).)
The fan-out's memory scales with the data; the native path's stays roughly flat
no matter how many series or anchors you ask for — which is precisely what lets
it serve the million-row queries that would otherwise hit the cap.

**What you give up** is exact bit-for-bit agreement with Prometheus — but only
just barely. The native path is the *same algorithm*; the only difference is the
order the floating-point arithmetic happens in inside C++ vs. SQL. A dual-emit
parity test (`internal/chsql/range_window_native_chdb_test.go`) runs both paths
on the same data and compares the decoded `float64` grids: the overwhelming
majority of grid cells are **bit-identical**, and the few that differ do so by
**exactly one ULP** — one unit in the last place, the next representable double
(e.g. `0.12000000000000001` vs `0.12`). This is *not* a correctness defect: it
is the inherent last-bit difference between two correct evaluation orders of the
identical calculation, far below anything Prometheus can observe — the wire
format and Grafana both render `0.12` either way. The test pins this exactly
(every cell within 1 ULP, no more than the documented two cells diverging, none
by more than 1 ULP) rather than papering over it with a tolerance.

The native path is **experimental and off by default** for two honest reasons,
not because the rounding matters: it requires ClickHouse **≥ 25.6** (older
servers reject the unknown function), and it rides ClickHouse's experimental
`allow_experimental_time_series_aggregate_functions` setting, so it has not yet
been swept against a real (non-chDB) server where that setting is enforced.
Scope is **`rate` only** — `increase` / `delta` / `deriv` / `predict_linear`
stay on the fan-out until each native sibling is differentially proven against
Prometheus.

### The decision rule

| Your situation                                                       | Use                          |
| -------------------------------------------------------------------- | ---------------------------- |
| Normal dashboards / alerting — exact Prometheus parity matters       | **Default (off)**            |
| Your ClickHouse is older than 25.6                                   | **Default (off)** (required) |
| Large `rate(...)` range queries that hit the memory cap or feel slow | **Enable native (on)**       |

Enable native only when **all three** hold: the slow/large query is a
`rate(...)` range query over millions of rows, your ClickHouse is ≥ 25.6, and an
imperceptible last-bit rounding difference is acceptable for that panel.

In short: **leave it off** unless you are specifically running large
`rate(...)` range queries, you are on ClickHouse ≥ 25.6, and you would trade a
sub-observable rounding difference for an order-of-magnitude drop in memory and
latency. When the flag is on, an eligible `rate(<counter>[<range>])` query_range
lowers to a `chplan.RangeWindowNative` node that emits the native aggregate; the
wrapping outer aggregate (`sum by (...)`) is byte-identical, so only the
windowed-rate subquery changes, and turning the flag back off restores the
established, Prometheus-exact fan-out. The full env-var contract and CH-version
constraint live in
[`operations.md`](operations.md#experimental-native-rate-timeseriesratetogrid).

### The lesson

The bottleneck for rate-range is the **per-anchor extrapolation arithmetic
(irreducible)**, not data movement. So data-movement optimizations
(sharding / ASOF / MV / single-pass) don't help at realistic scale — and
realistic scale is already fast. When an optimization targets memory or
cardinality but the user-felt cost is wall, confirm which axis actually
dominates *before* building the alternative: here, four of them were built
before the 14 ms scan vs ~98%-arithmetic split was measured.

## See also

- [`benchmarks.md`](benchmarks.md) — live before/after wins, scaling curves,
  micro-benchmarks, and end-to-end query latency, regenerated by
  `just bench-report`.
- [`engine.md`](engine.md) — the pipeline stages in depth: the IR algebra, the
  optimizer rule table, and the typed CH-native emitter.
- [`solver.md`](solver.md) — the sharded-pushdown solver reference: the
  eligibility signals (which queries route B), the slicing geometry, the
  execution/cursor model, and the failure/cancellation contract.
- [`test-strategy.md`](test-strategy.md) — the full test-layer map the perf
  lanes sit inside.
- [`operations.md`](operations.md) — runtime memory, admission control, and
  scaling contract.
