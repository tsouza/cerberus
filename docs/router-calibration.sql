-- Router calibration (Stage 0) — go/no-go analysis over cerberus_router_corpus.
--
-- The corpus (internal/optcorpus, populated when
-- CERBERUS_CH_OPT_CORPUS_ENABLED=1 with SINK_MODE=chtable) joins every routing
-- DECISION the pure classifier made (internal/solver Planner.Plan) to the
-- OBSERVED ClickHouse cost it actually paid (read_rows / read_bytes /
-- query_duration_ms / memory_usage / exit_status). route values (the full set
-- internal/optcorpus can write):
--   'A' — a single CH query: the classifier looked at the plan and declined to
--         shard it, for whichever decision_reason it recorded.
--   'B' — a sharded query (decision_reason 'routed').
--   ''  — unclassified: the classifier never ran on this query at all. Every
--         LogQL and TraceQL row lands here, since the solver only classifies
--         PromQL. Those rows carry decision_reason 'non-promql', which names the
--         absence explicitly rather than leaving it to be inferred; a PromQL row
--         with the Solver switched off also lands here, with an EMPTY reason.
--         Either way route is '', which is what distinguishes an unclassified
--         row from an 'A' refusal — select the unclassified population by
--         `route = ''` or by `language`, never by a non-empty decision_reason.
-- N/F/D are the RAW classifier scalars (n_anchors / fanout / cumulative_d),
-- recorded for both classified routes and left zero for the unclassified ones.
-- The misroute queries below therefore say `route = 'A'` rather than
-- `route != 'B'`: a rate over rows the classifier never saw is not a misroute
-- rate.
--
-- exit_status values (the full set internal/optcorpus can write):
--   * CH-side (derived from system.query_log):
--       ok       — clean finish.
--       oom      — ClickHouse MEMORY_LIMIT_EXCEEDED.
--       timeout  — ClickHouse TIMEOUT_EXCEEDED / TOO_SLOW.
--       aborted  — the query was abandoned by its client mid-flight: a
--                  cancellation, a destroyed socket, a broken pipe. The client
--                  walked away; the route it was on had nothing to do with it.
--       error    — the query failed for a reason that is none of the above.
--                  The honest floor for any exception code cerberus does not
--                  recognise, never folded back into 'ok'.
--   * Cerberus-side (recorded in-process; the query_log cannot reflect these):
--       sample_budget — the query.maxSamples 422. The CH query FINISHED (cost
--                       columns are real) but cerberus rejected the drain: too
--                       big. Authoritative over a query_log 'ok'.
--       breaker       — circuit-breaker 503 fast-fail; no CH query ran (cost = 0).
--       rejected      — resolution-cap / body-limit 400; no CH query ran (cost = 0).
-- All three cerberus-side values are MISROUTE signals on route A: a route-A
-- query that hit the sample budget, tripped the breaker, or was cap-rejected is
-- a query the heuristic kept single-path but that cerberus could not serve.
-- The heuristic-failure predicate is therefore
-- `exit_status NOT IN ('ok', 'aborted')` — every CH- and cerberus-side failure
-- class, minus the client-abandonment rows, which say nothing about the route.
--
-- Cost columns (query_duration_ms / memory_usage / read_rows / read_bytes) are
-- TRUNCATED on anything but a clean finish: a timeout's duration is the
-- deadline, an OOM's memory is the cap, an abort's read_rows is however far it
-- got. Every cost percentile below is therefore scoped to exit_status = 'ok',
-- the same clean-finish population internal/routerrules requires a
-- corpus_percentile over a runtime-cost column to declare. Counts are NOT
-- scoped that way — a failure only counts if it is counted.
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
    count()                                                                 AS queries,
    countIf(exit_status = 'ok')                                             AS ok_queries,
    -- Percentiles over the CLEAN-FINISH population only: a truncated counter
    -- describes how the query died, not what the query costs.
    round(quantileIf(0.50)(query_duration_ms, exit_status = 'ok'))          AS p50_ms,
    round(quantileIf(0.90)(query_duration_ms, exit_status = 'ok'))          AS p90_ms,
    round(quantileIf(0.99)(query_duration_ms, exit_status = 'ok'))          AS p99_ms,
    round(quantileIf(0.50)(memory_usage, exit_status = 'ok') / 1e6, 1)      AS p50_mem_mb,
    round(quantileIf(0.99)(memory_usage, exit_status = 'ok') / 1e6, 1)      AS p99_mem_mb,
    round(quantileIf(0.99)(read_rows, exit_status = 'ok') / 1e6, 1)         AS p99_read_mrows,
    -- CH-side terminal outcomes. `aborts` is the client walking away, held
    -- apart from the failure classes; the columns below sum to `queries`.
    countIf(exit_status = 'oom')                                            AS ooms,
    countIf(exit_status = 'timeout')                                        AS timeouts,
    countIf(exit_status = 'aborted')                                        AS aborts,
    countIf(exit_status = 'error')                                          AS errors,
    -- Cerberus-side terminal outcomes (query_log cannot show these). On route A
    -- each is a misroute signal; sample_budget rows carry real CH cost (the
    -- query finished, cerberus rejected the drain), breaker/rejected are zero-cost.
    countIf(exit_status = 'sample_budget')                                  AS sample_budget_rejects,
    countIf(exit_status = 'breaker')                                        AS breaker_rejects,
    countIf(exit_status = 'rejected')                                       AS cap_rejects,
    -- Route-B fan-out shape. A route-B row folds K shard rows into ONE row, and
    -- the cost columns are ALREADY comparable to a route-A row's: memory_usage
    -- is the sum of the `parallelism` largest shard peaks (only that many
    -- coexist) and query_duration_ms the makespan bound over the fan-out. Do
    -- NOT renormalise them by k_shards or parallelism — the fold has done it.
    -- The columns below are diagnostics on the fan-out's shape, not correction
    -- factors; all three are 0 on route A. shards_observed < k_shards means the
    -- fan-out was cancelled part-way (the first failing shard cancels the rest,
    -- which then never reach query_log), so such a row's folded cost covers
    -- only the shards named here.
    round(avgIf(k_shards, route = 'B'), 1)                                  AS avg_k_shards,
    round(avgIf(shards_observed, route = 'B'), 1)                           AS avg_shards_observed,
    round(avgIf(parallelism, route = 'B'), 1)                               AS avg_parallelism,
    countIf(route = 'B' AND shards_observed < k_shards)                     AS partial_fanouts
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
-- Both floors, and both sides of every comparison against them, read the
-- clean-finish population: a truncated counter would place a query in the wrong
-- cost territory purely because it died early.
--
-- The route-B side of both floors is a FOLD over the fan-out's shards, already
-- taken at the executor's effective concurrency (see the fan-out columns in
-- query 1), so both floors are read raw: renormalising by the wave count here
-- would apply the same correction twice and push route B's territory off by
-- roughly k_shards / parallelism in both directions at once.
WITH
    (SELECT quantile(0.50)(memory_usage)
       FROM cerberus_router_corpus
      WHERE route = 'B' AND exit_status = 'ok')                              AS b_mem_floor,
    (SELECT quantile(0.50)(query_duration_ms)
       FROM cerberus_router_corpus
      WHERE route = 'B' AND exit_status = 'ok')                              AS b_dur_floor
