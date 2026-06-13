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
  narrow by construction.** The old lock ("one CH query per request — no
  scatter-gather,
  no merge layer to add later") was relaxed by the maintainer on 2026-06-12 for
  the single class route A cannot solve bounded: high **anchor fan-out**
  (`F = Range/Step`, e.g. `sum(rate(m[5m]))` at a fine step over a wide range),
  where one statement's peak intermediate cardinality exceeds the CH memory cap.
  For *that class only*, the `internal/solver` orchestrator
  ([`query-solver-design.md`](query-solver-design.md)) re-anchors `K` deep
  copies of the **same already-optimized plan** onto disjoint slices of the
  anchor grid, emits each via the existing `chsql.Emit`, and concatenates the
  result streams behind the existing cursor. There is **no new evaluator and no
  new SQL template** — every shard runs the same compat-gated route-A SQL,
  restricted to its anchor sub-grid. The phase-2 flip landed on 2026-06-13: the
  solver now routes by default (`CERBERUS_EVAL_ROUTE=auto`), gated on the
  `compatibility/prometheus-forced-route` CI job, which forces every eligible
  plan onto route B (`CERBERUS_EVAL_ROUTE=sharded`) over the WHOLE upstream
  PromQL corpus and fails on any diff vs reference Prometheus — the corpus-wide
  proof that route B is byte-identical to route A. Routing remains
  fail-toward-A: only ELIGIBLE, above-threshold plans take route B; ineligible
  queries (instant / `now64` / un-sliceable / grid-mismatch) always stay on
  route A. Operators pin `CERBERUS_EVAL_ROUTE=single` to disable routing
  entirely. The additive `X-Cerberus-Route-Decision` response header reports
  the per-request classification in every mode (`routed` / `below-threshold` /
  `instant` / `not-sliceable` / …). See
  [`evaluation-architecture.md`](evaluation-architecture.md) for the operator
  classification the solver's safe set maps onto, and
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
  anti-join) instead of re-materialising the left arm per level. (A residual
  super-linearity from the left-associative *nesting* of N arms is tracked
  separately — see "Known residuals" below.)
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
and every rule must earn its place (two inert rules were retired in 2026-06).
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
The bugs above were originally caught by a human manually sweeping Grafana —
which is not a control. Cerberus replaces that with four automated layers,
spanning *static* (cheap, every PR) to *broad* (corpus-wide, nightly).

1. **Static fan-out lint** — `internal/perf/fanout`, always-on in the
   regression suite. Flags the structurally-unbounded shapes — a `CrossJoin`
   with neither side bounded, an `arrayJoin` feeding a `JOIN`, an uncapped
   `WITH RECURSIVE`, a correlated subquery — on the lowered plan *and* emitted
   SQL of **every** corpus fixture. Cheap, pre-execution, no chDB needed.
2. **Per-construct scaling harness** — `test/perf/scaling`, in the required
   `perf-guards` chDB job. For a known-hot construct it sweeps a parameter
   (step count, chain depth, recursion depth) and asserts wall-time stays
   **sub-linear** in it *and* peak intermediate cardinality stays **bounded**.
   This is the compute-fan-out axis the original read-side harness was blind
   to.
3. **Corpus-wide fan-out profiler** — `test/perf/profile`, nightly +
   push-to-main, informational. Profiles all ~636 fixtures via in-process chDB
   `EXPLAIN` + per-subquery `count()`, ranks them by fan factor, and surfaces
   the worst as a job step-summary. The wide net for a fan-out in a construct
   nobody thought to write a guard for.
4. **Cardinality ratchet** — `test/perf/cardinality_ratchet_test.go`, in the
   required `perf-guards` chDB job. Pins every fixture's fan factor + structural
   flags + recursion depth in `test/perf/cardinality-baseline.json` and fails
   the PR on an **upward** fan-factor regression, a new CROSS JOIN /
   `WITH RECURSIVE` where the baseline had none, or a deeper recursion. A new
   fixture must add a baseline row, so a new construct's absolute fan factor
   lands in the diff as a built-in cost review.

The lint + ratchet are the per-PR gates; the scaling harness pins the
known-hot shapes; the profiler is the wide net for the unknown ones.
Improvements are always allowed (a fan-factor *decrease* never blocks); the
ceiling only tightens when a maintainer re-runs
`just update-cardinality-baseline`.

### Known residuals

The framework is honest about what is *not yet* flat. Set-op chains are
single-pass per binary operator, but `a or b or c …` still lowers
left-associatively into K nested levels, so wall-time grows ~2.6×/level even
though the intermediate stays bounded. The scaling harness records this as a
tracked `KnownSuperlinear` finding (the cardinality axis still hard-gates), and
the true fix — flattening the chain into one N-ary single pass — is tracked
rather than silently accepted.

