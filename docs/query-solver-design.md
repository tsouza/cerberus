# Query Solver — Sharded Pushdown Execution for Memory-Unbounded Queries

- **Status:** proposed
- **Date:** 2026-06-13
- **Supersedes:** the open "route B" question in `docs/evaluation-architecture.md`
- **Context:** the "single CH query per request — no scatter-gather" lock in `docs/performance.md` was explicitly relaxed by the maintainer (2026-06-12) for queries that cannot be solved bounded in one SQL statement. This document is the design that spends that relaxation.

## Summary

Cerberus gains a thin orchestrator, `internal/solver`, that recognizes the narrow class of plans whose single-statement execution is memory-unbounded on ClickHouse (the measured shape: high anchor fan-out `F = Range/Step`, e.g. `sum(rate(m[5m]))` @ 15s over 1h — run-27277793810 wanted 2.12 GiB against the 1 GiB cap). For those plans only, it re-anchors K deep copies of the already-optimized chplan onto disjoint slices of the anchor grid, emits each via the existing `chsql.Emit`, executes them with bounded parallelism behind a global connection gate, and concatenates the result streams behind the existing `chclient.Cursor` interface. There is **no new evaluator and no new SQL template**: every shard runs the same compat-gated SQL route A runs, restricted to its anchor sub-grid, so parity reduces to one mechanically provable lemma. Route A remains the default for the overwhelming majority of traffic; the solver is the exception, off by default until a forced-route compatibility lane proves zero diffs. The shapes time-slicing structurally cannot reach (instant cross-series aggregations, single windows exceeding CH memory) are documented as the ceiling, with a named successor (a narrow reference-engine solver behind the same seam) reserved as the escalation path.

## Routing

**Hook point.** `internal/engine/engine.go` — inside `Engine.QueryPlan` (engine.go:202) and `Engine.QueryPlanCursor` (engine.go:309), at the existing seam between `e.Optimizer.Run` and `chsql.Emit`. This is the one point that covers all four entry points with the final optimized `chplan.Node`, `Meta`, `Lang`, and ctx in hand. The engine calls `e.Solver.Route(...)`; a `false` return means route A, byte-identical to today. This is a query-strategy decision, not HTTP routing, so it respects the `docs/engine.md` "not a router" boundary. The plan is never mutated by the decision.

**Eligibility signals** — all static, gathered in one pass that walks both the node tree *and* every expression tree (mandatory: `chplan.Walk` does not recurse into `ScalarSubquery.Input`):

1. **Slice-invariance, by marker, not by type.** A node is admissible only if it is registered `SliceInvariant` — an explicit, machine-checkable assertion that its per-(series, anchor) output is a pure function of in-window samples, independent of the scan lower bound. Phase 1 registers: `Scan`, `Filter`, `Project`, `Aggregate` keyed per-anchor/per-series, `RangeWindow`, `RangeLWR`, `RangeBucketFanout`, `StepGrid`, `UnionAll`. The registry exists because of the #92 interaction: if the A-prime cumulative-counter idiom ships a formulation whose per-anchor value depends on scan order (lagInFrame seeded at scan start), a type-based whitelist would route it silently. New nodes — including any #92-substituted shape — default to route A until their marker is proven by the fixture family in §Parity. Any unmarked node anywhere in the plan → route A, no exceptions.
2. **Both `Start` and `End` pinned (non-zero) on every windowed node.** Zero bounds mean `now64(9)` resolution per statement — two statements would see different wall-clocks. Instant queries (`Step == 0` or `OuterRange == 0`) are not time-slice routed in phase 1–3.
3. **No `now64` anywhere**, including expression positions the node check never sees: the expr-walk rejects any `chplan.FuncCall{Name: "now64"}` inside `Filter` predicates, projections, and `ScalarSubquery.Input`. Belt-and-braces: the Executor's emit loop asserts no shard SQL string contains `now64(` and falls back to route A (with a counter) if one does.
4. **Grid-prediction check (the @-modifier guard).** `ReanchorRange` is defensive, not blanket: a windowed node is rewritten only if its `(Start, End, Step, OuterRange)` exactly equal the values the request grid predicts at that spine depth (computable via the `widenSubquerySpine` recursion). Any mismatch — an @-pinned anchor, a future route-A fix that pins `End ≠ ctx.end` — aborts the Decision → route A. This makes the copy safe by construction today and after the known `lowerRangeFn` @-clobber bug (see §Parity) is fixed.
5. **Grid commensurability for nested spines.** Inner anchors are generated backward from each node's `End` with no epoch alignment, so slicing shifts the inner grid unless the slice quantum is a multiple of every inner resolution. Constraint: `m·Step ≡ 0 (mod lcm(res_1..res_d))` for every matrix `RangeWindow` resolution down the spine. If no `m ∈ [minAnchorsPerSlice, N/2]` satisfies it → route A.
6. **Cost signals**, computed over the whole plan *including* expr-embedded sub-plans: `F` = max `Range/Step` (or `Lookback/Step`) over windowed nodes; `N = OuterRange/Step + 1`; `D` = cumulative spine lookback (Σ `Range` down nested matrix windows + leaf `RangeLWR.Lookback`). A `ScalarSubquery` whose interior scan-span × fan-out exceeds a configured fraction of the outer plan's → route A (the slice benefit cannot pay for replicating an expensive scalar; see §Decomposition for the hoist that usually avoids this).

**Decision.** Route B iff eligible ∧ `F ≥ Fmin` ∧ `N×F ≥ MinAnchorPairs` ∧ `K ≥ 2`, where

```text
K = clamp( floor(N / minAnchorsPerSlice), 2, min(MaxK, floor(OuterRange / max(D, Step))) )
```

