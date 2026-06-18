# `timeSeriesIncreaseToGrid` — implementation guide

CI-UNVALIDATED: not compiled in this environment; fidelity-reviewed against
ClickHouse `master` source as of 2026-06-18.

This adds a new member to ClickHouse's experimental `timeSeries*ToGrid`
aggregate family: `timeSeriesIncreaseToGrid`, the PromQL `increase()` over a
regular time grid. It is `timeSeriesRateToGrid` WITHOUT the final
divide-by-window — the identity `increase(c[w]) == rate(c[w]) * w`.

## The one load-bearing fact

In `src/AggregateFunctions/TimeSeries/AggregateFunctionTimeseriesExtrapolatedValue.h`,
the `is_rate` flag is OVERLOADED across three behaviours:

1. **counter-reset accumulation** — `doInsertResultInto` sets
   `constexpr bool adjust_to_resets = is_rate;` and accumulates
   `accumulated_resets_in_window` when a sample is smaller than its predecessor
   (a counter wrap).
2. **rising-counter zero-extrapolation clamp** — inside `fillResultValue`:
   `if (is_rate && value_difference > 0 && first_value >= 0) { ... duration_to_start = std::min(duration_to_zero, duration_to_start); }`.
3. **divide-by-window** — the final factor:
   `if constexpr (is_rate) factor = factor * static_cast<Float64>(Base::timestamp_scale_multiplier) / static_cast<Float64>(Base::window);`.

`increase()` needs behaviours (1) and (2) ON — identical to rate — but behaviour
(3) OFF. The existing `delta` path (`is_rate = false`) is therefore WRONG for
increase: delta also skips (1) and (2). So increase is a genuinely new third
mode, not a relabel of an existing one.

The laziest correct mechanism: add a compile-time `is_increase` flag to the
Traits. Register increase with `is_rate = true` (keeping reset + zero-clamp +
the rate branch of `getName()`), plus `is_increase = true`. Then guard ONLY the
divide-by-window with `if constexpr (is_rate && !is_increase)`. No base-class
change, no new file required for the math — one Traits field, one `getName`
branch, one guard tweak, one registration line, one test.

## Files to edit (in a ClickHouse checkout)

1. `src/AggregateFunctions/TimeSeries/AggregateFunctionTimeseriesExtrapolatedValue.h`
   — three small edits (Traits flag, `getName`, divide guard).
2. `src/AggregateFunctions/TimeSeries/AggregateFunctionTimeseriesHelpers.cpp`
   — one new `FunctionDocumentation` block + one `factory.registerFunction`
   line.
3. `tests/queries/0_stateless/03999_timeseries_increase.{sql,reference}`
   — new functional test (provided in this directory).

No new source files. No registration plumbing beyond the one factory line —
`createAggregateFunctionTimeseries` and `createWithValueType` are fully generic
over the Traits/Function templates already.

## Edit 1 — `AggregateFunctionTimeseriesExtrapolatedValue.h`

### 1a. Add the `is_increase` template parameter + flag to the Traits

The current Traits (template params, mirrored from source lines ~21-34):

```cpp
template <bool array_arguments_, typename TimestampType_, typename IntervalType_, typename ValueType_, bool is_rate_>
struct AggregateFunctionTimeseriesExtrapolatedValueTraits
{
    static constexpr bool array_arguments = array_arguments_;
    static constexpr bool is_rate = is_rate_;
    // ...
    static String getName()
    {
        return is_rate ? "timeSeriesRateToGrid" : "timeSeriesDeltaToGrid";
    }
    // ... Bucket ...
};
```

Add a trailing `bool is_increase_ = false` template parameter, expose it as a
static constexpr, and extend `getName()`:

```cpp
template <bool array_arguments_, typename TimestampType_, typename IntervalType_, typename ValueType_, bool is_rate_, bool is_increase_ = false>
struct AggregateFunctionTimeseriesExtrapolatedValueTraits
{
    static constexpr bool array_arguments = array_arguments_;
    static constexpr bool is_rate = is_rate_;
    static constexpr bool is_increase = is_increase_;
    // ...
    static String getName()
    {
        if (is_increase)
            return "timeSeriesIncreaseToGrid";
        return is_rate ? "timeSeriesRateToGrid" : "timeSeriesDeltaToGrid";
    }
    // ... Bucket unchanged ...
};
```

The trailing default `= false` keeps every existing instantiation
(`...Traits<..., is_rate_>`) source-compatible — rate and delta do not pass the
new arg.

### 1b. Mirror the flag onto the function class

Alongside the existing `static constexpr bool is_rate = Traits::is_rate;` (source
line ~67):

```cpp
    static constexpr bool is_rate = Traits::is_rate;
    static constexpr bool is_increase = Traits::is_increase;
```

### 1c. Guard ONLY the divide-by-window

In `fillResultValue`, the current final-factor block (verbatim from source):

```cpp
        Float64 factor = extrapolate_to_interval / static_cast<Float64>(sampled_interval);

        if constexpr (is_rate)
            factor = factor * static_cast<Float64>(Base::timestamp_scale_multiplier) / static_cast<Float64>(Base::window);

        value_difference *= factor;

        result = static_cast<ValueType>(value_difference);
        null = 0;
```

Change ONLY the guard so increase keeps the extrapolation factor but skips the
`/ Base::window` divide:

```cpp
        Float64 factor = extrapolate_to_interval / static_cast<Float64>(sampled_interval);

        if constexpr (is_rate && !is_increase)
            factor = factor * static_cast<Float64>(Base::timestamp_scale_multiplier) / static_cast<Float64>(Base::window);

        value_difference *= factor;

        result = static_cast<ValueType>(value_difference);
        null = 0;
```

