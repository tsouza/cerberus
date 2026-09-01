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
   `RangeWindow` matrix family, the `RangeWindowGridNative` ClickHouse-native
   `timeSeries*ToGrid` family, the `RangeLWR` bare-selector
   last-with-respect-to family, and the `RangeBucketFanout` array-aggregate
   fan-out behind the classic-histogram families. Every other grid carrier —
   `RangeWindowStaleResample`, `StepGrid`, `AbsentOverTime` —
   carries its own eval grid that re-anchoring clones verbatim, so a plan whose
   spine bound-carrier is one of those fails closed to route A (every shard
   would otherwise emit stale bounds).

   `UnionAll` carries no grid of its own and re-anchors every arm onto the same
   sub-grid, so a mixed spine — `UnionAll{RangeWindowGridNative, RangeWindow}`, the
   shape a cumulative/delta temporality split emits — slices as a unit. All arms
   move together or none does: the arms are concatenated positionally, so a
   shard that re-gridded one and shared another verbatim would compose the
   shared arm's full-grid answer `K` times over.

   The four re-anchorable kinds are exactly the kinds the slicer's `unpinSpineCOW`
   zeroes and exactly the kinds `carrierGeometryOf` marks re-anchorable. A kind
   that re-anchoring learns but `unpinSpineCOW` does not stays pinned at the full
   request grid, so every slice aborts with a grid mismatch and the plan falls
   back to route A while still classifying as routable.

   Measurement is separate from admissibility: the signal walk records the cost
   grid of **every** carrier kind, routable or not, because the corpus needs to
   know how expensive the refused plans were. Being seen never makes a carrier
   sliceable — that is decided solely by the marker in signal 1 and by whether
   re-anchoring knows the node's grid.
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
6. **Grid commensurability for end-phased nested spines.** A nested grid whose
   anchors are generated backward from the node's own `End` — a nested
   `RangeLWR`, or a nested matrix `RangeWindow` that is not `StepAlign`'d —
   shifts phase when the slice boundary shifts `End`, so it routes A unless the
   selected shard geometry's emitted quantum `m = ceil(N/K)` satisfies
   `m·Step ≡ 0 (mod lcm(end-phased resolutions))`. A `StepAlign`'d nested spine
   (the PromQL subquery inner-sample grid, snapped to absolute-epoch multiples
   of its own `Step`) is phase-0 for **any** `End`, so its per-shard anchor set
   is always a subset of the unsliced one and it imposes no quantum constraint
   at all — the ordinary `expr[range:res]` subquery keeps routing.
