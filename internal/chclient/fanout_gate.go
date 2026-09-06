package chclient

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"golang.org/x/sync/semaphore"
)

// fanout_gate.go — the data-shard fan-out admission gate (cerberus issues
// #3081, #3128, epic #3074).
//
// HISTORY: cerberus issue #3081 first introduced this gate inside
// internal/solver's Executor, acquired ONCE per routed (solver-split)
// dispatch with weight kEff x DataShardCount. That placement bounded only
// the sharded-pushdown "route B" path; the ordinary, non-split "route A"
// path — the vast majority of real traffic — dispatches straight through
// this package with NO involvement from internal/solver at all, and a
// route-A query against a `Distributed`-engine table ALSO fans out to
// DataShardCount-many real per-shard ClickHouse statements, structurally
// identical in kind to what the gate was built to bound. Because route A
// never acquired it, the gate bounded only a fraction of the aggregate
// concurrent per-shard statement count ClickHouse actually saw — cerberus
// issue #3128's real e2e evidence (peak concurrent per-shard statements
// 1.5x-4x over DataShardFanoutCap on both N=2 and N=4 datashard lanes).
//
// THE FIX: the gate moves HERE, to the one seam every dispatch this
// package makes — route A's single statement AND every one of route B's
// per-shard dispatches alike — passes through: queryOpen (the row-decoder
// path every synchronous drain method and QueryCursor's default strategy
// share) and queryCursorColumnar (the columnar matrix-decode strategy's own
// dial). internal/solver's Executor no longer acquires this gate itself
// (see executor.go's admitAndGate, cerberus issue #3128) — its own K-shard
// fan-out reaches ClickHouse through THIS Client's QueryCursor per shard,
// so each of its kEff dispatches acquires weight DataShardCount here,
// summing to the exact same kEff x DataShardCount the old single upfront
// acquisition charged, just decomposed to the point of actual dispatch
// instead of charged in one lump ahead of it. Charging at dispatch time
// rather than admission time is what closes the route-A gap: route A is
// exactly one such dispatch (weight DataShardCount), so it is now bounded
// by the identical mechanism with no separate code path.
//
// DataShardCount <= 1 (every deployment that predates cerberus issue #3081,
// and every single-data-shard deployment) leaves dataShardFanoutGate nil —
// this file's ENTIRE mechanism is structurally unreached, matching the
// pre-#3081 behaviour bit-for-bit.
//
// CANCELLATION FIX (cerberus issue #3128's post-move investigation, real e2e
// dispatch runs 34032272914's second attempt: N=2 peak concurrent 10 > cap 8,
// N=4 peak concurrent 24 > cap 8; every run's own client-side accounting
// stayed within cap the whole time, ruled out with a temporary held-weight
// counter that never once observed an over-cap acquisition). The root cause:
// acquireDataShardFanout's release used to fire the instant
// queryOpen/queryCursorColumnar's underlying c.conn.Query / pool.Do call
// RETURNED — but when that call returns because ctx was cancelled
// (internal/solver/executor.go's errgroup.WithContext cancels every sibling
// shard's ctx the instant ANY one of a routed query's kEff concurrently-
// admitted shards errors, and an HTTP client disconnect/timeout cancels
// r.Context() the same way), clickhouse-go v2's own connect.cancel()
// (conn_process.go, both process() and firstBlock()) sends a single
// ClientCancel packet and closes the LOCAL connection immediately — it does
// NOT wait for ClickHouse to confirm the (possibly still-running, possibly
// Distributed-fanned-out) statement has actually stopped. Releasing the
// gate weight right there let a new dispatch admit on top of ClickHouse-side
// work that had not actually stopped yet. The observed overshoot magnitude
// was consistent with exactly this: on both real runs above, (peak
// concurrent - cap) was an integer multiple of DataShardCount (16 = 4x4 on
// N=4, 2 = 1x2 on N=2) — i.e. a small, plausible number of
// premature-cancellation releases, not a diffuse accounting error.
//
// The fix: acquireDataShardFanout's release closure now checks ctx.Err() at
// the instant it runs (not the error the underlying call returned — a typed
// *clickhouse.Exception means CH already finished the statement on its own
// and needs no help). A non-nil ctx.Err() means THIS dispatch's own ctx was
// cancelled or hit its deadline — the reason the call returned, not a normal
// server-side finish — so before releasing the weight, killDataShardQuery
// issues `KILL QUERY WHERE query_id = ? SYNC` for this dispatch's own
// query_id (queryIDFromContext, stamped by queryContext before the gate is
// ever acquired) on a FRESH, uncancelled, short-lived connection/context —
// never the dispatch's own already-cancelled one. SYNC blocks until
// ClickHouse itself confirms the COORDINATING statement is dead (or was
// already gone, the common race-free outcome when it finished naturally in
// the tiny window between local cancellation and this call landing) before
// the gate weight releases — see this file's own "Residual gap" doc below
// for why that confirmation, real and useful as it is, does not by itself
// prove every downstream per-shard statement the coordinator had already
// fanned out has also stopped. A failed or slow KILL QUERY (network error
// reaching CH, bounded by killDataShardQueryTimeout) is logged, never
// fatal — the weight still releases unconditionally afterward, so a KILL
// QUERY failure can never leak gate capacity, only (rarely) fail to close
// this specific race.
//
// The normal, non-cancelled finish path is untouched: ctx.Err() is nil, the
// check is a cheap single field read, and no extra round-trip is ever made
// for the overwhelming majority of dispatches that simply finish.
//
// Residual gap, round 3 findings (cerberus issue #3128, real e2e dispatch
// runs 34043234494 through 34048062673, AFTER this fix landed): `datashard
// (N=2)` and `(N=4)` both still show a real point-2 overshoot even after the
// cancellation fix above. Direct instrumentation against a real cluster
// (a temporary chclient acquire/release diagnostic, and an
// e2e-datashard-verify.mjs point-2 query extended with query_kind,
// initial_query_id, and full query text — both since removed, see the PR
// that added and removed them for the full trace) resolved THREE separate
// questions this investigation had open, two of them conclusively:
//
//  1. CONFIRMED AND FIXED — seeder contamination. is_initial_query=0 also
//     counts `just e2e-seed-rolling`'s rolling seeder (test/e2e/seed/cmd/
//     seed), which writes to ClickHouse DIRECTLY over the native protocol —
//     bypassing cerberus, and so this gate, entirely — throughout the
//     burst window. Its Insert/Alter children were never subject to this
//     gate, so counting them against DataShardFanoutCap tested something
//     the gate was never built to bound. e2e-datashard-verify.mjs's point 2
//     now scopes its assertion to query_kind='Select' — the only kind this
//     gate's one seam (queryOpen/queryCursorColumnar) ever dispatches. This
//     alone fully explained at least one real run's N=2 failure (unfiltered
//     peak 9 > cap 8; Select-only peak 7 <= cap 8, with the 2-statement
//     excess being exactly 1 Insert + 1 unrelated overlap).
//  2. REFUTED — self-join / distributed_product_mode=global amplification.
//     A dispatch whose own child count exceeds DataShardCount (grouped by
//     the CHILDREN's shared initial_query_id, which is exactly the
//     coordinator's own globally-unique per-dispatch query_id —
//     mintQueryID's process-wide atomic counter makes two distinct
//     dispatches sharing one id structurally impossible) was hypothesized
//     to be a query that self-references the Distributed table more than
//     once (distributed_query_settings.go's settingDistributedProductMode
//     doc lists the shapes: TraceQL structural operators, nested-set-
//     annotate, `compare`). Real over-width dispatches were pulled back to
//     their FULL emitted SQL text (not the 120-char prefix Select-only
//     filtering alone needed) and traced through the actual Go dispatch
//     path (internal/api/tempo's handleSearch -> engine.QueryPlan for the
//     dominant offending shape, a plain non-structural TraceQL attribute
//     filter): the emitted SQL is a flat, single-table
//     `SELECT ... FROM otel_traces WHERE ... AND match(...)` with no JOIN,
//     subquery, or CTE, dispatched through EXACTLY ONE chclient.Client.Query
//     -> QueryCursor call — Route A, one queryOpen call, one gate
//     acquisition of weight DataShardCount, confirmed by direct code
//     tracing (internal/engine/engine.go's QueryPlan, internal/chclient/
//     client.go's Query/QueryCursor) to be the ONLY dispatch this logical
//     HTTP request makes. Self-join amplification, solver route-B K-shard
//     multiplication, and a same-context multi-round-trip pattern are all
//     therefore ruled out for this shape: cerberus's own code makes exactly
//     one physical dispatch, and the FULL emitted SQL never re-reads the
//     Distributed source.
//  3. CONFIRMED PRESENT, ROOT CAUSE NOT YET LOCATED — real ClickHouse-side
//     multiplication independent of cerberus's own dispatch code. With (1)
//     and (2) fully accounted for, MANY real dispatches' own initial_query_id
//     groups still show far more Select-kind is_initial_query=0 children
//     than DataShardCount predicts (observed: a DataShardCount=2 dispatch
//     with 3 children, own internal peak concurrency 2 — consistent with one
//     extra, largely-sequential statement; a DataShardCount=4 dispatch with
//     15-18 children, own internal peak concurrency up to 10 — genuinely
//     CONCURRENT children, not sequential retries, at roughly 2.5x the
//     expected fan-out width). Ruled out as causes: cerberus's own gate
//     bookkeeping (PR #3132's held-weight counter, and this round's own
//     acquire/release diagnostic, never observed an over-cap acquisition or
//     a cancellation-driven release in the windows captured); query_id
//     collision (structurally impossible, see above); cluster topology
//     (deploy/helm/cerberus/templates/clickhouse/configmap-config.yaml's
//     `remote_servers` renders exactly DataShardCount `<shard>` blocks, each
//     with exactly `replicas` (1, this e2e lane) `<replica>` entries,
//     confirmed by direct template inspection); and every cerberus-side
//     dispatch-multiplication mechanism named in (2). ClickHouse's own
//     documented cancellation-propagation behavior (KILL QUERY / a native
//     ClientCancel both drive the SAME in-process pipeline-cancel path that
//     synchronously waits for shard-side cancel acks — see
//     RemoteQueryExecutor::cancel()/tryCancel() in ClickHouse's own source)
//     argues AGAINST hypothesis 1 above being the dominant mechanism either,
//     though it has not been exhaustively re-tested now that (1) and (2) are
//     closed. What remains is consistent with a real, hard ClickHouse-
//     server-side or transport-layer effect (e.g. connection-level retries
//     under genuine concurrent-connection pressure against this e2e lane's
//     deliberately thin per-shard pod sizing — see cerberus-values-
//     datashard.yaml's own "sized down" doc) that creates extra per-shard
//     statement executions this gate has no visibility into and cannot
//     bound from the client side. Cerberus issue #3128's own filing text
//     explicitly forbids a raised threshold as the resolution, so this is
//     NOT worked around here; it stays open, and the concrete next step is
//     ClickHouse-side profiling (system.text_log at a higher verbosity
//     during a burst, or EXPLAIN PIPELINE against the exact offending SQL)
//     that this investigation's tooling (an external e2e verify script and
//     cerberus's own client-side logs) cannot reach.
//
// Route A's admission gap this file closes (Route A was completely
// ungated before #3128's move) is a genuine, confirmed improvement over the
// pre-move state regardless, and the seeder-contamination fix (1 above) is a
// genuine, confirmed improvement over the pre-round-3 measurement.
//
// ROUND 4 — finding 3's root cause, LOCATED (cerberus issue #3128, real e2e
// dispatch runs 34050971889 and 34051673070). Direct ClickHouse-side
// introspection this investigation's prior tooling could not reach — an
// EXPLAIN PIPELINE against a real over-width dispatch's own SQL, and an
// ISOLATED re-run of that exact SQL with zero concurrent burst load —
// settled the question round 3 left open:
//
//   - The isolated re-run (query_id tagged, no concurrent load at all)
//     reproduced the SAME over-width child count as the live burst
//     (N=2: 3 children for a DataShardCount=2 dispatch; N=4: 15 children
//     for a DataShardCount=4 dispatch) — PROOF the multiplication is
//     deterministic and structural, not a load/retry/connection-pressure
//     effect. This REFUTES the "connection-level retries under concurrent
//     pressure" hypothesis this doc's round-3 text floated as the likely
//     next step, and separately REFUTES a ClickHouse parallel-replicas
//     mechanism (a real candidate given the EXPLAIN PIPELINE shape below):
//     `system.settings` read back from the SAME connection showed
//     enable_parallel_replicas=0 (changed=0 — ClickHouse's own default,
//     never touched by cerberus or the chart) on this ClickHouse 26.3
//     server, where enable_parallel_replicas is the master gate the
//     legacy max_parallel_replicas=1000 default cannot bypass on its own.
//   - The full, untruncated SQL text of the over-width dispatch (recovered
//     from its own is_initial_query=1 system.query_log row) is the plain,
//     non-structural TraceQL search shape round 3's finding 2 already
//     examined — WITH the /api/search trace-limit restriction round 3's
//     trace happened not to carry:
//
//       SELECT s.* FROM (SELECT * FROM otel_traces WHERE <window> AND
//         match(...)) AS s
//       WHERE TraceId IN (
//         SELECT TraceId FROM (SELECT * FROM otel_traces WHERE <window>
//           AND match(...)) GROUP BY TraceId
//         ORDER BY min(Timestamp) DESC, TraceId LIMIT 20)
//
//     internal/chsql/search_trace_limit.go's emitSearchTraceLimit renders
//     EXACTLY this shape, and its own doc already names the mechanism in
//     plain words: "the input subquery is emitted twice (outer drain +
//     inner ranking)". Both arms scan the SAME otel_traces Distributed
//     table. Round 3's finding 2 examined a query shape without an active
//     /api/search trace limit and correctly found no self-reference for
//     THAT shape — round 3 did not generalise to the (far more common in
//     practice) limited-search shape, which is what this round's sampled
//     dispatch happened to be.
//   - distributedProductMode=global (cerberus issue #3118, pinned
//     UNCONDITIONALLY on every query — see distributed_query_settings.go)
//     rewrites the inner `TraceId IN (subquery)` into a GLOBAL IN: the
//     subquery is materialised ONCE by fanning the SAME Distributed table
//     out across the cluster (the EXPLAIN PIPELINE this round captured
//     shows exactly this — a CreatingSets node wrapping a Union of
//     ReadFromMergeTree, the local shard's direct read under
//     prefer_localhost_replica, and ReadFromRemote, the other shards),
//     ON TOP OF the outer query's own independent Distributed fan-out for
//     the drain. One SearchTraceLimit-shaped dispatch therefore makes
//     ClickHouse execute the SAME Distributed scan roughly twice over —
//     genuinely more real per-shard Select statements than the
//     DataShardCount-wide weight acquireDataShardFanout charges it, a real
//     gap in the gate's charging model, not a measurement artefact.
//
// THE FIX (round 4, since generalised — see ROUND 5 below): acquireDataShardFanout
// multiplies its charged weight by a per-request fan-out multiplier
// (WithDataShardFanoutMultiplier, default 1). Round 4 stamped it to a
// per-shape constant (2) for plans carrying a chplan.SearchTraceLimit node
// — the ONE shape that round had proved against a real cluster — and left
// distributed_query_settings.go's three other self-referencing shapes
// (TraceQL structural operators, `select(nestedSet*)`, `| compare(...)`)
// to cerberus issue #3141's audit rather than guess a blanket constant.
//
// Cerberus issue #3128's own filing text explicitly forbids a raised
// DataShardFanoutCap as the resolution; this fix does not touch the cap —
// it corrects the WEIGHT one specific, proven dispatch shape charges
// against the unchanged cap, exactly the kind of real fix the issue asks
// for.
//
// ROUND 5 — the round-4 reading above was INCOMPLETE (cerberus issue #3128,
// real e2e dispatch runs 34055025272 and 34055887965, the first runs to
// attribute every per-shard statement to the cerberus pod that dispatched
// it via query_log.client_hostname). "Roughly twice" was wrong: a
// SearchTraceLimit dispatch produced DataShardCount²-1 per-shard Select
// statements — 3 at N=2, 15 at N=4 — with up to 10 of them CONCURRENT at
// N=4, so a single dispatch charged 2 x 4 = 8 still overshot the 8-wide
// cap on its own, and each pod peaked at 13-14. The `global` rewrite the
// round-4 text relied on never applied: distributed_product_mode=global
// rewrites an IN to GLOBAL only when the subquery's FROM is the
// Distributed table DIRECTLY, and emitSearchTraceLimit's ranking subquery
// reads it through a derived table (`FROM (<input>)`), so every shard the
// outer drain fanned out to re-executed the ranking subquery as a
// distributed query of its own (N x N). THE FIX is at the source: the
// ranking subquery is written GLOBAL IN — chsql.InSubquery does so for
// every subquery that renders a physical table scan (issue #3141 extended
// the same rule to the structural closures and nestedSet annotate, which
// measured 2.4x-2.5x over-fan-out on a real two-shard cluster until it
// did) — which the initiator honours regardless of nesting: the ranking
// subquery runs once (N children), its LIMIT-bounded id set is broadcast,
// and the drain fans out once more (N children), two sequential phases of
// DataShardCount statements each.
//
// The same run also retired the per-shape constant itself. With the trace
// search fixed, the remaining over-width dispatches were PromQL rate()
// range queries: their SQL renders the metrics Distributed table THREE
// times (the extrapolation's window arms — three Union(ReadFromMergeTree,
// ReadFromRemote) blocks in the captured EXPLAIN PIPELINE), so a dispatch
// charged DataShardCount produced 3 x DataShardCount statements. A
// per-shape multiplier would have needed a third round to learn that, and
// a fourth for the next shape. The multiplier is now the EMITTED
// statement's physical-table scan count, counted where the text is written
// (chsql.EmitCounted / Builder.physicalScans — one per rendered table
// reference, once per splice of a pre-rendered sub-statement, one per
// merge() member) and stamped by every dispatch site (internal/engine's
// route A, internal/solver's runShard for route B). Per-shape auditing of
// the WEIGHT is thereby closed for every present and future emitter; issue
// #3141's audit is about the other half — whether a shape's self-reference
// nests through a derived table the way SearchTraceLimit's did, which is a
// correctness/plan question the count cannot answer. Because a single
// statement's width can now legitimately exceed the whole cap (3 scans x
// 4 shards = 12 against a cap of 8), acquireDataShardFanout admits such a
// statement ALONE with the cap as its weight instead of parking it until
// its deadline — see that function's own doc.
//
// ROUND 4 — a SECOND, real, still-open contributing cause: this gate's
// admission ceiling is PER-PROCESS, but a real deployment runs multiple
// cerberus PODS. Re-verifying the SearchTraceLimit fix above against a real
// e2e dispatch (run 34052929455) confirmed it is a genuine, measurable
// improvement — `datashard (N=4)`'s Select-only peak concurrent dropped from
// 15-16 (pre-fix) to 14 (post-fix) — but 14 still exceeds
// DataShardFanoutCap=8. Direct inspection of the SAME run's own cluster
// state (its kubectl describe/get-pods dump) found the reason: this e2e
// lane's cerberus Deployment runs TWO pods (`cerberus-6f8d687c45-7pqdl` and
// `cerberus-6f8d687c45-zp4wp`, both Running, both serving traffic behind the
// SAME k8s Service) — deploy/helm/cerberus/values.yaml's own top-level
// `replicaCount` defaults to 2, and neither test/e2e/k3s/cerberus-values.yaml
// nor cerberus-values-datashard.yaml overrides it down to 1 for this lane.
//
// c.dataShardFanoutGate (this file) is a field on *Client, constructed ONCE
// per process by assembleClientFromConn — a bare in-memory
// *semaphore.Weighted with no cross-process visibility whatsoever. Each of
// the two pods therefore runs its OWN independent copy of this gate, each
// independently admitting up to DataShardFanoutCap (8) units of weight. A k8s
// Service round-robins (or randomly load-balances) the burst's concurrent
// HTTP requests across both pods, so the REAL aggregate ceiling ClickHouse
// can see across the whole Deployment is up to `replicaCount x
// DataShardFanoutCap` (up to 16 here), not DataShardFanoutCap alone — this
// gate's own doc and docs/solver.md's sibling "one process-wide dispatch-
// token semaphore" section have always described the MECHANISM as
// process-wide (matching NewDataShardFanoutGate's own doc: cap "mirrors how
// the pre-move mechanism defaulted to the solver's own connection Gate's
// size", itself an inherently per-process MaxOpenConns pool), but neither
// this file nor docs/solver.md had previously connected that scope to what
// it means once replicaCount > 1: DataShardFanoutCap stops being a real
// cluster-wide ceiling on ClickHouse's own concurrent per-shard exposure —
// the exact resource-safety property #3081/#3128 exist to guarantee — and
// silently becomes `replicaCount` times looser instead. 14 (measured, two
// pods, post-SearchTraceLimit-fix) sits comfortably under 16 (the two-pod
// theoretical ceiling this explains) and clearly above 8 (the single-pod cap
// the test asserts against), which is exactly the signature this cause
// predicts — not proof beyond doubt (no per-pod query_log breakdown was
// captured this round), but a coherent, evidenced explanation consistent
// with every number gathered so far, including round 3's own higher
// observations (12-31) against whatever replicaCount those earlier dispatch
// runs happened to run.
//
// NOT fixed here — cerberus issue #3128 stays OPEN for this second cause. A
// correct fix needs the resolved per-pod DataShardFanoutCap to know its own
// share of the operator's INTENDED cluster-wide budget — e.g. dividing by
// replicaCount at the Helm chart / config layer — and doing that correctly
// also has to account for
// docs/project_per_head_split's per-head split mode (each head can run a
// DIFFERENT replicaCount under `split.<head>.replicaCount`, and each such
// pod would need its OWN correctly-apportioned share) and the
// `autoscaling.enabled` HPA case (values.yaml: "When true, replicaCount is
// ignored" — the real pod count becomes dynamic, which a value baked in at
// Helm render time cannot track). Getting either wrong without real
// multi-pod e2e coverage of split mode would risk trading a real,
// evidenced bug for a guessed, unverified one — exactly what this
// investigation's own discipline (round 3's "REFUTED" entry above) exists
// to avoid. Cerberus issue #3128 stays open for this: the concrete next
// step is a replica-count-aware cap (or a genuine cross-pod coordination
// mechanism) with its own dedicated multi-replica e2e verification, not a
// guess landed alongside this round's unrelated SearchTraceLimit fix.

