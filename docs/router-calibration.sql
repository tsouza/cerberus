-- Router calibration (Stage 0) — go/no-go analysis over cerberus_router_corpus.
--
-- The corpus (internal/optcorpus, populated when
-- CERBERUS_CH_OPT_CORPUS_ENABLED=1 with SINK_MODE=chtable) joins every routing
-- DECISION the pure classifier made (internal/solver Planner.Plan) to the
-- OBSERVED ClickHouse cost it actually paid (read_rows / read_bytes /
-- query_duration_ms / memory_usage / exit_status). route = 'A' is a single CH
-- query (not routed / below-threshold); route = 'B' is a time-slice sharded
-- query (routed). N/F/D are the RAW classifier scalars (n_anchors / fanout /
-- cumulative_d) recorded for BOTH routes.
--
-- exit_status values:
--   * CH-side (derived from system.query_log):
--       ok       — clean finish.
--       oom      — ClickHouse MEMORY_LIMIT_EXCEEDED.
--       timeout  — ClickHouse TIMEOUT_EXCEEDED / TOO_SLOW.
--   * Cerberus-side (recorded in-process; the query_log cannot reflect these):
--       sample_budget — the query.maxSamples 422. The CH query FINISHED (cost
--                       columns are real) but cerberus rejected the drain: too
--                       big. Authoritative over a query_log 'ok'.
--       breaker       — circuit-breaker 503 fast-fail; no CH query ran (cost = 0).
--       rejected      — resolution-cap / body-limit 400; no CH query ran (cost = 0).
-- All three cerberus-side values are MISROUTE signals on route A: a route-A
-- query that hit the sample budget, tripped the breaker, or was cap-rejected is
-- a query the heuristic kept single-path but that cerberus could not serve.
-- `exit_status != 'ok'` therefore captures every failure class, CH- and
-- cerberus-side alike.
--
-- The question this file answers: is the current PURE heuristic good enough, or
-- does the misroute rate justify a learned / calibrated router?
--
-- Read the result as:
--   * Clean separation (route-A queries are cheap, route-B queries are
--     expensive, little overlap) => the heuristic is fine. YAGNI: stop here.
--   * High overlap (many route-A queries land in the expensive cost territory
--     that route-B queries occupy, and/or route-A queries OOM/timeout)
--     => the heuristic misroutes; calibration is justified.
--
-- All queries below are read-only and run against the operator-owned table.
-- Replace the table name if you created the corpus under a different name.

----------------------------------------------------------------------------
-- 1. Cost distribution by route. The headline separation check.
--    If route A's high percentiles approach route B's, the routes overlap.
----------------------------------------------------------------------------
SELECT
    route,
    count()                                         AS queries,
    round(quantile(0.50)(query_duration_ms))        AS p50_ms,
    round(quantile(0.90)(query_duration_ms))        AS p90_ms,
    round(quantile(0.99)(query_duration_ms))        AS p99_ms,
    round(quantile(0.50)(memory_usage) / 1e6, 1)    AS p50_mem_mb,
    round(quantile(0.99)(memory_usage) / 1e6, 1)    AS p99_mem_mb,
    round(quantile(0.99)(read_rows) / 1e6, 1)       AS p99_read_mrows,
    countIf(exit_status = 'oom')                     AS ooms,
    countIf(exit_status = 'timeout')                 AS timeouts,
    -- Cerberus-side terminal outcomes (query_log cannot show these). On route A
    -- each is a misroute signal; sample_budget rows carry real CH cost (the
    -- query finished, cerberus rejected the drain), breaker/rejected are zero-cost.
    countIf(exit_status = 'sample_budget')           AS sample_budget_rejects,
    countIf(exit_status = 'breaker')                 AS breaker_rejects,
    countIf(exit_status = 'rejected')                AS cap_rejects
FROM cerberus_router_corpus
GROUP BY route
ORDER BY route;

