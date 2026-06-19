# Native ClickHouse Roadmap — Next-Release Execution Dossier

*Addendum to `docs/native-clickhouse-roadmap.md`. Synthesized from 5 adversarially-verified design specs (increase, reduce_over_time moments, holt_winters, histogram_quantile, xrate/xincrease). Seam citations re-checked against `origin/main` on 2026-06-19.*

---

## 1. Executive summary

Every candidate in the `timeSeries*ToGrid` family depends on a native ClickHouse aggregate that **does not exist in any released ClickHouse** — verified five times over against `master` (`AggregateFunctionTimeseriesHelpers.cpp` registers only rate/delta/instant/deriv/predictLinear/changes/resets/resample; nothing else), so the next-release deliverable is necessarily split: (a) **upstream ClickHouse PRs** that add the aggregates, and (b) **cerberus SQL-emission lowerings + golden specs landed DORMANT** behind chopt features whose `MinVersion` floors are set to an unreachable future CH version. The dormancy interlock is real and double-locked — `Experimental` stability is never auto-enabled, and an explicit opt-in below the placeholder floor fail-fasts at boot (`chopt/resolve.go` Enforcing mode) — so the lowerings cannot 502 against today's servers even if mis-configured. The clean win for next release is **`timeSeriesIncreaseToGrid`** (roadmap rank-1, "code-now"): it is one defaulted template bool plus one `if constexpr` guard-flip upstream, its cerberus seam is mechanical and false-lowering-proof, and its only flaw is expository (correct the invariant wording to `increase = rate × window_seconds`). The **moments family** (`*_over_time`, rank-2) may land its cerberus half dormant next release too, but its upstream C++ spec is mis-architected (streaming-Welford `Bucket` returns wrong values whenever `window > step`, plus a traits template-arity mismatch) and must be rewritten before any real floor. Everything heavier — **holt_winters** (rank-4, gated behind moments proving the Traits pattern), **histogram_quantile** (needs a new non-scalar-array C++ base AND has a bare-selector window-semantics landmine), and **xrate/xincrease** (rank-7, needs a shared-base left-closed-window change + a parser-fork policy decision) — is **DEFER / upstream-PR-first**, not shippable next release in any form.

---

## 2. Prioritized execution order

