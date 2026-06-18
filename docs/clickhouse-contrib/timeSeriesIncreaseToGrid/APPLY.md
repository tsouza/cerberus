# Applying `timeSeriesIncreaseToGrid` to a ClickHouse fork

CI-UNVALIDATED: not compiled in this environment; fidelity-reviewed against
ClickHouse `master` source as of 2026-06-18. Build + run the test on a real
ClickHouse checkout before opening the upstream PR.

## 1. Get a ClickHouse checkout

```sh
git clone --recursive https://github.com/ClickHouse/ClickHouse.git
cd ClickHouse
git checkout -b feature/timeSeriesIncreaseToGrid   # branch off master (or a release tag)
```

## 2. Apply the source change

Two ways — the patch, or by hand (the hunks are tiny and line numbers drift, so
hand-application is often easier):

### Option A — git apply

```sh
git apply --reject /path/to/cerberus/docs/clickhouse-contrib/timeSeriesIncreaseToGrid/timeSeriesIncreaseToGrid.patch
```

If it rejects on offset (likely, because the family churns), fall back to
Option B using the `.rej` files as a guide.

### Option B — by hand (per IMPLEMENTATION.md)

Three edits in
`src/AggregateFunctions/TimeSeries/AggregateFunctionTimeseriesExtrapolatedValue.h`:

1. Add trailing `bool is_increase_ = false` to
   `AggregateFunctionTimeseriesExtrapolatedValueTraits`; expose
   `static constexpr bool is_increase = is_increase_;`.
2. Extend `getName()` to return `"timeSeriesIncreaseToGrid"` when `is_increase`.
3. Add the `AggregateFunctionTimeseriesIncreaseTraits` alias (namespace scope),
   mirror `static constexpr bool is_increase = Traits::is_increase;` onto the
   function class, and change the divide guard to
   `if constexpr (is_rate && !is_increase)`.

One edit in
`src/AggregateFunctions/TimeSeries/AggregateFunctionTimeseriesHelpers.cpp`:
add the `documentation_timeSeriesIncreaseToGrid` block and the
`factory.registerFunction("timeSeriesIncreaseToGrid", {createAggregateFunctionTimeseries<true, false, AggregateFunctionTimeseriesIncreaseTraits, AggregateFunctionTimeseriesExtrapolatedValue>, documentation_timeSeriesIncreaseToGrid});`
line.

## 3. Install the functional test

```sh
cp /path/to/cerberus/docs/clickhouse-contrib/timeSeriesIncreaseToGrid/03999_timeseries_increase.sql        tests/queries/0_stateless/
cp /path/to/cerberus/docs/clickhouse-contrib/timeSeriesIncreaseToGrid/03999_timeseries_increase.reference  tests/queries/0_stateless/
```

If `03999_` collides, renumber both files to a free slot (e.g. `04xxx`).

## 4. Build

```sh
mkdir -p build && cd build
cmake .. -DCMAKE_BUILD_TYPE=RelWithDebInfo
ninja clickhouse        # or `ninja` for the full build
```

## 5. Run the test

```sh
cd ..
./tests/clickhouse-test 03999_timeseries_increase
```

The `.reference` asserts two impl-independent invariants:

- Query 1 prints `ALL OK\t<null_cells>\t<value_cells>` — never `HAS MISMATCH`.
  This is the real test: every grid cell is both-NULL or
  `increase == rate * window`.
- Query 2 prints `0` — last grid point's
  `increase - rate * window` rounds to zero.

The shipped reference predicts `4` NULL cells and `17` VALUE cells, derived from
the rate golden in `03254_timeseries_functions.reference` (same 21-point grid,
same window=300, same sample timestamps -> same insufficient-sample NULL
pattern). If your build reports a different split but still says `ALL OK` and
`0`, the function is CORRECT — regenerate the reference:

```sh
./tests/clickhouse-test --generate 03999_timeseries_increase
# or capture the actual output and overwrite the .reference
```

The `ALL OK` / `0` strings are the assertions; the counts are timing-derived.

## 6. Open the upstream PR

```sh
git add src/AggregateFunctions/TimeSeries/ tests/queries/0_stateless/03999_timeseries_increase.*
git commit -m "Add timeSeriesIncreaseToGrid aggregate (PromQL increase on a grid)"
git push origin feature/timeSeriesIncreaseToGrid
gh pr create --repo ClickHouse/ClickHouse --base master \
  --title "Add timeSeriesIncreaseToGrid aggregate function" \
  --body "Adds timeSeriesIncreaseToGrid, the PromQL increase() over a regular grid, as the next member of the experimental timeSeries*ToGrid family (PR #80590). It reuses the existing AggregateFunctionTimeseriesExtrapolatedValue kernel: increase == rate * window, i.e. the rate path (counter-reset correction + boundary extrapolation) without the final divide-by-window. Gated by allow_experimental_time_series_aggregate_functions. Closes the obvious gap: rate/delta shipped, increase did not."
```

PR notes worth pre-empting for the reviewer:

- This is purely additive and reuses the kernel; no behaviour change to existing
  functions (`is_increase` defaults `false`).
- `is_increase` is a compile-time flag; rate/delta codegen is unchanged.
- Mention the `increase == rate * window` identity and that it is NOT the `delta`
  path (delta lacks reset correction).

## 7. Cerberus-side follow-up (after the function ships in a ClickHouse release)

Once `timeSeriesIncreaseToGrid` is in a ClickHouse version cerberus targets,
wire it in behind the EXISTING experimental gate — no new setting:

1. `internal/chsql/range_window_native.go:35` — add the map entry:

   ```go
   var nativeTSGridFn = map[string]string{
       "rate":     "timeSeriesRateToGrid",
       "increase": "timeSeriesIncreaseToGrid",
   }
   ```

2. `internal/promql/lower.go:1924-1926` — widen the eligibility gate so
   `rw.Func == "increase"` is admitted (today the comment explicitly excludes it:
   "no timeSeriesIncreaseToGrid; the timeSeriesDeltaToGrid + reset-semantics
   mapping is unverified"). Update that comment.

3. `internal/chopt` — `FeatureTSGridRange` already stamps
   `allow_experimental_time_series_aggregate_functions=1` on plans that use the
   native node (`internal/chopt/registry.go`; engine path co-stamps via
   `chclient.WithTSGridSetting`). Increase rides the SAME feature flag and the
   same experimental setting — no registry change needed.

4. Differential proof BEFORE flipping: run cerberus's chDB differential harness
   (`range_window_native_chdb_test.go` pattern) comparing the native increase
   emit against the fan-out goldens, paying attention to the pre-window-sample /
   partial-reset edge cases (the same correctness class flagged for rate). Only
   then is the map entry safe to ship.

The minimum server version for the feature should be bumped to the ClickHouse
release that includes `timeSeriesIncreaseToGrid` (set `MinVersion` on the
`FeatureTSGridRange` registry entry, or add a per-func guard if increase needs a
later floor than rate).
