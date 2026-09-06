package chclient

// settingSkipUnavailableShards / settingFallbackToStaleReplicasForDistributedQueries
// name the two ClickHouse settings that decide what a Distributed-engine
// query does when it cannot reach every shard/replica it fans out to.
// Cerberus issue #3078 pins BOTH to the fail-loud value, and this file is
// that pin, cited inline exactly as the issue's own acceptance criteria
// require:
//
//   - skip_unavailable_shards=0 — ClickHouse's own default is already 0
//     (verified against docs/reference/settings/session-settings/
//     skip-unavailable-shards.mdx in github.com/ClickHouse/ClickHouse), so
//     this pin changes no server behavior today; it is stamped explicitly
//     so the decision survives a future ClickHouse default change rather
//     than being left to chance. With it, a Distributed data shard that
//     has NO reachable replica at all aborts the whole query with
//     ALL_CONNECTION_TRIES_FAILED (chclient's own *ShardUnavailableError,
//     see distributed_shard_error.go) instead of silently answering from
//     whichever shards happened to respond (skip_unavailable_shards=1's
//     documented "returns a result based on partial data" behavior) — a
//     silent partial answer is a worse failure mode for a correctness-
//     focused gateway than a loud one.
//   - fallback_to_stale_replicas_for_distributed_queries=0 — the OPPOSITE
//     of ClickHouse's own default, which is 1 (verified against
//     docs/reference/settings/session-settings/other.mdx: "Forces a query
//     to an out-of-date replica if updated data is not available ... By
//     default, 1 (enabled)"). Cerberus overrides it to 0 so a shard whose
//     reachable replicas are ALL stale aborts the query with
//     ALL_REPLICAS_ARE_STALE (chclient's own
//     *StaleReplicaFallbackDeniedError) instead of silently answering from
//     data that may be missing recent writes. This is the same "fail
//     loud, never silently wrong" posture cerberus already applies
//     elsewhere (skip_unavailable_shards above, timeout_overflow_mode=
//     throw in this same package) — an operator who genuinely wants
//     eventual-consistency-tolerant reads over a lagging replica can still
//     override this via a server-side settings profile; cerberus's own
//     default never chooses that trade-off silently.
//
// settingLoadBalancing / settingLoadBalancingFirstOffset (cerberus issue
// #3086, epic #3074) close the SECOND consistency gap issue #3086 opened:
// `sessionAffinity: ClientIP` (issue #3075) pins a cerberus pod's own
// connection to one replica of the ONE shard it dials directly, but once
// that connection issues a query against a `Distributed` wrapper table,
// ClickHouse's OWN replica-selection logic — NOT any k8s Service — picks
// which replica of EVERY OTHER shard answers, independently per statement.
// Two statements from the SAME cerberus multi-statement request (e.g. the
// sharded-pushdown solver's own time-range fan-out) could therefore land on
// two DIFFERENT replicas of the SAME remote shard, reopening the exact
// cross-replica divergence risk sessionAffinity exists to close, one level
// removed.
//
//   - load_balancing=first_or_random — ClickHouse's default is `random`
//     (verified against `src/Core/Settings.cpp` at the pinned
//     `v25.8.1.5101-lts` tag: `DECLARE(LoadBalancing, load_balancing,
//     LoadBalancing::RANDOM, ...)` — NOT `round_robin`, contrary to this
//     issue's own initial "default round_robin" text, which this citation
//     corrects). `random` picks arbitrarily among the least-erroring
//     replicas on EVERY call (`GetPriorityForLoadBalancing::getPriorityFunc`
//     leaves `get_priority` unset for the RANDOM case, so
//     `PoolWithFailoverBase`'s ordering tuple falls through to its trailing
//     `random` tie-breaker), so it can and will pick a different replica of
//     the same remote shard on every fan-out statement — the DEFAULT itself
//     is the bug this pin closes.
//
//     `first_or_random` (`src/Common/GetPriorityForLoadBalancing.cpp`,
//     confirmed at the pinned tag) assigns priority 0 to the replica at
//     index `load_balancing_first_offset` (default 0) and priority 1 to
//     every other replica, so — absent any recorded connection errors —
//     EVERY query against the SAME `Distributed` table's remote-shard
//     connection pool deterministically selects that ONE offset-0 replica.
//     Selection state (`PoolWithFailoverBase::Pool::error_count`) lives on
//     the ClickHouse SERVER process (the shard-0 initiator this cerberus
//     pod's own sessionAffinity pins to), not on any per-client or
//     per-session state, so the guarantee is actually STRONGER than
//     sessionAffinity's own: every statement from every cerberus pod,
//     across every request, converges on the SAME physical replica per
//     remote shard for as long as it stays healthy — not merely "the same
//     replica for the lifetime of one client's affinity window."
//     `deploy/helm/cerberus/templates/clickhouse/configmap-config.yaml`
//     lists each shard's `<replica>` entries in StatefulSet-ordinal order
//     (`until (int $b.replicas)`, no `<priority>` tag), so offset 0 always
//     names that shard's own `-0` pod.
//
//     `first_or_random` is chosen over the plainer `in_order` (same
//     steady-state determinism — `IN_ORDER`'s priority function is also a
//     pure, error-count-independent function of config index, per the same
//     source file) because ClickHouse's own docs
//     (docs/reference/operations/settings/settings.md, "First or random"
//     section, same pinned tag) name `in_order`'s failure mode explicitly:
//     "if one replica goes down, the next one gets a double load" — a
//     doubled-load failover is a worse trade than `first_or_random`'s
//     "evenly distributed among replicas that are still available", and
//     both give the IDENTICAL steady-state guarantee this issue needs.
//
//   - load_balancing_first_offset=0 — already ClickHouse's own default
//     (`DECLARE(UInt64, load_balancing_first_offset, 0, ...)`, same pinned
//     Settings.cpp), stamped explicitly for the same reason
//     skip_unavailable_shards=0 is stamped explicitly above: the pin
//     changes no behavior today, but survives a future ClickHouse default
//     change rather than leaving the "which replica is offset 0" choice to
//     chance.
//
//   - `updateSettingsAndClientInfoForCluster`
//     (src/Interpreters/ClusterProxy/executeQuery.cpp, same pinned tag)
//     force-overrides `load_balancing` to `ROUND_ROBIN` ONLY when
//     `context->canUseParallelReplicasCustomKeyForCluster(cluster)` is true
//     AND the query left `load_balancing` unchanged
//     (`!settings[Setting::load_balancing].changed`). Cerberus stamps
//     `load_balancing` on every query (marking it `.changed`), so this
//     override never fires against a cerberus-issued query even if a future
//     operator config were to enable a parallel-replicas custom key —
//     verified directly against that function's source at the pinned tag.
//
// TRADE-OFF, DOCUMENTED NOT HIDDEN: pinning `first_or_random` (or
// `in_order`) means EVERY cerberus read against a remote shard's Distributed
// connection concentrates on that shard's ONE offset-0 replica while it
// stays healthy — the other replicas exist for durability/failover, not
// read-scaling, for as long as this pin stands. Cerberus already makes this
// exact trade (correctness/consistency over throughput) for
// skip_unavailable_shards and fallback_to_stale_replicas_for_distributed_queries
// above; this is the same posture applied one level further down the
// replica-selection stack. See docs/helm-clickhouse.md's multi-replica
// consistency section for the full decision record.
//
// settingDistributedProductMode (cerberus issue #3118, epic #3074) closes a
// THIRD gap DataShardCount > 1 opens: several of cerberus's own query shapes
// self-reference the `otel_traces` Distributed wrapper table twice in one
// query — a JOIN or an IN-subquery where BOTH sides eventually scan the
// SAME Distributed table. Concretely (all confirmed against ClickHouse
// 25.8, dispatch run 34017639602):
//
//   - TraceQL's structural-child/parent/sibling operators (`>` / `<` / `~`)
//     — internal/chsql/structural_join.go's emitStructuralDirectJoin/
//     emitStructuralSiblingJoin — INNER-JOIN an L subquery against an R
//     subquery, both ultimately Scan(s.SpansTable).
//   - TraceQL's descendant/ancestor operators (`>>` / `<<`) — emitStructuralRecursive
//     — additionally run a `WITH RECURSIVE` closure whose step arm
//     self-joins `otel_traces` against the growing CTE.
//   - EVERY structural operator's L side is first gated through
//     rootedStructuralLeftSub, itself a `WITH RECURSIVE … SELECT * FROM (<L>)
//     WHERE (TraceId, SpanId) IN (<recursive closure over otel_traces>)` — the
//     literal "RecursiveCTESource" the CH error names.
//   - `select(nestedSetLeft, nestedSetParent, nestedSetRight)` —
//     internal/chsql/nested_set_annotate.go's emitNestedSetAnnotate — LEFT
//     JOINs the search result against its own `WITH RECURSIVE` nested-set
//     numbering, again built from a bare `otel_traces` Scan.
//   - `| compare(...)` — internal/traceql/metrics_compare.go's
//     compareRootLookup — LEFT JOINs the metric pipeline's own Scan against
//     a second, independent Scan(s.SpansTable) that resolves each trace's
//     root span.
//   - Every LIMIT-bounded plain /api/search — internal/chsql/
//     search_trace_limit.go's emitSearchTraceLimit — emits its own row
//     source TWICE (an outer drain query plus an inner top-N
//     trace-ranking subquery), an `IN` self-reference of the identical
//     shape the four shapes above make deliberately, just without a
//     literal JOIN keyword. Added to this list by cerberus issue #3128
//     round 4, which additionally found this shape's self-reference
//     under-charges internal/chclient/fanout_gate.go's own admission-
//     control weight (that file's own "ROUND 4" doc has the real-cluster
//     evidence and the fix) — a gap the four shapes above may share too,
//     tracked by cerberus issue #3141 rather than assumed fixed here.
//
// LIMIT OF THE `global` REWRITE (cerberus issue #3128, real-cluster
// evidence in internal/chsql/search_trace_limit.go's doc): the server
// rewrites an IN/JOIN to GLOBAL only when the subquery's FROM is the
// Distributed table DIRECTLY. A subquery that reads it through a derived
// table — `IN (SELECT … FROM (SELECT … FROM otel_traces …))`, which is what
// every emitter that renders a plan subtree as `(<input>)` produces — is
// executed as written on every shard, each execution fanning out again
// (DataShardCount² statements per dispatch). This pin therefore covers the
// direct shapes; an emitter nesting the reference through a derived table
// must write GLOBAL itself (chsql.GlobalInSubquery — emitSearchTraceLimit
// does). The four shapes above are audited for exactly this under issue
// #3141.
//
// On a single-shard/non-Distributed deployment `otel_traces` is a plain
// MergeTree table, so none of these ever double-references a Distributed
// table and the shape is unremarkable. Once epic #3074 wraps it in
// `Distributed(cluster, db, otel_traces_local, <shardingKey>)`
// (internal/schema/ddl.renderDistributedWrapper), every one of the shapes
// above becomes exactly the pattern ClickHouse's `distributed_product_mode`
// guard exists to catch, and the SERVER's own default (`deny`) rejects it
// outright with code 288.
//
// Three remedies exist server-side (docs/reference/operations/settings/
// settings.md, "distributed_product_mode"), and only one is safe for
// cerberus to apply BLANKET, unconditionally, at the transport layer:
//
//   - `local` rewrites the subquery to read its OWN shard's local table —
//     REJECTED. It is only correct when the two self-joined sides are
//     guaranteed to be CO-LOCATED on the same physical shard (e.g. both
//     keyed by TraceId under a TraceId-based sharding key), and cerberus
//     makes no such guarantee: Config.DataShardingKey defaults to `rand()`
//     (ddl.go's dataShardingKey, "an unweighted default is correctness-
//     neutral for read queries" — true for a plain fan-out scan, false the
//     moment a query joins across shards). Under the default key a trace's
//     spans land on shards independently at random, so a `local` rewrite
//     would silently drop every structural match / root-span lookup whose
//     two sides happen to land on different shards — a wrong ANSWER, not a
//     loud error. Worse than the `deny` this issue is fixing.
//   - `allow` merely lifts the guard and executes the JOIN/IN exactly as
//     written against each shard's LOCAL data independently (ClickHouse's
//     own docs: "responsibility for the correctness of this query lies on
//     the user") — REJECTED for the identical reason `local` is: same
//     silent-data-loss failure mode under cerberus's random default
//     sharding key, just reached without even the rewrite's explicitness.
//   - `global` rewrites every non-GLOBAL IN/JOIN in the query to GLOBAL
//     IN/GLOBAL JOIN — ACCEPTED. The inner (right-hand / L) side is
//     computed ONCE at the query initiator and broadcast as a temporary
//     table to every shard, so each shard's local execution sees the FULL
//     cross-shard result regardless of where any given span or trace
//     physically landed. This is correct under ANY sharding key, including
//     the default `rand()` — the same posture cerberus already takes with
//     skip_unavailable_shards/fallback_to_stale_replicas_for_distributed_queries
//     above: correctness over throughput, chosen outright rather than left
//     to an operator's per-deployment sharding-key discipline.
//
// TRADE-OFF, DOCUMENTED NOT HIDDEN: `global` mode broadcasts the inner
// subquery's FULL materialized result to every shard on every query it
// rewrites, rather than letting each shard prune independently. Every
// self-join shape above already bounds that inner side before it ever
// reaches this point — the structural closures cap recursion depth
// (defaultStructuralRecursionDepth), phase B restricts to a top-N trace-id
// set (traceIDRestrictionFrag), and compareRootLookup groups down to one
// row per trace — so the broadcast payload is the same small, already-
// bounded working set these queries were designed to keep small, not an
// unbounded table scan. A future query shape that self-joins an
// UNBOUNDED side would pay a real broadcast cost under this pin; no such
// shape exists in cerberus today (see this issue's PR body for the sweep).
//
// Stamped UNCONDITIONALLY (see the four pins above and Client.querySettings)
// rather than gated on DataShardCount, for the same reason those are: a
// single-shard/non-Distributed deployment has no Distributed table for the
// setting to act on, so ClickHouse accepts and simply never consults it —
// a harmless no-op — and pinning it once, at the transport layer, covers
// every query shape above PLUS any future one with the identical self-join-
// over-Distributed shape, without cerberus's query-emission code needing to
// know it is running under DataShardCount > 1 at all.
const settingDistributedProductMode = "distributed_product_mode"