7. **Scalar replication cost.** A `ScalarSubquery` / `InSubquery` whose
   interior carries a windowed node is expensive to replicate `K×` UNLESS
   that node is anchor-compatible with the grid predicted at the point it is
   embedded — the same `(Start, End[, OuterRange])` equality signal 5 already
   requires of the main spine, applied to a node reached through an Expr slot
   instead of a Node child, PLUS an exact `Step` match against the cadence
   predicted there (`scalarInteriorAnchorCompatible`, called from
   `Planner.checkScalarHeavy`). The `Step` half is this check's own addition
   — the main spine's own guard deliberately allows a nested subquery
   resolution to differ from its parent's `Step`, but nothing here re-derives
   an interior node's fan-out the way the main spine's `recordGridCarrier`
   does, so a same-bounds-different-cadence interior is not provably bounded
   and stays heavy. Route B
   never re-anchors an Expr-embedded interior — `chplan.ReanchorRange` and the
   slicer's `UnpinSpine` both share it verbatim into every shard, unmodified —
   so an anchor-compatible node still evaluates the exact span it always
   evaluated under route A, identically in every shard: admitting it changes
   cost, never correctness. What the equality test buys is telling that
   bounded shape (one value per OUTER anchor, e.g. the per-step scalar
   argument `clamp_max(v, scalar(bound))` binds since #1455/#1886) apart from
   a genuinely independent, unboundedly wide scan — an `@`-pinned interior, or
   one whose span has nothing to do with the outer grid — which really would
   multiply `K×` into real extra cost and stays heavy. A `RangeBucketFanout`
   is never admitted here regardless of its bounds — unlike the main spine
   (signal 2, where it IS now routable), no equality/reanchor argument has
   been built for the Expr-embedded, never-reanchored replication case this
   check governs, so it stays conservatively heavy; a purely row-wise scalar
   interior (no windowed node at all) was always cheap and stays admissible.

When every signal passes, the plan is **eligible**. The cost grid then decides
whether slicing is worthwhile:

- `F` = max per-sample intermediate-row count over windowed nodes: `Range/Step`
  (or `Lookback/Step`) for a carrier that materialises the `(sample, anchor)`
  matrix, and `1` for the native `timeSeries*ToGrid` family, which reads each
  sample exactly once into a per-series grid array whatever its window width is.
  `F` is the memory proxy the auto gate reads, so reporting `Range/Step` for a
  native carrier would shard a flat-memory single-pass statement `K` ways and
  pay `K` scans for a matrix that is never built. A native carrier is still
  fully sliceable — it simply does not clear `MinFanout` on its own, so it
  routes when a fan-out sibling arm prices the plan or when the threshold-free
  `Eligible` seam (the failure-driven route memo, which fires on a real route-A
  resource failure) asks for it.
- `N` = `OuterRange/Step + 1`, the outer anchor count.
- `D` = cumulative spine lookback (Σ `Range` down nested matrix windows + leaf
  `RangeLWR.Lookback`). Unlike `F`, `D` is unaffected by which emitter reads the
  window: it measures per-slice SCAN redundancy, and a native carrier's shard
  widens its input by `Offset + Range` exactly as a fan-out carrier's does. The
  one exception is a `RangeWindow` with `DownsampleTier` set (issue #2751):
  `DownsampleTierInput` reads a table bounded to one row per
  `schema.DownsampleTierBucket` per series regardless of `Range`, a different
  and sparser SOURCE than the raw scan `D`'s invariance assumes, so that
  carrier's `D` contribution is capped at the bucket width instead of growing
  with `Range` (issue #2859). `F` is untouched by the same case — the tier
  emitter still arrayJoins each tier row across its covering anchors exactly
  like the raw fan-out, so the materialised-rows-per-row ratio is still
  `Range/Step`.
- `K = clamp(floor(N / MinAnchorsPerSlice), 2, min(MaxK, floor(OuterRange /
  max(D, Step))))`.

The upper clamp `floor(OuterRange / max(D, Step))` is the high-`D` floor: when
cumulative lookback is large relative to the range it drives `K` below 2 and
the plan stays on route A. Under `auto`, an eligible plan routes only when
`F ≥ MinFanout`, `N×F ≥ MinAnchorPairs`, and `K ≥ 2`. Under `sharded` the cost
thresholds drop to the floor, so every eligible plan routes at `K_min = 2`
(ineligible plans always stay on A — the force knob never breaks anything).
Under `single` the Planner classifies but never routes, and records
`routing-disabled` — not `below-threshold`, which would claim a threshold was
consulted when none was.

`F` is a cost proxy, and for one class of carrier its sign is inverted, so
`auto` carries one gate the thresholds cannot express. A carrier answers
`chplan.GridCarrier.AnchorGridDivides` to say whether slicing the anchor grid
`K` ways actually divides its peak by `K`. Every kind answers yes except the
classic/native bucket fan-out (`RangeBucketFanout`), whose peak intermediate is
`Θ(rows × Lookback/Step)` — **constant in `N`** — so each shard rebuilds the
whole per-`(series, anchor)` bucket-ladder fold over its own window, and
adjacent shards re-read the `Lookback` of history every anchor needs. For that
carrier `F` is a redundancy multiplier rather than a divisor: the thresholds
route hardest exactly where routing hurts most. An `auto` plan above every
threshold whose spine carries such a carrier therefore stays on route A and
records `anchor-grid-indivisible`. The gate is consulted **last**, after the
thresholds, so a plan that was already below threshold keeps that reason and the
route-A analyzer's population does not shift underneath it.

The gate is a PREDICTION, not a correctness refusal, and it lives in `Plan`'s
`auto` branch alone. `Eligible` — the re-derivation the failure-driven route
memo calls after a real route-A resource failure — ignores it, so measured
evidence still overrides the model and such a plan can escape to route B; and
`sharded` still routes it, so the force knob keeps its meaning.

Every classification — routed or not — produces a `Decision` carrying the
reason (`routed`, `below-threshold`, `anchor-grid-indivisible`, `not-sliceable`,
`instant`, `instant-join`, `high-D`, `now64`, `grid-mismatch`, `incommensurate`,
`scalar-heavy`, `routing-disabled`, `extraction-failed`) for the shadow header,
alongside the plan's cost grid (`N`, `F`, `D`, `OuterRange`, `Step`).

The grid is populated on **every** Decision, including the refusals. The signal
walk that derives it makes no routing decision and mutates nothing, so it runs
before the gates rather than after them. This is what makes the calibration
corpus replayable: a refusal recorded with a zero grid says "we declined"
without saying what we declined, which is indistinguishable from a plan that
genuinely had no geometry. It also makes one silent failure mode legible — a
range query whose grid carrier the classifier fails to find looks instant, and
now records a non-zero `N`/`F`/`OuterRange` next to a zero `Step`, which a
genuine instant query cannot have. Both halves of that signature matter:
`reason=instant` is also recorded for a range request carrying an unpinned or
instant-shaped window, and that one has a real grid — it is the zero `Step`
beside a populated grid that identifies a missed carrier.

The complementary case — a range request whose plan yields no carrier the walk
can measure at all — carries its own token, `extraction-failed`. It is the one
refusal whose cost grid is legitimately all zeros, and it says so in the row
rather than presenting as a cheap plan. That distinction is load-bearing for
calibration: a threshold fitted over the refused population must exclude rows
that were never measured, and no aggregate over `N`/`F`/`D` can tell an
unmeasured plan from a trivial one unless the reason does.

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
spine-unpinned, copy-on-write view (`unpinSpineCOW`): the windowed-spine bounds
(`RangeWindow` / `RangeWindowGridNative` / `RangeLWR` `Start`, `End`, and the matrix
`OuterRange`) are zeroed. This is safe because signal 5 already proved every spine node sits on
the predicted grid, so the zeroed information is exactly what the re-anchor
recomputes. `unpinSpineCOW` clones ONLY the spine-path nodes it zeroes (and their
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
   static gate. The dispatch context carries the same plan-shape-gated
   ClickHouse settings a route-A statement would get — notably
   `allow_experimental_time_series_aggregate_functions=1` when a shard plan
   carries a `timeSeries*ToGrid` node, without which every shard answers code
   63 rather than degrading.
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
   budget, opens its cursor **on its own cancellable child of the group ctx**,
   and drains it into a bounded channel (cap 4096 samples). Each producer owns
   its cursor end to end: it opens it, drains it, and tears it down. While
   sending it selects on both the stop signal and the group ctx, so it
   terminates promptly on either.

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

**Two teardown signals.** Stopping the stream and aborting the queries are
distinct signals, and conflating them costs connections. clickhouse-go hands a
pooled connection back only when its query ends on a live context; cancel first
and the socket is destroyed instead (see
[`operations.md`](operations.md#connection-teardown-contract)). So the composed
cursor carries a **stop** channel alongside the group ctx:

- **stop** — stop streaming. A producer blocked on a send unblocks here and
  tears its own cursor down while its query ctx is still LIVE, so `K`
  connections go back to the pool. This is the routine path, and it is what the
  per-request output cap trips: the cap is a decision about how many rows to
  hand OUT, never a reason to abort the ClickHouse queries dirtily.
- **cancel** — abort. It belongs to a real failure (a sibling's error, the
  wall-clock timeout, the client walking away) and is also the bounded fallback
  for the one shape stop cannot reach: a producer parked inside `cur.Next()` on
  a stalled shard, which observes stop only at its next send.

**Lifecycle.** `Close` is idempotent (runs once): it closes the stop channel,
waits up to a teardown budget for every producer to drain and release its
connection, cancels the group ctx (unconditionally, so nothing derived from it
outlives the request, and past the budget so a parked producer still
terminates), stops the deadline timer, and releases the gate slots and the
admission top-up exactly once each. Because each producer owns its own cursor,
there is no registry of children to race against teardown. `Close` returns the
first non-nil child teardown error. A client disconnect propagates through the
request ctx to the group ctx to every shard. Every handler entrypoint is
goleak-gated with routed queries.

## Failure-driven route memo

`Planner` classifies a plan as route-A- or route-B-worthy from static signals
alone (`N`, `F`, `D`) — it has no visibility into data-dependent cost:
cardinality skew, TTL-driven part counts, concurrent load. A plan that looks
route-A-cheap by those signals can still exhaust ClickHouse's memory cap when
it actually runs. The failure-driven route memo (`internal/routememo`) turns
that failure into signal instead of a dead end: when a route-A dispatch fails
with a ClickHouse resource-exhaustion error, cerberus retries it once on route
B, and if B succeeds, remembers the verdict against a literal-free fingerprint
of the plan's cost shape — so a later, cost-equivalent request routes to B
directly instead of paying the same route-A failure again.

The memo is process-local (one instance per process, no cross-pod state, no
ClickHouse-side bookkeeping) and it can only ever change WHICH route a request
takes, never how either route computes its answer: route B's own
output-equivalence proof (the slice-invariance markers and eligibility signals
above) is unconditional on the memo — a stale or wrong verdict costs an extra
retry or a request that stays on route A when B would have helped, and can
never change a result. Wiring lives in
`internal/engine/route_memo_wiring.go`; the outcome classifier is
`internal/engine/route_outcome.go`; the memo itself is `internal/routememo`.

"Resource exhaustion" is two things, not one. A ClickHouse memory-limit abort
is the obvious half. The other is a client cancellation that arrives after the
dispatch has already been running for `costlyCancellationFloor` (5s): the work
was committed and the caller gave up waiting for it, which is evidence about
this route's cost in exactly the way a memory abort is. A cancellation BELOW
that floor is a caller who navigated away and stays `OutcomeNoEvidence`. A
costly cancellation is **recorded, never retried** — the caller is already gone,
so a retry dispatch would run for nobody; it teaches the next request instead.
That is why `retryOnRouteAResourceFailure` observes before its dead-context
return rather than after it.

### Key: a literal-free cost-shape fingerprint

`routememo.Key` (`internal/routememo/key.go`) is a `comparable` struct built
only from closed, plan-shape vocabulary: the plan root's Go type name, the
per-level range/aggregate function names walked in tree pre-order, a bitmask
of matcher operator KINDS (`=`, `!=`, `=~`, `!~` — operator kinds, never label
names or values), boolean flags for join/union/limit/native/resample shape,
and bucketed (log2, clamped) exponents for matcher count, anchor count,
fanout, and step. Two requests share a Key when they are judged
cost-equivalent enough to share a routing verdict — a dashboard panel
re-querying the same PromQL shape on a rolling window keeps hitting the same
Key across refreshes even though its anchor count drifts by one as time moves,
because bucketing (not the raw value) is the equivalence relation. Nothing in
Key can identify a metric name, a label value, or a timestamp: it carries no
more information than `planShapeID` (`internal/engine/plan_shape_id.go`)
already exposes at a coarser grain, and no more than ClickHouse's own
normalized `query_log` text.

### Verdict lifecycle

`Memo.Lookup` returns one of three `LookupState` values:

- **`Unknown`** — no verdict recorded (or it aged out, or route A hasn't
  failed on this Key enough times to be probe-eligible yet). The caller takes
  the normal route-A path; the outcome is still `Observe`d so corroboration
  bookkeeping progresses.
- **`PreferB`** — route A has failed on this Key at least
  `minCorroboratingFailures` (2) consecutive times with no intervening
  success, and a route-B probe subsequently succeeded. The caller MAY memo-hit
  route B directly, subject to every other gate re-checked at dispatch time
  (eligibility, freshness, breaker, admission). A `PreferB` past its
  re-validation midpoint is reported with `stale=true`, and the caller must
  NOT memo-hit it — it routes through plain route A instead, "as if the Key
  were unknown", so the verdict can be honestly re-confirmed by real traffic.
- **`BothFail`** — route B was tried for this Key and itself failed with a
  resource failure. The caller stays on route A; no further probing is
  attempted until the entry ages out at the memo's TTL.

Requiring `minCorroboratingFailures = 2` consecutive failures (not one) exists
so a single transient rejection never mints a verdict on its own: probing
route B is itself a real dispatch, and the memo's premise only holds if
repeated failure is treated as real signal rather than noise from one bad
request.

### Cluster-wide pressure damper

A `pressureTracker` (`internal/routememo/pressure.go`) records every resource
failure's Key and timestamp, independent of which route produced it.
`Memo.UnderPressure()` reports true once more than `pressureFailureThreshold`
(`minCorroboratingFailures + 1` = 3) DISTINCT keys have shown a resource
failure within a pressure window. While under pressure, the memo admits no
probe and no memo-hit dispatch — both fall back to route A — and `Observe`
writes no verdict state at all; only the pressure tracker itself keeps
recording, so the window decays naturally once traffic quiets. This exists
because a correlated, cluster-wide event (e.g. every query slowing down at
once) produces resource failures on many UNRELATED keys simultaneously;
without the damper, one shared external cause would look like independent
evidence against every one of those keys and stampede all of them into
probing route B at once — exactly the wrong response to a cause that has
nothing to do with any individual key's cost shape. `pressureFailureThreshold`
is defined relative to `minCorroboratingFailures` specifically so a single hot
key's own corroboration count can never trip the damper by itself — tripping
it takes failures spread across more than one key.

The pressure window is the solver's own effective wall-clock deadline
(`Solver.EffectiveTimeout()`, wired in `cmd/cerberus/main.go`'s
`buildRouteMemo`) — the horizon a single fan-out's resource pressure
plausibly stays live server-side — rather than a second, independently
configured duration that could drift out of step with it.

### TTL and re-validation

Every verdict is stamped with `createdAt` at the transition that created or
last confirmed it, and expires unconditionally once
`now - createdAt >= entryTTL` (default 30 minutes, `memoEntryTTL`) — `Lookup`
and every internal accessor evict an aged-out entry back to `Unknown` on
read. A live `PreferB` verdict is additionally re-validated at its TTL
midpoint (`entryTTL / reValidationFraction`, default fraction 2, i.e. 15
minutes): past that point `Lookup` reports it `stale=true`, declining the
memo-hit and routing the request through plain route A instead, so the
underlying premise ("route A still fails on this shape") gets honestly
re-confirmed by real traffic rather than trusted forever.

**The ordering bug `ObserveRouteAFailureAndMaybeBeginProbe` fixes.** The
re-validation path depends on a route-A dispatch actually failing again while
the entry looks stale. A naive two-call sequence —
`Observe(k, RouteA, OutcomeResourceFailure)` (which refreshes `createdAt`,
un-staling the entry) followed by `BeginProbe(k)` (which admits only an
`Unknown` entry) — loses exactly the request that should trigger the rescue:
by the time `BeginProbe` looks, `Observe`'s own side effect has already made
the entry look fresh again, so `BeginProbe`'s Unknown-only gate refuses it.
The caller's actual HTTP request is then stuck on the very failure the memo
already knows how to avoid, with no rescue, even though the memo has been
confidently routing this shape to B for the entry's entire life.
`ObserveRouteAFailureAndMaybeBeginProbe` closes the gap by combining
record-and-decide into one critical section: it captures whether the entry
WAS stale before the state transition, then decides admission on that
pre-transition snapshot rather than the post-transition one. Callers on the
route-A failure path (`retryOnRouteAResourceFailure`) MUST use this one atomic
method — never the two separate calls — for a route-A resource-exhaustion
failure.

### Admission: one process-wide dispatch-token semaphore

`AdmitDispatch` is a single, non-blocking, process-wide semaphore of size
`maxConcurrentRoutedDispatches` (4) covering EVERY non-baseline dispatch the
memo can trigger — a first probe, a stale-verdict re-validation rescue, and a
plain memo-hit all draw from the same token pool. A denied admission always
falls back to route A; the memo never queues or blocks a request waiting for
a token. This bounds route memo's total worst-case added ClickHouse-side
concurrency to a small, fixed constant regardless of how many distinct keys
are simultaneously probe-eligible: a burst of concurrent failures on the same
hot key (a client retry storm) or spread across many distinct keys can never
mint more admitted dispatches than the budget allows, because the
read-decide-admit sequence holds the memo's lock for its entire span (pinned
in `internal/routememo/concurrency_bound_test.go`).

### Mandatory per-shard memory apportionment

Route A's single statement runs under the client's configured
`max_memory_usage` cap. A naive `K`-shard fan-out running every shard under
that SAME full cap would let total server-side memory exposure reach up to
`K` times route A's — exactly backwards from the mechanism's premise that
sharding reduces resource use. `internal/solver/executor.go`'s `Execute`
therefore divides the cap by the shard count it actually dispatches
(`perShardMemoryBytes = cap / kEff`) and stamps that value onto every shard's
`max_memory_usage` query setting. This is unconditional — there is no config
knob to disable it — because closing an accidental resource-amplification
hole is a correctness property of routing itself, not a togglable safety
feature; `kEff` is already bounded above by the structural `MaxK` clamp, so
the minimum possible per-shard cap is `cap / MaxK`, a floor the mechanism
already guarantees. When no cap is configured
(`Client.MaxQueryMemoryBytes() == 0`, meaning route A itself runs uncapped),
apportionment is skipped entirely — stamping a per-shard cap in that case
would make routed traffic MORE restrictive than route A, not merely no worse.
Verified against a real ClickHouse instance in
`internal/solver/executor_realch_integration_test.go`
(`TestExecutor_PerShardMaxMemoryUsage_RealClickHouse`).

### Live-edge freshness exception

Route B's disjoint-anchor equivalence proof holds only once every shard's
window has closed with respect to live ClickHouse table state — a shard
reading a strictly newer snapshot than an earlier shard is real skew the
proof does not cover. Because the newest grid anchor is, by construction,
still inside the write window that produced it, anchors strictly older than
one step have crossed the write frontier for any step-aligned ingestion
pattern. `freshEnoughForRouteMemo` (`internal/engine/route_memo_wiring.go`)
enforces this directly: a request whose `End` has not aged past
`liveEdgeFreshnessMarginSteps` (1) step is never routed by the memo mechanism
at all — probe, retry, or memo-hit alike — regardless of what the memo has
recorded for its Key. This is a grid-relative margin, not a
wall-clock-duration guess, and it applies independent of, and in addition to,
the Planner's own eligibility signals. Pinned in
`internal/solver/avb_chdb_lane_test.go`'s `TestSolver_AvsB_ChDB_LiveEdgeBoundary`.

### Per-rung predictive admission refinement

The classic-histogram native ladder (`chplan.RangeBucketGridNative`) reports a
flat `F = 1` — its per-`(series, le rung)` amplification is unmeasurable at
plan time (its own doc, `internal/solver/planner.go`'s
`minAnchorsForPerRungShard`) — so `auto`'s per-rung branch admits it on the
anchor axis alone, bypassing both `MinFanout` and `MinAnchorPairs`, whenever
`N >= minAnchorsForPerRungShard`. Anchor count is pure grid geometry: it says
nothing about how much data backs the grid, and issue #2709 found a real case
where that bites — a 24h/1m dashboard panel over a low-cardinality metric
clears the anchor floor by 10x+ and predictively shards a nearly-empty table,
paying `K` concurrent ClickHouse queries' contention for no benefit. #2709
also shows geometry alone cannot fix this: a genuine incident the bypass
exists to catch (#2677) has FEWER anchors than that false positive, so no
anchor-only threshold can separate the two populations.

`internal/engine/per_rung_admission.go`'s `PerRungAdmissionLearner` closes the
gap with evidence instead: it watches what a `Decision.PerRungPredictive`
route's cursor ACTUALLY, CLEANLY drains (a cancelled/errored drain is never
recorded — see the file's own doc for why), and once a plan shape has
produced a small enough composed result on `perRungEvidenceMinObservations`
consecutive clean dispatches, it downgrades that shape's FUTURE per-rung
routes back to route A until a fresh, larger drain is observed or the
evidence ages past `perRungEvidenceTTL`. It never refuses a plan the Planner
judged eligible and never touches `ModeSharded` or the failure-driven memo
above — it only ever downgrades an ALREADY-`PerRungPredictive` route once
evidence says the shape does not need it, falling back to today's
anchor-only default with no evidence. `Engine.PerRungAdmission` is nil
(feature off, byte-unchanged) unless wired from `cmd/cerberus`
(`buildPerRungAdmission`), gated by the same `CERBERUS_SOLVER_ADAPTIVE_ENABLED`
flag as the route memo above.

### Configuration

On by default. It is the only half of the routing decision that reacts to what
actually happened: with it off, `auto`'s thresholds are a prediction made once
from plan shape alone, a wrong prediction stays wrong forever, and a route-A
resource failure is a 5xx rather than a slower answer. The default-off
convention cerberus applies to a new runtime-behavior-changing feature
(`CERBERUS_CH_OPT_CORPUS_ENABLED`, `CERBERUS_EXPERIMENTAL_TS_GRID_RANGE`)
covers behaviour an operator might not want; this one only ever turns a FAILURE
into an answer, and can never change a result.

| Variable                                           | Type     | Default               | Description                                                                                                                                                                                                                                                          |
| -------------------------------------------------- | -------- | --------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `CERBERUS_SOLVER_ADAPTIVE_ENABLED`                 | bool     | `true`                | Wires `internal/routememo` onto the engine. Set `false` and the engine's `RouteMemo` field stays nil and every function in `route_memo_wiring.go` no-ops through its own `routeMemoActive()` guard — dispatch stays byte-unchanged.                                  |
| `CERBERUS_SOLVER_ROUTE_MEMO_ENABLED`               | bool     | (unset)               | Soft-deprecated spelling of the row above. It still applies; `CERBERUS_SOLVER_ADAPTIVE_ENABLED` wins when both are set, and setting it logs a deprecation notice at startup (`solver.DeprecatedEnvWarnings`) so an explicit opt-out survives the rename.             |
| `CERBERUS_SOLVER_ROUTE_MEMO_ENTRY_TTL`             | duration | package default (30m) | Overrides how long a recorded verdict is trusted before it ages out. Zero/unset means "use the routememo package's own default", not "TTL zero" — `SetEntryTTL` treats a non-positive value as a no-op so a misconfigured value can never silently disable the memo. |
| `CERBERUS_SOLVER_ROUTE_MEMO_REVALIDATION_FRACTION` | int      | package default (2)   | Overrides the divisor that places re-validation at the TTL midpoint. Same non-positive-is-no-op contract as the TTL knob.                                                                                                                                            |

`cmd/cerberus/main.go`'s `buildRouteMemo` is the only constructor: it returns
nil (feature off) unless `solver.Config.AdaptiveEnabled` is set and a solver is
configured, and it always applies both setters unconditionally after
construction — safe because both are no-ops on the Go zero value.

### Metrics

Four instruments (`internal/telemetry/metrics.go`):

- **`cerberus_route_memo_hit_skipped_total`** (counter) — one increment per
  real decline branch in `route_memo_wiring.go`, labeled `reason` with one
  of: `not-eligible`, `not-fresh`, `no-preferb`, `stale`, `breaker-open`,
  `no-dispatch-token`, `under-pressure`, `probe-not-admitted`.
- **`cerberus_route_memo_pressure_active`** (up/down counter) — mirrors
  `Memo.UnderPressure()`'s current level as a transition delta (+1 entering
  pressure, −1 leaving), so it always reads the current active/inactive
  state without double-counting repeated same-state decisions.
- **`cerberus_routed_dispatch_inflight`** (up/down counter) — memo-triggered
  route-B dispatches currently in flight (a memo-hit, or a probe/retry that
  was admitted a dispatch token), distinct from the general
  `cerberus_query_inflight`, which counts every engine query regardless of
  route.
- **`cerberus_route_ab_success_total`** (counter) — every
  `classifyRouteOutcome` resolution to `OutcomeSuccess` for a memo-tracked
  dispatch, labeled `cerberus.route_choice` = `"a"` / `"b"`.

### Relationship to the retired route-threshold autotune

Route memo is cerberus's current answer to "how does the solver's routing
adapt without an operator hand-tuning it" — but it is a different mechanism
from the self-driving threshold-fit loop it replaces, not a variant of it.
The retired autotune loop adjusted the same static cost-threshold proxy
(`MinFanout` / `MinAnchorPairs`) `Planner` already used, fitting its value
against a corpus of past decisions — it could tune the number the proxy
compares against, but not fix a proxy measuring the wrong cost dimension for
a given shape. Route memo does not touch those thresholds at all: it is a
per-key, per-outcome evidence ledger sitting entirely downstream of the
Planner's classification, keyed on a request's own cost shape rather than
fit against a corpus globally. A plan the Planner misclassifies as
route-A-cheap can still end up on route B for that specific shape once it
has actually failed enough times to prove the classification wrong — no
threshold anywhere has to change for that to happen.

## Advisory EXPLAIN ESTIMATE (issue #2787)

Every mechanism above — the K clamp, the failure-driven route memo, per-rung
admission — reasons from either pure PLAN geometry (`N`, `F`, `D`) or
FAILURE-DRIVEN evidence (a real route-A resource exhaustion). Neither one
ever asks ClickHouse what its own index analysis already knows about the
window before routing decides anything. Issue #2787 closes that gap with one
more input, strictly advisory: `EXPLAIN ESTIMATE`, ClickHouse's no-execution
scan estimator (parts / rows / marks after index analysis, available since
21.9 — well below cerberus's own 24.8 floor).

**Granule-resolution upper bound, not selectivity.** `EXPLAIN ESTIMATE`
reports how many marks the index analysis could not prune, times the table's
granule size (typically 8192) — an upper bound on matching rows, never an
exact count and never selectivity-aware. Every consumer below treats it as a
bias input to a COST decision, never as a correctness gate: it can only ever
make the solver more conservative (skip a route the pure-geometry thresholds
would have taken) or less conservative WITHIN the same cost-decision
machinery (raise the `K` ceiling) — it never changes which rows a query
returns, and the pre-#2787 pure-geometry path remains the permanent,
fully-supported fallback (a nil estimate — the default, until the chopt
`explain_estimate` feature is explicitly enabled — reproduces it exactly).

### What it feeds

1. **K clamping (`internal/solver/planner.go`).** `classify()`'s cost-grid
   section gains two advisory checks once `RequestMeta.Estimate` is non-nil:
   - **Near-empty skip.** A window whose total `Estimate.Rows` is at or below
     `Config.EstimateNearEmptyRowFloor` is refused outright
     (`ReasonEstimateNearEmpty`), independent of what the pure geometry
     thresholds would have decided — this is issue #2709's own case: a
     wide-window panel over a table with almost nothing in it clears every
     geometric threshold and pays `K` concurrent round trips for negligible
     work, and only DATA (never geometry) can see that in advance.
   - **K-ceiling raise.** The structural `MaxK` backstop is raised to
     `Config.MaxKWithEstimate` exactly when the estimate shows enough data to
     back it: each shard above `MaxK` must be justified by
     `Config.EstimateMinRowsPerAdditionalShard` rows of the granule-resolution
     upper bound. This is issue #2685's own case reopened safely: a
     production-cardinality panel whose own grid asks for more shards than the
     structural backstop allows, now granted the extra sharding once real data
     volume — not geometry alone — supports it.
2. **Per-rung admission priors (`internal/engine/per_rung_admission.go`'s
   `PerRungAdmissionLearner.SeedPriorFromEstimate`).** A near-empty advisory
   estimate seeds ONE observation into the SAME rolling evidence the learner
   already accumulates from real clean drains, so a shape can decline the
   per-rung bypass on its first request instead of waiting for
   `perRungEvidenceMinObservations` real dispatches. Only this learner is
   seeded this way — see "Why the failure-driven route memo is NOT seeded"
   below.

### Cost bound (the constraint this issue states as VERIFIED)

`per_rung_admission.go`'s own doc already rejected "a new live round-trip on
every per-rung request" once. `internal/engine/explain_estimate_wiring.go`'s
`ScanEstimateAdvisor` exists specifically not to reintroduce that, through
three independent narrowings — see that file's own doc for the exact
mechanics, and `TestScanEstimateAdvisor_SkipsSecondProbeForSameShape` for the
pinned proof that a second identical-shape request never re-issues the round
trip:

1. **ModeAuto only, and only a plan that reached the cost-grid section** of a
   baseline (no-estimate) classification — a structurally-refused plan
   (`now64`, an instant query, ...) cannot be affected by an estimate at all,
   so probing one is pure waste.
2. **Cached per plan shape** (the SAME literal-free `routememo.Key`
   fingerprint the route memo and the per-rung learner already key on), TTL-
   and capacity-bounded exactly like `PerRungAdmissionLearner`'s own cache.
3. **Skipped entirely once the route memo OR the per-rung admission learner
   already holds a verdict** for the shape — either mechanism having already
   resolved the shape makes an additional advisory signal worthless.

A probe failure (breaker-open, transport error, emission error) is treated as
"no estimate" and never surfaces as a query failure: advisory-only, fail-open
by construction.

### Why the failure-driven route memo is NOT seeded

The issue's proposal names both the route memo and per-rung admission as
seeding targets. Only per-rung admission is seeded. The route memo's
`minCorroboratingFailures = 2` and its cluster-wide pressure damper (both
documented above) exist specifically to reject exactly the kind of single-
shot, non-corroborated evidence a granule-resolution upper bound is: seeding
`PreferB` from an estimate would let one advisory signal — not selectivity-
aware, never re-confirmed by a real dispatch — route real production traffic
onto route B without the two independent real resource failures the whole
mechanism is built to require before trusting anything. Per-rung admission's
own contract is the opposite shape and is safe to seed: it can only ever
DOWNGRADE an already-`PerRungPredictive` route back to route A
(`refinePerRungAdmission`'s own doc), so a wrong prior costs one shape one
unnecessary shard for `perRungEvidenceTTL` — the same always-safe direction a
wrong REAL observation already costs there. This is a deliberate scope
narrowing from the issue's own proposal, not an oversight.

### Calibration

Measured against `test/perf/smoke/testdata/samples/svc_http_requests_total.parquet`
(real, scrubbed production Sum-metric sample, ~18.6M rows over a real 14-day
span — see that directory's own `README.md`), loaded into a real MergeTree
table (`ORDER BY (MetricName, TimeUnix)`, matching the production sorting
key) and probed with `EXPLAIN ESTIMATE` directly:

| window                                               | rows    | marks | parts |
| ---------------------------------------------------- | ------- | ----- | ----- |
| 1h over the sample's single densest real hour        | 598,016 | 73    | 1     |
| 6h spanning that hour (real data, mixed density)     | 895,858 | 110   | 1     |
| 6h a year outside the sample's captured span (empty) | 0       | 0     | 0     |

This confirms both advisory checks against real data rather than assumption:
a genuinely empty window reports EXACTLY zero — `EstimateNearEmptyRowFloor`'s
default of 1,000 sits comfortably above ClickHouse's own noise floor for a
truly empty scan and comfortably below any window carrying real samples — and
a real, single-metric dense window already reports rows in the hundreds of
thousands, an order of magnitude with room to justify multiple shards above
`MaxK` at `EstimateMinRowsPerAdditionalShard`'s default of 50,000 (895,858
rows / 50,000 = 17 additional shards' worth of headroom, the same order of
magnitude as the #2685 incident's own K=22 grid-derived ask). `defaultMaxKWithEstimate`
(32) reinstates the exact ceiling #2685 raised and #2709 reverted, now gated
on this real-data justification instead of geometry alone.

This measurement validates the MECHANISM (a genuinely empty window and a
genuinely dense window are cleanly distinguishable by `EXPLAIN ESTIMATE`, at
real production row counts) rather than reproducing the exact #2685/#2709
production incidents byte-for-byte, which this repository does not have
access to. The router corpus (`internal/optcorpus`, "Routing-decision
calibration corpus" below) is the intended mechanism for refining these
constants further against real production traffic once the feature is opted
into a live deployment — exactly the same measurement-only feedback loop that
governs `MinFanout` / `MinAnchorPairs` today.

### Configuration

| Variable                                                 | Type  | Default | Description                                                                                                              |
| -------------------------------------------------------- | ----- | ------- | ------------------------------------------------------------------------------------------------------------------------ |
| `CERBERUS_CH_OPTIMIZATIONS=explain_estimate`             | flag  | off     | Enables the chopt `explain_estimate` feature (opt-in only, `AutoSelect=false` — see `docs/clickhouse-optimizations.md`). |
| `CERBERUS_SHARD_ESTIMATE_NEAR_EMPTY_ROW_FLOOR`           | int64 | 1,000   | `Config.EstimateNearEmptyRowFloor`.                                                                                      |
| `CERBERUS_SHARD_MAX_K_WITH_ESTIMATE`                     | int   | 32      | `Config.MaxKWithEstimate`. Must be `>= CERBERUS_SHARD_MAX_K`.                                                            |
| `CERBERUS_SHARD_ESTIMATE_MIN_ROWS_PER_ADDITIONAL_SHARD`  | int64 | 50,000  | `Config.EstimateMinRowsPerAdditionalShard`.                                                                              |

## Bounded cardinality pre-probe (issue #2788)

`EXPLAIN ESTIMATE` (above) answers "how many marks did the index analysis
fail to prune" — a granule-resolution SCAN-side upper bound. It has no
comparable answer for a different, equally real question: how many DISTINCT
SERIES actually back a window. Issue #2788 closes that gap with a second,
independent advisory input — a bounded, REAL aggregate (`count()`,
`uniqUpTo(100)(...)`) run over the plan's already-pruned scan window, gated
and cached by `internal/engine/cardinality_probe_wiring.go`'s
`CardinalityProbeAdvisor`, exactly the way `ScanEstimateAdvisor` gates and
caches `EXPLAIN ESTIMATE`.

**Real execution, not estimation — and that is the whole point.** Unlike
`EXPLAIN ESTIMATE`, this probe DOES read data: `count()` is an exact row
count, and `uniqUpTo(100)(...)` is an exact distinct-series count up to 100
(ClickHouse's own hard cap on `uniqUpTo`'s parameter — issue #2788 verified
`uniqUpTo(K_max*16)` throws rather than saturating past it — see
`chplan.FnUniqUpTo`'s own doc for the "reports 101" saturation contract).
Both advisors are wired as two SEPARATE `*Engine` fields
(`ScanEstimateAdvisor`, `CardinalityProbeAdvisor`) whose results merge into
one `solver.ScanEstimate` at the `Engine.classify` call site: the
cardinality probe's real `count()` overwrites `Rows` when it runs (strictly
more precise than the granule upper bound for the SAME K-clamp arithmetic
below), and its `DistinctSeries` is a field only this probe ever populates.

### What it feeds

1. **K clamping, for free.** `planner.go`'s existing `Rows`-based near-empty
   skip and `MaxKWithEstimate` raise (documented above) already consume
   `RequestMeta.Estimate.Rows` — no new code, no new `Config` knob, no new
   `Reason` token. Feeding it a REAL count when the probe ran is strictly an
   accuracy improvement over the granule-resolution upper bound
   `EXPLAIN ESTIMATE` alone would have supplied.
2. **Per-rung admission priors, answered more directly.**
   `CardinalityProbeAdvisor.maybeSeedPerRungPrior` mirrors
   `ScanEstimateAdvisor.maybeSeedPerRungPrior`'s one-directional contract (only
   ever seeds `cheap=true`, comparing against the SAME
   `perRungCheapRowsPerAnchor` threshold `PerRungAdmissionLearner.Observe`
   itself uses) — but compares `DistinctSeries`, not a raw scan-row upper
   bound. A per-rung carrier fans a classic-histogram bucket ladder out per
   SERIES, so the composed output `Observe()` measures scales with distinct
   series far more directly than with raw scanned rows — issue #2788's own
   "answer per-rung admission's rows/anchor question directly" phrase.
3. **Route memo corroboration — deliberately NOT wired**, for the identical
   reason `EXPLAIN ESTIMATE` is not: see "Why the failure-driven route memo
   is NOT seeded" above. A real bounded aggregate is still one non-drain
   observation, not the repeated real-traffic corroboration
   `minCorroboratingFailures` + the pressure damper exist to require.

### Cost bound and scope (deliberately narrow)

Gated identically to `ScanEstimateAdvisor`'s own three narrowings (ModeAuto
AND reached-cost-grid only; cached per shape; skipped once the route memo or
per-rung admission already holds a verdict) — reusing that file's own
`reachedCostGrid` / `shapeKey` / `hasExistingVerdict` helpers rather than
re-deriving them. Two further narrowings are specific to this probe, both
documented at length on `cardinality_probe_wiring.go`'s own top-level doc:

- **Carrier kind:** only `*chplan.RangeWindow` (the "matrix" family — by far
  the most common ModeAuto shape, and the one issue #2709's own incident and
  this issue's own dashboard-panel example both concern). Every other
  routable carrier kind (`RangeLWR`, `RangeBucketFanout`,
  `RangeBucketGridNative`, `RangeWindowGridNative`) fails open — no probe,
  exactly as if the feature did not exist for that shape. Extending the
  probe to them, and adding the issue's own bracketed-optional third stat
  (`avg(length(ExplicitBounds))`, a classic-histogram bucket-width bias
  signal neither consumer above needs), are tracked as separate follow-up
  work (issue #2840) rather than folded into this narrower landing.
- **Metric identity:** the probe's cache key is `(routememo.Key, metric)` —
  the ONE literal this signal cannot do without, because cardinality is
  fundamentally metric-specific in a way a literal-free structural shape
  alone cannot capture. It fires only when the carrier's nearest `Filter`
  gates on exactly one literal `MetricName = '...'` equality; a regex
  `__name__` matcher or a multi-metric selector has no single metric to key
  on and is skipped.

The bound (`Start - Offset - Range, End - Offset]`) the probe scans is the
EXACT window `chsql`'s own `maybePushInnerScanTimeBounds` pushes down for
this SAME `RangeWindow`'s real matrix emission — the probe reads precisely
the rows the real dispatch would read, no more.

A probe failure (breaker-open, transport error, emission error, or the
probe's own strict `cardinalityProbeTimeout` firing) is treated as "no
signal" and never surfaces as a query failure — advisory-only, fail-open by
construction, same as `EXPLAIN ESTIMATE`.

### Measurement

Measured with `buildCardinalityProbePlan`'s own real chplan tree, executed
via chDB against `test/perf/smoke/testdata/samples/svc_http_requests_total.parquet`
(the SAME corpus `EXPLAIN ESTIMATE` above was calibrated against) and
`kube_pod_status_reason.parquet` (the set's highest-cardinality sample, up
to ~4,800 series in a single window per its own `README.md`):

| window                                                          | rows    | distinct_series  |
| --------------------------------------------------------------- | ------- | ---------------- |
| 1h over `svc_http_requests_total`'s densest real hour           | 596,424 | 101 (saturated)  |
| 6h spanning that hour                                           | 895,858 | 101 (saturated)  |
| 6h a year outside the sample's captured span (empty)            | 0       | 0                |
| 1h over `kube_pod_status_reason`'s densest real hour            | 567,360 | 101 (saturated)  |

The 6h row count (895,858) lands EXACTLY on `EXPLAIN ESTIMATE`'s own
measurement for the identical window (the "Calibration" table above) — the
two probes independently scanning the same real data agree, cross-validating
that this probe's `(Start - Offset - Range, End - Offset]` bound is the
same window the granule-upper-bound probe already reasons about. Every dense
real window this sample carries saturates `uniqUpTo(100)` at 101 — this
sample's own real per-panel cardinality already exceeds the cap throughout
its captured span, confirming issue #2788's own verified constraint (a K
above 100 throws rather than silently under-counting) matters in practice,
not only in theory: a deployment probing production traffic at this
sample's own density needs `uniqCombined`/`uniqCombined64` (issue #2788's
own named alternative) to see past 100 distinct series, not `uniqUpTo`
alone — left for the same follow-up (issue #2840) that widens the carrier
scope, since neither of this landing's two consumers (K-clamp `Rows`,
per-rung `cheap` seeding) needs an exact count above the 100-series
threshold either already answers.

### Configuration

| Variable                                      | Type | Default | Description                                                                                                               |
| --------------------------------------------- | ---- | ------- | ------------------------------------------------------------------------------------------------------------------------- |
| `CERBERUS_CH_OPTIMIZATIONS=cardinality_probe` | flag | off     | Enables the chopt `cardinality_probe` feature (opt-in only, `AutoSelect=false` — see `docs/clickhouse-optimizations.md`). |

No numeric knobs: `cardinalityProbeUniqUpToCap` (ClickHouse's own hard
`uniqUpTo` ceiling, not a tuning value), `cardinalityProbeTimeout`, and the
cache capacity/TTL (reused from `ScanEstimateAdvisor`'s own constants) are
fixed Go constants pending real-world calibration evidence, mirroring
`per_rung_admission.go`'s own unexported constants (`perRungCheapRowsPerAnchor`
et al.) rather than growing a `Config` surface ahead of that evidence.

## Query actuals: predicted-vs-actual drift detection (issue #2789)

Both advisory pre-flight signals above — `EXPLAIN ESTIMATE` and the
cardinality pre-probe — predict a plan's scan cost BEFORE dispatch and are
never checked against what the query actually consumed. `internal/actuals`
closes that loop: a bounded, in-process `Tracker`, keyed by the SAME
literal-free plan-shape id `SettingsRules` stamps into ClickHouse's
`log_comment` (`internal/engine/plan_shape_id.go`), records the most recent
advisory prediction and a bounded exponential moving average of the REAL
resource usage a dispatch of that shape actually consumed.

**Not a chopt registry entry.** ProfileEvents on the native protocol and
`system.query_log` are both ancient, always-available ClickHouse surfaces
with no version floor to probe — this is a plain solver-policy config knob
(`CERBERUS_QUERY_ACTUALS_ENABLED`, default `false`), mirroring
`CERBERUS_SOLVER_ADAPTIVE_ENABLED`'s own posture rather than
`CERBERUS_CH_OPTIMIZATIONS`'s AutoSelect/version-floor machinery.

**Anti-autotune stance**, inherited from the cardinality pre-probe's own
precedent above: the tracker is a bounded, ADVISORY input to existing
policies, never a new fitting loop. Every EMA update moves at most
`EMAAlpha` (default 0.2) of the way toward a single new observation, and
`CalibrationFactor` clamps its output to `[0.5, 2.0]` — a burst of
anomalous actuals can shift a shape's tracked state gradually, never
violently in one request. This is the same boundary the retired
threshold-fitting autotune (issue #1273) crossed and cerberus does not
repeat.

### Two capture sources

1. **Native-protocol packets** (`internal/chclient/progress.go`, the FAST
   path) — free, since the production deployment's driver already streams
   both `Progress` (rows/bytes) and `ProfileEvents` packets for every
   query; cerberus previously parsed only the former.
   `MemoryTrackerPeakUsage`, a real ClickHouse `ProfileEvents` counter
   (verified against a live ClickHouse 26.6 server; not documented on
   ClickHouse's own reference, only in `system.query_log`'s column docs),
   supplies peak memory. Registering the `WithProfileEvents` callback adds
   no extra round trip — only the (already-flowing) packet's parse cost —
   so it is wired ONLY when the actuals feature is on (`WithActualsCapture`),
   never unconditionally.
2. **`system.query_log`** (`internal/engine/query_log_actuals.go`, the SLOW
   batch/fallback path) — a background reconciler polls for
   `log_comment LIKE 'cerb:%'` `QueryFinish` rows, for the dispatches the
   packet path could not observe (one that failed before completing, or a
   deployment mode where packet capture is not wired). Genuinely slow by
   construction: `system.query_log`'s own async flush lag means a row
   surfaces well after the query that produced it finished, so the
   reconciler is watermark-based (retries from the same point on a query
   failure, never advances past unread rows) rather than assumed
   synchronous.

`log_comment` is stamped onto every dispatch the actuals feature covers
REGARDLESS of `SettingsRules.LogCommentShape` — that flag governs a
separate, purely-observability concern (an operator manually clustering
`system.query_log` by hand); actuals capture needs the correlation key
whether or not the operator separately opted into it.

### Four consumers

1. **Drift-detection core** (the issue's primary ask): every `RecordActual`
   call computes `actualEMA / predicted` and flags the shape ALERTING once
   `MinObservations` (default 2) is reached and the ratio falls outside
   `[DriftLowerRatio, DriftUpperRatio]` (default `[0.1, 3.0]` — deliberately
   asymmetric: `EXPLAIN ESTIMATE` is a granule-resolution UPPER BOUND, so
   the actual is EXPECTED to run well below the prediction most of the
   time; the dangerous direction is the actual EXCEEDING it). Every
   observation — alerting or not — is recorded on the
   `cerberus_solver_estimate_drift_ratio` histogram and
   `cerberus_solver_estimate_drift_alerts_total` counter (attribute
   `cerberus.actuals_source`, `"packet"` / `"query_log"`), so an operator
   graphs `rate(cerberus_solver_estimate_drift_alerts_total[5m])` rather
   than polling the tracker out of process.
2. **Carrier-geometry cost-model calibration** (`internal/engine/actuals_wiring.go`'s
   `calibrateEstimate`): before an advisory `solver.ScanEstimate` reaches
   the K clamp, its `Rows` field is multiplied by the shape's own bounded
   `CalibrationFactor` — a real correction to `classify()`'s existing
   `EstimateNearEmptyRowFloor` / `MaxKWithEstimate` arithmetic, not a new
   threshold.
3. **Per-rung admission tightening** (`maybeSeedPerRungAdmissionFromActuals`):
   reuses `PerRungAdmissionLearner.SeedPriorFromEstimate` — the SAME
   one-directional (`cheap=true` only) seeding mechanism issue #2787's own
   `maybeSeedPerRungPrior` uses for a live `EXPLAIN ESTIMATE` round trip —
   applied to a ZERO-I/O read of a shape's tracked actuals instead. Same
   safety argument as that mechanism's own doc: it can only ever DOWNGRADE
   an already-`PerRungPredictive` route, never promote or block one.
4. **Route-memo priors with real magnitudes** (`internal/routememo/magnitude.go`,
   `RecordActualMagnitude` / `MagnitudeFor`): a deliberately THIN hook —
   see "Why the failure-driven route memo is NOT seeded" above for why the
   memo's routing VERDICT is never touched by an advisory or actuals
   signal. This hook adds a second, purely OBSERVATIONAL axis on top of an
   ALREADY-LIVE verdict (it never creates, deletes, or changes
   `LookupState`, eviction order, or TTL) — wired from
   `per_rung_admission.go`'s existing `perRungObservingCursor.Close()`,
   which already computes the identical `routememo.Key` for a per-rung
   predictive route-B dispatch's own clean drain. Architecture the memo
   supports for a future routing use; this landing makes no routing
   decision from it.

### Verified against a live server, not assumed

`system.query_log`'s column shape was checked against a live ClickHouse
26.6 server before relying on it (the same discipline issue #2789's own
risk note calls out — issue #2770's Loki catalog PR caught a real bug from
an unverified assumption about `system.view_refreshes`'s columns):
`log_comment` is `String`, `read_rows`/`read_bytes`/`memory_usage` are all
`UInt64`, `event_time` is `DateTime`, `type` is the expected
`Enum8('QueryStart'=1,'QueryFinish'=2,...)` — exactly as expected.

A realistic drift scenario was also reproduced live, rather than
constructed synthetically: `EXPLAIN ESTIMATE` was run against a MergeTree
table holding 1,000 rows (`Rows: 1000`, the "cached admission-time
estimate" `ScanEstimateAdvisor`'s own 30-minute TTL would hold); 1,000,000
more matching rows were then inserted (a realistic traffic burst landing
between the cached estimate and the real dispatch); the identical query
was dispatched for real and `system.query_log` reported
`read_rows = 1,001,000` for it — a **1001x** predicted-vs-actual ratio, far
outside the default `[0.1, 3.0]` band. Replayed through the real
`internal/actuals.Tracker` (two corroborating observations, reaching
`MinObservations`): `DriftReport.Alerting = true`, and
`CalibrationFactor` correctly clamped to its `2.0` ceiling rather than
propagating the raw 1001x multiplier — proving the bounded-influence
property holds even on a genuine, large real-world divergence, not only on
a small synthetic one.

A second, cleaner comparison — `EXPLAIN ESTIMATE` vs. real `read_rows` for
the SAME query against a STABLE (non-growing) table, no `PREWHERE`, no
skip index — landed within noise of each other (8,192 predicted vs. 8,192
actual on a `minmax`-prunable `PREWHERE` range; 5,000,000 vs. 5,000,000 on
a full-table `PREWHERE` scan), confirming the mechanism does NOT
false-alarm on the common case where nothing has actually drifted: the two
independent probes (`EXPLAIN ESTIMATE`'s no-execution index analysis and a
real dispatch's own storage-layer read) agree closely when the underlying
data is stable between them. The dominant real driver of drift this
sandbox reproduces is TEMPORAL — a cached advisory estimate going stale
relative to data that grew after it was taken — rather than a structural
`PREWHERE`/skip-index mismatch in row-count terms specifically; both are
covered by the SAME mechanism regardless of which one produced the
divergence.

### Configuration

See [`configuration.md`](configuration.md#schema-overrides-and-prometheus-resource-labels)
for the full `CERBERUS_QUERY_ACTUALS_*` knob table.

## Routing-decision calibration corpus (measurement-only)

The router (`Planner.Plan`) is a **pure** classifier: a query routes A (single
CH query) or B (time-slice sharded) by cost thresholds over the grid it
derives — `N` anchors, fan-out `F`, cumulative spine lookback `D`. Those
thresholds are **static configuration**, so classification is a pure function of
`(plan, meta, Config)` — no per-request state, no RNG, and identical across every
replica running the same configuration. The corpus is the **measurement-only**
substrate for tuning them: it records, **without itself changing any threshold or
routing behavior**, the `(N, F, D)` context and the observed cost of every
decision.

To answer it the engine closes the loop the optimization corpus
(`internal/optcorpus`) already half-built:

- **Decision read-out.** Every `solver.Decision` now carries the RAW classifier
  scalars (`NAnchors` / `Fanout` / `CumulativeD` / `OuterRange` / `Step`)
  alongside `Strategy` / `K` / `Reason`, populated for **both** routed and
  not-routed decisions. The overlap analysis compares route-A and route-B cost
  at equal `(N, F, D)`, so route A must record its grid too. Buckets key on the
  raw scalars, never on `Reason` — the reason names WHICH gate refused a plan,
  not where that plan sat relative to it, and a counterfactual re-fit needs the
  latter.
- **Join to observed cost.** At the dispatch seam the engine hands the corpus
  reconciler the decision read-out next to the CH `query_id`s the dispatch will
  run under — one on route A, K on a route-B fan-out. The reconciler
  joins `query_id` → `system.query_log` (the cost columns plus a derived
  `exit_status` of `ok` / `oom` / `timeout` / `aborted` / `error` from the row
  type + exception code) and writes one corpus row per dispatch. Terminal rows
  are selected by naming the `type` Enum8 members through
  `CAST(<name>, toTypeName(type))`: a bare string in the `IN`-list would make
  ClickHouse coerce the comparison to `String`, so a member name that does not
  exist would match nothing *silently*, whereas the CAST form raises
  `UNKNOWN_ELEMENT_OF_ENUM`. A distributed query logs one row per participating
  node under the same `query_id`, so the group's terminal state is reduced as
  `max(type != QueryFinish)` — a comparison against the enum, never against the
  member names, whose lexical order would rank a clean finish above an
  exception. `ExceptionBeforeStart` rows are excluded: the query never ran, so
  their zero cost would deflate every watermark learnt from the corpus.
  An exception whose code cerberus does not recognise is `error` — the honest
  floor, never folded back into the healthy `ok` population. All optcorpus
  invariants hold: it is
  flag-gated, production-only, failure-open, and the observe call is a
  non-blocking channel send — the hot path is byte-unchanged when the corpus is
  off.
- **One row per REQUEST, K statements or one.** The corpus row is the unit of
  A/B comparison and carries no request identifier to group by after the fact,
  so a route-B fan-out is folded into a single row rather than K: a row per
  shard would put a fraction of route B beside a whole route-A query in every
  comparison. All K shard ids index one ring record, and the reconciler folds
  each shard in as its `query_log` row becomes visible, deferring the write
  until the group is complete — a partially joined fan-out is never written
  under-counted while it can still complete. The fold keeps a route-B row
  meaning what a route-A row means: `read_rows` / `read_bytes` /
  `profile_events` are summed (total work done), `exit_status` is the most
  severe shard status (an OOMed shard is an OOM for the request), and the two
  schedule-dependent columns read the Executor's EFFECTIVE concurrency P, which
  is routinely below K — `memory_usage` is the sum of the P largest shard peaks
  (only P coexist) and `query_duration_ms` is the makespan bound
  `max(slowest shard, total / P)`. Assuming a fully parallel fan-out instead
  would understate route-B latency and overstate its memory by roughly K/P at
  the default P. The row stores that P as `parallelism`, so a calibration reads
  a route-B cost against the schedule that produced it rather than against K
  alone.
- **Partial fan-outs are published, not dropped.** The shard errgroup is
  first-error-wins: when a shard fails, its in-flight siblings are cancelled and
  the later waves are never dispatched at all, so their query_log rows will
  never exist. Waiting for all `K` would therefore wait forever and lose the
  record — and the records lost that way are exactly route B's failures, the
  population the watermarks are learnt from. The reconciler instead accumulates
  each shard as it lands and publishes what it has when the dispatch can no
  longer complete: either its remaining ids age out of the query_log lookback,
  or the bounded ring reuses its slot (on a busy deployment the ring wraps long
  before the lookback expires, so slot reuse is the common trigger). Such a row
  carries `shards_observed` &lt; `k_shards`; its cost columns describe only the
  shards named there, so every absolute route-B cost comparison must exclude it.
  A fan-out where **no** shard reached query_log publishes nothing — a row with
  no observed cost would be a fabricated data point.
- **Cerberus-side terminal outcomes.** `system.query_log` only reflects what
  ClickHouse saw — it cannot show a request cerberus *itself* terminated. Four
  cerberus-side outcomes are captured in-process and take precedence over (or
  complement) the query_log-derived `exit_status`:
  - **`sample_budget`** — the `query.maxSamples` 422. It fires during the
    Go-side result drain *after* the CH query finished cleanly, so query_log
    shows `ok` with real cost. The corpus **keeps that cost** but overrides
    `exit_status` to `sample_budget`: the richest calibration signal is "CH cost
    = X, but cerberus rejected the client: too big." Stamped onto the existing
    dispatch record by `query_id` (eager path in the engine; cursor path via the
    handler's drain seam), so the in-process outcome wins over a query_log `ok`.
  - **`byte_budget`** — the drain byte-budget 422, the byte-axis sibling of
    `sample_budget` and stamped through the same seam for the same reason. It
    bounds the cumulative *result bytes* a drain may charge — the Tempo span
    search's wide-projection attribute maps, and the PromQL matrix drain's
    per-row native-histogram payloads — an axis a row count does not measure.
    Like `sample_budget` it fires *after* the CH query finished cleanly, so the
    corpus keeps the real cost and overrides only the `exit_status`.
  - **`breaker`** — the chclient circuit-breaker 503. Cerberus fast-fails
    *before* dispatching, so there is no CH query and no query_log row. The
    corpus emits a **decision-only** row carrying the routing read-out known at
    classify time, `exit_status = breaker`, and zero cost.
  - **`rejected`** — the resolution-cap / body-limit 400. These guards fire
    pre-parse, so there is likewise no CH query: a **decision-only** row with
    `exit_status = rejected`, zero cost, and no routing read-out (no
    classification ran). These outcomes carry the same invariants — flag-gated,
    failure-open, non-blocking, drop-under-burst — and the in-process capture
    works even where the query_log reconcile (production-only) does not.
- **ClickHouse-side abort vs error.** A query that ended in a ClickHouse
  exception is `aborted` when the code says the query was abandoned rather than
  faulted — a client cancellation or a connection that went away mid-flight —
  and `error` otherwise. The split matters because the two classes mean
  opposite things for calibration: an abort measures how long a client was
  willing to wait, so its truncated cost is not evidence about the route,
  whereas an error is a fault whose cost is real.
- **Sink.** With `CERBERUS_CH_OPT_CORPUS_SINK_MODE=chtable` the corpus lands in
  the `cerberus_router_corpus` MergeTree (DDL built with the typed `chsql` DDL
  builder, 30-day TTL); the default `jsonl` mode appends the same rows to the
  sink-path file (load them into the same table shape for analysis). Sink
  construction reconciles the deployed table with the schema the running binary
  writes — `CREATE TABLE IF NOT EXISTS`, then an `ALTER TABLE … ADD COLUMN IF
  NOT EXISTS` per corpus column, then an `ALTER TABLE … MODIFY COLUMN` that
  widens `exit_status` to the member set this binary can emit, then one read of
  `system.columns` that fails construction if a column is still missing or a
  member still absent. `CREATE … IF NOT EXISTS` alone cannot do this: it is a
  no-op against an existing table however its columns are declared, so a binary
  that learnt a new member would write a value the deployed column cannot hold,
  and a binary that learnt a new COLUMN is worse still — the batch appends
  POSITIONALLY, so a missing column does not lose one field, it binds every
  later value to the wrong column or invalidates the batch outright, on every
  reconcile interval, forever. Both ALTERs are **best-effort** — a CH user with
  `INSERT` + `CREATE` but no `ALTER` grant, or an operator-owned table that
  needs `ON CLUSTER`, still gets a working sink whenever the deployed table
  already matches. The `system.columns` read is the authority: it is the
  server's own answer, so the sink is never built over a table that cannot hold
  what the binary writes.
- **A sink that cannot be built disables the reconciler**, logged at startup
  with the underlying error; it does not silently switch modes. There is no
  fallback from `chtable` to `jsonl` — an operator who asked for the CH table
  and got a local file instead would be told the corpus is healthy while nothing
  reads it. The corpus is failure-open with respect to the **data plane**, which
  is the invariant that matters: no query path depends on the sink, so a sink
  outage costs calibration data and nothing else.

This is a pure additive read-out: it records values the classifier already
computed and **changes no routing behavior**. The captured features suffice to
**replay the classifier offline** — feed a corpus row's `(N, F, D, OuterRange,
Step)` back through `Planner.Plan` and it reproduces the recorded route, so an
operator can sweep counterfactual thresholds against history without touching
production (proven by `TestPlan_OfflineReplay_ReproducesRoute`).

### Blind spot: a cerberus process OOM-kill

The in-process recorder dies with the process. If the **cerberus process
itself** is OOM-killed — the Go-side result-buffering class the sample budget
exists to bound (e.g. an unbounded `matrixFromCursor` double-buffer) — the
recorder cannot emit a `cerberus-oom` row, because the goroutine that would
write it is gone with the rest of the process. That specific event is
**unrecordable in-process** and is explicitly **out of the corpus's scope**.

Partial recovery is two-fold, and neither is an authoritative marker:

- The decision read-out is stamped *at dispatch*, before the drain that OOMs, so
  the dispatch record exists in the ring — but it is lost on the kill (the ring
  is in-memory).
- After a restart, the reconciler backfills the CH **cost** for any `query_id`
  that did finish on the CH side and still falls inside the query_log lookback
  window — but it joins to the query_log-derived `ok` / `oom` / `timeout` /
  `aborted` / `error`, never to a cerberus-oom outcome, because no in-process
  call survived to stamp one.

An authoritative "cerberus-oom" marker would require an **external** signal — a
k8s `OOMKilled` container event correlated back to the in-flight requests —
which is outside the corpus's in-process boundary and is not part of the corpus.

### Reading the go/no-go analysis

Run [`router-calibration.sql`](router-calibration.sql) against the corpus table:

- **Heuristic is fine (YAGNI — stop).** Route A's cost percentiles sit well
  below route B's with little overlap, few route-A queries land in route B's
  cost territory at the same `(N, F)`, and essentially no route-A query
  OOMs/times out. The fixed thresholds separate the two populations cleanly;
  calibration would add machinery for no measurable win.
- **Calibration is justified.** A large share of route-A queries exceed route
  B's median cost (query 2's `pct_a_misrouted_by_mem`), buckets show route A as
  expensive as route B at the same `(N, F)` (query 3), or — the decisive signal
  — route-A queries OOM/timeout (query 4). Any non-trivial route-A failure
  count is a standalone go signal: the query died on the single path the
  heuristic chose for it.