The reset accumulation (behaviour 1) and the zero-clamp (behaviour 2) are gated
by `is_rate` alone, which remains `true` for increase — so they are untouched
and increase inherits rate's full counter-correctness. This is the entire math
change.

## Edit 2 — `AggregateFunctionTimeseriesHelpers.cpp` registration

The existing rate registration (verbatim, source line ~371):

```cpp
    factory.registerFunction("timeSeriesRateToGrid",
        {createAggregateFunctionTimeseries<true, false, AggregateFunctionTimeseriesExtrapolatedValueTraits, AggregateFunctionTimeseriesExtrapolatedValue>, documentation_timeSeriesRateToGrid});
```

The `createAggregateFunctionTimeseries` template signature (source ~267-275):

```cpp
template <
    bool is_rate_or_resets,
    bool is_predict,
    template <bool, typename, typename, typename, bool> class FunctionTraits,
    template <typename> class Function
>
AggregateFunctionPtr createAggregateFunctionTimeseries(...)
```

Note the `FunctionTraits` template-template parameter is declared with exactly
FIVE parameters (`<bool, typename, typename, typename, bool>`). Because Edit 1
added the new flag as a TRAILING defaulted (`= false`) parameter, the Traits
template still MATCHES this five-parameter template-template signature — the
defaulted sixth parameter does not change the template-template arity. So the
existing factory machinery binds `is_increase = false` for rate and delta with
zero changes.

For `timeSeriesIncreaseToGrid` we cannot reuse that exact factory line, because
the factory only threads `is_rate_or_resets` into the FIRST four (it passes
`is_rate_or_resets` as the Traits' 5th arg and never sets a 6th). The minimal
options, in laziness order:

### Option A (recommended, laziest): register with an explicitly-bound Traits alias

Introduce a one-line alias that pins `is_increase = true`, and a thin
`createAggregateFunctionTimeseries`-compatible binding. Concretely, add a Traits
alias template near the increase registration:

```cpp
    // timeSeriesIncreaseToGrid reuses the ExtrapolatedValue kernel with the
    // rate counter-correction (is_rate=true) but WITHOUT the divide-by-window
    // (is_increase=true). The alias pins is_increase so the generic factory
    // (which only threads is_rate) still produces a five-arg template-template.
    template <bool array_arguments_, typename TimestampType_, typename IntervalType_, typename ValueType_, bool is_rate_>
    using AggregateFunctionTimeseriesIncreaseTraits =
        AggregateFunctionTimeseriesExtrapolatedValueTraits<array_arguments_, TimestampType_, IntervalType_, ValueType_, is_rate_, /*is_increase=*/true>;
```

Then register exactly like rate, but with the alias and a dedicated doc block:

```cpp
    factory.registerFunction("timeSeriesIncreaseToGrid",
        {createAggregateFunctionTimeseries<true, false, AggregateFunctionTimeseriesIncreaseTraits, AggregateFunctionTimeseriesExtrapolatedValue>, documentation_timeSeriesIncreaseToGrid});
```

The alias is itself a five-parameter template-template (it forwards the first
five and pins the sixth), so it satisfies `createAggregateFunctionTimeseries`'s
`template <bool, typename, typename, typename, bool> class FunctionTraits`
parameter with no factory edits. `is_rate_or_resets = true` keeps reset/zero
correction; `is_increase = true` (pinned by the alias) drops the divide.

Place the alias in the same translation unit; if a free-function template alias
is awkward inside the registration function, hoist it to namespace scope at the
top of the `.cpp` (or beside the Traits in the `.h`).

### Option B (if aliases are disfavoured): add a third factory bool

Thread an `is_increase` bool through `createAggregateFunctionTimeseries` /
`createWithValueType` and pass it as the Traits' sixth arg. Larger blast radius
(touches the generic factory) — avoid unless a maintainer prefers it.

### Documentation block

Copy the `documentation_timeSeriesRateToGrid` block and adapt: name, the
`increase` PromQL link
(`https://prometheus.io/docs/prometheus/latest/querying/functions/#increase`),
the `ReturnedValue` text ("increase values" not "rate values"), and the example
output (rate example values multiplied by the window, e.g. the rate doc's
`0.06666667` at window 60 becomes `4` for increase). `IntroducedIn` should be
the release the PR targets (e.g. `{25, 11}` — set to the actual target). Keep
the `:::warning ... allow_experimental_ts_to_grid_aggregate_function=true :::`
note identical to the rest of the family.

## Experimental gate (unchanged)

No new setting. The function is gated by the same check at
`AggregateFunctionTimeseriesHelpers.cpp:253`:

```cpp
    if (settings && (*settings)[Setting::allow_experimental_time_series_aggregate_functions] == 0
        && (*settings)[Setting::allow_experimental_time_series_table] == 0)
        throw Exception(...);
```

The family's docs and stateless tests enable it via the alias setting
`allow_experimental_ts_to_grid_aggregate_function = 1` (which the test in this
directory uses). Both names work on `master`; confirm aliasing on the target
tag.

## What is deliberately NOT done (laziness discipline)

- No new `.h`/`.cpp` source file — the kernel is reused.
- No base-class (`AggregateFunctionTimeseriesBase.h`) change.
- No new Bucket type — increase shares rate's
  `absl::flat_hash_map<TimestampType, ValueType>` bucket, `serializeBucket` /
  `deserializeBucket`, and `FORMAT_VERSION`.
- No new experimental setting.
- No cerberus reset-semantics reimplementation — increase inherits rate's.
