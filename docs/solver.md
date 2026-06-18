# Sharded-pushdown solver — reference

The sharded-pushdown solver (`internal/solver`) is the route-B execution
strategy for the one query class route A cannot bound: high **anchor fan-out**
(`F = Range/Step`, e.g. `sum(rate(m[5m]))` at a fine step over a wide range),
where one statement's peak intermediate cardinality exceeds the ClickHouse
memory cap. It re-anchors `K` deep copies of the **same already-optimized
plan** onto disjoint slices of the anchor grid, emits each via the existing
`chsql.Emit`, executes them with bounded parallelism behind a global
connection gate, and concatenates the result streams behind the existing
`chclient.Cursor`. There is no new evaluator and no new SQL template: every
shard runs the same compat-gated SQL route A runs, restricted to its anchor
sub-grid.

This document is the deeper reference. For the runtime contract (knobs,
modes, the shadow header, memory sizing) see
[`operations.md`](operations.md#sharded-pushdown-solver); for where route B
sits relative to route A and the alternatives see
[`performance.md`](performance.md). This reference covers the four
reader-facing specifics that live nowhere else: the eligibility signals, the
slicing geometry, the execution/cursor model, and the failure/cancellation
contract.

## Eligibility signals

The `Planner` is pure, read-only classification of a post-optimize plan. It
never mutates the plan. A single pass walks both the node tree **and** every
expression tree — including `ScalarSubquery.Input`, which `chplan.Walk` does
not recurse into — gathering static signals; the cost thresholds and the `K`
clamp then decide. A plan routes B only when every signal passes; any failure
falls through to route A, byte-identical to the single-statement pipeline.

The signals, each gathered in the one pass:

1. **Slice-invariance, by marker.** A node is admissible only if it is
   registered `SliceInvariant` — a machine-checkable assertion that its
   per-`(series, anchor)` output is a pure function of in-window samples,
   independent of the scan lower bound. Any unmarked node anywhere in the plan
   sends the whole plan to route A. A new node defaults to route A until its
   marker is proven.
2. **Routable spine family.** Re-anchoring rewrites the grid carried by the
   `RangeWindow` matrix family and the `RangeLWR` bare-selector
   last-with-respect-to family. A `RangeBucketFanout` or `StepGrid` spine
   carries its own eval grid that re-anchoring clones verbatim, so a plan whose
   spine bound-carrier is one of those fails closed to route A (every shard
   would otherwise emit stale bounds).
3. **Pinned bounds.** Both `Start` and `End` must be pinned (non-zero) on the
   outermost windowed node — it anchors the whole grid. An inner subquery node
   may be unpinned (both bounds zero, the shape the re-anchor fills); a
   half-pinned node (exactly one zero bound) is malformed and routes A. An
   instant-shape windowed node (`Step == 0` or outermost `OuterRange == 0`)
   has no anchor grid and routes A.
4. **No `now64` anywhere.** Two statements resolving `now64()` independently
   would observe different wall-clocks, breaking the disjoint-anchor argument.
   The expr-walk rejects any `now64` call in `Filter` predicates, projections,
   aggregate group keys / arguments, and `ScalarSubquery.Input`.
5. **Grid-prediction (the @-modifier guard).** A pinned windowed node must sit
   exactly on the grid the request predicts at its spine depth: its
   `(Start, End, OuterRange)` must equal the predicted values, where the
   outermost node predicts `[meta.Start, meta.End]` and each nested matrix
   window widens its start by the parent's `Range`. An @-pinned anchor diverges
   from the prediction and routes A.
6. **Grid commensurability for nested spines.** Inner anchors are generated
   backward from each node's `End` with no epoch alignment, so slicing shifts
   the inner grid unless the slice quantum is a multiple of every inner
   resolution. A nested spine routes A unless some quantum `m` in
   `[MinAnchorsPerSlice, N/2]` satisfies `m·Step ≡ 0 (mod lcm(inner
   resolutions))`.
7. **Scalar replication cost.** A `ScalarSubquery` whose interior carries its
   own windowed spine is too expensive to replicate `K×`, so it routes A: the
   slice benefit cannot pay for `K` copies of an expensive scalar. A purely
   row-wise scalar interior is cheap and admissible.

When every signal passes, the plan is **eligible**. The cost grid then decides
whether slicing is worthwhile:

- `F` = max `Range/Step` (or `Lookback/Step`) over windowed nodes.
- `N` = `OuterRange/Step + 1`, the outer anchor count.
- `D` = cumulative spine lookback (Σ `Range` down nested matrix windows + leaf
  `RangeLWR.Lookback`).
- `K = clamp(floor(N / MinAnchorsPerSlice), 2, min(MaxK, floor(OuterRange /
  max(D, Step))))`.

The upper clamp `floor(OuterRange / max(D, Step))` is the high-`D` floor: when
cumulative lookback is large relative to the range it drives `K` below 2 and
the plan stays on route A. Under `auto`, an eligible plan routes only when
`F ≥ MinFanout`, `N×F ≥ MinAnchorPairs`, and `K ≥ 2`. Under `sharded` the cost
thresholds drop to the floor, so every eligible plan routes at `K_min = 2`
(ineligible plans always stay on A — the force knob never breaks anything).
Under `single` the Planner classifies but never routes.

Every classification — routed or not — produces a `Decision` carrying the
reason (`routed`, `below-threshold`, `not-sliceable`, `instant`, `high-D`,
`now64`, `grid-mismatch`, `incommensurate`, `scalar-heavy`) for the shadow
header.

## Slicing geometry

Slicing decomposes the eval grid into `K` disjoint, on-grid anchor sub-grids
and re-anchors a copy-on-write view of the plan onto each: each shard SHARES the
immutable off-spine subtree verbatim and clones only the `O(spine-depth)`
re-gridded spine path. It is pure arithmetic over the anchor grid plus one
re-anchor per slice; the input plan is never mutated, and no shard ever aliases
a mutable node.

Anchors are defined backward from `End`:

```text
a_i = End - Offset - i*Step,   i ∈ [0, N),   N = OuterRange/Step + 1
```

With `m = ceil(N/K)` anchors per slice, slice `j` owns index range
`[j·m, min((j+1)·m, N))`, giving grid bounds:

```text
End_j   = End - j·m·Step             (newest anchor of the slice)
Start_j = End - (j·m + count_j - 1)·Step   (oldest anchor of the slice)
```

Because `End_j` sits on the original grid and `OuterRange_j` is a Step
multiple, the union of slice anchor sets equals the original set exactly and
the slices are pairwise disjoint — there is no compose-time reconciliation, and
every window is evaluated whole within one slice (a counter reset cannot
straddle a shard edge by construction).

`K` is capped so every slice owns at least 2 anchors (`K ≤ floor(N/2)`).

**Singleton-tail merge.** The oldest slice is the only one that can carry fewer
than `m` anchors; if it would carry fewer than 2, it folds into its newer
neighbor. An `OuterRange_j == 0` slice would flip the emitter from the matrix
template to the instant template, and keeping every shard on the identical
template keeps the parity argument trivial. A grid that collapses to a single
produced slice after the merge is not a sharded route — one shard is route A
with extra machinery — so such a plan reports below-threshold and stays on A.

Slices are returned **oldest-first** (the composition order): slice 0 is the
oldest sub-grid, the last slice ends at the original `End`.

**Per-shard scan floor.** The matrix emitters are offset-blind, so the solver
derives each slice's input lower bound itself:

```text
ScanFrom_j = Start_j - D - Offset
```

`Offset` enters sign-carrying — a negative offset widens the scan window to the
right past `End_j`, and the left floor moves with it — so a window of
`rate(m[5m] offset 1h)` is scanned fully within its slice rather than silently
emptied. `D` is the cumulative spine lookback recovered by walking the spine.

**Re-anchoring.** The plan that reaches the slicer is pinned at the full
request grid (the grid-prediction guard already verified it sits exactly
there). To re-grid each slice onto a sub-window, the slicer first builds one
spine-unpinned, copy-on-write view (`unpinSpine`): the windowed-spine bounds
(`RangeWindow` / `RangeLWR` `Start`, `End`, and the matrix `OuterRange`) are
zeroed. This is safe because signal 5 already proved every spine node sits on
the predicted grid, so the zeroed information is exactly what the re-anchor
recomputes. `unpinSpine` clones ONLY the spine-path nodes it zeroes (and their
ancestors back to the root, the `O(spine-depth)` chain) and SHARES every
immutable off-spine subtree -- with a descend-and-clone guard that, on an
off-spine subtree which itself carries a windowed node needing zeroing (e.g. a
`TopK.KExpr` computed-K plan), clones the path down to that inner node so the
shared original is never mutated. `ReanchorRange` then fills each slice's grid
in, again sharing the immutable off-spine across all `K` shards rather than deep
copying it. The original plan is never touched, and the no-mutate-after-slice
invariant holds: the shards run through emit only, which never mutates a plan
node in place, so the shared off-spine is safe to alias (enforced by the
immutability guards in `internal/solver`). A later pass that mutates a shared
off-spine node must add its own clone.

## Execution and cursor model

The `Executor` is the bounded-parallel shard dispatcher. It owns no
per-request state itself: every routed request gets a fresh cursor that holds
the gate and admission releases and dies with the request, so the no-caching
invariant holds. Execution proceeds in a fixed order, all before any cursor is
returned to the handler:

1. **Half-open pre-flight.** The Executor peeks the circuit-breaker state
   read-only. A non-`CLOSED` breaker fails fast with `ErrCircuitOpen` without
   consuming the single half-open recovery probe — a `K`-shard fan-out must
   never burn the probe on a doomed request; recovery probing is left to
   route-A traffic.
2. **Emit first.** All `K` shard SQLs are emitted before any cursor opens, so
   an emit failure aborts with zero CH work. A belt-and-braces assertion
   rejects any shard SQL string still containing a `now64(` call despite the
   static gate.
3. **Two-stage weighted admission (degrade, don't reject).** The handler has
   already charged weight 1; the Executor asks for `(P-1)` additional admission
   units. On a partial or zero grant it clamps effective parallelism to
   `1 + granted` — down to fully sequential — and proceeds. It never returns
   503 and never proceeds at full `P` on a failed top-up; a clamp is recorded
   as a metric but changes only latency, never the response.
4. **Atomic gate acquisition.** One global connection gate, sized
   `MaxOpenConns − reserve` and shared across all heads, is acquired all at
   once: `K_eff = min(K, P_eff, gate/2)` slots in a single call before any
   cursor opens, released together at `Close`. Acquiring all slots atomically
   avoids the hold-and-wait deadlock shape; the `gate/2` cap guarantees at
   least two routed requests can always make progress. A gate-acquire denial
   honours the request ctx (timeout / client cancel) and is breaker-neutral —
   no CH connection was opened.
5. **Wall-clock deadline.** A dedicated cancel cause bounds the routed request
   end-to-end (`Config.Timeout`). The distinct cause makes a solver timeout
   breaker-neutral and distinguishable from a real `DeadlineExceeded`; it maps
   to a typed 504.
6. **Per-shard execution.** Producers run under an errgroup limited to `P_eff`,
   launched **newest-slice-first** (which minimizes live-edge snapshot skew;
   composition order stays oldest-first because the channels buffer). Each
   producer derives its own progress recorder (one per ctx key — sharing would
   corrupt the rows/bytes histograms), carries the shared per-request sample
   budget, opens its cursor, and drains it into a bounded channel
   (cap 4096 samples). Producers select on the group ctx while sending, so they
   terminate promptly on cancellation.

**Composition is concatenation, not evaluation.** Each anchor belongs to
exactly one slice and every shard emits final per-`(series, anchor)` values in
the canonical shape, so the cursor computes nothing — zero arithmetic, zero
window logic, zero merge-by-key. The composing cursor drains channel 0 (the
oldest slice) to exhaustion, then channel 1, and so on. Oldest-first drain
keeps per-series timestamps nearly ascending, so the handler's insertion sort
stays roughly linear. Two guards run during the drain:

- **Per-request output cap.** Route B turns a high-cardinality query that a
  single statement would 422 into a success, and a success lands `O(rows)` in
  the gateway's matrix buffers. The cursor enforces `Config.MaxOutputRows` with
  a **distinct** typed
  422 (`OutputCapError`) whose message is deliberately not the upstream
  max-samples text — that text is a parity surface, and the output cap is a
  separate gateway-memory guard.
- **Cross-shard label re-interning.** The same series arriving from `K` shards
  is re-interned across children by a canonical label key, so it holds one
  label-map copy during the drain rather than `K`. This is per-request state,
  born and dying with the request; labels stay read-only.

The shared per-request sample budget keeps the upstream max-samples 422 parity
per request across all shards (the budget is decremented by whichever shard
crosses it).

## Failure and cancellation contract

The contract is **first-error-wins, all-or-nothing, cause-threaded**. The
errgroup runs under a cancel-cause ctx: the first *real* shard error is set as
the cancellation cause; a sibling's induced `context.Canceled` never enters the
latch. Producers, on an open-time or mid-drain error, prefer the group's cause
when one is already latched, so a racing induced cancel never masquerades as a
shard failure and a deterministic error never flips to `context.Canceled` under
a race. The composing cursor surfaces that cause through `Err()`, which the
handler maps to a wire status:

- `*MemoryLimitError` (CH code 241) → 422, breaker-neutral.
- `*TooManySamplesError` → 422, verbatim upstream message.
- `OutputCapError` → 422, distinct cerberus message.
- `ErrCircuitOpen` → 503.
- `SolverTimeoutError` (the wall-clock deadline) → 504, breaker-neutral.
- `context.Canceled` (client gone) → breaker-neutral.

Because the handler drains the composed cursor fully before writing a byte, a
shard failure is one typed error response, never a partial body — the
all-or-nothing wire contract holds for free.

**Breaker interaction.** A degraded ClickHouse can fail several concurrent
shard opens from one logical request; a request-scoped dedup latch makes only
the first real failure count and treats siblings as breaker-neutral, so one
routed request advances the shared breaker counter by at most one. The gate
acquire timeout and the solver timeout are likewise breaker-neutral: they
signal local pool sizing or a gateway-chosen deadline, not CH health.

**Lifecycle.** `Close` is idempotent (runs once): it cancels the group ctx so
every producer unblocks and exits, waits for all producers, stops the deadline
timer, closes every child cursor (releasing its connection and flushing its
progress recorder), and releases the gate slots and the admission top-up
exactly once each. A late-registered child cursor that races teardown is closed
immediately so no connection leaks. A client disconnect propagates through the
request ctx to the group ctx to every shard. Every handler entrypoint is
goleak-gated with routed queries.