| Candidate | Tier | Upstream-CH PR effort | Cerberus-lowering effort | Lands in next cerberus release | Waits for | Recommendation |
|---|---|---|---|---|---|---|
| **`timeSeriesIncreaseToGrid`** (`increase()`) | code-now (rank 1) | **Small** — 1 defaulted template `bool is_increase` threaded through `createAggregateFunctionTimeseries`; guard the divide with `is_rate && !is_increase`; retain `is_rate` zero-clamp; 1 registration line | **Small** — map entry + switch arm + boot-wired Native/Fanout lowerers + experimental feature (placeholder floor) | **YES** — B.1–B.5 lowering + C.1 golden, dormant behind placeholder floor | A real floor (only after CH PR tags) | **PURSUE-NOW (cerberus dormant) + UPSTREAM-FIRST (the CH PR gates going live)** |
| **`timeSeriesReduceOverTimeToGrid`** (`*_over_time`: sum/avg/min/max/count/stddev/stdvar) | ambitious (rank 2) | **Medium-Large** — must KEEP per-bucket sample map (like ChangesTraits' `flat_hash_map`), compute moments in `fillResultValue` over the slid `samples_in_window` (single-pass Welford there); **7 distinct 5-param traits structs** `<bool, Ts, Iv, Val, bool>`, NOT one enum-parameterized traits | **Small-Medium** — 7 names into the func-agnostic emitter + switch; first/last_over_time carve-out preserved | **YES (cerberus half only)** — B mechanical lowering + C.1 golden, dormant; do NOT advertise, do NOT set real floor | A **rewritten** CH PR (current A.1/A.2 won't compile and returns wrong values for `window > step`) | **UPSTREAM-FIRST** (cerberus scaffold may bank dormant) |
| **`timeSeriesHoltWintersToGrid`** (`double_exponential_smoothing`) | ambitious (rank 4) | **Small-Med** — copy LinearRegression sort-by-ts; recurrence, no cross-fn invariant | **Medium** — net-new `Scalars []float64` on `RangeWindowNative` (+ `Equal`), thread sf/tf through the 4-param `Parametric` emit, new feature | **NO** — IR `Scalars` plumbing has no consumer until the aggregate exists; speculative surface | CH PR tagged + moments proving Traits pattern first | **DEFER (sequence after moments)** |
| **`timeSeriesHistogramQuantileToGrid`** (classic + OTel exp histograms) | ambitious (rank 5) | **Large** — new non-scalar-array `Bucket` sibling base + merge + `FORMAT_VERSION` serialize for distributed/two-level agg | **Medium** — net-new IR node + emit path (does NOT flow through `RangeWindow`); gate MUST be narrowed | **NO** — bare-selector window-semantics bug would bury a wrong-results landmine behind the gate | CH PR + **gate narrowed** to aggregated-`rate`-over-range idiom only (reject bare selector) | **DEFER / UPSTREAM-FIRST (gate fix mandatory before any dormant land)** |
| **`timeSeriesXRateToGrid` / `XIncrease`** (MetricsQL `xrate`/`xincrease`) | ambitious (rank 7) | **Med-Large** — structural change to **shared** `AggregateFunctionTimeseriesBase` left-open→left-closed window (gated) + reset-accumulator reshuffle; regression surface across whole family | **Small** — additive map/switch/lowerers; PLUS a parser-fork change (`tsouza/prometheus` can't tokenize `xrate`/`xincrease`) | **NO** | Semantic definition pinned (Mimir fixed-window, drop VictoriaMetrics citation) + parser-surface policy + CH PR | **DEFER (strictly behind increase)** |

---

## 3. The two concrete cerberus PRs that can land in the next release

Both ship **SQL emission + dormant feature only — NOT enabled**. Neither touches a real version floor.

### PR-A — `feat(chsql): lower increase() to timeSeriesIncreaseToGrid (dormant, experimental)`

The clean, recommended next-release landing. Mirrors the shipped changes/resets adoption field-for-field.

**Files touched:**
- `internal/chsql/range_window_native.go` — add `"increase": "timeSeriesIncreaseToGrid"` to `nativeTSGridFn` (currently lines ~51-54, rate/changes/resets); add `increase` to the `ErrUnsupported` hint string at `:122` (`supported: rate, changes, resets` → `…, increase`).
- `internal/promql/lower.go` — add `case "increase": node = ctx.lowerers.Increase.LowerIncrease(rw, s)` to the `switch c.Func.Name` (the changes/resets/default block verified at `:1932-1939`); update the two stale "no `timeSeriesIncreaseToGrid` aggregate yet" comments. Reuse the **func-agnostic** `nativeTSGridMatrixNode` (`:2039`, `if rw.Func != wantFunc → nil`) — it admits ONLY the eligible shape (Step>0, Start/End pinned, `!Identity`, plain Scan/Filter input), so an ineligible query falls through to fan-out. **No false-lowering risk.**
- `internal/promql/lower_strategy.go` — `NativeIncreaseLowerer` / `FanoutIncreaseLowerer` mirroring `NativeChangesLowerer`/`FanoutChangesLowerer`, with `withDefaults` boot-wiring.
- `cmd/.../main.go` — boot-wire the Increase strategy (the `main.go:412-435` DI block).
- `internal/chopt/registry.go` — `FeatureTSGridIncrease`, `Experimental`, `MinVersion` = **placeholder future floor** (e.g. `{26,99}`), explicitly commented as unreachable-until-upstream-merge.

**Golden specs added:**
- `test/spec/promql/increase_to_grid_native.txtar` — asserts the emitted SQL text (`timeSeriesIncreaseToGrid(<start>,<end>,<step>,<window>)`), behind a test-only feature toggle.
- A differential VALUES test, **`t.Skip`-gated on a `system.functions` probe** (un-skips automatically once a chDB carrying the function exists). Use a relative/ULP tolerance — increase goes through extrapolation arithmetic (unlike integer-exact changes/resets).
- Assertion wording MUST read `increase == rate × window_seconds` (see §4 note on the internal `Base::window` rescale). Do NOT assert the unqualified `rate × window`.

### PR-B — `feat(chsql): lower *_over_time to timeSeriesReduceOverTimeToGrid (dormant, experimental)` *(optional this release; banks mechanical work)*

Safe to land dormant **only as scaffolding** — do NOT advertise the family or set a real floor; the upstream C++ it depends on is not yet correct (see §4).

**Files touched:**
- `internal/chsql/range_window_native.go` — add the seven names (`sum/avg/min/max/count/stddev/stdvar_over_time` → `timeSeriesReduceOverTimeToGrid` variants) to `nativeTSGridFn`; extend the `:122` hint. Emitter is genuinely func-agnostic (selects only the aggregate NAME, identical SQL shape).
- `internal/promql/lower.go` — widen the `:1933` switch; **keep `first_over_time`/`last_over_time` in `default`** (they hit the `__name__`-preservation wrap at `:1965` and are absent from the map — mis-routing errors rather than mis-lowers). Exclude `present_over_time`/`quantile_over_time`/`mad_over_time`.
- `internal/promql/lower_strategy.go` — `Native`/`Fanout` `ReduceOverTime` lowerers.
- `internal/chopt/registry.go` — `FeatureTSGridReduceOverTime`, `Experimental`, unreachable `MinVersion`.

**Golden specs added:**
- `test/spec/promql/reduce_over_time_to_grid_native.txtar` — SQL-text goldens for the seven names.
- A `t.Skip`-gated differential test. NULL-vs-NaN reconciliation already holds (native `WHERE grid_val IS NOT NULL` ≡ fan-out `WHERE length(window_vals) >= 1`). **`count_over_time` zero-fill carve-out** stays untouched (the Tempo/metrics emitter `emitRangeWindowMetrics` is a different branch and zero-fills empty buckets).

> Recommendation: ship **PR-A this release**. Ship PR-B **only if** you want to bank the mechanical scaffold now; otherwise hold it until the rewritten moments CH PR is in flight, to avoid a dormant lowerer whose upstream is still mis-designed.

---

## 4. Upstream ClickHouse PR queue

In strict dependency order. Each is a separate PR against `ClickHouse/ClickHouse` master, `src/AggregateFunctions/TimeSeries/`.

1. **`timeSeriesIncreaseToGrid`** — *patch essence:* add a defaulted `bool is_increase` template param to the `ExtrapolatedValue` traits, thread it through `createAggregateFunctionTimeseries`, and guard ONLY the per-window divide with `if constexpr (is_rate && !is_increase)` (retain the `is_rate` zero-clamp); 1 `factory.registerFunction` line. *Differential invariant:* `increase == rate × window_seconds`. **Note for the PR description/golden header:** this holds because `Base::window` is internally rescaled by `timestamp_scale_multiplier` (via `normalizeParameter`), so dropping `/Base::window` yields `value_diff × increase_factor = rate × window_seconds`; the multiplier cancels. State the invariant in seconds; do not hand-wave it as `rate × window`. The retained zero-clamp is what distinguishes `increase` from `delta` on resets (roadmap reference: `ts=1734955680`, `rate×300 = 8.1138` vs `delta = 8.3491`).

2. **`timeSeriesReduceOverTimeToGrid` (family)** — *patch essence (REWRITTEN from spec):* **keep per-bucket raw-sample storage** exactly like `AggregateFunctionTimeseriesChangesTraits` (`absl::flat_hash_map`); compute sum/avg/min/max/count/stddev/stdvar in `fillResultValue` over the finalize-time slid `samples_in_window` deque (single-pass Welford *there*, for the #400 large-value stability win) — **NOT** as streaming `Bucket` state. Define **seven distinct traits structs** matching the real 5-param arity `<bool array_arguments_, typename TimestampType_, typename IntervalType_, typename ValueType_, bool is_X_>` (the factory template-template slot is 5-param with leading+trailing `bool`; a leading-enum 4-param traits cannot bind). *Differential invariants:* `Sum == arraySum`, `Avg == arrayAvg`, `StdVar == Σ(x-μ)²/N` (population, matching cerberus `varPopTwoPassFrag`), empty window → NULL (matching `IS NOT NULL`). **These invariants are correct as targets but are NOT met by the spec's streaming-Welford `Bucket`, which returns wrong values whenever `window > step` (the common case, e.g. `avg_over_time(m[5m])` at 1m step) because each sample lands in one bucket but contributes to up to 5 overlapping grid points.**

3. **`timeSeriesHoltWintersToGrid`** — *patch essence:* copy the LinearRegression sort-by-ts traits; recurrence seeds `s = y[1]`, `b = y[1]-y[0]`, folds over `y[2..]`, `<2 samples → NaN`. Takes sf/tf as the **5th positional grid parameter** (predict-linear's `is_predict` 5-param form is the precedent). *Differential invariant:* **none** — Holt-Winters is a recurrence with no cross-function algebraic identity; assert structural pins instead: (a) 2-sample window → `y[1]` for any sf/tf; (b) flat series, tf→0 → constant; (c) `<2 → NaN`; (d) a hand-computed 4+-sample recurrence at fixed (sf,tf). *Sequenced after #2 proves the Traits pattern.*

4. **`timeSeriesHistogramQuantileToGrid`** — *patch essence:* the ONLY candidate needing a **new sibling base** (non-scalar-array `Bucket` holding an exp-histogram + perfect-subset downscale merge `offset >> k`, `i >> k`, plus `FORMAT_VERSION` serialize for distributed/two-level aggregation). *Differential invariant:* native grid == SQL-walk grid — **but this fails for the bare selector unless the gate is narrowed.** The bare selector `histogram_quantile(φ, exp_hist{...})` uses **latest-sample** staleness (`argMax`/5m, i.e. `timeSeriesResampleToGridWithStaleness` semantics), while only the aggregated `rate(exp_hist[r])` idiom merges all in-window samples. A merge-everything function reproduces only the latter. **Restrict the aggregate (and the cerberus gate) to the aggregated-range idiom; hard-reject the bare selector** (route it to resample-with-staleness + quantile, or leave it on fan-out). `phi` is the 5th positional slot (predict-linear precedent) — drop the "parametric like nothing else" framing.

5. **`timeSeriesXRateToGrid` / `timeSeriesXIncreaseToGrid`** — *patch essence:* a **structural, gated** change to the **shared** `AggregateFunctionTimeseriesBase` window from left-open `(range_start, t]` to left-closed `[range_start, t]` (to retain the first-class pre-window sample), plus the corresponding reset-accumulator reshuffle — NOT a leaf-Traits constexpr, and it carries regression surface across rate/delta/changes/resets/deriv/predict. *Differential invariant:* `xincrease == xrate × window_seconds` — **true ONLY under the fixed-window divisor** (Mimir-anchored `rate = delta / rangeSeconds`, matching CH's `factor *= scale_mult / Base::window`), **NOT** under VictoriaMetrics `rollupDerivFast` (`dv/dt`, actual inter-sample dt). Pin the semantics to Mimir fixed-window; drop the VictoriaMetrics citation.

---

## 5. Honest non-goals for next release

- **No live (floor-enabled) native lowering for ANY candidate.** Every function is absent from released ClickHouse; an enabled lowering 502s with `UNKNOWN_AGGREGATE_FUNCTION`. All five verdicts converge on this. The next-release ceiling is **dormant scaffolding behind a placeholder future floor**.
- **No moments family advertised as available, and no real `MinVersion` for it.** Verdict 2 is NEEDS-FIX: the upstream C++ is mis-architected (wrong results for `window > step`; traits won't compile). Land PR-B only as banked scaffolding, if at all.
- **No holt_winters cerberus PR — not even dormant.** Verdict 3: the only independently-valuable piece (the `Scalars []float64` IR plumbing) has **no consumer** until the aggregate exists, making it speculative surface behind a permanently-floored feature. Roadmap explicitly gates rank-4 behind rank-2 proving the Traits pattern.
- **No histogram_quantile cerberus PR until the gate is narrowed.** Verdict 4: landing the spec as-written buries a wrong-results landmine (bare-selector latest-sample vs merge-everything) that detonates the day someone sets a real floor — worse than a hard error. Gate-narrowing is a hard precondition.
- **No xrate/xincrease work, and no parser-fork change this release.** Verdict 5: blocked on (a) a contested semantic decision (Mimir vs VictoriaMetrics divisor), (b) a structural shared-base window change with cross-family regression risk, and (c) a `tsouza/prometheus` parser-fork policy decision (it can't even tokenize `xrate`/`xincrease` today). Strictly sequenced behind `timeSeriesIncreaseToGrid`, which establishes the Traits axis WITHOUT the left-closed-window complication.
- **No "drop SQL entirely" / chDB-embed / QueryPlan-packet bypass work.** Out of scope for this addendum; the roadmap's Bypass-SQL verdict already scopes native columnar decode as the only sane version, independent of this family.
- **Do NOT fill a real floor or un-skip any differential VALUES test** for any candidate until its upstream PR merges AND tags into a release the chDB differential substrate can be bumped to. Until then every native path is "correct to write, unsafe to enable."

---

*Key seam anchors (re-verified against `origin/main`, 2026-06-19): `nativeTSGridFn` map and `ErrUnsupported` hint — `internal/chsql/range_window_native.go:51-54,122`; func dispatch switch — `internal/promql/lower.go:1932-1939`; func-agnostic eligibility node `nativeTSGridMatrixNode` — `internal/promql/lower.go:2039`; strategy mirror — `internal/promql/lower_strategy.go`; feature floors + `Experimental` + `allow_experimental_time_series_aggregate_functions` — `internal/chopt/registry.go`; Enforcing-mode fatal-on-unmet-floor — `internal/chopt/resolve.go`. Roadmap rankings — `docs/native-clickhouse-roadmap.md` (increase rank-1/code-now; moments rank-2/ambitious). Upstream template — `ClickHouse/ClickHouse:src/AggregateFunctions/TimeSeries/` (registration in `AggregateFunctionTimeseriesHelpers.cpp` lists only rate/delta/instant/deriv/predictLinear/changes/resets/resample — none of the five candidates exist yet).*