// distributedProductModeGlobal is settingDistributedProductMode's stamped
// value — see this file's own doc for why `global`, not `local` or `allow`,
// is the only one of ClickHouse's three non-`deny` remedies that stays
// correct under cerberus's default (`rand()`) data-shard sharding key.
const distributedProductModeGlobal = "global"

// All five settings are stamped UNCONDITIONALLY on every data-plane read-path
// query (see Client.querySettings), mirroring how settingTimeoutOverflowMode
// is pinned outright rather than exposed as an operator knob: they are
// harmless, version-safe no-ops against a single-shard/non-Distributed
// deployment (cerberus's default, and every deployment before epic #3074) —
// ClickHouse accepts and simply never consults any of them on a query that
// touches no Distributed-engine table — so pinning them unconditionally
// costs nothing on the common case and only changes behavior once
// internal/chopt.ClusterTopology.DataShardCount > 1 (cerberus issues #3077,
// #3086, #3118).
const (
	settingSkipUnavailableShards                        = "skip_unavailable_shards"
	settingFallbackToStaleReplicasForDistributedQueries = "fallback_to_stale_replicas_for_distributed_queries"
	settingLoadBalancing                                = "load_balancing"
	settingLoadBalancingFirstOffset                     = "load_balancing_first_offset"

	// loadBalancingFirstOrRandom is settingLoadBalancing's stamped value —
	// see this file's own doc for why it, not in_order, is the pick.
	loadBalancingFirstOrRandom = "first_or_random"
)