// ErrDataShardFanoutGateBusy is the sentinel wrapped into the error
// [Client.acquireDataShardFanout] returns when the request's own ctx
// expires or is cancelled while waiting for aggregate data-shard fan-out
// admission. This call never reached ClickHouse, so — exactly like
// [clickhouse.ErrAcquireConnTimeout] — it is a LOCAL admission-control
// signal, not a ClickHouse health signal: classifyBreakerOutcome scopes it
// breakerScopeClient (see its own doc), so a gate denial can never trip the
// circuit breaker regardless of whether the underlying ctx error was
// Canceled or DeadlineExceeded.
var ErrDataShardFanoutGateBusy = errors.New("chclient: data-shard fanout gate: DataShardFanoutCap admission budget exceeded")

// minDataShardFanoutCap is the floor NewDataShardFanoutGate clamps the
// resolved cap to, so a bare Config (DataShardCount > 1 set without a
// positive MaxOpenConns or override — a test-construction shape only;
// every production Config.FromEnv value validates MaxOpenConns > 0) never
// allocates a permanently-empty, always-blocking semaphore. Named so the
// floor is never a bare literal (invariant 13).
const minDataShardFanoutCap = 1

// NewDataShardFanoutGate derives the (gate, cap) pair [assembleClientFromConn]
// wires onto a Client, from cfg alone. cap defaults to cfg.MaxOpenConns —
// this package's own connection-pool size is the natural sibling bound,
// mirroring how the pre-move mechanism defaulted to the solver's own
// connection Gate's size — unless cfg.DataShardFanoutCapOverride is set.
// The gate itself is nil (never allocated) whenever cfg.DataShardCount <= 1,
// the one place that decision is made, so every consumer of the resolved
// (gate, cap) pair need not repeat the check. Exported so a regression test
// can assert the DataShardCount <= 1 case never allocates a semaphore
// without duplicating this arithmetic.
//
// The resolved cap must be >= cfg.DataShardCount whenever the gate exists:
// acquireDataShardFanout charges weight DataShardCount, and
// semaphore.Weighted never admits a weight above its size (it parks the
// caller until ctx is done). config.FromEnv refuses that shape at boot for
// both cap sources; this constructor trusts it rather than clamping —
// silently widening a cap the operator set is worse than a boot error.
func NewDataShardFanoutGate(cfg Config) (gate *semaphore.Weighted, cap int64) {
	cap = int64(cfg.MaxOpenConns)
	if cfg.DataShardFanoutCapOverride != nil {
		cap = *cfg.DataShardFanoutCapOverride
	}
	if cap < minDataShardFanoutCap {
		cap = minDataShardFanoutCap
	}
	if cfg.DataShardCount <= 1 {
		return nil, cap
	}
	return semaphore.NewWeighted(cap), cap
}

