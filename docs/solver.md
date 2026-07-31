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
   last-with-respect-to family. Every other grid carrier —
   `RangeWindowNative`, `RangeWindowResample`, `RangeBucketFanout`, `StepGrid`,
   `AbsentOverTime` — carries its own eval grid that re-anchoring clones
   verbatim, so a plan whose spine bound-carrier is one of those fails closed to
   route A (every shard would otherwise emit stale bounds).

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
6. **Grid commensurability for nested spines.** Inner anchors are generated
   backward from each node's `End` with no epoch alignment, so slicing shifts
   the inner grid unless the slice quantum is a multiple of every inner
   resolution. A nested spine routes A unless the selected shard geometry's
    emitted quantum `m = ceil(N/K)` satisfies `m·Step ≡ 0 (mod lcm(inner
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
Under `single` the Planner classifies but never routes, and records
`routing-disabled` — not `below-threshold`, which would claim a threshold was
consulted when none was.

Every classification — routed or not — produces a `Decision` carrying the
reason (`routed`, `below-threshold`, `not-sliceable`, `instant`, `instant-join`,
`high-D`, `now64`, `grid-mismatch`, `incommensurate`, `scalar-heavy`,
`routing-disabled`, `extraction-failed`) for the shadow header, alongside the
plan's cost grid (`N`, `F`, `D`, `OuterRange`, `Step`).

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

### Configuration

Off by default, matching cerberus's convention for a new
runtime-behavior-changing feature (alongside `CERBERUS_CH_OPT_CORPUS_ENABLED`,
`CERBERUS_EXPERIMENTAL_TS_GRID_RANGE`): an operator opts in explicitly rather
than picking up new ClickHouse dispatch/resource behavior on a routine
upgrade.

| Variable                                           | Type     | Default               | Description                                                                                                                                                                                                                                                          |
| -------------------------------------------------- | -------- | --------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `CERBERUS_SOLVER_ROUTE_MEMO_ENABLED`               | bool     | `false`               | Wires `internal/routememo` onto the engine. Left unset (or `false`), the engine's `RouteMemo` field stays nil and every function in `route_memo_wiring.go` no-ops through its own `routeMemoActive()` guard — dispatch stays byte-unchanged.                         |
| `CERBERUS_SOLVER_ROUTE_MEMO_ENTRY_TTL`             | duration | package default (30m) | Overrides how long a recorded verdict is trusted before it ages out. Zero/unset means "use the routememo package's own default", not "TTL zero" — `SetEntryTTL` treats a non-positive value as a no-op so a misconfigured value can never silently disable the memo. |
| `CERBERUS_SOLVER_ROUTE_MEMO_REVALIDATION_FRACTION` | int      | package default (2)   | Overrides the divisor that places re-validation at the TTL midpoint. Same non-positive-is-no-op contract as the TTL knob.                                                                                                                                            |

`cmd/cerberus/main.go`'s `buildRouteMemo` is the only constructor: it returns
nil (feature off) unless `RouteMemoEnabled` is set and a solver is
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
  ClickHouse saw — it cannot show a request cerberus *itself* terminated. Three
  cerberus-side outcomes are captured in-process and take precedence over (or
  complement) the query_log-derived `exit_status`:
  - **`sample_budget`** — the `query.maxSamples` 422. It fires during the
    Go-side result drain *after* the CH query finished cleanly, so query_log
    shows `ok` with real cost. The corpus **keeps that cost** but overrides
    `exit_status` to `sample_budget`: the richest calibration signal is "CH cost
    = X, but cerberus rejected the client: too big." Stamped onto the existing
    dispatch record by `query_id` (eager path in the engine; cursor path via the
    handler's drain seam), so the in-process outcome wins over a query_log `ok`.
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