Defaults, tuned deliberately conservative against the over-routing attack (Grafana's auto-step makes `rate[5m]` @ 15s — the dominant production shape — hit F=20, N≥241; it must **not** route at default thresholds unless the total expansion is spike-class): `Fmin = 16` (`CERBERUS_SHARD_MIN_FANOUT`), `MinAnchorPairs = 4000` (`CERBERUS_SHARD_MIN_ANCHOR_PAIRS`, the N×F product — the spike had ≈4820), `MaxK = 8`, `minAnchorsPerSlice = 16`, `P = 3` (`CERBERUS_SHARD_PARALLEL`). The clamp form is a smooth ramp: near the boundary, behavior degrades to K=2 (mild slicing) rather than flipping K=8 ↔ K=0, so Grafana's width-dependent step changes cannot flap a panel between dramatically different execution shapes.

**Honest weakness — cardinality blindness.** The no-caching invariant forbids stats and a pre-flight `count()` is a round trip we reject for the default path, so the router cannot distinguish a 20-series panel from a 50k-series spike. Two consequences, both bounded: over-routing costs K small queries plus ≈1.5× scan refetch (correct, slower); under-routing produces exactly today's 422 — never worse than status quo. Three controls: (a) the **shadow header** `X-Cerberus-Route-Decision: <decision>;reason=<reason>` ships in phase 1 while execution stays on route A, so thresholds are tuned against observed production/compose traffic before the auto flip; (b) a perf-guard pins the routed fraction of a recorded representative corpus under a budget (target < 5–10% of query_range traffic), so threshold regressions fail CI; (c) `CERBERUS_EVAL_ROUTE=auto|single|sharded` — `single` disables the solver entirely; `sharded` drops thresholds to the floor (K_min=2) so every *eligible* query routes (ineligible queries always stay on A, so force-sharded never breaks anything). This is the force knob every test lane uses.

## The solver framework

New package `internal/solver`. The framework is general: `Planner` is policy, the `Slicer` is geometry, `Executor` is scheduling, `shardCursor` is composition — a future series-shard dimension or a second head plugs in a new Slicer without touching the Executor.

```go
package solver

type Config struct {
    Mode               string        // "auto" | "single" | "sharded" (CERBERUS_EVAL_ROUTE)
    MinFanout          int           // Fmin, default 16
    MinAnchorPairs     int           // N×F floor, default 4000
    MaxK               int           // default 8
    MinAnchorsPerSlice int           // default 16 (also enforces ≥2 anchors/slice)
    Parallel           int           // P, per-request shard concurrency, default 3
    Timeout            time.Duration // CERBERUS_SOLVER_TIMEOUT, wall-clock per routed request, default 60s
    MaxOutputRows      int64         // CERBERUS_SHARD_MAX_OUTPUT_ROWS, default 2_000_000
    MemoryApportion    bool          // CERBERUS_SHARD_MEMORY_APPORTION: per-shard max_memory_usage = cap/P (256 MiB floor)
}

// Decision is the routing output. Slices are ordered oldest-first (composition order).
type Decision struct {
    Strategy string // "sharded-timeslice"
    K        int
    Reason   string // shadow-header vocabulary: routed | below-threshold | not-sliceable |
                    // instant | high-D | now64 | grid-mismatch | incommensurate | scalar-heavy
    Slices   []Slice
}

type Slice struct {
    Index      int
    Start, End time.Time   // anchor-grid-aligned slice bounds
    ScanFrom   time.Time   // offset- and D-aware input lower bound — owned by the solver (see Decomposition)
    Plan       chplan.Node // re-anchored DEEP COPY of the optimized plan
}

// Planner: pure, read-only classification of the post-optimize plan.
type Planner struct{ Cfg Config }
func (p *Planner) Plan(plan chplan.Node, meta engine.Meta) (*Decision, bool)

// Executor: bounded-parallel dispatch + stream concatenation.
type Executor struct {
    Client  engine.CursorQuerier
    Cfg     Config
    Gate    *semaphore.Weighted // GLOBAL, sized MaxOpenConns − reserve
    Breaker breakerPeeker       // pre-flight state check (see Failure)
    Admit   admitTopUp          // two-stage weighted admission (see Parallel execution)
}
func (x *Executor) Execute(ctx context.Context, langName string, d *Decision,
    budget *chclient.SampleBudget) (chclient.Cursor, *ExecInfo, error)

type ExecInfo struct {
    SQLs        []string // one per shard, for X-Cerberus headers / tracing
    ShardArgs   [][]any
    Parallelism int      // effective P after admission clamp
}

// Solver bundles both; the Engine holds one (nil = feature off).
type Solver struct { Planner Planner; Executor Executor }
func (s *Solver) Route(ctx context.Context, langName string, plan chplan.Node,
    meta engine.Meta, budget *chclient.SampleBudget) (chclient.Cursor, *ExecInfo, bool, error)
```

**Supporting changes outside the package:**

- `internal/chplan/reanchor.go` — `func ReanchorRange(n Node, start, end time.Time) (Node, error)`: head-agnostic generalization of `promql.widenSubquerySpine` (subquery.go:419) that **copies instead of mutating**, applies the defensive grid-prediction check, recurses into matrix spines (`start.Add(-Range)`), and copies `ScalarSubquery.Input` verbatim. Highest-defect-density component; pinned by an equivalence test against `widenSubquerySpine` run on **post-optimizer** plans (so optimizer-substituted shapes are what gets validated), plus deep-copy isolation tests.
- `internal/chplan` — the `SliceInvariant` marker registry described in §Routing.
- `internal/chclient` — `Config.MaxOpenConns/MaxIdleConns/ConnMaxLifetime` (task #81, wired into `clickhouse.Options` in `New`, client.go:159); and a request-scoped shared sample budget so the 422 max-samples parity stays per-REQUEST:

```go
type SampleBudget struct{ remaining atomic.Int64 } // born and dies with one request — no cross-request state
func NewSampleBudget(max int64) *SampleBudget
func WithSampleBudget(ctx context.Context, b *SampleBudget) context.Context
// rowsCursor.Next consults the ctx budget when present, else the per-cursor max.
```

- `internal/engine` — `Engine{Optimizer *optimizer.Driver; Client Querier; Solver *solver.Solver}`; one new branch in each of `QueryPlan`/`QueryPlanCursor`; `strategyFor` (engine.go:55) gains the `sharded-timeslice` arm with a **composable header grammar** (ordered comma list, e.g. `mv-substituted,sharded-timeslice`) so the MV signal is never lost; new consts `HeaderShards = "X-Cerberus-Shards"`, `HeaderRouteDecision = "X-Cerberus-Route-Decision"`.

## Decomposition strategies

**Primary dimension (phases 1–3): the anchor grid.** Anchors are defined backward from `End`: `a_i = End − i·Step`, `i ∈ [0, N)` — matching every matrix emitter (range_window.go:576/834/1140/2240). With `m = ceil(N/K)`, slice `j` owns anchor indices `[j·m, min((j+1)·m, N))`; `End_j = End − j·m·Step`, `OuterRange_j = (count_j − 1)·Step`. Because `End_j` sits on the original grid and `OuterRange_j` is a Step-multiple, the union of slice anchor sets equals the original set exactly, pairwise disjoint — no reconciliation at compose time, and a window is always evaluated **whole** within one slice (counter resets cannot straddle a shard edge by construction). **Singleton-tail rule:** a slice with `count_j < 2` merges into its neighbor — `OuterRange_j = 0` would flip the emitter from the matrix template to the instant template, and keeping every shard on the identical template keeps the parity argument trivial.

**Per-shard scan bounds are new, owned solver logic — not inherited emitter behavior.** Two facts force this: (a) `maybePushInnerScanTimeBounds` (range_window.go:1481) is called only by the Tempo metrics emitters — the PromQL matrix emitters `emitWindowedArrayPairsMatrix` (:571) and `emitWindowedArrayExtrapolatedMatrix` (:2234, the exact OOM shape this design exists to fix) never call it; (b) the existing bound formula is **offset-blind** (`ts > Start − Range AND ts ≤ End`), while matrix anchors are generated from `End − Offset` (range_window.go:639) — windows live in `(Start − Offset − Range, End − Offset]`, so naive shard bounds silently empty every window of `rate(m[1m] offset 1h)`, and negative offsets truncate the newest windows into *wrong* (not missing) values. The solver therefore derives each slice's input interval itself, sign-aware:

```text
ScanFrom_j = Start_j − D − Offset_spine        (Offset enters with its sign;
ScanTo_j   = End_j − Offset_spine               negative offset widens to the RIGHT past End_j)
```

unit-tested against the windows `sampleAnchorFanoutFrag` actually generates for every `(Range, Step, Offset)` combination including `Offset > Range` and `Offset < 0`. The offset-aware pushdown also lands on **route A's** matrix emitters as its own PR (tracked as #93) — independently valuable, since route A scans full series history today; until it lands, the refetch model in §Memory assumes the pushdown exists and the phase-1 gate requires it.

**`ScalarSubquery`: hoist, don't replicate.** Replicating a scalar sub-plan into K shards is K× the cost of the most expensive fragment of some queries (`... / scalar(quantile(0.99, sum_over_time(big[24h])))`), with P× concurrent server memory. The relaxed lock explicitly permits an extra round trip here: the solver executes the scalar sub-plan **once**, before the shards, and binds the result as a literal arg into every shard plan — the one-row, anchor-independent contract makes this trivially safe and makes scalar cost 1× instead of K×. Plans whose scalar interior cannot be hoisted (or exceeds the cost fraction in signal 6) route A. Pinned by a scaling-harness construct sweeping scalar-fragment weight.

**Operator-class map** (per `docs/evaluation-architecture.md`'s classification):

- Row-shape range family (`rate`/`increase`/`delta`/`*_over_time` matrix mode, `RangeLWR`, `RangeBucketFanout`, `StepGrid`) — sliceable, phase 1.
- Per-anchor reductions over the grid (`topk` as per-anchor `LIMIT K BY anchor_ts`, step-aligned `VectorJoin`/`VectorSetOp`, `HistogramQuantile`, `AbsentOverTime`, nested matrix spines under the lcm clamp) — phase 3, one node family per PR, each with forced-route compat evidence and a fresh `SliceInvariant` proof.
- Instant queries with a sliceable inner matrix and a slice-mergeable outer reducer (`max/min/sum/count_over_time` over a subquery) — phase 3b: the handler knows the eval timestamp natively (`executeInstant`), so lowering pins `Start = End = ts` (killing the now64 hazard at the source) and a second Slicer partitions the **inner** `RangeWindow`'s anchor grid. `quantile_over_time` over a subquery is not slice-mergeable — route A. This recovers part of the subquery family the motivating doc names as "where A strains worst"; the high-D residue (`K ≤ OuterRange/D` clamp) is the documented ceiling, observable in production via `reason=high-D` on the shadow header.
- Cross-series instant aggregations, global `Limit`/`OrderBy`, LogQL line-limit queries — **never** routed; compose-side limit/merge logic would be a new evaluator in disguise. These are the reserved territory of the phase-5 successor (§Alternatives).

**Secondary dimension (phase 4): series sharding** for instant series-local shapes — a `Filter` predicate over a **canonical** series hash above the leaf Scan. The key must not be `cityHash64(toString(Attributes))`: CH `Map` preserves insertion order, so the same logical series ingested with permuted key order would hash to two shards and merge as two wrong partial windows (cerberus's own `rowsCursor.internLabels` canonicalizes by sorted k=v for exactly this reason). The shard key is `cityHash64(toString(mapSort(Attributes)))` (or sorted keys+values zipped), added as a typed chsql Frag with a CH-level unit test asserting equal hashes for `map('a',1,'b',2)` and `map('b',2,'a',1)`, plus an A-vs-B fixture whose seed deliberately inserts the same series with permuted map key order. Hard boundary: never under any cross-series node. LogQL metric queries join in phase 4 (the `RangeLWR` family is already in the safe set).

## Parallel execution

Scheduler in `Executor.Execute`, on `errgroup` + `semaphore` (`golang.org/x/sync` already in go.mod; zero new deps):

1. **Emit first.** `chsql.Emit(ctx, slice.Plan)` for all K slices before any query opens — emit failure aborts with zero CH work (`engine: emit:` classification preserved). The now64-string assertion from §Routing runs here.
2. **Two-stage weighted admission (degrade-don't-reject).** `admit.Middleware` charges weight 1 at handler entry — before the route is known, so front-door weighting is unimplementable as stated. At routing time the solver `TryAcquire`s `(P−1)` additional admit units on the existing `semaphore.Weighted` (the hook admit.go:91 reserves exactly this). On top-up failure it does **not** 503 and does **not** proceed at full P: it clamps effective parallelism to `1 + units obtained`, down to fully sequential shard execution — still correct, still per-statement memory-bounded, just slower. Releases are exactly-once at `shardCursor.Close`. Metric: `cerberus_solver_parallelism_clamped_total`; regression test pins that top-up failure never changes the response, only latency.
3. **Atomic gate acquisition — no hold-and-wait.** One global `Gate` sized `MaxOpenConns − reserve` (reserve ≈ 8 conns for route-A traffic, `/readyz` Ping, metadata). The Executor acquires **all** `K_eff = min(K, P_eff, gate/2)` slots atomically (`Gate.Acquire(ctx, K_eff)`) before opening any child cursor, and releases them all at Close. Per-shard incremental acquire — producers holding some slots while waiting for more — is the textbook hold-and-wait shape and is rejected. The `gate/2` cap guarantees ≥2 routed requests can always make progress. Saturation degrades to FIFO queueing bounded by the solver timeout, never to wedge. A dedicated deadlock-hammer stress lane proves it.
4. **Wall-clock deadline.** Cerberus has no per-query deadline today (only `ReadHeaderTimeout`); a K-shard fan-out pinning slots+conns until client disconnect is a worse starvation shape than a single cursor. `Config.Timeout` (`CERBERUS_SOLVER_TIMEOUT`, default 60s) bounds `Execute` end-to-end via `context.WithCancelCause` with a dedicated sentinel cause, so a gateway-chosen deadline is **breaker-neutral** (the breaker currently counts `DeadlineExceeded` as failure) and maps to a typed 504.
5. **`ErrAcquireConnTimeout` is breaker-neutral — committed, not "to be decided".** It signals local pool sizing, not CH health; counting it as failure lets two routed requests under mixed load trip the shared breaker and 503 all three heads against a healthy ClickHouse. `breaker.record` gains the neutral arm next to the existing code-241/`Canceled` filters, with a regression test. The gate makes shard-side acquire-blocking structurally rare; the neutrality makes the residual race (route-A cursors occupying the pool) harmless to the breaker.
6. **Breaker failure dedup.** A degraded CH can fail all `P_eff` concurrent shard opens before cancellation propagates — up to P failure records from one logical request, tripping the shared breaker P× faster than today. The Executor records at most **one** breaker failure per logical request: the first real shard error records normally; siblings (induced-cancel or concurrent real failures after the latch) record breaker-neutral via the cancel-cause sentinel. Regression test: K-shard request, all P opens fail concurrently, failure counter advances by exactly 1.
7. **Per-shard execution.** `g, gctx := errgroup.WithContext(...)` under the cause-carrying ctx; `g.SetLimit(P_eff)`. Each producer derives its own ctx via `chclient.WithProgressFor(gctx, langName)` (one progress recorder per ctx key; sharing would corrupt rows/bytes histograms), opens `Client.QueryCursor`, drains into a bounded `chan chclient.Sample` (cap 4096), closes its cursor. Producers select on `gctx.Done()` while sending — provably terminating under goleak. **Launch order is newest-slice-first** (minimizes live-edge snapshot skew, §Failure); composition order is oldest-first regardless (channels buffer).
8. **Server-side memory.** Default: each shard keeps the per-statement `max_memory_usage = CERBERUS_CH_QUERY_MAX_MEMORY` (worst case per routed request = `P_eff × cap`, e.g. 3 GiB — a config-derived constant, bounded by P, not K). The collateral-241 hazard (concurrent routed requests pushing CH's *server-total* limit, failing innocent route-A queries nondeterministically) is bounded three ways: the opt-in apportionment knob (`MemoryApportion`: per-shard cap = `cap/P` with a 256 MiB floor, holding total exposure at exactly the single-query cap); gate sizing documented in GiB-equivalents (`slots × per-shard cap ≤ CH headroom`) in `docs/operations.md` with the required `max_server_memory_usage` arithmetic; and a compose-smoke variant running N routed requests concurrently to catch collateral 241s pre-production.
9. **Config validation — fail fast.** When `Mode != "single"`: `MaxOpenConns` must be explicitly set (task #81 is a hard phase-0 prerequisite; the shipped default rises to 32 when sharding is on, enforced by validation, not convention); `gate = MaxOpenConns − reserve ≥ max(P, 4)`; `P ≤ gate/2`. Violation → refuse to start. The arithmetic is pinned by a regression test, and defaults are derived jointly in one place in `internal/config`.
10. **Required stress pins** (promoted from nice-to-have): 64 concurrent routed requests **plus 64 concurrent route-A requests** against `MaxOpenConns=32` — zero `ErrAcquireConnTimeout` surfacing as 5xx, zero breaker trips, bounded p99 admission latency; plus the deadlock hammer.

**Budgets:** every shard ctx carries the shared `chclient.SampleBudget` (per-REQUEST max-samples 422 parity, verbatim upstream message). Eager path (`QueryPlan`): same Executor; the engine drains the composed cursor to `[]Sample`, keeping both engine bodies twinned, with `ObserveQueryInflight`/`ObserveStage` applied identically plus a new `solve` span wrapping the fan-out.

## Result composition

Composition is **concatenation, not evaluation**. Each anchor belongs to exactly one slice; every shard emits final per-(series, anchor) values in the canonical 4-col shape (`Lang.ProjectSamples` was applied before the split), so cerberus computes nothing: zero arithmetic, zero window logic, zero merge-by-key. `shardCursor` (internal/solver/cursor.go) implements the 4-method `chclient.Cursor` verbatim; the entire wire path — handler `defer Close`, `matrixFromCursor` drain/group/sort/clip, `writeEngineHeaders`, `writeJSON` — runs unchanged. Handlers drain fully before writing a byte, so the all-or-nothing wire contract is preserved for free: a shard failure is one typed error response, never a partial body. Oldest-first drain order keeps per-series timestamps nearly ascending so `matrixFromCursor`'s insertion sort stays ~O(n); anchors are disjoint, so this is never a k-way value merge.

Two guards added beyond the obvious:

- **Per-request output cap.** Route B's purpose is making previously-422 high-cardinality×high-F queries *succeed* — and success lands O(rows) in `matrixFromCursor`'s buffers (handler.go:947). A 50k-series × N=241 result (12M rows) passes the 50M sample budget but would ship multi-GiB into the shared gateway heap, converting a clean per-statement CH 422 into a process OOM that kills all three heads. `shardCursor.Next` enforces `Config.MaxOutputRows` (default 2M) with a **new typed 422** — its message must NOT reuse upstream's verbatim max-samples text (that message is a parity surface).
- **Cross-shard label re-interning.** Interning is per-cursor (`rowsCursor.internLabels`), so the same series arriving from K shards would hold K label-map copies alive during the drain. `shardCursor` re-interns across children keyed by `format.CanonicalKey` — per-request state, born and dying with the request, so the no-caching invariant is untouched. Labels remain read-only throughout.

## Parity

**Structural argument.** Route B contains no new evaluator and no new SQL templates: every shard is the same post-optimize plan, re-anchored, emitted by the same `chsql.Emit` the 536/536 compat gate already proves against reference Prometheus. Window membership is a pure function of `(anchor, Range, Offset)`; each shard's input scan covers its windows fully (offset-aware bounds, §Decomposition), so every (series, anchor) value is computed by byte-identical SQL over identical input rows — including extrapolation arithmetic, empty-window drops, NaN handling, and min-window-size guards. Parity reduces to one lemma: *ReanchorRange over disjoint anchor sub-grids, concatenated, equals the original.*

**Transitivity is not assumed where the corpus is thin.** Route-A-as-oracle holds only for shapes the compat corpus exercises, and two live gaps are already known: (a) `lowerRangeFn` (internal/promql/lower.go:1497–1521) unconditionally clobbers @-pinned `Start`/`End` with the step grid whenever `ctx.step > 0` — a wrong-results route-A bug the zero-@-query corpus never sees, filed and fixed in phase 0 (the defensive grid-prediction check in §Routing protects the solver both before and after that fix); (b) matrix-mode anchor timestamps are offset-shifted (`End − Offset − i·step`) with no compat or golden coverage for matrix+offset wire timestamps. Phase 0 therefore extends the corpus (`cerberus-test-queries.yml` + seeded fixtures) with @-literal, `@ start()/end()`, matrix+offset, negative-offset, and co-prime res/step nested-subquery range queries **before** any lane is declared the workhorse.

**Lanes** (force knob: `CERBERUS_EVAL_ROUTE=sharded`):

1. **A-vs-B chDB differential** (per-PR workhorse): for every seeded TXTAR fixture the Planner routes under force-sharded, execute route A's SQL and all K shard SQLs under chDB, sort, compare. Comparator specified now, not discovered: NaN-equals-NaN bit-class comparison (route A legitimately emits literal `nan`; `reflect.DeepEqual(NaN, NaN)` is false), NaN-stable total order in `sortRows` (key = `(isNaN, value)`), and deliberately seeded duplicate-timestamp + NaN-emitting fixtures in the eligible set — silently pruning boundary fixtures is the only escape hatch the no-allowlists rule leaves open, so the comparator must make it unnecessary. Tie-prone shapes (phase-3 topk) compare per-anchor value multisets + tie-class membership, never tied-row identity.
2. **Forced-route compatibility job**: a second `compatibility/prometheus` job with `CERBERUS_EVAL_ROUTE=sharded` injected — the full upstream corpus on route B for every eligible query, `X-Cerberus-Strategy` asserted per response so coverage is *measured* (queries the Planner never routes are known-untested, not assumed-covered), zero diffs, no allowlists.
3. **Upstream-engine referee** (test-only): rapid-generated series fed to both cerberus's emitted SQL (chDB) and a real `promql.NewEngine` over `storage.NewListSeries` — the tsouza/prometheus fork already resolves promql+storage at zero new module cost. This is the only lane that catches *shared* A/B divergence (an edge case route A itself gets wrong makes A-vs-B agree while both diverge from reference). Gates every operator family added to the safe set.
4. **Property arm**: the same generated dataset+query compared three ways — single-SQL vs sharded-union vs the from-scratch oracle.
5. **Compose-level cross-route HTTP differential**: same build, route A vs route B over live HTTP with real clickhouse-go — chDB's lenient type coercion (the documented UInt8→float64 gap) means the chDB lane alone cannot prove the shard decode path against prod-driver strictness.
6. **Schema-override + MV arm**: the eligible fixture set re-run with `CERBERUS_SCHEMA_METRICS_*_TABLE` pointed at renamed clone tables, and with the MV-substitution rule forced together with force-sharded (the optimizer runs before the Planner; the composition is otherwise untested).
7. **Slice-invariance fixture family** (the #92 contract): counters with resets placed at the first sample inside a shard's padding, at the first sample of a slice's oldest window, and straddling the seam — generated for several K values so the seam moves across the reset.
8. **Unit**: rapid property tests over random `(Start, End, Step, Range, Offset, K)` — slice-union == original anchor set, pairwise disjoint, `End_j` on-grid, per-spine-LEVEL inner-anchor union equality, singleton-tail merge, all clamps; ReanchorRange-vs-widenSubquerySpine equivalence on post-optimizer plans; deep-copy isolation.

**Error parity:** shared `SampleBudget` keeps the verbatim upstream max-samples 422 per request; per-statement memory cap keeps the code-241→422 mapping; phase-3 `throwIf` join-cardinality guards fire inside whichever shard contains the offending anchor with the same fixed exception text. The output-rows cap is a new, deliberately distinct 422.

## Memory model

Worked case — 5M samples / 500 series / 1h, `sum(rate(metric[5m]))` @ 15s; F = 20, N = 241; measured route-A demand 2.12 GiB vs 1 GiB cap (run-27277793810, OOM 422):

- **K** = min(8, floor(241/16)=15, floor(60m/5m)=12) = **8**; m = ceil(241/8) = 31 (slices 0–6: 31 anchors; slice 7: 24). N×F ≈ 4820 ≥ 4000 → routes.
- **CH side, per shard:** slice span = 7.5 min; input window = 12.5 min (span + 5 min lookback) ⇒ ~1.04M scanned rows. Expanded (sample, anchor) pairs ≈ 31 × 417k ≈ 12.9M vs 100.4M unsharded — 7.8×, tracking 1/K (the arrayJoin only materializes in-slice anchors). Estimated peak ≈ 2.12 GiB / 7.8 ≈ **278 MiB** — under the 1 GiB cap with 3.7× headroom.
- **CH side, concurrent:** P=3 ⇒ ≈ 834 MiB across 3 statements, each individually capped; worst-case per routed request = `P × cap` = 3 GiB (or exactly `cap` with apportionment on). Scan refetch: 8 × 1.04M ≈ 8.3M rows vs 5.42M = 1.54× — inside the `K ≤ OuterRange/D` ⇒ ≤2× design bound (which depends on the #93 offset-aware pushdown; without it, refetch is K× and phase 1 does not flip the default).
- **Cerberus side:** output rows identical to route A — 500 × 241 ≈ 120.5k samples; `matrixFromCursor` buffers ≈ 14 MB either way, unchanged. Solver overhead: P × 4096-sample channels ≈ 1.4 MB + ≤P driver block buffers (low single-digit MB) + K SQL strings (KB). General bound: cerberus peak = O(output rows) [pre-existing] + ~10–15 MB [new, fixed], with output rows now additionally capped by `MaxOutputRows`. Wire stays ~120k reduced rows (single-digit MB), not the 89 MiB / 4.99M raw rows an evaluator-in-gateway design would ship.
- **Honest gap:** per-shard CH peak scales with cardinality × F / K — a *reduction*, not a hard bound. A dataset ~4× this one OOMs even at K=8 and surfaces the same 422 as today (a strictly better threshold, never a regression). High-D shapes fall back to route A + #92. The scaling harness gains two axes with never-weaken baselines: **wire rows/bytes shipped** and **Go peak heap** (`runtime.ReadMemStats` around the solve+drain path), sweeping **cardinality** — the axis the router cannot see — not just anchors.

## Failure, cancellation, observability

**Error contract: first-error-wins, all-or-nothing, cause-threaded.** `context.WithCancelCause` under errgroup is mandatory, not optional: the first **real** shard error is set as the cancellation cause; siblings' induced `context.Canceled` never enters the latch; `shardCursor.Err()` returns `context.Cause(gctx)` when non-Canceled. Without this, a sibling's induced cancel racing ahead of the real error would flip a deterministic 422 into a "client gone" no-op depending on goroutine scheduling — a flaky required compat check, the worst failure class. Types preserved unwrapped: `*MemoryLimitError` → 422 breaker-neutral; `*TooManySamplesError` → 422 verbatim; output-cap → typed 422; `ErrCircuitOpen` → 503 + `Retry-After: 5`; solver-timeout sentinel → 504 breaker-neutral; `context.Canceled` (client gone) → breaker-neutral; else 502. Open-time errors keep the `engine: execute:` prefix; decomposition failures get a new `engine: solve:` arm (→ 500; should never fire on a routed plan by Planner construction). `classifyDrainError` (handler.go:634) needs zero changes. Deterministic chaos matrix: for each shard index × error class (241, sample-budget, output-cap, transport, CH exception), inject and assert exact wire status + verbatim message, under `-race` with `GOMAXPROCS` variation.

**Breaker interaction:** one failure record per logical request (§Parallel #6); `ErrAcquireConnTimeout` neutral (§Parallel #5); solver-timeout neutral. **HALF-OPEN pre-flight:** a routed request needs K admissions but HALF-OPEN admits one probe — a fan-out would burn the probe slot on a doomed request and could wedge recovery. The Executor peeks breaker state before emitting; if not CLOSED, it fails fast with `ErrCircuitOpen` without consuming the probe, leaving recovery probing to route-A traffic. Chaos case: trip breaker, send routed query during HALF-OPEN, assert single-probe semantics preserved.

**Lifecycle:** `shardCursor.Close` is `sync.Once`: cancel gctx → `wg.Wait` all producers → Close every child cursor (each `closeOnce`: conn released, per-shard progress recorder flushed, execute span ended) → release all gate slots + admit top-up units → end solve span → return first non-nil child close error. Client disconnect propagates reqCtx → gctx → every shard. goleak gates every handler entrypoint with routed queries; the chaos lane gains shard-kill-mid-drain (typed error, zero leaks, all conns returned). Cancellation burns conns (driver closes on cancel) — up to P per aborted routed request; `cerberus_solver_conns_burned_total` + an ops-docs sizing note (worst burn rate = abort_rate × P; if the compose panel-abort-storm chaos scenario breaks route-A p99, lower P before raising the pool).

**In-request retries: rejected for v1.** One transport blip on shard 7 of 8 discards K shards of work and client retries cost K× — accepted, with breaker dedup as the damper. A bounded single re-execution of a failed shard's statement (transport-class open errors only, never 241/budget/mid-drain) is stateless re-execution, not memoization, and is recorded here as invariant-compatible future work, default off.

**Cross-statement snapshot skew — an accepted, documented anomaly class.** K statements execute over a window of wall-clock seconds against a live table; CH offers no cheap cross-statement snapshot. Late-arriving rows (live edge or backfill) can land between shard executions, producing seam artifacts (a phantom rate discontinuity at a slice boundary) no single-snapshot evaluation could emit — the same class Thanos/Mimir distributed evaluation accepts. Every seeded lane is structurally blind to it. Package: (a) documented explicitly in this doc + `docs/operations.md` with the seam-artifact signature for incident triage (self-heals on refresh); (b) newest-slice-first launch order minimizes live-edge exposure; (c) `cerberus_solver_shard_spread_ms` histogram makes the exposure window observable; (d) a seed-while-querying compose-smoke scenario asserts the anomaly stays confined to the freshest `D + slice-span` suffix. The single-statement alternative — emitting the K slices as arms of one `UnionAll` (single snapshot, single 1 GiB cap) — is recorded as rejected: it surrenders exactly the per-statement memory headroom that motivates the design. If intra-response consistency for `end ≈ now` is later judged non-negotiable, watermark-bounded routing is the named follow-up decision.

**Telemetry spec** (in-PR, not ad-hoc): rows/bytes histograms gain a `route` attribute plus per-request aggregates (`cerberus_solver_shards_per_query`, summed rows/bytes), so capacity trends survive the one-becomes-K shift; `X-Cerberus-CH-Millis` for routed = max shard wall, with `X-Cerberus-Solver-CH-Millis-Total` for the sum, documented in `docs/observability.md`; span topology pinned by a unit test — solve span parents all per-shard execute spans via the gctx-derived ctxs, `ObserveStage(StageExecute)` wraps the fan-out once with a shard-count attribute; the strategy header grammar is composable from day one (`mv-substituted,sharded-timeslice`). Shadow/observability headers: `X-Cerberus-Shards`, `X-Cerberus-Route-Decision` (decision + reason vocabulary from `Decision.Reason`).

## What stays on route A

Route A — one optimized plan, one statement, all reduction in CH — **remains the default and the overwhelming majority**. The solver is the exception, and every check in it fails toward A. Permanently or currently on A:

- All instant queries (until phase 3b, and cross-series instant aggregations permanently).
- Any plan containing a node without a `SliceInvariant` marker; any windowed node with unpinned bounds; any `now64` in any position; any grid-prediction or commensurability mismatch.
- High-D shapes where `K ≤ OuterRange/max(D, Step)` clamps below 2 (e.g. `rate(m[30m])[2h:1m]` over 2h) — the documented floor; #92 is their relief.
- Below-threshold queries: typical dashboard panels (`rate[5m]` @ 1m, modest N×F) never route; the default thresholds are tuned so the routed fraction of representative traffic stays under single-digit percent, pinned by a perf guard.
- Global `Limit`/`OrderBy`, LogQL line-limit queries, scalar-heavy plans that cannot hoist.
- Single windows exceeding CH memory (instant `quantile_over_time(m[30d])` on a dense series) — unfixable by slicing on either dimension; the ceiling, with the reference-engine successor as the named escalation.

## Migration plan

- **Phase 0 — prerequisites** (each PR independently valuable, no behavior change): (a) chclient pool config — task #81 — with the joint config-validation arithmetic and the committed `ErrAcquireConnTimeout` breaker-neutrality + regression test; (b) `chplan.ReanchorRange` + `SliceInvariant` registry + equivalence/deep-copy tests; (c) `chclient.SampleBudget`; (d) route-A parity fixes that must precede any routing work: the `lowerRangeFn` @-clobber bug, and the `quantile_over_time` literal-phi t-digest approximation (extend the exact arraySort-interpolation branch the computed-phi path already uses — a live exactness gap on today's default path, orthogonal to routing); (e) corpus extension (@/offset/nested fixtures + compat queries); (f) #93 — offset-aware scan-bound pushdown on the PromQL matrix emitters (route-A golden churn, independently fixes full-history scans); (g) forks-monitor watch-scope widening + version-skew CI gate (fork base == compat reference container tag) — hardens today's parity story and keeps the phase-5 option open; (h) rewrite the now-stale "one query per request — no scatter-gather" prose in `docs/performance.md` + engine comments before any code ships, so reviewers enforce the new invariant set, not the old one.
- **Phase 1 — framework ships dark:** `internal/solver`, engine wiring, headers (including the shadow `X-Cerberus-Route-Decision` emitted while execution stays on A), telemetry spec, config validation, default `Mode="single"`. Safe set: the phase-1 marker list; PromQL query_range only. Gates: A-vs-B chDB lane green over every eligible seeded fixture; goleak; fanout-lint over each shard plan+SQL; slicing-geometry unit suite; deadlock hammer; the mixed-load stress pin; the byte-reproduction pin (`single` == today).
- **Phase 2 — proof + default flip:** forced-route compat job; property arm; upstream-engine referee lane; scaling constructs (cardinality sweep with per-shard bound, wire-rows and Go-heap axes); chaos fault points; routed-fraction guard tuned against shadow-header data. Flip to `auto` only after zero diffs on the full forced-route corpus for 3 consecutive runs **and** an observed routed fraction within budget.
- **Phase 3 — widen the safe set:** one node family per PR (TopK, step-aligned VectorJoin/VectorSetOp, HistogramQuantile, nested spines under the lcm clamp, AbsentOverTime), each with its own marker proof, referee coverage, and forced-route evidence. **Phase 3b:** instant inner-grid slicing for slice-mergeable reducers. **#92 placement:** the A-prime cumulative rewrite proceeds independently on route A; its new idiom enters the solver's safe set only via an explicit `SliceInvariant` proof + the reset-at-seam fixture family — never by default.
- **Phase 4 — second dimension + second head:** series-shard with the canonical `mapSort` hash key + permuted-map-order fixtures; LogQL metric queries with a forced-route loki-compliance job. Mutation phase at 95% efficacy for `internal/solver` once the package stabilizes.
- **Phase 5 — reserved successor:** the narrow reference-engine solver (upstream `promql.Engine` over a ~400 LoC CH-backed Queryable) for the residue this design structurally cannot reach, behind the **same** `Engine.Solver` seam, strategy-header vocabulary, route knob, and pool/gate/budget substrate. Built only when shadow-header `reason=high-D`/`reason=instant` production frequency demonstrates the need; the phase-0 version-skew gate is its standing prerequisite.

## Alternatives considered

- **Reference-Engine Solver** (upstream `promql.Engine`/`logql.Engine` over a CH-backed Queryable). The strongest parity story for the routed shapes — staleness, lookback, @/offset, NaN are upstream code — and the most general. Rejected *for now* on three grounds from the judging: it inverts the memory architecture (O(selected samples) in the shared gateway heap, where one budget-accounting bug converts a per-query CH 422 into a process OOM killing all three heads — the worst failure-mode trade available); it ships raw samples over the wire (89 MiB for the worked case vs single-digit MB of reduced rows); and "parity by construction" is conditional on a permanent fork-vs-reference version-skew discipline. It is explicitly **not** discarded: it is the named phase-5 successor for the ceiling shapes, and this design's substrate is deliberately a strict subset of what it needs.
- **Plan-DAG Decomposition** (from-scratch local operators over k-way-merged leaf streams). Best-in-class cerberus-side memory bounds, but parity-by-reimplementation of exactly the hardest semantics (extrapolation, interpolation, staleness, float-op ordering), making every upstream evaluator change a port task forever — disqualifying under non-negotiable exact parity for a single-maintainer project, at 10–14k LoC across 9–13 weeks. Its best ideas are adopted here: the upstream-engine referee lane, the atomic semaphore acquisition, the cancel-cause discipline, and the t-digest finding.
- **Pure-SQL single statement (route A + #92 alone).** The A-prime cumulative rewrite attacks the same arrayJoin fan-out and should ship regardless — but the empirical numbers say it is not sufficient alone: the measured spike demanded 2.12 GiB against the 1 GiB cap, per-statement demand still scales with cardinality × F with no second knob, and a single statement has a single cap. The `UnionAll`-of-slices variant (one statement, one snapshot) was likewise rejected: it inherits the single 1 GiB cap, surrendering the per-statement headroom that is the entire point. Sharding and #92 are complementary — #92 lowers the curve, K divides it.

## Risks

Remaining minors (majors are addressed by design changes above):

- **HALF-OPEN residual:** pre-flight peek is racy by nature; a routed request can still occasionally consume a probe. Bounded by the chaos case; accepted.
- **Conn-burn churn** on panel-abort storms (≤P per abort): counter + dashboard alert + documented sizing; P is the knob.
- **Retry amplification** on flaky CH (K× per client retry): breaker dedup is the damper; bounded in-request shard re-open recorded as default-off future work.
- **Observability trend break** at flip time even with the `route` attribute: capacity dashboards need a one-time annotation.
- **Schema-override / reserved-alias collision:** config-load validation rejects future column overrides colliding with emitter aliases (`anchor_ts`, `window_pairs`, …); override arm in the differential lane.
- **Misconfiguration surface** (pool vs gate vs P): fail-fast validation + pinned arithmetic; residual risk is operator override of validated defaults.
- **NaN/tie comparator drift:** comparator is specified and fixture-seeded; risk is future contributors weakening it — the no-allowlists meta-greps apply.
- **Residual threshold cliff:** route A's OOM boundary still exists for under-routed shapes; diagnostic recipe (strategy + shards headers) documented in `docs/operations.md`.
- **Throughput tax on mis-routed queries:** K statements cost more total CH CPU/IO than one; routed-fraction guard + nightly profiler watch.
- **Snapshot-skew residue:** bounded and observable, not eliminated; watermark-bounded routing is the named follow-up if unacceptable.

## Pointers

- `docs/performance.md` — the relaxed lock; rewritten in phase 0 to state the new invariant set (route A default, solver exception, no caching, all-or-nothing wire contract).
- `docs/evaluation-architecture.md` — the operator classification this design's safe set maps onto; the ceiling + reference-engine escalation section lands there.
- `docs/engine.md` — the `Lang` contract and the engine seam (`QueryPlan`/`QueryPlanCursor`) the solver hooks.
- `docs/operations.md` — pool/gate/memory sizing arithmetic, collateral-241 headroom, seam-anomaly triage, conn-burn sizing.
- `docs/observability.md` — header semantics (`X-Cerberus-Strategy` grammar, `X-Cerberus-Shards`, `X-Cerberus-Route-Decision`, CH-millis definitions), span topology, new metrics.
- `docs/test-strategy.md` — where the A-vs-B lane, forced-route compat job, referee lane, and stress pins slot into the 12-layer map.
