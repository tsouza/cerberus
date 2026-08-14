# Spec: temporality-aware native rate lowering

Tracks issue #2114, the remaining native `ts_grid_range` branch of umbrella
issue 1963.

## Problem

The native `timeSeriesRateToGrid` lowering is deliberately unavailable whenever
the range window carries `AggregationTemporality`:

- `internal/promql/lower.go`'s `nativeTSGridMatrixNode` returns `nil` when
  `rw.TemporalityColumn != ""`.
- The default metrics schema declares that column, so `rate()` always falls back
  to the array-join fan-out path despite the `ts_grid_range` feature being
  enabled.

The guard is correct. ClickHouse's `timeSeriesRateToGrid` has no per-row
temporality argument, while DELTA metrics need the runtime branch the fan-out
emitter applies. Sending a DELTA series through the native aggregate would
answer it as a CUMULATIVE counter.

ClickHouse 26.5.1.1 exposes no temporality-aware member of the
`timeSeries*ToGrid` family, so an emitter-only change cannot close the gap.

## Scope

Included:

- Range-mode PromQL `rate()` over shape-eligible counter selectors when the
  schema has an aggregation-temporality column.
- A plan-level split that keeps CUMULATIVE-known series native and evaluates
  DELTA-known series through the existing fan-out lowering.
- Correctness coverage for all-CUMULATIVE, all-DELTA, and mixed inputs,
  including a solver Route-B mixed-union assertion.

Excluded:

- `increase()` and `delta()`: no equivalent native aggregate has been proven.
- Native histogram rate folding: issue #2115 is a separate child of #1963.
- A metadata preflight query or cache to discover temporality. That would add a
  round trip and introduce a staleness contract where the data-plane split is
  exact at query time.
- Any change to the existing native paths for `changes()`, `resets()`, or other
  functions.

## Design

When `NativeRateLowerer` receives a shape-eligible `rate()` range window with
`TemporalityColumn` set, it produces a `chplan.UnionAll` with two disjoint arms:

1. A `RangeWindowNative` arm whose input is filtered to rows whose aggregation
   temporality is not DELTA. This arm uses `timeSeriesRateToGrid`.
2. The existing `RangeWindow` fan-out arm whose input is filtered to DELTA rows.
   This retains the runtime counter-or-delta arithmetic already validated for
   DELTA data.

Both arms preserve the request `Start`, `End`, `Step`, `Offset`, grouping, and
output-column order, so their concatenation is a valid matrix input to any
outer plan node. The predicates are complementary and data-local: no series can
be evaluated by both arms, and no lookup can become stale between planning and
execution.

The native eligibility helper remains the only construction funnel for
`RangeWindowNative`. Rather than teaching it an unsafe per-row branch, the
rate-specific strategy constructs the two arm inputs before calling the same
helper for the cumulative arm. A failed shape check still delegates unchanged to
the existing fan-out fallback.

The native and fan-out arms must remain re-anchorable. #2121 established the
necessary `UnionAll` and `RangeWindowNative` solver support; this change proves
the production lowering reaches that shape rather than relying only on the
synthetic solver fixture.

## Verification

- Add lowering tests that assert the temporality-bearing native rate shape is a
  two-arm union with complementary predicates, and that unsupported shapes
  still use fan-out only.
- Add seeded PromQL fixtures covering CUMULATIVE, DELTA, and mixed series. Their
  answers must match the existing fan-out path and their native sections must
  show the intended split.
- Run the chDB-backed fixture cells so both arms execute rather than merely
  snapshotting plan structure.
- Extend the native-union solver coverage with the production-derived shape and
  assert Route B reanchors both arms on every shard.
- Regenerate required PromQL, solver, and cardinality generated artefacts with
  `just update-golden` after implementation; review every generated diff.
- The required Prometheus compatibility and real-ClickHouse lanes remain CI
  evidence for wire compatibility and strict production execution.

## Risks

- A predicate that is not complementary duplicates or drops samples. Tests must
  exercise a mixed-temporality input whose two arms contribute distinct values.
- A row-level filter placed above the wrong projection can hide the temporality
  column from one arm or change label shaping.
- Mixed arm output columns or grids can make a valid-looking union return
  incorrect matrix rows. The plan assertion and execution fixture cover both
  contracts.
- A native arm left pinned during Route-B slicing would repeat whole-query rows
  in each shard. The solver test must inspect both arm bounds per slice.

## Task List For Owner Review

1. Add the minimal lowering helper/strategy change that creates complementary
   CUMULATIVE-native and DELTA-fan-out arms for eligible `rate()` windows.
   Pin the emitted plan shape and preserve fan-out fallback behavior.
2. Add mixed-temporality seeded PromQL fixtures with chDB expected rows, plus
   all-CUMULATIVE and all-DELTA controls. Regenerate only the required PromQL
   generated artefacts.
3. Extend solver coverage from the synthetic mixed union to the production
   lowering and prove Route B reanchors both arms exactly once per request
   anchor. Regenerate required solver/cardinality baselines.
4. Review generated artefacts and the final diff, run the targeted required
   checks, then commit, push, and create the issue-closing PR. Let CI supply the
   real-ClickHouse and compatibility evidence.