----------------------------------------------------------------------------
-- 2. WRONG-ROUTE overlap — the core calibration signal.
--    Define the route-B "expensive" floor as route B's median memory. Then
--    count route-A queries that exceeded it: those are queries the heuristic
--    KEPT on route A but that landed in cost territory route B occupies (the
--    territory slicing historically helped). A high share = misroute.
----------------------------------------------------------------------------
WITH
    (SELECT quantile(0.50)(memory_usage) FROM cerberus_router_corpus WHERE route = 'B') AS b_mem_floor,
    (SELECT quantile(0.50)(query_duration_ms) FROM cerberus_router_corpus WHERE route = 'B') AS b_dur_floor
SELECT
    countIf(route = 'A')                                                       AS route_a_total,
    countIf(route = 'A' AND memory_usage      >= b_mem_floor)                  AS a_in_b_mem_territory,
    countIf(route = 'A' AND query_duration_ms >= b_dur_floor)                  AS a_in_b_dur_territory,
    countIf(route = 'A' AND exit_status != 'ok')                               AS a_failed,
    round(100 * countIf(route = 'A' AND memory_usage >= b_mem_floor)
              / nullIf(countIf(route = 'A'), 0), 1)                            AS pct_a_misrouted_by_mem,
    -- The inverse: route-B queries that were CHEAP (below route A's median),
    -- i.e. sliced when slicing bought nothing (wasted shard machinery).
    countIf(route = 'B' AND memory_usage <
        (SELECT quantile(0.50)(memory_usage) FROM cerberus_router_corpus WHERE route = 'A')) AS b_wasted_slicing
FROM cerberus_router_corpus;

----------------------------------------------------------------------------
-- 3. Overlap by cost-grid bucket. Where on the (N, F) grid do the routes
--    disagree on cost? A bucket where route A is as expensive as route B at
--    the SAME (N, F) is exactly where a calibrated threshold would move the
--    boundary. Buckets with clean A<<B separation confirm the heuristic.
----------------------------------------------------------------------------
SELECT
    n_anchors,
    fanout,
    route,
    count()                                          AS queries,
    round(quantile(0.90)(query_duration_ms))         AS p90_ms,
    round(quantile(0.90)(memory_usage) / 1e6, 1)     AS p90_mem_mb,
    countIf(exit_status != 'ok')                      AS failures
FROM cerberus_router_corpus
GROUP BY n_anchors, fanout, route
HAVING queries >= 5            -- ignore thin buckets with no statistical weight
ORDER BY n_anchors, fanout, route;

----------------------------------------------------------------------------
-- 4. The decisive misroute count: route-A queries that did not finish 'ok' —
--    CH-side (oom / timeout) OR cerberus-side (sample_budget / breaker /
--    rejected). These are unambiguous heuristic failures — the query died, was
--    cap-rejected, or blew the sample budget on the single path the classifier
--    chose for it. Any non-trivial count here is a standalone go signal for
--    calibration, independent of the overlap math. The exit_status breakdown
--    shows which failure class dominates (a sample_budget-heavy bucket says the
--    single-path result set is too large — exactly route B's reason to exist).
----------------------------------------------------------------------------
SELECT
    decision_reason,                                 -- why the classifier kept it on A
    n_anchors,
    fanout,
    cumulative_d,
    count()                                          AS failed_route_a_queries,
    countIf(exit_status = 'oom')                      AS ooms,
    countIf(exit_status = 'timeout')                  AS timeouts,
    countIf(exit_status = 'sample_budget')            AS sample_budget_rejects,
    countIf(exit_status = 'breaker')                  AS breaker_rejects,
    countIf(exit_status = 'rejected')                 AS cap_rejects,
    round(quantile(0.99)(memory_usage) / 1e6, 1)     AS p99_mem_mb
FROM cerberus_router_corpus
WHERE route = 'A' AND exit_status != 'ok'
GROUP BY decision_reason, n_anchors, fanout, cumulative_d
ORDER BY failed_route_a_queries DESC
LIMIT 50;
