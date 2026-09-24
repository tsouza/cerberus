# ClickHouse optimizations — background

This document collects the design rationale, rejected alternatives, upstream
history, and the measurements behind
[`clickhouse-optimizations.md`](clickhouse-optimizations.md). It answers "why
is it built this way" rather than "what does it do" — nothing here is needed to
configure or operate the optimization suite; `clickhouse-optimizations.md` is
self-sufficient for that.

## Why a feature is `autoSelect: no`

Two representative reasons a feature lands there: `columnar_result_decode` (a
perf tradeoff), and `ts_grid_changes` (a correctness gap — the native builtin
diverges from reference Prometheus on NaN-adjacent windows; the divergence and
its reproduction are recorded in
[#1721](https://github.com/tsouza/cerberus/issues/1721), and the posture lifts
when a ClickHouse release fixes the builtin, not on a cerberus-side change).

## Why `aggregation_in_order` is a registry entry

The `aggregation_in_order` entry is the migration of the dark
`optimize_aggregation_in_order` rule into the registry. The eligibility check
itself is unchanged; only its enablement now flows from the resolved set.

## Why `condition_cache` is withheld on most builds before the 26.x backports

The query condition cache is meant to be result-equivalent, which is why the
feature ships under `auto`. Two upstream defects break that on builds cerberus
supports, and both were reproduced with cerberus's own DDL and emitted queries
rather than taken from the changelog:

- **Skip-index attribution (ClickHouse#105686).** With
  `use_skip_indexes_on_data_read` on (the default from 26.1), the bloom index on
  `mapValues(ResourceAttributes)` drops whole marks ahead of `PREWHERE`, and the
  empty read was recorded as "the `PREWHERE` predicate matches nothing here".
  TraceQL lowers `{ resource.service.name = "s7" && duration > 100ms }` to
  `PREWHERE (Duration > ?) WHERE (ResourceAttributes[?] = ?)`, so it poisoned the
  entry every later `PREWHERE (Duration > ?)` read — a broad
  `{ duration > 100ms && resource.service.name != "none" }` count over 600,000
  spans came back as 100,288. The reproduction needs enough rows that a whole
  read batch falls outside the narrow predicate; at 200,000 spans it did not
  trigger. PromQL label matchers are not exposed to this defect: they lower to
  `coalesce(nullIf(Attributes[?], ''), nullIf(ResourceAttributes[?], ''), '') = ?`,
  which no skip index can serve.
- **Row-policy attribution (ClickHouse#107145).** A row policy is applied ahead
  of `PREWHERE`, so granules a restricted user cannot see were recorded as
  non-matching for the predicate itself. A restricted user reading
  `otel_metrics_gauge` directly with `PREWHERE (MetricName = 'x')` poisons
  cerberus's PromQL read of the same metric, which goes through
  `merge(currentDatabase(), '^(otel_metrics_gauge|otel_metrics_sum)$')` with the
  identical predicate: an instant `max_over_time` over 50 series returned 19.
  Two narrower shapes did not reproduce: a restricted read *through* `merge()`
  wrote nothing harmful, and a metric name with OTel-to-Prometheus spelling
  alternatives lowers to `WHERE MetricName IN (...)`, which ClickHouse moves to
  `PREWHERE` itself without attributing it.

Every boundary was located on released images, with the cerberus-shaped
reproductions of both defects run against each:

| Build         | Skip-index defect | Row-policy defect |
| ------------- | ----------------- | ----------------- |
| `25.3.14.14`  | clean             | reproduces        |
| `25.4.13.22`  | clean             | not probed        |
| `25.6.13.41`  | clean             | not probed        |
| `25.8.33.6`   | clean             | reproduces        |
| `25.10.7.6`   | clean             | not probed        |
| `25.12.11.4`  | clean             | reproduces        |
| `26.1.12.23`  | reproduces        | reproduces        |
| `26.2.19.43`  | reproduces        | reproduces        |
| `26.3.12.3`   | reproduces        | reproduces        |
| `26.3.13.31`  | clean             | not probed        |
| `26.3.16.16`  | clean             | reproduces        |
| `26.3.17.4`   | clean             | reproduces        |
| `26.3.17.56`  | clean             | clean             |
| `26.3.33.24`  | clean             | clean             |
| `26.4.3.37`   | reproduces        | not probed        |
| `26.4.4.38`   | clean             | reproduces        |
| `26.4.5.143`  | clean             | clean             |
| `26.5.1.882`  | reproduces        | not probed        |
| `26.5.2.39`   | clean             | not probed        |
| `26.5.5.8`    | clean             | reproduces        |
| `26.5.6.64`   | clean             | clean             |
| `26.5.7.64`   | clean             | not probed        |
| `26.6.1.1193` | clean             | clean             |
| `26.6.8.7`    | clean             | clean             |
| `26.7.13.12`  | clean             | not probed        |
| `26.8.10.6`   | clean             | clean             |

Each range's upper bound is the first released build the reproduction came
back clean on, not the upstream backport build number (`26.3.17.50`,
`26.4.5.134`, …): the backport build is a CI build of the release branch, and
the recorded bound is the published tag an operator can actually run. No
released tag sits between the two. `26.3.17.4` and `26.3.17.56` share a patch
number and differ in the fix, which is why the version model carries the build
component and not only the patch.

The real-server test pins the last affected and first fixed release of every
backported line for each defect — thirteen builds — and runs them three at a
time; the reproductions are correctness checks, so sharing CPUs cannot change
a verdict, and one after another they would add six minutes to the lane.

The override is decided across every node cerberus can reach rather than the
one node the resolution probes, because the poisoning is per server: a mixed
fleet with one affected replica returns wrong results for every query that
lands there, however healthy the rest is. A failed resolution must not decide
it either: under `enforcing`, a rollback into an affected build makes the
explicit selection unresolvable, and keeping the previous set would otherwise
keep both the engine's `use_query_condition_cache=1` and a lifted override —
silent wrong results on exactly the build the gate exists for.

Vendor builds are judged by line because their build numbers are not
upstream's: an Altinity build such as `26.3.17.10034.altinitystable` compares
above `26.3.17.56` numerically whether or not it carries the backport. Treating
the whole line as affected costs a vendor build that has the fix its condition
cache; crediting it with a fix it may lack would cost wrong results.

An explicit `condition_cache` under `enforcing` now refuses to boot on every
25.x build, where it used to enable the cache. That is deliberate: the
operator asked for a mechanism that returns wrong results there, and
`enforcing` exists to make "I require this" fail loudly rather than degrade.

Raising the global floor or dropping the feature would have been the blunt
alternatives. Neither was needed: the defects are confined to known builds,
and on every other build the cache stays a result-equivalent win. The row-policy
defect is also why the gate cannot key on whether cerberus's own user has a
row policy — the poisoning query can come from any user of the server, and a
restricted user often cannot see `system.row_policies` to find out.

## Why every cancelled dispatch kills its own statement

The cancellation probes drive a `double_exponential_smoothing` over one series
whose samples all fall in the window (one `arrayFold` call) and a PromQL
selector over a label name tens of millions of characters long that all need
`replaceRegexpAll` normalization. In the exploratory runs, under a 1 s
`max_execution_time`, `26.6.8.7` ran the fold for 9.7 s and `26.7.13.12`
stopped it at 1.05 s; a 20 MB label name ran 2.3 s on `26.6.1.1193` and
2.6 s on `26.7.13.12` against a 1 s limit, and stopped at 1.01 s on
`26.8.1.2041`.

The probes also showed that a client disconnect was not bounded even on the
fixed builds. A cancelled clickhouse-go dispatch sends `ClientCancel`, which
ClickHouse notices only between pipeline blocks, not inside a long function
call, while the in-function checks the upstream fixes added honour `KILL QUERY`
and `max_execution_time` only. Before cerberus killed every cancelled dispatch,
the single-shard path — the default deployment — freed the request's admission
slot and its connection while the fold or replacement ran to completion, on
every build; only the data-shard fan-out gate already issued `KILL QUERY`. The
cost of the kill is one extra statement per cancelled dispatch, and a wait
bounded by the kill timeout before the release proceeds when the server cannot
interrupt the call or the kill itself queues for a pooled connection — the
honest account of a slot whose work is still running.

A dispatch cancelled before the driver got the server's answer — still waiting
for a pooled connection, a dial, or the send — issues no kill. Under pool
saturation the kill would queue for the same pool the dispatch was waiting on,
holding the admission slot for the full timeout to kill a statement that never
existed.

The probes size their own workload. The same seed ran in 5.7 s on one build
and 10.9 s on another, and about twice as fast on a CI runner as on a
developer host, so a fixed seed that suits one substrate starves another: on
CI a fixed fold left 1.9–3.5 s of work at the cancellation, too little to tell
the outcomes apart. Each shape is therefore calibrated per build: seeded at a
base size, run uncancelled, and re-seeded in proportion until the run takes
about 10 s, within a size bound. The server runs at half a CPU during these
probes. The functions are single-threaded, so the throttle stretches one call
without growing it, and growing the fold instead costs about 4 KiB of memory
per sample — two concurrent siblings at the size a fast runner needs would not
fit — while its own teardown after an interrupt grows with it: unthrottled at
700,000 samples, interrupted siblings took 2.5 s to end.

The verdict is relative to the work that was left at the cancellation: an
interrupted call must end within a quarter of it, an uninterrupted one no
sooner than half of it. Observed: interrupted calls ended within 0.14 of the
remainder, uninterrupted ones after 0.7 or more. A fixed threshold would have
to be retuned per substrate; a relative one scales with the calibrated work.

`KILL QUERY` on one's own query is allowed under `readonly = 1` and
`readonly = 2`, so a read-only cerberus user can issue it.

## Why `ts_grid_range` is auto-enabled, and the scan-order gate that was not shipped

A prod-data validation proved the native path result-correct (more correct than
the buggy fan-out for `rate`) at flat memory, so `auto` picks it by version.

An order-independent scan-side gate exists and is sound, but
[#2924](https://github.com/tsouza/cerberus/issues/2924) measured it at a
~2.1-2.4x wall-clock tax on this exact path even under the best-case ORDER BY
alignment, for no memory benefit, and closed without shipping it (see
`chsql.nativeTSGridFn`'s own "Verdict on the scan-order gate" doc for the full
measurement). The only remaining path is an upstream ClickHouse report, which
needs authorization this repo has not given.

## Why `ts_grid_changes` is permanently opt-in, and why its floor is 25.9

Tracked as [#1721](https://github.com/tsouza/cerberus/issues/1721), closed by
making the feature permanently opt-in (`autoSelect: no`) rather than waiting on
an upstream fix: the divergence lives inside the ClickHouse builtin, which
cerberus cannot patch, so `CERBERUS_CH_OPTIMIZATIONS=ts_grid_changes` must be
listed explicitly and `auto` never selects it.

A 25.6 floor would mis-advertise support on 25.6-25.8 servers and 502 with
ClickHouse error code 46, `Function with name timeSeriesChangesToGrid does not
exist` — an absent `timeSeries*ToGrid` member is reported as an unknown
FUNCTION, not as `UNKNOWN_AGGREGATE_FUNCTION` (verified against 25.7 and 25.8
servers).

## Why `ts_grid_deriv` / `ts_grid_predict_linear` pin the family's 25.9 floor

Their aggregates (`timeSeriesDerivToGrid` / `timeSeriesPredictLinearToGrid`)
shipped in ClickHouse **25.8** (PR #84328) — a quarter EARLIER than
changes/resets — but the registry pins them to the family's shared **25.9**
floor (the left-open-window fix, PR #86588) so one probed capability verdict
governs every member.

The native == fan-out numeric differential (a Float64 fit, so ULP-close rather
than bit-identical) is proven on a `>= 25.9` server in the prod/e2e lane, not
on the sub-25.9 chDB CI substrate, where the version gate keeps both on the
fan-out; the always-on SQL-shape goldens (`native_deriv_range_step.txtar`,
`native_predict_linear_range_step.txtar`) pin the native emit unconditionally.

## Why `ts_grid_recollapse` splits the grid into `-State` / `-Merge`

On a reference deployment's heaviest APM range query — 35,094 raw series over a
300s window — this is **-28.5% CPU time (1.40x)**: 310.3 CPU-seconds down to
221.9. Wall time on the same paired runs moved 28.0s to 19.8s, but wall and CPU
diverge with concurrency and shard count, so the resource saving is the claim
rather than the latency.

The `-State`/`-Merge` pair is load-bearing rather than incidental. Key
sanitisation is **non-injective**: those 35,094 raw series shape onto 33,557
output series, and the colliding groups are frequently time-disjoint with a
splice gap smaller than the range window, so grid anchors near a splice have
windows straddling both halves. Prometheus `rate()` is defined over the POOLED
sample set at each anchor, which is what merging partial states computes;
combining two FINISHED grids arithmetically is a different number, wrong at 481
of 93,757 points on that query and NULL where one half holds fewer than two
samples.

### Where the 25.9 floor comes from, and the merge-exactness probe

The **25.9** floor is INHERITED from `ts_grid_range` rather than independently
derived: with `ts_grid_range` off there is no native node to defer anything
past, so that feature's floor is the effective one and nothing about the
re-collapse raises it. Merge exactness is not the binding constraint. The
exactness probe compares one pooled pass against a merge of two per-group
partial states, over samples split into two time-disjoint halves whose splice
gap is smaller than the range window, so the grid anchors near the splice
straddle both halves:

```sql
-- pooled
SELECT timeSeriesRateToGrid(start, end, 60, 300)(ts, val) FROM src
-- merged
SELECT timeSeriesRateToGridMerge(start, end, 60, 300)(st) FROM (
  SELECT timeSeriesRateToGridState(start, end, 60, 300)(ts, val) AS st
  FROM src GROUP BY raw)
```

Executed against 25.8.28.1, 25.9.7.56, 26.1.12.23, 26.2.19.43, 26.3.17.56,
26.4.5.143 and 26.5.6.64 (no 26.0 image is pullable, so that version is
bracketed by its neighbours) in each of three data regimes — time-disjoint,
interleaved, and counter-reset-straddling — the merged grid equals the pooled
grid in all 21 cells. The reset-straddling regime yields a grid that DIFFERS
from the other two, which is what shows the reset correction is applied through
the merge rather than skipped. The floor is therefore the same one every other
family member pins, so one probed capability verdict still governs the whole
set.

## What `ts_grid_histogram` measured, and where its floor comes from

Measured against a real ClickHouse 26.6 at realistic scale, same rows read
(~88-89k), 121 anchors, 5m window: the array fold takes 4,123 ms / 3.411 GB
peak / 51.5 CPU-s, the native aggregate 148 ms / 0.130 GB peak / 0.4 CPU-s —
**28x faster, 26x less memory, 129x less CPU**.

The **25.9** floor is INHERITED from the family rather than independently
derived: the shape rides the same `timeSeriesRateToGrid` `ts_grid_range` pins,
so it inherits that feature's binding constraint (the left-open / right-closed
membership window, upstream PR #86588). The presence aggregate is a
`ts_grid_resets` sibling from PR #86010, released in the same 25.9, so the
floor is unchanged either way.

## Why `quantile_prom_histogram` is opt-in, and where its cardinality ceiling comes from

A real-CH differential (`internal/chsql`'s
`TestHistogramQuantile_RankWalkNative_DifferentialRealCH`) confirmed exact
agreement with the legacy walk across representative bucket layouts (a normal
crossing, a duplicate-bound layout, the equal-length/no-overflow-rung shape, an
empty histogram, a first-bucket non-positive upper bound, and an all-zero-count
histogram) and the full phi domain (below range, the two saturating edges,
interior crossings, above range, and a runtime NaN phi).

`AutoSelect` is `false`: correctness parity is proven, but a real-scale
measurement (25.10.7.6, a real OTel classic-histogram export) found a genuine
performance TRADEOFF, not just an unproven new floor — the ORIGINAL emission's
`ARRAY JOIN` multiplied row count by the bucket-ladder length before `GROUP BY`
collapsed it back down, which the legacy walk never does. At real-world
dashboard scale (3,677 series) the native path was ~2x faster at equal memory;
at high series cardinality (73,540 series, ~880k post-unnest rows) wall time
stayed roughly even but memory grew ~3.3x. A follow-up real-ClickHouse 25.10
measurement at four additional cardinality points between those two (the same
real sample, synthetically fanned out) found memory crossed above the classic
walk's between roughly 18,000 and 22,000 series (~215k-265k post-unnest rows at
this sample's 12-bucket layout) and kept growing roughly linearly with series
count past that point ([#2790](https://github.com/tsouza/cerberus/issues/2790)
PR 1).

A second real-ClickHouse 25.10 measurement, run after rewriting the emission to
the BOUNDED-window shape above (#2790 PR 2), found the native path's OWN peak
memory drops ~1.5x-2x at every one of five cardinality points re-tested (3,677
through 73,540 series) relative to the original unbounded emission, because the
`ARRAY JOIN` now unnests a small constant number of rungs per row instead of
the whole ladder. The tradeoff shrinks rather than disappears — a per-row rank
search plus a narrower `ARRAY JOIN` still costs more than the classic walk's
pure array-expression form once cardinality is high enough — so `AutoSelect`
stays `false`.

The operator-facing ceiling is 1.5x PR 1's original ~15,000-series guidance,
taking the CONSERVATIVE (1.5x) end of the measured ~1.5x-2x memory-reduction
range so an operator whose workload sits at the low end of that range still has
real headroom; up to ~30,000 series is achievable in the best-measured (2x)
case, but is not the safe default to design around. See
[#2790](https://github.com/tsouza/cerberus/issues/2790) for the full numbers
and methodology from both PRs.

## Why `map_bucketed_serialization`'s floor is 26.4 rather than 26.3

**Version floor is 26.4, not the 26.3 the upstream backport (v26.3.2.3-lts)
technically landed in**: this registry compares `(major, minor)` only, and
26.3.0/26.3.1 lack the feature, so a "26.3" floor would wrongly claim they have
it.

## Why `column_statistics` is opt-in

**PREWHERE reordering IS verified, not merely claimed**: the issue itself
flagged as unverified "whether statistics-based condition reordering hooks into
cerberus's explicitly written PREWHERE clause (vs only the WHERE→PREWHERE move
optimizer)"; upstream RFC
[ClickHouse#53240](https://github.com/ClickHouse/ClickHouse/pull/53240) ("use
statistic to order prewhere conditions better") confirms
`allow_statistics_optimize` reorders an ALREADY-multi-condition PREWHERE
clause's own conjuncts by statistics-derived selectivity — exactly cerberus's
own emission shape, not only the promotion decision. `AutoSelect` is still
`false`, though: the Cloud gap remains, the real-world MAGNITUDE of a PREWHERE
reorder or a join-side pick on cerberus's own production query shapes is not
yet measured, and a plan-shape change from better join-side selection could
interact with the solver's calibrated fanout-guard constants the way spill
settings did in [#2665](https://github.com/tsouza/cerberus/issues/2665) —
real-world calibration questions this feature alone cannot answer, so enabling
it is a deliberate operator choice pending that evidence.

## Why `join_spill` exists, and why it stamps an explicit byte bound

`join_spill` closes the last gap in the "spill settings only protect GROUP
BY/sort" pain point: join memory was previously backstopped only by throwIf
cardinality guards (`VectorJoin`'s own `ManyToManyMatchMessage`) and structural
shape restrictions, neither of which bounds memory, so a big hash build could
hit a destructive `MEMORY_LIMIT_EXCEEDED` (code 241) abort.

**Version floor is 26.4**: `max_bytes_before_external_join` carries an
EXPERIMENTAL marker at introduction and is treated as production-grade from
26.5, where its ratio- default sibling
(`max_bytes_ratio_before_external_join=0.5`) ships — this registry entry pins
the floor to 26.4, where the setting first exists to stamp, and separately
marks `Stability` as `experimental` to keep that honestly reflected here.

**Explicit stamp, not the ratio default**: a ratio setting is silently ignored
when no server/user memory limit is configured — the same failure mode
[ClickHouse#76740](https://github.com/ClickHouse/ClickHouse/issues/76740)
documents for the analogous group_by ratio — so cerberus cannot rely on it
regardless of an operator's ClickHouse profile.

## Why `lazy_materialization` replaced the hand-rolled `late_mat.go` rewrite

This replaces cerberus's own hand-rolled late-materialisation rewrite (formerly
`late_mat.go`, deleted alongside this feature): that structural
`Project(Limit(Filter?(Scan)))` matcher never fired on any production query
path, because at the time all three production `Limit` constructions wrapped an
`OrderBy` directly under `Limit` (the matcher's switch only accepted
`Filter`/`Scan` there) and the Loki line path built no SQL `Limit` at all,
applying the request limit Go-side in `buildRangeData` — the gap since closed
by `maybePushLogLineLimit`, described in
[`clickhouse-optimizations.md`](clickhouse-optimizations.md).

Sizing the knob to the request's own LIMIT rather than to a fixed ceiling was
verified on a live chDB 26.5 probe that a max-limit knob BELOW the query's
actual LIMIT silently falls back to eager reads (no `LazilyReadFromMergeTree`
step in `EXPLAIN PLAN`), so a fixed constant would silently stop helping the
instant a caller's limit grew past it.

`AutoSelect` is `true`: the same chDB probe confirmed the stamp is
RESULT-EQUIVALENT (identical row count and column set with and without it —
only the read order changes) and that ClickHouse's own top-N PREWHERE promotion
(`__topKFilter` on the sort column) fires independently of this setting, so
there is no negative PREWHERE interaction. Forcing `enable_analyzer=0` on the
same probe made the `LazilyReadFromMergeTree` step disappear entirely, which is
why cerberus co-stamps `enable_analyzer=1` alongside the setting.

## Why window-slide anchor injection is gated on a Lookback/Step ratio of 10

The measured speedup **tracks that ratio directly and is not a flat
multiplier**: a 5-minute window at a 1-minute step (ratio 5, the modal Grafana
panel shape) measured only 1.12x — explicitly below the eligibility threshold,
so that shape stays on the fan-out — while a 5-minute window at a 30-second
step (ratio 10, the threshold) measured 1.70x, a 5-minute window at a 15-second
step (ratio 20) measured 2.65x, and a 30-minute window at a 15-second step
(ratio 120) measured 10-14x.

## Why `ts_grid_delta` / `ts_grid_irate` / `ts_grid_idelta` are auto-enabled

Each shipped after a chDB differential sweep against its aggregate
([#2745](https://github.com/tsouza/cerberus/issues/2745) for delta,
[#2746](https://github.com/tsouza/cerberus/issues/2746) for irate and idelta)
that isolated every rule the ClickHouse documentation leaves underspecified —
the counter-reset posture, the trailing-pair selection, the two-sample NULL
rule, the left-open window edge, the extrapolation clamp branch — and matched
reference Prometheus on each. The one gap those sweeps found is the family-wide
duplicate-timestamp NaN survivor (originally
[#2798](https://github.com/tsouza/cerberus/issues/2798)), which the already
auto-selected rate / increase / resets / deriv / predict_linear members carry
identically, so it is not a reason to treat any one member differently.
ClickHouse #115920 made that survivor order-independent from 26.8.1.2041 on
(the finite sample wins), which removes the nondeterminism but not the
disagreement with the fan-out, whose rule elects the NaN;
[#3648](https://github.com/tsouza/cerberus/issues/3648) aligns the fan-out's
rule. The two
family members that ARE opt-in carry divergences ordinary data reaches:
`ts_grid_changes` on any NaN-adjacent window with no duplicate at all, and
`ts_grid_group_array` because it would import the nondeterminism into paths
that do not have it today.

## Why `ts_grid_vector_agg` is opt-in

It is a new code path with no fielded validation yet — the same posture
`ts_grid_instant` took on its own new floor — whereas `ts_grid_recollapse`, the
other narrowing of the native grid, had its merge exactness checked across
time-disjoint, interleaved and reset-straddling regimes before shipping
auto-on. The five outer functions are the set proven element-wise over a
finished per-series grid; `stddev` / `stdvar` / `quantile` / `group` are not
element-wise and stay on the exploded Aggregate permanently.

## Why `laginframe_adjacency` is auto-enabled and `fixed_accumulator_extrapolated` is not

The annotation pass carries the same kernels as the fan-out (`curr < prev` for
resets; `curr != prev AND NOT both-NaN` for changes; the DELTA / CUMULATIVE
pair delta for irate) and was proven bit-identical by dual-emit parity, so
there was nothing to weigh. The fixed-accumulator shape was also proven
bit-identical, but [#2894](https://github.com/tsouza/cerberus/issues/2894)'s
real ClickHouse 26.6.4.55 A/B (query_range `rate()` / `increase()`, 2,200
series, 1h span, 60 anchors, 5m window) measured half the source rows and ~20%
fewer bytes at neutral wall-clock (within ~10%) against ~1.8-2x the array-fold's
peak memory — six concurrent per-group accumulators against one `groupArrayIf`.
A memory increase for no consistent wall-clock win is an OOM-risk uplift on
every default `rate()` request, so it stays opt-in.
[#3556](https://github.com/tsouza/cerberus/issues/3556) re-ran the A/B for the
multi-name, high-fan-out selector shape that times out on the 24.8-pinned
compatibility floor lane and reproduced the same trade (~1.6x memory), so no
narrower auto rule changes the verdict. That investigation also found and
fixed a real bug in the shape: the counter-delta term rendered unparenthesised
on a scan with no temporality column, silently dropping the extrapolation
factor; the parity corpus had only ever covered the temporality-bearing
fixture.

## Why `sorted_slab_over_time` is opt-in

[#2894](https://github.com/tsouza/cerberus/issues/2894)'s A/B found the shape's
own "O(samples per series), independent of anchor count" memory claim did not
hold as shipped: at 60-240 anchors it used 6-9x the array-fold's peak memory
for no wall-clock win, and at 500 series / 480 anchors / 5m it OOMed a 6 GiB
cap where the array-fold finished at 535 MiB.
[#3046](https://github.com/tsouza/cerberus/issues/3046) traced that to
ClickHouse's block execution retaining the per-anchor `arrayFilter` /
`arrayMap` intermediates across a whole block of series rows, and fixed it
with the mandatory `max_block_size=1` stamp; every re-measured case then beat
the array-fold, including the one that OOMed. The negative measurement was
taken under the unfixed shape, so it is stale rather than a verdict on the
fixed one — but promotion to auto needs a fresh A/B against the fixed shape,
including mixed-shape queries, since the block-size stamp applies to the whole
statement.

## Why the two `*_merge_summap` features are opt-in

`classic_bucket_merge_summap` first shipped with a correctness gap on
heterogeneous groups, closed by
[#2817](https://github.com/tsouza/cerberus/issues/2817) (each row now
cumulates over its own buckets before the key-wise `sumMap`). The per-row
`arraySort` / `arrayCumSum` that fix added erased the speedup the feature
existed for: [#2923](https://github.com/tsouza/cerberus/issues/2923)'s
real-ClickHouse re-measurement against the shipped construction found its
cost within ~1% of the fold's at every controlled point, converging to parity
as volume grows. The existing `maxClassicBucketMergeCostUnits` guard protects
this path within the same margin, so no second ceiling exists.

`exp_histogram_merge_summap`'s reconstruction step is width-squared in the
worst case, independent of row count.
[#2757](https://github.com/tsouza/cerberus/issues/2757)'s real ClickHouse 26.6
measurements found 13-43x less memory at the ~160-bucket OTel-SDK default
width once rows reach the hundreds-to-thousands range, roughly parity at a
single series, and MORE memory than the fold for a single series with a wide
individual layout (1,280 buckets and up). The budget guards are per shape
(single-group, multi-group instant, multi-group range), but the single-series
wide-layout regression is a property of the design, not a calibration gap.

## Why `arg_and_max_fusion` and `ts_throw_duplicate_series_if` are auto-enabled

Both are tie-invariant substitutions with no operator tradeoff: ClickHouse
keeps one hash-table entry per GROUP BY key regardless of aggregate count and
per-group memory is dominated by the Attributes `Map` key, so `argAndMax`
saves one aggregate state and one per-row comparison — roughly a third of the
two states' payload, not a halving; `timeSeriesThrowDuplicateSeriesIf` changes
only the message of a query that aborts either way. Both are `experimental` in
maturity purely because the underlying functions are new (25.11 and 26.2) with
no fielded history. `argAndMax`'s floor is 25.11 from upstream PR
[#89884](https://github.com/ClickHouse/ClickHouse/pull/89884) and the 25.11
release notes, not the "v1.1.0" badge on its docs page, which is a
docs-tooling artefact.

## Why `ts_tag_groups` is permanently opt-in

The mechanism is sound — order-independent group ids, lossless rehydration,
a verified 26.2 floor with no experimental setting — but it loses on every
site tried. [#2750](https://github.com/tsouza/cerberus/issues/2750)'s real
ClickHouse 26.2 A/B (2M rows, `FORMAT Null`) on the name-drop guard measured
the UInt64-group-id path 2-3x slower at 100 distinct label sets (666-1083ms vs
265-305ms) and ~50% slower at 200,000 (1358-1506ms vs 881-957ms), with peak
memory ~1.8x higher at low cardinality and only 10-20% lower at high.
[#2880](https://github.com/tsouza/cerberus/issues/2880) re-ran the same
methodology on the strongest multi-stage candidate — the one-to-one
vector-vector JOIN — and it lost by a wider margin (~2.2x slower, 3-4x more
memory at low cardinality; ~1.9x slower, 25-30% more memory at high): the
per-query tag collector's bookkeeping is paid per input ROW on each side
before any reduction, while the cheaper UInt64 equality applies only to the
already-reduced comparison, so more downstream reuse cannot amortise it. The
label ops the tag-group family also offers (`by` / `without` /
`label_replace` / `group_left`) are excluded for a second reason: a group id
is per-query state, every label op reaches range mode, and route B
concatenates per-shard cursors without re-aggregating, so an id cannot cross
a shard.

## Why `trace_id_external_table` is opt-in

The external-table form was EXPLAIN-verified
([#2783](https://github.com/tsouza/cerberus/issues/2783)) to prune
`idx_trace_id` identically to the literal splice at every closure size tested,
so it fixes only the SQL-text-size axis, and its production win at the
`MaxSearchLimit=1000` phase-A width is unmeasured beyond that issue's synthetic
corpus — the same posture `trace_id_projection` took on a fresh mechanism that
only a real server exercises.

## Audited, not adopted

Not every settings family the audit epic (#2778) reviews earns a registry
entry. Recording an audit that found nothing to stamp is itself the useful
artifact — the alternative is the same family getting silently re-reviewed by a
future pass with no memory of this one.

### S3/remote-filesystem read tuning (prefetch + concurrent-read thresholds)

Production is a single node with S3-backed storage, so the working hypothesis
was that cerberus's cold dashboard scans need explicit tuning of ClickHouse's
prefetch and remote-filesystem concurrent-read settings. Probed live via chDB
(ClickHouse 26.5.1.1) rather than assumed from the settings' documented
defaults — `system.settings` reports:

| Setting                                                             | Live value | Live default |
| ------------------------------------------------------------------- | ---------- | ------------ |
| `remote_filesystem_read_prefetch`                                   | `1`        | `1`          |
| `allow_prefetched_read_pool_for_remote_filesystem`                  | `1`        | `1`          |
| `merge_tree_min_rows_for_concurrent_read_for_remote_filesystem`     | `0`        | `0`          |
| `merge_tree_min_bytes_for_concurrent_read_for_remote_filesystem`    | `0`        | `0`          |

Prefetch is already on. The remote-filesystem concurrent-read thresholds are
already at `0` — the most aggressive setting available, meaning ClickHouse
already runs EVERY remote-filesystem part read concurrently regardless of size,
unlike the local-disk counterparts (`merge_tree_min_rows_for_concurrent_read` /
`..._bytes_...`, which default to 163840 rows / 240 MiB — a real threshold,
because a local read is cheap enough that a small part isn't worth
parallelizing). ClickHouse's own defaults, on the exact deployment shape this
issue targets, are already tuned past what a `chopt` stamp forcing
`remote_filesystem_read_prefetch=1` or lowering an already-zero threshold could
add — the entire premise "stamp only where the default is off or measurably
wrong" resolves to neither being true. Per the audit-epic mandate (#2778) to
verify a proposal against the live server rather than assume it from
documentation alone, no `chopt` feature is registered for either family: it
would be machinery duplicating what the server already does unconditionally.

**The follow-up question this audit surfaced — settled negative (cerberus
issue #2827).** The issue's own "fan-out-heavy wide scans vs point lookups"
framing implied the OPPOSITE tuning direction might pay off: deliberately RAISING the
remote-filesystem concurrent-read thresholds (toward the local-disk defaults,
163840 rows / 240 MiB) on a detected narrow, highly-selective point-lookup
shape, to avoid the coordination overhead of spinning up concurrent S3 fetches
for a read that only touches a handful of granules. Benchmarked directly
against a real ClickHouse 26.8 server backed by a live MinIO (S3-compatible)
disk — 20M rows, a single `id = <literal>` point-lookup shape,
`clickhouse-benchmark` over 2,000 distinct random-id queries per configuration
with the mark cache dropped between runs: the default (`0`/`0`, max
concurrency) and the raised local-disk-matching thresholds measured
statistically indistinguishable throughput (83.577 vs 83.655 QPS, a ~0.1% gap,
well inside run-to-run noise). Smaller repeated trials (500 queries x 3) leaned
the OPPOSITE direction from the hypothesis — raised thresholds ~2-3% SLOWER,
not faster. A single-key point lookup only ever touches on the order of one
granule regardless of the concurrent-read threshold, so there is no
coordination overhead this setting family removes for that shape on real
S3-backed storage: the threshold governs whether a read gets SPLIT into
concurrent sub-ranges, and a read this narrow has nothing left to split either
way. No `chopt` feature is adopted; #2827 is closed with this evidence rather
than left open.

**Local filesystem cache** (`enable_filesystem_cache` + the server-side cache
disk) IS adopted by this issue, but as documented operator guidance plus
`/info` reporting rather than a `chopt` stamp — it is a server-config /
disk-sizing concern, not a per-query setting. See
[`docs/operations.md`](operations.md)'s "Local filesystem cache" section.