## Rate-range windowing: why we ship the fan-out

This section is a decision record. `sum(rate(metric[5m]))` as a `query_range`
(e.g. 1h @ 15s = **240 anchors**) over the OTel-CH counter table is the one
metrics shape route A cannot fold flat — `rate` needs the per-window sample
pairs, so the RangeLWR collapse doesn't apply (see the
[`range query (240 steps)` note in benchmarks.md](benchmarks.md#end-to-end-query-latency)).
The emit fans each sample into the `~Range/Step` overlapping windows it belongs
to (`arrayJoin`), groups + sorts per `(series, anchor)`, then applies
Prometheus's `extrapolatedRate`. That fan-out *looks* like the expensive part,
and every instinct says to attack the data movement. We built and measured the
alternatives. They lose. This records why, so they are never re-attempted.

All numbers below were **measured this session on real ClickHouse 24.8, 8-core**
(not chDB — the benchmarks.md curves run in-process chDB; these are prod CH).

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

### The measured dead-ends

Each row is an alternative we built and benchmarked against the fan-out. They
are recorded here with numbers precisely so nobody re-spends the time:

| alternative                            | what it does                                                        | result                                                                             | why it loses                                                                                                                                                        |
| -------------------------------------- | ------------------------------------------------------------------- | ---------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Anchor-grid **sharding** (route B)     | K parallel shards over disjoint anchor sub-grids                    | **+8% slower** AND **8× scan amplification** (40M vs 5M `read_rows`)               | the 5m lookback straddles shards, so every shard re-scans the whole table. It's a MEMORY mechanism, not a wall one.                                                 |
| **ASOF JOIN** boundary lookup (#97)    | window-function enrichment instead of `arrayJoin`                   | cardinality down (fan_factor **5.83 → 1.0**) but wall **REGRESSED 6.31s → 36.90s** | ASOF + window enrichment is far slower than `arrayJoin` + `GROUP BY`. The cardinality ratchet missed it — the ratchet is blind to wall. (#851, closed.)             |
| **MV substitution / downsampling**     | rollup the 14 ms scan                                               | attacks the wrong axis; also lossy                                                 | breaks exact parity; cerberus already removed its `MVSubstitution` rule as a no-op (no rollups exist).                                                              |
| **Naive single-pass array**            | per-anchor `arrayCount` / `arrayFirstIndex` over a per-series array | **6.9s**                                                                           | per-anchor LINEAR rescans — same `O(n × windows)` class as the fan-out; only cut memory ~5×.                                                                        |
| **`arrayReduceInRanges`** segment-tree | range-aggregate the per-series sample array                         | dominated                                                                          | cannot produce the reset-adjusted increase — counter resets are a global per-series property needing the cumulative prefix-sum, which a segment-tree doesn't carry. |
| **B2 prefix-sum / two-pointer**        | the asymptotically-optimal single pass, byte-exact parity           | LOSES at realistic scale, WINS only past ~1M (crossover **~1M**)                   | the optimal algorithm is the wrong *default* because the extrapolation arithmetic is the floor, not the data movement it optimizes. See below.                      |

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

### Why we ship the fan-out

At realistic scale it is already Prometheus-class, and every alternative we
built **loses at realistic scale** because they all optimize data movement
while the irreducible cost is the per-anchor extrapolation arithmetic. Sharding
and ASOF and single-pass each cut memory or cardinality — the axes the
cardinality ratchet watches — but pay for it in wall, and wall is the axis the
user feels. The fan-out is the right default until the arithmetic floor itself
moves.

### The durable answer

Native ClickHouse **`timeSeriesRateToGrid`** — CH copied Prometheus's rate code
verbatim, so it computes the same `extrapolatedRate` *inside the engine*,
moving the arithmetic floor down rather than around it. It lands in CH
**≥ 25.6**; the deployment is on **24.8**. The plan is to adopt it outright when
the CH floor moves; until then the fan-out is the right default.

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
- [`query-solver-design.md`](query-solver-design.md) — the sharded-pushdown
  solver: the eligibility signals, the slicing geometry, and the phased
  migration that keeps route A the default.
- [`evaluation-architecture.md`](evaluation-architecture.md) — the operator
  classification (slice-invariant vs. not) the solver's safe set maps onto, and
  the ceiling shapes time-slicing cannot reach.
- [`test-strategy.md`](test-strategy.md) — the full test-layer map the perf
  lanes sit inside.
- [`operations.md`](operations.md) — runtime memory, admission control, and
  scaling contract.