// dataShardFanoutMultiplierKeyType/dataShardFanoutMultiplierKey carry the
// per-request data-shard fan-out multiplier WithDataShardFanoutMultiplier
// installs — see that function's own doc for why this exists (cerberus
// issue #3128 round 4).
type dataShardFanoutMultiplierKeyType struct{}

var dataShardFanoutMultiplierKey = dataShardFanoutMultiplierKeyType{}

// defaultDataShardFanoutMultiplier is what acquireDataShardFanout charges
// absent a WithDataShardFanoutMultiplier override — the pre-round-4
// behaviour, unconditionally: weight = DataShardCount exactly, matching
// every plan shape that makes exactly one real per-shard Distributed
// statement per dispatch. Named so the multiplier is never a bare literal
// (invariant 13).
const defaultDataShardFanoutMultiplier = 1

// WithDataShardFanoutMultiplier returns a ctx that makes
// acquireDataShardFanout charge multiplier x DataShardCount for the dispatch
// instead of DataShardCount alone (cerberus issue #3128).
//
// multiplier is the number of physical (schema) table references the
// dispatched statement contains — chsql.EmitCounted's physicalScans, which
// the engine (route A) and the solver's runShard (route B) stamp right after
// emitting the SQL they are about to dispatch. On a multi-data-shard
// deployment every such reference is a `Distributed` wrapper that fans out
// DataShardCount per-shard statements, so the product is exactly the
// ClickHouse-side statement count this one dispatch produces: 1 for a plain
// scan, 2 for SearchTraceLimit's two arms, 3 for rate()'s window arms, and
// whatever a future emitter renders — counted where the text is written, so
// no per-shape audit or guessed constant is involved (rounds 4 and 5 of
// issue #3128 each found one more shape a per-shape constant had missed).
//
// multiplier <= 0 is treated as defaultDataShardFanoutMultiplier (1) — a
// caller that dispatches SQL without stamping the count must never
// UNDER-charge the gate below one full fan-out.
//
// A context value, not a Client field, so it is per-request: two concurrent
// dispatches of different statements never cross-contaminate each other's
// charged weight.
func WithDataShardFanoutMultiplier(ctx context.Context, multiplier int) context.Context {
	return context.WithValue(ctx, dataShardFanoutMultiplierKey, multiplier)
}