SELECT
    countIf(route = 'A')                                                       AS route_a_total,
    countIf(route = 'A' AND exit_status = 'ok')                                AS route_a_ok,
    countIf(route = 'A' AND exit_status = 'ok' AND memory_usage      >= b_mem_floor) AS a_in_b_mem_territory,
    countIf(route = 'A' AND exit_status = 'ok' AND query_duration_ms >= b_dur_floor) AS a_in_b_dur_territory,
    -- Heuristic failures: every failure class, minus client abandonment.
    countIf(route = 'A' AND exit_status NOT IN ('ok', 'aborted'))               AS a_failed,
    round(100 * countIf(route = 'A' AND exit_status = 'ok' AND memory_usage >= b_mem_floor)
              / nullIf(countIf(route = 'A' AND exit_status = 'ok'), 0), 1)     AS pct_a_misrouted_by_mem,
    -- The inverse: route-B queries that were CHEAP (below route A's median),
    -- i.e. sliced when slicing bought nothing (wasted shard machinery). Both
    -- sides read the same quantity — the memory the request held at once — so
    -- the route-B side needs no adjustment either.
    countIf(route = 'B' AND exit_status = 'ok'
            AND memory_usage <
        (SELECT quantile(0.50)(memory_usage) FROM cerberus_router_corpus WHERE route = 'A' AND exit_status = 'ok')) AS b_wasted_slicing
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
    count()                                                                 AS queries,
    countIf(exit_status = 'ok')                                             AS ok_queries,
    -- On the route = 'B' rows these two are the fold's bounds rather than direct
    -- readings (query 1 says what each bounds), but they are already taken at
    -- the executor's effective concurrency, so they compare across routes as
    -- they stand.
    round(quantileIf(0.90)(query_duration_ms, exit_status = 'ok'))          AS p90_ms,
    round(quantileIf(0.90)(memory_usage, exit_status = 'ok') / 1e6, 1)      AS p90_mem_mb,
    -- ok_queries + failures + aborts = queries.
    countIf(exit_status NOT IN ('ok', 'aborted'))                           AS failures,
    countIf(exit_status = 'aborted')                                        AS aborts
FROM cerberus_router_corpus
GROUP BY n_anchors, fanout, route
HAVING queries >= 5            -- ignore thin buckets with no statistical weight
ORDER BY n_anchors, fanout, route;

----------------------------------------------------------------------------
-- 4. The decisive misroute count: route-A queries that FAILED — CH-side
--    (oom / timeout / error) OR cerberus-side (sample_budget / breaker /
--    rejected). These are unambiguous heuristic failures — the query died, was
--    cap-rejected, or blew the sample budget on the single path the classifier
--    chose for it. Client-abandoned rows ('aborted') are excluded: the client
--    walked away, which indicts nothing about the route. Any non-trivial count
--    here is a standalone go signal for calibration, independent of the overlap
--    math. The exit_status breakdown shows which failure class dominates (a
--    sample_budget-heavy bucket says the single-path result set is too large —
--    exactly route B's reason to exist).
----------------------------------------------------------------------------
SELECT
    decision_reason,                                 -- why the classifier kept it on A
    n_anchors,
    fanout,
    cumulative_d,
    count()                                                                 AS failed_route_a_queries,
    -- The six classes below sum to failed_route_a_queries.
    countIf(exit_status = 'oom')                                            AS ooms,
    countIf(exit_status = 'timeout')                                        AS timeouts,
    countIf(exit_status = 'error')                                          AS errors,
    countIf(exit_status = 'sample_budget')                                  AS sample_budget_rejects,
    countIf(exit_status = 'breaker')                                        AS breaker_rejects,
    countIf(exit_status = 'rejected')                                       AS cap_rejects,
    -- Read as cost ACCRUED BEFORE the failure, not as the query's cost: except
    -- on sample_budget rows (where the CH query finished) the counter stopped
    -- wherever ClickHouse gave up, and breaker/rejected rows never ran at all.
    round(quantile(0.99)(memory_usage) / 1e6, 1)                            AS p99_accrued_mem_mb
FROM cerberus_router_corpus
WHERE route = 'A' AND exit_status NOT IN ('ok', 'aborted')
GROUP BY decision_reason, n_anchors, fanout, cumulative_d
ORDER BY failed_route_a_queries DESC
LIMIT 50;
