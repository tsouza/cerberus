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
// THE FIX: acquireDataShardFanout now multiplies its charged weight by a
// per-request fan-out multiplier (WithDataShardFanoutMultiplier, default 1)
// that internal/engine.Engine.execContext stamps to
// searchTraceLimitFanoutMultiplier (2) whenever the plan being dispatched
// contains a chplan.SearchTraceLimit node — the ONE shape this round
// directly proved against a real cluster. distributed_query_settings.go's
// own doc already named three OTHER shapes that self-reference a
// Distributed table under distributed_product_mode=global (TraceQL
// structural operators, `select(nestedSet*)`, `| compare(...)`) that this
// round's e2e burst never exercises (it fires only a bare, non-structural
// TraceQL attribute search, a PromQL range query, and a LogQL range query)
// and this fix does NOT audit or multiplier-charge — cerberus issue #3141
// tracks auditing and, where warranted, extending the SAME multiplier
// mechanism to those shapes with their own real-cluster evidence, the same
// rigor this round applied to SearchTraceLimit, rather than a guessed
// blanket multiplier applied without verification.
//
// Cerberus issue #3128's own filing text explicitly forbids a raised
// DataShardFanoutCap as the resolution; this fix does not touch the cap —
// it corrects the WEIGHT one specific, proven dispatch shape charges
// against the unchanged cap, exactly the kind of real fix the issue asks
// for.

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

// WithDataShardFanoutMultiplier returns a ctx that scales
// acquireDataShardFanout's charged weight by multiplier x DataShardCount
// instead of the default DataShardCount alone (cerberus issue #3128 round
// 4 — see this file's own "ROUND 4" doc above for the real-cluster evidence
// that motivated this).
//
// This exists because acquireDataShardFanout charges a FIXED weight per
// dispatch on the assumption that one chclient dispatch makes exactly one
// real Distributed-table fan-out. That assumption is false for a plan shape
// that references the SAME Distributed table more than once WITHIN one
// physical SQL statement — internal/chsql/search_trace_limit.go's
// emitSearchTraceLimit is the first PROVEN instance (its own doc: "the
// input subquery is emitted twice"), and distributed_query_settings.go
// separately documents three OTHER shapes with the same self-reference
// property (TraceQL structural operators, nestedSet annotate, compare) that
// have not been round-4-verified against a real cluster and so do NOT set
// this yet (cerberus issue #3141 tracks that audit). A shape that has not
// been proven to over-fan-out must never claim it has — this carrier
// exists precisely so that claim is made per-shape, with evidence, not
// guessed globally.
//
// multiplier <= 0 is treated as defaultDataShardFanoutMultiplier (1) — a
// caller bug should never UNDER-charge the gate below what the pre-round-4
// mechanism already charged.
//
// A context value, not a Client field, so it is per-request: two concurrent
// dispatches, one SearchTraceLimit-shaped and one not, never
// cross-contaminate each other's charged weight.
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
