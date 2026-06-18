-- Functional test for timeSeriesIncreaseToGrid.
-- CI-UNVALIDATED: authored against ClickHouse master source as of 2026-06-18,
-- NOT executed in this environment. Place at
-- tests/queries/0_stateless/03998_timeseries_increase.sql in a ClickHouse fork
-- (03998 verified free against master 2026-06-18; renumber if it collides on
-- your target tag).
--
-- Core assertion: increase(c[w]) == rate(c[w]) * w, cell by cell, including
-- NULL alignment. The new aggregate reuses the rate kernel's counter-reset
-- correction and boundary extrapolation and only drops the divide-by-window,
-- so the identity must hold exactly (modulo Float64 representation, hence the
-- abs-diff tolerance on non-NULL cells).

CREATE TABLE ts_increase_raw(timestamp DateTime64(3,'UTC'), value Float64) ENGINE = MergeTree() ORDER BY timestamp;

-- A monotone-ish counter with one reset (drops 8 -> 0 at 1734955601) so the
-- reset-correction path is exercised, mirroring the shipped rate/delta golden
-- in tests/queries/0_stateless/03254_timeseries_functions.sql.
INSERT INTO ts_increase_raw SELECT arrayJoin(*).1::DateTime64(3, 'UTC') AS timestamp, arrayJoin(*).2 AS value
FROM (
select [
(1734955421.374, 0),
(1734955436.374, 0),
(1734955451.374, 0),
(1734955466.374, 0),
(1734955481.374, 0),
(1734955496.374, 0),
(1734955511.374, 1),
(1734955526.374, 3),
(1734955541.374, 5),
(1734955556.374, 5),
(1734955571.374, 5),
(1734955586.374, 5),
(1734955601.374, 0),  -- counter reset
(1734955616.374, 2),
(1734955631.374, 4),
(1734955646.374, 6),
(1734955661.374, 8),
(1734955676.374, 8)
]);

SET allow_experimental_ts_to_grid_aggregate_function = 1;

-- 1) Identity check, collapsed to ONE deterministic verdict row so the
--    .reference is stable WITHOUT having to hand-predict the extrapolation
--    floats. Asserts: every grid cell is either both-NULL or
--    increase == rate * window (within Float64 tolerance). A correct impl
--    prints exactly:  ALL OK <n_null_cells> NULL  <n_value_cells> VALUE
--    No cell may be a MISMATCH.
WITH
    1734955380 AS start, 1734955680 AS end, 15 AS step, 300 AS window
SELECT
    if(countIf(verdict = 'MISMATCH') = 0, 'ALL OK', 'HAS MISMATCH') AS overall,
    countIf(verdict = 'NULL') AS null_cells,
    countIf(verdict = 'VALUE') AS value_cells
FROM (
    SELECT
        multiIf(
            rate_v IS NULL AND inc_v IS NULL, 'NULL',
            rate_v IS NULL OR inc_v IS NULL, 'MISMATCH',
            abs(inc_v - rate_v * window) <= 1e-9 * (1 + abs(inc_v)), 'VALUE',
            'MISMATCH'
        ) AS verdict
    FROM (
        SELECT
            arrayJoin(arrayZip(
                timeSeriesRateToGrid(start, end, step, window)(timestamp, value),
                timeSeriesIncreaseToGrid(start, end, step, window)(timestamp, value)
            )) AS pair,
            pair.1 AS rate_v,
            pair.2 AS inc_v
        FROM ts_increase_raw
    )
);

-- 2) Self-checking spot value: the last grid point's increase MINUS
--    (rate * window) must be ~0. Printing the difference (not the raw value)
--    keeps the .reference impl-independent: it is 0 regardless of the exact
--    extrapolated magnitude.
WITH
    1734955380 AS start, 1734955680 AS end, 15 AS step, 300 AS window
SELECT
    round(
        arrayElement(timeSeriesIncreaseToGrid(start, end, step, window)(timestamp, value), -1)
        - arrayElement(timeSeriesRateToGrid(start, end, step, window)(timestamp, value), -1) * window,
        9
    ) AS increase_minus_rate_times_window
FROM ts_increase_raw;

DROP TABLE ts_increase_raw;