// DataShardFanoutMultiplierFromContext returns the multiplier
// WithDataShardFanoutMultiplier installed, and whether one was installed at
// all — the QueryTimeoutFromContext pattern (timeout.go's own doc: "so the
// layer that installs the carrier can prove it did"), exported so
// internal/engine's own execContext tests can assert the stamp fires on
// exactly the plan shapes it should, without duplicating
// dataShardFanoutMultiplierFromContext's default-fallback logic.
func DataShardFanoutMultiplierFromContext(ctx context.Context) (int, bool) {
	n, ok := ctx.Value(dataShardFanoutMultiplierKey).(int)
	return n, ok
}

// dataShardFanoutMultiplierFromContext returns the multiplier
// WithDataShardFanoutMultiplier installed, or defaultDataShardFanoutMultiplier
// (1) when none was set or the stored value is non-positive.
func dataShardFanoutMultiplierFromContext(ctx context.Context) int {
	if n, ok := ctx.Value(dataShardFanoutMultiplierKey).(int); ok && n > 0 {
		return n
	}
	return defaultDataShardFanoutMultiplier
}

// acquireDataShardFanout acquires this dispatch's share of the aggregate
// data-shard fan-out budget — weight c.dataShardCount x the per-request
// multiplier WithDataShardFanoutMultiplier installs (default 1, see that
// function's own doc — cerberus issue #3128 round 4), floored to 1 — and
// returns the idempotent release closure the caller MUST invoke exactly
// once the dispatch's ClickHouse-side work has finished (queryOpen ties it
// to the returned driver.Rows' Close via gatedRows; queryCursorColumnar
// ties it directly to its own synchronous pool.Do call, since that call
// already blocks until the statement is fully drained).
//
// The returned release closure closes over ctx (the SAME context the
// caller's dispatch runs under, already stamped with the per-dispatch
// query_id by queryContext — see the callers' own doc for why that
// ordering matters) so it can distinguish, at the instant it actually
// runs, a normal server-side finish from a cancellation-driven unwind: see
// this file's own "CANCELLATION FIX" doc above.
//
// A nil c.dataShardFanoutGate (DataShardCount <= 1, see
// NewDataShardFanoutGate) returns a no-op release and a nil error
// immediately — the pre-#3081 behaviour, unconditionally.
func (c *Client) acquireDataShardFanout(ctx context.Context) (release func(), err error) {
	if c.dataShardFanoutGate == nil {
		return func() {}, nil
	}
	weight := c.dataShardCount * int64(dataShardFanoutMultiplierFromContext(ctx))
	if weight < 1 {
		weight = 1
	}
	// A statement whose own fan-out exceeds the whole budget (several
	// Distributed scans x a wide shard set) is admitted ALONE — weight capped
	// at the gate's size — rather than never: semaphore.Weighted parks any
	// acquisition wider than its size until ctx expires, which would turn
	// such a query into a guaranteed deadline error. The gate then bounds
	// concurrent dispatches exactly as before; what it cannot do is shrink a
	// single statement below its inherent width, and operators size
	// DataShardFanoutCap with that in mind (docs/solver.md, point 5).
	if weight > c.dataShardFanoutCap {
		weight = c.dataShardFanoutCap
	}
	if aerr := c.dataShardFanoutGate.Acquire(ctx, weight); aerr != nil {
		return nil, fmt.Errorf("chclient: data-shard fanout gate acquire: %w: %w", ErrDataShardFanoutGateBusy, aerr)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			// ctx.Err() != nil means THIS dispatch's own ctx — the one
			// Acquire was just called with above — was cancelled or hit its
			// deadline: that is why the caller's underlying c.conn.Query /
			// pool.Do call returned, not a normal server-side finish (which
			// leaves ctx.Err() nil even when the call itself errored with a
			// typed *clickhouse.Exception). Only the cancellation-unwind
			// path pays for the extra KILL QUERY round-trip; a normal finish
			// falls straight through to Release below, unconditionally.
			if ctx.Err() != nil {
				if queryID := queryIDFromContext(ctx); queryID != "" {
					c.killDataShardQuery(queryID)
				}
			}
			c.dataShardFanoutGate.Release(weight)
		})
	}, nil
}

