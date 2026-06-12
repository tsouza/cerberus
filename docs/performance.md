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

- **One query per request — no scatter-gather.** ClickHouse parallelises a
  single MergeTree scan server-side, so cerberus never splits a query into
  shards and merges client-side. Memory is bounded instead by
  `max_memory_usage`, the sample budget, an 11k-point resolution cap, and a
  streaming cursor. (Loki's query-frontend exists because object storage has
  no parallel scan — that constraint doesn't transfer to CH.) This freezes the
  execution architecture: there is no merge layer to add later.
- **No caching.** Cerberus is a stateless query gateway, not a result cache.
  The only TTL anywhere is the `/readyz` health probe. Speed comes from
  emitting a better query, never from memoising a previous one.

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

## See also

- [`engine.md`](engine.md) — the pipeline stages in depth: the IR algebra, the
  optimizer rule table, and the typed CH-native emitter.
- [`test-strategy.md`](test-strategy.md) — the full test-layer map the perf
  lanes sit inside.
- [`operations.md`](operations.md) — runtime memory, admission control, and
  scaling contract.
