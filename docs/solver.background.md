# Sharded-pushdown solver — background

This document collects the design rationale, rejected alternatives, incident
history, and measurements behind [`solver.md`](solver.md). It answers "why is
it built this way" rather than "what does it do" — nothing here is required to
operate or extend the solver correctly; `solver.md` is self-sufficient for
that.

## Why the data-shard fanout gate lives in `internal/chclient`, not the Executor

The data-shard fanout gate (cerberus issues #3081, #3128, epic #3074) was
originally scoped to this package's own `Executor`. Issue #3128 found that an
Executor-only gate covered only route B (this solver's own K-shard splits),
leaving route A (the ordinary, non-split query path — the vast majority of
real traffic) completely unbounded, since route A dispatches straight through
`internal/chclient` with no Executor involvement at all and fans out
identically once `DataShardCount > 1`. That is why the gate was moved to the
one seam every dispatch actually shares (`chclient`'s `queryOpen` /
`queryCursorColumnar`), where both routes are now bounded by the identical
mechanism.

## Why `MinCorroboratingFailures` is 2, not 1

Requiring 2 consecutive failures (not one) exists so a single transient
rejection never mints a `PreferB` verdict on its own: probing route B is
itself a real dispatch, and the memo's premise only holds if repeated failure
is treated as real signal rather than noise from one bad request.

## Why the cluster-wide pressure damper exists

A correlated, cluster-wide event (e.g. every query slowing down at once)
produces resource failures on many UNRELATED keys simultaneously. Without the
damper, one shared external cause would look like independent evidence
against every one of those keys and stampede all of them into probing route B
at once — exactly the wrong response to a cause that has nothing to do with
any individual key's cost shape.

## Why `ObserveRouteAFailureAndMaybeBeginProbe` has to be one atomic step

The re-validation path depends on a route-A dispatch actually failing again
while the entry looks stale. Recording that failure refreshes `createdAt`,
which un-stales the entry — so an admission check that ran as a SEPARATE step
afterwards would see a fresh `PreferB` entry and refuse, losing exactly the
request that should have triggered the rescue. The caller's actual HTTP
request would then be stuck on the very failure the memo already knows how to
avoid, even though the memo has been confidently routing this shape to B for
the entry's entire life. `ObserveRouteAFailureAndMaybeBeginProbe` holds the
memo's mutex across both steps for exactly this reason: it captures whether
the entry WAS stale before the state transition, then decides admission on
that pre-transition snapshot rather than the post-transition one. That
snapshot is why the memo exposes no separate admission entry point for a
route-A resource failure — this method is the only way route-A failure
admission happens, so the record-then-admit sequence cannot be written any
other way.

## Relationship to the retired route-threshold autotune

Route memo is cerberus's current answer to "how does the solver's routing
adapt without an operator hand-tuning it" — but it is a different mechanism
from the self-driving threshold-fit loop it replaces, not a variant of it.
The retired autotune loop adjusted the same static cost-threshold proxy
(`MinFanout` / `MinAnchorPairs`) `Planner` already used, fitting its value
against a corpus of past decisions — it could tune the number the proxy
compares against, but not fix a proxy measuring the wrong cost dimension for
a given shape. Route memo does not touch those thresholds at all: it is a
per-key, per-outcome evidence ledger sitting entirely downstream of the
Planner's classification, keyed on a request's own cost shape rather than fit
against a corpus globally. A plan the Planner misclassifies as route-A-cheap
can still end up on route B for that specific shape once it has actually
failed enough times to prove the classification wrong — no threshold anywhere
has to change for that to happen.

## Why an anchor-only threshold can't gate per-rung admission

Issue #2709 found a real case where anchor-count-alone bites: a 24h/1m
dashboard panel over a low-cardinality metric clears the anchor floor by
10x+ and predictively shards a nearly-empty table, paying `K` concurrent
ClickHouse queries' contention for no benefit. #2709 also showed geometry
alone cannot fix this: a genuine incident the bypass exists to catch (#2677)
has FEWER anchors than that false positive, so no anchor-only threshold can
separate the two populations — which is why `PerRungAdmissionLearner` closes
the gap with evidence (observed drain size) instead of a geometry threshold.

## How issue #2840 arrived at the cardinality-probe carrier set

Issue #2840 set out to add four carrier kinds, each expected to need its own
"real-matrix time-bound-pushdown formula," and instead found `chsql` had
already collapsed all of them onto the one shared helper
(`maybePushRangeScanTimeBound`) the `RangeWindow` carrier already used — see
`cardinality_probe_wiring.go`'s own top-level doc for the finding.
`RangeWindowStaleResample` shares that identical formula too (same helper,
same field shape as `RangeLWR`) but was excluded from #2840's original count
purely for not being one of that PR's four additions — a circular basis
corrected once noticed, in a later ACPR pass. The issue's own
bracketed-optional third stat (`avg(length(ExplicitBounds))`, a
classic-histogram bucket-width bias signal) stayed out for the same reason
it started out: no consumer reads it, and #2840's own text names it the
lowest-priority of its three items.

## Why the failure-driven route memo is NOT seeded from the advisory signals

The EXPLAIN ESTIMATE issue's proposal named both the route memo and per-rung
admission as seeding targets. Only per-rung admission is seeded. The route
memo's `MinCorroboratingFailures = 2` and its cluster-wide pressure damper
exist specifically to reject exactly the kind of single-shot, non-corroborated
evidence a granule-resolution upper bound is: seeding `PreferB` from an
estimate would let one advisory signal — not selectivity-aware, never
re-confirmed by a real dispatch — route real production traffic onto route B
without the two independent real resource failures the whole mechanism is
built to require before trusting anything. Per-rung admission's own contract
is the opposite shape and is safe to seed: it can only ever DOWNGRADE an
already-`PerRungPredictive` route back to route A, so a wrong prior costs one
shape one unnecessary shard for `perRungEvidenceTTL` — the same always-safe
direction a wrong REAL observation already costs there. This is a deliberate
scope narrowing from the issue's own proposal, not an oversight.

## Incidents behind the EXPLAIN ESTIMATE near-empty skip and K-ceiling raise

The near-empty skip is issue #2709's own case: a wide-window panel over a
table with almost nothing in it clears every geometric threshold and pays
`K` concurrent round trips for negligible work, and only DATA (never
geometry) can see that in advance.

The K-ceiling raise is issue #2685's own case reopened safely: a
production-cardinality panel whose own grid asks for more shards than the
structural backstop allows, now granted the extra sharding once real data
volume — not geometry alone — supports it.

## EXPLAIN ESTIMATE calibration measurement

Measured against `test/perf/smoke/testdata/samples/svc_http_requests_total.parquet`
(real, scrubbed production Sum-metric sample, ~18.6M rows over a real 14-day
span — see that directory's own `README.md`), loaded into a real MergeTree
table (`ORDER BY (MetricName, TimeUnix)`, matching the production sorting
key) and probed with `EXPLAIN ESTIMATE` directly:

| window                                                | rows    | marks | parts |
| ----------------------------------------------------- | ------- | ----- | ----- |
| 1h over the sample's single densest real hour         | 598,016 | 73    | 1     |
| 6h spanning that hour (real data, mixed density)      | 895,858 | 110   | 1     |
| 6h a year outside the sample's captured span (empty)  | 0       | 0     | 0     |

This confirms both advisory checks against real data rather than assumption:
a genuinely empty window reports EXACTLY zero — `EstimateNearEmptyRowFloor`'s
default of 1,000 sits comfortably above ClickHouse's own noise floor for a
truly empty scan and comfortably below any window carrying real samples — and
a real, single-metric dense window already reports rows in the hundreds of
thousands, an order of magnitude with room to justify multiple shards above
`MaxK` at `EstimateMinRowsPerAdditionalShard`'s default of 50,000 (895,858
rows / 50,000 = 17 additional shards' worth of headroom, the same order of
magnitude as the #2685 incident's own K=22 grid-derived ask). `defaultMaxKWithEstimate`
(32) reinstates the exact ceiling #2685 raised and #2709 reverted, now gated
on this real-data justification instead of geometry alone.

This measurement validates the MECHANISM (a genuinely empty window and a
genuinely dense window are cleanly distinguishable by `EXPLAIN ESTIMATE`, at
real production row counts) rather than reproducing the exact #2685/#2709
production incidents byte-for-byte, which this repository does not have
access to. The router corpus (`internal/optcorpus`, "Routing-decision
calibration corpus" in `solver.md`) is the intended mechanism for refining
these constants further against real production traffic once the feature is
opted into a live deployment — exactly the same measurement-only feedback
loop that governs `MinFanout` / `MinAnchorPairs` today.

## Cardinality pre-probe measurement

Measured with `buildCardinalityProbePlan`'s own real chplan tree, executed
via chDB against `test/perf/smoke/testdata/samples/svc_http_requests_total.parquet`
(the SAME corpus `EXPLAIN ESTIMATE` above was calibrated against) and
`kube_pod_status_reason.parquet` (the set's highest-cardinality sample, up
to ~4,800 series in a single window per its own `README.md`):

| window                                                           | rows    | distinct_series  |
| ---------------------------------------------------------------- | ------- | ---------------- |
| 1h over `svc_http_requests_total`'s densest real hour            | 596,424 | 101 (saturated)  |
| 6h spanning that hour                                            | 895,858 | 101 (saturated)  |
| 6h a year outside the sample's captured span (empty)             | 0       | 0                |
| 1h over `kube_pod_status_reason`'s densest real hour             | 567,360 | 101 (saturated)  |

The 6h row count (895,858) lands EXACTLY on `EXPLAIN ESTIMATE`'s own
measurement for the identical window (the calibration table above) — the two
probes independently scanning the same real data agree, cross-validating that
this probe's `(Start - Offset - Span, End - Offset]` bound is the same window
the granule-upper-bound probe already reasons about. Every dense real window
this sample carries saturates `uniqUpTo(100)` at 101 — this sample's own real
per-panel cardinality already exceeds the cap throughout its captured span,
confirming the verified constraint (a K above 100 throws rather than silently
under-counting) matters in practice, not only in theory.

The probe's landing reasoned that neither of its two consumers (K-clamp
`Rows`, per-rung `cheap` seeding) needed an exact count above the 100-series
threshold `uniqUpTo` already answers, and left `uniqCombined`/`uniqCombined64`
(its own named alternative) for a follow-up. Issue #2840 re-examined that
assumption against `maybeSeedPerRungPrior`'s own threshold —
`NAnchors * perRungCheapRowsPerAnchor`, routinely in the thousands — and
found it did NOT hold: a saturated `uniqUpTo` reading is a CONSTANT 101
regardless of how far past the cap the true count lies, so on every dense
real window this sample carries, the per-rung seeding comparison was reading
"101 < a threshold in the thousands" and seeding `cheap=true` no matter how
large the true series count actually was — a near-constant, falsely-cheap
signal on exactly the traffic that threshold exists to gate. `uniqCombined64(...)`
closes that gap by falling back to it whenever `uniqUpTo` reports the
saturation value, so the threshold check compares against the approximate
UNCAPPED reading instead of the pinned 101 on exactly the windows this table
shows saturate. K clamping (`Rows`) is unaffected — it was always fed
`count()`, never `DistinctSeries`.

## Query-actuals verification against a live server

`system.query_log`'s column shape was checked against a live ClickHouse 26.6
server before relying on it (the same discipline issue #2789's own risk note
calls out — issue #2770's Loki catalog PR caught a real bug from an
unverified assumption about `system.view_refreshes`'s columns): `log_comment`
is `String`, `read_rows`/`read_bytes`/`memory_usage` are all `UInt64`,
`event_time` is `DateTime`, `type` is the expected
`Enum8('QueryStart'=1,'QueryFinish'=2,...)` — exactly as expected.

A realistic drift scenario was also reproduced live, rather than constructed
synthetically: `EXPLAIN ESTIMATE` was run against a MergeTree table holding
1,000 rows (`Rows: 1000`, the "cached admission-time estimate"
`ScanEstimateAdvisor`'s own 30-minute TTL would hold); 1,000,000 more matching
rows were then inserted (a realistic traffic burst landing between the cached
estimate and the real dispatch); the identical query was dispatched for real
and `system.query_log` reported `read_rows = 1,001,000` for it — a **1001x**
predicted-vs-actual ratio, far outside the default `[0.1, 3.0]` band. Replayed
through the real `internal/actuals.Tracker` (two corroborating observations,
reaching `MinObservations`): `DriftReport.Alerting = true`, and
`CalibrationFactor` correctly clamped to its `2.0` ceiling rather than
propagating the raw 1001x multiplier — proving the bounded-influence property
holds even on a genuine, large real-world divergence, not only on a small
synthetic one.

A second, cleaner comparison — `EXPLAIN ESTIMATE` vs. real `read_rows` for the
SAME query against a STABLE (non-growing) table, no `PREWHERE`, no skip index
— landed within noise of each other (8,192 predicted vs. 8,192 actual on a
`minmax`-prunable `PREWHERE` range; 5,000,000 vs. 5,000,000 on a full-table
`PREWHERE` scan), confirming the mechanism does NOT false-alarm on the common
case where nothing has actually drifted: the two independent probes
(`EXPLAIN ESTIMATE`'s no-execution index analysis and a real dispatch's own
storage-layer read) agree closely when the underlying data is stable between
them. The dominant real driver of drift this sandbox reproduces is TEMPORAL —
a cached advisory estimate going stale relative to data that grew after it
was taken — rather than a structural `PREWHERE`/skip-index mismatch in
row-count terms specifically; both are covered by the SAME mechanism
regardless of which one produced the divergence.