// killDataShardQueryTimeout bounds how long killDataShardQuery waits for
// KILL QUERY ... SYNC to confirm a cancelled dispatch's ClickHouse-side
// statement has genuinely stopped (or was already gone) before giving up.
// acquireDataShardFanout's release always frees the gate weight afterward
// regardless of the outcome — this bound only caps how long that release
// can be delayed by an unresponsive ClickHouse, so a hung KILL QUERY can
// never leak gate capacity, merely delay its release by at most this long.
// Named so the bound is never a bare literal (invariant 13).
const killDataShardQueryTimeout = 5 * time.Second

// killDataShardQuerySQL targets a single per-dispatch query_id. SYNC blocks
// until ClickHouse confirms the query is actually dead — or reports nothing
// to kill, the common case when the statement had already finished on its
// own in the small race window between local cancellation and this call
// landing — rather than merely accepting the request. Raw SQL text is fine
// here (unlike internal/chsql's plan-emission layer, invariant 10): this is
// an administrative statement against ClickHouse's own process table, not
// emitted query plan SQL.
const killDataShardQuerySQL = `KILL QUERY WHERE query_id = ? SYNC`

// killDataShardQuery issues killDataShardQuerySQL for queryID on a FRESH,
// uncancelled, short-lived context — deliberately NOT derived from the
// dispatch's own (already-cancelled) ctx, which would make the KILL request
// itself fail the exact same way before ever reaching ClickHouse. It calls
// c.conn.Exec directly rather than c.queryOpen or the public Client.Exec:
// this administrative statement is not itself a shard dispatch, so it must
// never recursively acquire c.dataShardFanoutGate (queryOpen's seam), and
// its outcome is not a signal about ClickHouse's general health, so it must
// never touch the circuit breaker (Client.Exec's gating) in either
// direction. clickhouse-go/v2's connection pool hands this call a fresh
// pooled connection even while the dispatch's own connection is mid-cancel,
// since that pooled connection was already evicted by connect.cancel()
// (this file's own "CANCELLATION FIX" doc) rather than being handed out
// again.
//
// Every non-nil outcome is logged at WARN for observability (the same
// breakerLogger() package-level accessor breaker.go's own transition logs
// use) but never treated as fatal: acquireDataShardFanout's release always
// frees the gate weight once this returns, regardless of whether it
// succeeded, timed out, or found no matching query to kill.
func (c *Client) killDataShardQuery(queryID string) {
	ctx, cancel := context.WithTimeout(context.Background(), killDataShardQueryTimeout)
	defer cancel()
	if err := c.conn.Exec(ctx, killDataShardQuerySQL, queryID); err != nil {
		breakerLogger().Warn(
			"chclient: data-shard fanout gate: KILL QUERY on a cancelled dispatch did not confirm the statement stopped",
			"query_id", queryID, "error", err,
		)
	}
}

// gatedRows decorates a driver.Rows so its Close() also releases the
// data-shard fan-out weight queryOpen acquired for it — the ClickHouse-side
// statement genuinely stays "in flight" (from this gate's point of view)
// for exactly as long as the caller keeps the result set open, mirroring
// how internal/solver's own connection Gate was already held until
// shardCursor.Close before this gate moved here.
//
// Embedding driver.Rows (rather than naming every method) means gatedRows
// satisfies the interface by forwarding every method except the one it
// overrides — Close — to the wrapped value.
type gatedRows struct {
	driver.Rows
	release func()
}

// Close releases the wrapped rows AND the fan-out weight, in that order,
// and always runs the release (even when rows.Close itself errors) since
// the ClickHouse-side statement is done either way. release is already
// idempotent (acquireDataShardFanout's sync.Once), so a caller that closes
// more than once — permitted by some driver.Rows implementations — cannot
// double-release.
func (g *gatedRows) Close() error {
	err := g.Rows.Close()
	g.release()
	return err
}
