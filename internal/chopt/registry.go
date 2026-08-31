package chopt

// The structural feature table in docs/clickhouse-optimizations.md (id /
// minVersion / stability) is generated from this registry. Regenerate it with
// `just gen-opt-docs`; CI fails any PR whose generated block drifts. See
// the optdocs subcommand under cmd/cerberus.
//go:generate go run github.com/tsouza/cerberus/cmd/cerberus optdocs -doc ../../docs/clickhouse-optimizations.md

// Feature id constants. Exported so internal/config and internal/engine
// reference the registry by symbol rather than a stringly-typed literal that
// could drift between the registry, the resolver, and the consumers.
const (
	// FeatureAggregationInOrder stamps optimize_aggregation_in_order=1 on a
	// query whose Aggregate GROUP BY is a bare-column prefix of the scanned
	// table's sorting key. Result-equivalent, 24.8-safe (the migration of the
	// dark optimize_aggregation_in_order rule into the registry).
	FeatureAggregationInOrder = "aggregation_in_order"

	// FeatureConditionCache stamps use_query_condition_cache=1 on a
	// predicate-stable read path when the server is >= 25.3. The query
	// condition cache is result-equivalent (a cache), so it ships under auto
	// for supporting servers; below 25.3 it is absent from the set (no-op).
	FeatureConditionCache = "condition_cache"

	// FeatureTSGridRange opts eligible rate(<counter>[<range>]) query_range
	// shapes onto the native timeSeriesRateToGrid aggregate. Its maturity is
	// Experimental, but it is AUTO-SELECTED on capable servers: the native path
	// is result-correct and runs at flat memory, so auto picks it whenever the
	// server can run it. It is also reachable by the legacy
	// CERBERUS_EXPERIMENTAL_TS_GRID_RANGE alias.
	//
	// IMPORTANT — the floor is 25.9, NOT the 25.6 the aggregate first shipped in.
	// timeSeriesRateToGrid was introduced in 25.6, but it used a CLOSED
	// [anchor-window, anchor] membership window until ClickHouse 25.9 (PR #86588,
	// merged 2025-09-08, "Make the staleness window in timeSeries*() functions
	// left-open and right-closed"). PromQL's range selector is half-open
	// (anchor-window, anchor], matching the fan-out. On a grid-aligned corpus
	// (scrape interval == range) the closed left edge is NOT measure-zero: it
	// admits the sample sitting exactly on anchor-window, so the native path
	// emits a rate at every grid point where reference Prometheus (and the
	// fan-out) emit nothing (< 2 in-window samples). Empirically confirmed wrong
	// on the 25.8 chDB substrate. A 25.6 floor would auto-enable a path that
	// systematically diverges from Prometheus on 25.6-25.8; 25.9 is the first
	// release whose window is Prometheus-equivalent.
	FeatureTSGridRange = "ts_grid_range"

	// FeatureTSGridResample opts the eligible range-mode instant-vector
	// selection / staleness shape (the query_range bare-selector LWR) onto the
	// native timeSeriesResampleToGridWithStaleness aggregate, retiring the
	// argMax sample-fan-out (internal/chsql.emitRangeLWR). Like ts_grid_range its
	// maturity is Experimental but it is AUTO-SELECTED on capable servers (no
	// legacy env alias — auto picks it, or list it in CERBERUS_CH_OPTIMIZATIONS).
	// It shares the timeSeries*ToGrid family's 25.9 floor (the left-open window
	// fix, PR #86588 — see FeatureTSGridRange) and the same experimental
	// allow_experimental_time_series_aggregate_functions gate. Below 25.9 the
	// closed-left staleness window admits a sample landing exactly on
	// anchor-lookback, diverging from the fan-out's half-open membership.
	FeatureTSGridResample = "ts_grid_resample"

	// FeatureColumnarResultDecode routes the four-column query_range matrix
	// projection through a dedicated ch-go (low-level) columnar decode path
	// instead of the per-row clickhouse-go/v2 Scan path, building each series'
	// label map once per contiguous run rather than once per row. It is a
	// CLIENT-SIDE decode optimization: it touches no server setting and works
	// on any native-protocol server, so it carries NO version floor
	// (AlwaysAvailable). It is opt-in-only (AutoSelect=false): a perf TRADEOFF (a
	// second ch-go dial, not a version-gated win), so auto MUST NEVER select it;
	// it engages only when listed explicitly in CERBERUS_CH_OPTIMIZATIONS —
	// typically alongside auto (`auto,columnar_result_decode`) to keep the
	// version-gated picks. This is the one feature auto leaves to the operator.
	FeatureColumnarResultDecode = "columnar_result_decode"

	// FeatureTSGridChanges opts eligible changes(<v>[<range>]) query_range
	// shapes onto the native timeSeriesChangesToGrid aggregate (the per-window
	// value-change count), retiring the arrayPopBack/arrayPopFront `c != p`
	// fan-out (internal/chsql.emitRangeWindowChanges).
	//
	// IMPORTANT — the floor is 25.9, shared with the rest of the family.
	// timeSeriesChangesToGrid/ResetsToGrid shipped a full quarter after the
	// 25.6 rate/resample aggregates (PR #86010, merged 2025-09-08, ClickHouse
	// 25.9), empirically confirmed ABSENT on the 25.8 chDB substrate. A 25.6
	// floor here would mis-advertise support on 25.6-25.8 servers and 502 with
	// UNKNOWN_AGGREGATE_FUNCTION. (rate/resample share the 25.9 floor for a
	// different reason — the left-open window fix, PR #86588; both PRs landed in
	// 25.9.) The experimental allow_experimental_time_series_aggregate_functions
	// gate is shared with the rest of the family.
	//
	// IMPORTANT — unlike the rest of the timeSeries*ToGrid family, this one is
	// NOT auto-selected. The native builtin overcounts by exactly 1 whenever a
	// window's chronologically-earliest in-window sample is NaN (#1721), and
	// separately implements no NaN-both-sides carve-out at all, so it diverges
	// from reference Prometheus's funcChanges on any NaN-adjacent window —
	// exactly the divergence #1489 fixed in the fan-out kernel
	// (internal/chsql.emitRangeWindowChanges) but cannot patch inside a
	// ClickHouse builtin. Confirmed against a real reference Prometheus on the
	// compatibility/prometheus substrate (ClickHouse 26.5, well above the 25.9
	// floor): auto-selecting this feature emits a wrong answer the instant a
	// NaN-bearing series is queried. AutoSelect is false — like
	// FeatureColumnarResultDecode, it's reachable only via an explicit
	// CERBERUS_CH_OPTIMIZATIONS=ts_grid_changes listing — until ClickHouse
	// fixes the builtin's NaN handling upstream and #1721 closes.
	FeatureTSGridChanges = "ts_grid_changes"

	// FeatureTSGridResets opts eligible resets(<counter>[<range>]) query_range
	// shapes onto the native timeSeriesResetsToGrid aggregate (the per-window
	// counter-reset count), retiring the arrayPopBack/arrayPopFront `c < p`
	// fan-out (internal/chsql.emitRangeWindowResets). Experimental maturity but
	// AUTO-SELECTED on capable servers, same 25.9 floor and same experimental
	// gate as FeatureTSGridChanges (the two are siblings from PR #86010).
	FeatureTSGridResets = "ts_grid_resets"

	// FeatureTSGridDeriv opts eligible deriv(<gauge>[<range>]) query_range
	// shapes onto the native timeSeriesDerivToGrid aggregate (the per-window
	// simple-linear-regression slope), retiring the
	// simpleLinearRegression/arrayReduce fan-out
	// (internal/chsql.emitRangeWindowDeriv). Experimental maturity but
	// AUTO-SELECTED on capable servers, same experimental gate and the same
	// 25.9 floor as the rest of the family.
	//
	// IMPORTANT — the floor is 25.9, shared with the family. The deriv/
	// predict_linear ToGrid aggregates first shipped in ClickHouse 25.8
	// (PR #84328); the registry pins them to the family's 25.9 floor for
	// consistency (the shared left-open-window fix, PR #86588), so a single
	// probed capability verdict governs every timeSeries*ToGrid member.
	FeatureTSGridDeriv = "ts_grid_deriv"

	// FeatureTSGridPredictLinear opts eligible predict_linear(<gauge>[<range>], t)
	// query_range shapes onto the native timeSeriesPredictLinearToGrid aggregate
	// (the per-window slope*t + intercept forecast), retiring the
	// simpleLinearRegression/arrayReduce fan-out
	// (internal/chsql.emitRangeWindowPredictLinear). The native path only fires
	// for a single whole-second literal horizon t (the aggregate's 5th
	// parametric arg); computed/fractional horizons stay on the fan-out.
	// Experimental maturity, AUTO-SELECTED on capable servers, same 25.9 floor
	// and same experimental gate as FeatureTSGridDeriv (the two are siblings
	// from PR #84328).
	FeatureTSGridPredictLinear = "ts_grid_predict_linear"

	// FeatureTSGridRecollapse defers the OTel -> Prometheus label-shaping tower
	// (the mapSort/mapConcat/mapUpdate reshape of the Attributes map) PAST an
	// eligible native rate grid aggregate, so it evaluates once per raw series
	// instead of once per raw sample row. The aggregate runs on the RAW keys
	// under the -State combinator and a second grouping level re-collapses the
	// partial states onto the shaped keys with -Merge (label shaping is
	// non-injective, so several raw series can land on one output series and
	// their samples must be POOLED, not combined arithmetically).
	//
	// It is a pure narrowing of FeatureTSGridRange: with ts_grid_range off
	// there is no native node to defer anything past, so cmd/cerberus only
	// consults this feature inside the ts_grid_range branch. The registry
	// cannot express that dependency directly, so the two carry the same 25.9
	// floor and the same experimental gate and are therefore resolved
	// identically by a single probed capability verdict.
	//
	// The 25.9 floor is therefore INHERITED, not independently derived: it is
	// whatever ts_grid_range pins, and nothing about the re-collapse itself
	// raises it. Merge exactness is not the binding constraint — merged partial
	// states are bit-identical to a single pooled pass at every server version
	// the aggregate is reachable on, across time-disjoint, interleaved, and
	// counter-reset-straddling series regimes (see docs/clickhouse-optimizations.md
	// for the probe shape and the versions it was executed against).
	FeatureTSGridRecollapse = "ts_grid_recollapse"

	// FeatureTSGridIncrease opts eligible increase(<counter>[<range>])
	// query_range shapes onto the native timeSeriesRateToGrid aggregate,
	// retiring the arrayJoin sample-per-anchor fan-out
	// (internal/chsql.emitWindowedArrayExtrapolatedMatrix) that increase()
	// otherwise shares with rate(). There is no dedicated
	// timeSeriesIncreaseToGrid aggregate upstream, so this reuses
	// timeSeriesRateToGrid and multiplies its per-grid-point result back by
	// the window's range in seconds: Prometheus's `increase()` IS
	// `extrapolatedRate()` with the final `/range` divide left out, so
	// `rate * range` recovers the undivided extrapolated increase in real
	// arithmetic. The same round-trip already backs
	// FeatureTSGridHistogram's classic-histogram ladder (see
	// internal/chsql/range_bucket_grid_native.go's own "multiplied back by
	// the window seconds" note).
	//
	// The floor is 25.9, shared with the rest of the family for the same
	// reason FeatureTSGridRange is: increase() rides the same
	// timeSeriesRateToGrid aggregate, whose membership window was CLOSED
	// (wrong) until the left-open / right-closed fix (PR #86588, ClickHouse
	// 25.9).
	//
	// AutoSelect is true, unlike FeatureTSGridChanges: the division-then-
	// multiply round trip introduces only a documented, measured 1-ULP
	// float64 rounding divergence from the fan-out's direct
	// multiply-then-sum (proven by a dual-emit chDB parity test against the
	// SAME fixture the fan-out's own Prometheus-pinned golden covers — see
	// internal/chsql/range_window_grid_native_increase_chdb_test.go), never a
	// wrong ANSWER the way ts_grid_changes' NaN-adjacent-window overcount is.
	FeatureTSGridIncrease = "ts_grid_increase"

	// FeatureTSGridHistogram opts the per-series `rate` window stage under the
	// range-mode classic-histogram quantile idiom
	// `histogram_quantile(phi, <agg> by(le) (rate(<bucket>[range])))` onto a
	// native timeSeriesRateToGrid ladder, retiring the array-expression fold
	// (internal/promql.classicBucketWindowLadderExpr).
	//
	// The fold it replaces is not merely slow, it is QUADRATIC in a way no
	// rewrite of the expression can fix: it walks the union bucket bounds with
	// `arrayMap(u -> …)` over a body that READS the group's bounds / counts
	// groupArrays, and ClickHouse materialises a lambda's captured columns once
	// per outer-array element (upstream issue #54967), so the fold builds one
	// copy of the whole per-series bucket matrix per rung. The
	// timeSeries*ToGrid family are AGGREGATE functions — they consume rows
	// through addBatch and never construct that replica — so expressing the
	// same arithmetic as an aggregate over the UNNESTED ladder (one scalar
	// counter series per `le` rung, which is exactly what reference Prometheus
	// models `<name>_bucket{le="X"}` as) removes the replication entirely.
	// Measured against a real ClickHouse 26.6 at realistic scale, same rows
	// read (~88-89k), 121 anchors, 5m window: 4,123 ms / 3.411 GB peak /
	// 51.5 CPU-s for the array fold against 148 ms / 0.130 GB peak / 0.4 CPU-s
	// for the native aggregate — 28x faster, 26x less memory, 129x less CPU.
	//
	// The 25.9 floor is INHERITED from the rest of the timeSeries*ToGrid
	// family, not independently derived: this shape rides the SAME
	// timeSeriesRateToGrid FeatureTSGridRange pins, so it inherits that
	// feature's binding constraint — the left-open / right-closed membership
	// window (upstream PR #86588, first released in 25.9). Below 25.9 the
	// window's closed left edge admits the sample sitting exactly on
	// anchor-window, so every rung would emit where reference Prometheus emits
	// nothing. It additionally reads timeSeriesResetsToGrid purely as a
	// per-grid-point PRESENCE signal (see internal/chsql's bucketGridSeenFn),
	// which is a FeatureTSGridResets sibling from PR #86010 — released in the
	// same 25.9, so the floor is unchanged either way. Both share the one
	// allow_experimental_time_series_aggregate_functions gate cerberus already
	// stamps for this family, so no new experimental setting is introduced.
	FeatureTSGridHistogram = "ts_grid_histogram"

	// FeatureQuantilePromHistogram opts the classic (non-native-histogram)
	// `histogram_quantile(phi, <classic-selector>)` rank walk onto
	// ClickHouse's own quantilePrometheusHistogram(phi)(le, cum) aggregate,
	// retiring the hand-rolled arrayCumSum / arrayFirstIndex / linear-
	// interpolation chain (internal/chsql/histogram_quantile.go) for every
	// shape that node handles — see chplan.HistogramQuantile.
	// UseNativeQuantileAggregate and its boot-wired seam,
	// promql.QuantileRankWalkLowerer.
	//
	// quantilePrometheusHistogram shipped non-experimental, no setting gate,
	// in ClickHouse 25.10 — confirmed present in system.functions on a real
	// 25.10.7.6 server (probed directly; it is NOT documented as of this
	// writing). Its sibling multi-phi form, quantilesPrometheusHistogram,
	// was probed present too, but nothing in this codebase's IR carries more
	// than one phi per classic-histogram-quantile node, so it has no
	// consumer and this feature does not reach for it.
	//
	// The aggregate reproduces reference Prometheus's bucketQuantile
	// natively — including the first-bucket non-positive-upper-bound
	// short-circuit and the phi==1 highest-explicit-bound answer — so the
	// native emission needs NONE of the existing emitter's edge-case
	// branches (hasOverflowRung / firstBucketNonPositive / the interpolation
	// formula). A real-CH differential sweep (25.10.7.6) against the
	// existing rank walk, across a normal crossing, a duplicate-bound
	// layout, the equal-length (no-overflow-rung) shape, an empty
	// histogram, and a negative-bound first bucket, matched exactly (0 ULP
	// difference) at every phi tested, including phi in {0, 1} and the
	// out-of-range / NaN phi guards — see the quantile_prom_histogram
	// real-CH integration test.
	//
	// Two input-contract quirks the emission MUST honor, both confirmed
	// against the real server rather than assumed:
	//
	//   - The aggregate returns nan whenever no row carries le = +Inf, even
	//     when every finite bucket is populated and phi is well within
	//     range — there is no total-from-last-row inference. The emitter
	//     therefore ALWAYS appends a terminal (+Inf, total) pair: the
	//     genuine overflow rung when the row carries one, or a synthetic
	//     tie-cum entry (le=+Inf, cum=<last coalesced cum>) when it does
	//     not — verified to reproduce the existing emitter's equal-length
	//     (no-overflow) shape exactly, including at phi == 1.
	//   - Feeding raw (possibly duplicate-le) rows answers WRONG (confirmed:
	//     a coalesced-vs-raw differential on a [1,1,5]-bound layout diverged
	//     by more than 3x). The emitter therefore keeps the existing Stage-1
	//     duplicate-bound coalescing (keptBoundIdx) ahead of the aggregate —
	//     the one piece of the legacy walk this path does not delete.
	//   - The parametric phi argument must be a compile-time-constant
	//     expression in [0, 1]: passing it a value outside that range, or
	//     NaN, throws PARAMETER_OUT_OF_BOUND and fails the WHOLE query — an
	//     aggregate's parametric argument is evaluated regardless of which
	//     branch of an enclosing scalar if() would select its result. The
	//     emitter clamps the argument itself
	//     (greatest(0, least(1, if(isNaN(phi), 0, phi)))) so the aggregate
	//     is always called with a safe value, and wraps the call in the
	//     SAME phi < 0 / phi > 1 / isNaN(phi) outer branch the existing
	//     emitter already uses to answer -inf / inf / nan — reference
	//     Prometheus's contract, not the aggregate's.
	//
	// AutoSelect is false. Correctness parity is proven by the sweep above,
	// but a real-scale measurement (25.10.7.6, a real OTel classic-histogram
	// export aggregated to one row per series, 12-bucket layout) found a
	// genuine PERFORMANCE TRADEOFF, not just an unproven new floor: the
	// emission's ARRAY JOIN multiplies row count by the bucket-ladder length
	// before GROUP BY collapses it back down, which the legacy walk never
	// does (every quantity it computes stays inside array-valued expressions
	// on the original one-row-per-series input). At real-world dashboard
	// scale (3,677 series) the native path was ~2x FASTER at EQUAL memory;
	// at high series cardinality (73,540 series, ~880k post-unnest rows, a
	// 20x synthetic fan-out of the same real seed) wall time stayed roughly
	// even but memory grew ~3.3x (219 MiB vs 66 MiB, reproduced across
	// repeated runs via system.query_log.memory_usage). See
	// https://github.com/tsouza/cerberus/issues/2790 for the full numbers
	// and the mitigation options that issue leaves for future investigation.
	// Combined with the 25.10 floor being very new (no fielded deployment
	// history yet) and this repo's own testcontainers substrate for real-CH
	// tests being pinned to 25.9-alpine elsewhere in the suite — below this
	// feature's own floor — the feature is reachable only by explicit
	// CERBERUS_CH_OPTIMIZATIONS=quantile_prom_histogram listing, mirroring
	// FeatureColumnarResultDecode's same conservative posture for a
	// different reason (there a client-side tradeoff, here a genuine
	// memory-vs-cardinality one).
	FeatureQuantilePromHistogram = "quantile_prom_histogram"

	// FeatureTSGridDelta opts eligible delta(<gauge>[<range>]) query_range
	// shapes onto the native timeSeriesDeltaToGrid aggregate, retiring the
	// arrayPopBack/arrayPopFront extrapolated-difference fan-out
	// (internal/chsql.emitRangeWindowDelta, extrapolationKindDelta). The
	// floor is 25.9, shared with the rest of the family: timeSeriesDeltaToGrid
	// shipped in the same 25.6 release as timeSeriesRateToGrid and inherits
	// the identical left-open/right-closed membership-window fix (PR #86588,
	// ClickHouse 25.9) FeatureTSGridRange's own doc explains.
	//
	// cerberus issue #2745 ran the differential sweep #2744's own delta()
	// TODO deferred (internal/promql/lower.go and internal/config/config.go
	// both used to record it verbatim): a battery of chDB probes against
	// timeSeriesDeltaToGrid directly, isolating each rule the doc
	// (https://clickhouse.com/docs/sql-reference/aggregate-functions/reference/timeSeriesDeltaToGrid)
	// leaves underspecified.
	//
	//   - DECISIVE counter-reset probe: two samples strictly inside the
	//     window whose value DECREASES (100 -> 10). A counter-repairing
	//     implementation (rate/increase's own extrapolatedRate(isCounter=true))
	//     would add the pre-drop value back, yielding +30 after
	//     extrapolation; the raw, non-repairing PromQL delta() answer is
	//     -270. ClickHouse returned exactly -270 — proof the aggregate does
	//     NOT counter-correct, matching PromQL's
	//     extrapolatedRate(isCounter=false, isRate=false) exactly, the
	//     central risk the issue named.
	//   - Left-open window membership, the >= 2 samples NULL rule, and the
	//     full Prometheus extrapolation formula INCLUDING the
	//     averageDurationBetweenSamples/2 clamp branch (not just the common
	//     "extrapolate fully" branch) were probed with hand-computed
	//     reference values and matched exactly, to the same floating-point
	//     bit pattern the closed-form arithmetic predicts.
	//   - The doc's duplicate-timestamp "highest value wins, NaN loses
	//     unless all NaN" rule matches for two real values. For a real-vs-NaN
	//     duplicate pair it is ORDER-DEPENDENT: NaN loses when it is
	//     inserted before the real sample, but WINS (propagates) when
	//     inserted after — traced to ClickHouse's own greatest() being
	//     asymmetric on NaN (greatest(nan, x) = x but greatest(x, nan) =
	//     nan) and the aggregate's internal dedup folding pairwise in
	//     encounter order. This is a genuine, reproducible divergence from
	//     the documented contract, filed as
	//     https://github.com/tsouza/cerberus/issues/2798 — but it is
	//     FAMILY-WIDE, not delta-specific: the identical probe against the
	//     already-shipped, auto-selected timeSeriesRateToGrid reproduces the
	//     same order-dependence. It therefore does not single out delta()
	//     for a different AutoSelect posture than its already-auto-selected
	//     siblings; the narrow, pre-existing gap (a real sample and a NaN
	//     sample sharing one series' exact timestamp) is tracked, not
	//     hidden.
	//
	// AutoSelect is true: the sweep found no delta-specific divergence from
	// PromQL — the one real gap it surfaced is a pre-existing, family-wide
	// ClickHouse tie-break bug already accepted for the auto-selected
	// rate/increase/resets/deriv/predict_linear siblings, not a reason to
	// treat delta differently from them.
	FeatureTSGridDelta = "ts_grid_delta"

	// FeatureTSGridIrate opts eligible irate(<counter>[<range>]) query_range
	// shapes onto the native timeSeriesInstantRateToGrid aggregate, retiring
	// the window_pairs[length]/[length-1] trailing-pair fan-out
	// (internal/chsql.emitRangeWindowIRate) for the shapes that reach it —
	// changes/resets/irate/idelta ALSO fall back to the lagInFrame annotation
	// (FeatureLagInFrameAdjacency) before the array-fold fan-out, so this
	// feature narrows an already-improved fallback rather than the raw
	// array-fold directly. Same 25.9 floor as the rest of the family:
	// timeSeriesInstantRateToGrid shipped in the same 25.6 release as
	// timeSeriesRateToGrid and inherits the identical left-open/right-closed
	// membership-window fix (PR #86588, ClickHouse 25.9).
	//
	// cerberus issue #2746 ran the differential sweep the issue named as its
	// precondition: a battery of chDB probes against
	// timeSeriesInstantRateToGrid directly on a real 26.5.1.1 substrate
	// (well above the 25.9 floor), isolating each rule the doc
	// (https://clickhouse.com/docs/sql-reference/aggregate-functions/reference/timeSeriesInstantRateToGrid)
	// leaves underspecified.
	//
	//   - DECISIVE counter-reset probe: a strictly-decreasing trailing pair
	//     (100 -> 10, 60 seconds apart). Prometheus's funcIrate DOES
	//     counter-repair the trailing pair (unlike delta/idelta) — the
	//     REPAIRED answer is the raw last value over the interval,
	//     10 / 60 = 0.1(6); the un-repaired raw answer would be
	//     (10-100)/60 = -1.5. ClickHouse returned exactly 0.16666666666666666
	//     — proof the aggregate DOES counter-correct, matching PromQL's
	//     funcIrate (CounterOrDeltaPairDelta's CUMULATIVE branch) exactly,
	//     the central risk the issue named.
	//   - Trailing-pair-only selection: a 3-sample window (1000, 10, 40)
	//     returned the rate implied by ONLY the last two samples (10 -> 40),
	//     ignoring the much larger first sample entirely — matching the
	//     fan-out's own window_pairs[length]/[length-1] selection, not a
	//     whole-window fold.
	//   - The >= 2 samples NULL rule and the left-open window-membership fix
	//     were probed directly: a sample sitting exactly on the window's
	//     trailing edge (anchor - staleness) is excluded, matching
	//     FeatureTSGridRange's own left-open fix.
	//   - The doc's duplicate-timestamp "highest value wins, NaN loses"
	//     rule reproduces the SAME order-dependence for a real-vs-NaN
	//     duplicate pair that cerberus issue #2798 already tracks as
	//     family-wide (NaN loses when inserted before the real sample, wins
	//     when inserted after). Unlike rate/delta, irate reduces every
	//     window to its trailing pair, so a duplicate-timestamp trailing
	//     pair is not a rare edge of a summed window but the whole answer —
	//     but the fan-out has no dedup layer of its own for this shape
	//     either (a duplicate-ts trailing pair there resolves by whatever
	//     order arraySort's stable tie-break happens to produce, itself
	//     unspecified), so this is not a regression against a well-defined
	//     fan-out contract, just the same #2798 gap surfacing through a
	//     different function.
	//
	// AutoSelect is true: the sweep found no irate-specific divergence from
	// PromQL — the one real gap it surfaced is the same pre-existing,
	// family-wide ClickHouse tie-break bug already accepted for the
	// auto-selected rate/increase/resets/deriv/predict_linear/delta
	// siblings, not a reason to treat irate differently from them.
	FeatureTSGridIrate = "ts_grid_irate"

	// FeatureTSGridIdelta is FeatureTSGridIrate's sibling for
	// idelta(<gauge>[<range>]), mapping onto native
	// timeSeriesInstantDeltaToGrid (internal/chsql.emitRangeWindowIDelta's
	// native competitor). Same 25.9 floor, same lagInFrame-then-fan-out
	// fallback chain, same family-wide #2798 duplicate-timestamp gap.
	//
	// The same cerberus issue #2746 sweep found idelta applies NO
	// counter-reset correction: the identical strictly-decreasing trailing
	// pair (100 -> 10) returned the raw -90, matching PromQL's funcIdelta
	// (which — unlike irate — never counter-repairs, the same posture as
	// delta() versus rate()/increase()). The trailing-pair-only selection,
	// >= 2 samples NULL rule, and left-open window-membership fix all
	// reproduced identically to irate's own probes.
	//
	// AutoSelect is true, for the same reason FeatureTSGridIrate's is.
	FeatureTSGridIdelta = "ts_grid_idelta"

	// FeatureLagInFrameAdjacency opts eligible query_range
	// changes()/resets()/irate()/idelta() matrix shapes onto a single sorted
	// lagInFrame/leadInFrame annotation pass with fixed-size per-anchor
	// accumulators, retiring the arrayPopBack/arrayPopFront /
	// window_pairs[length]/[length-1] array-fold fan-out
	// (internal/chsql.emitRangeWindowChanges/Resets/IRate/IDelta) for those
	// four functions (cerberus issue #2759).
	//
	// Unlike the timeSeries*ToGrid family this is a pure SQL-SHAPE
	// optimization — lagInFrame/leadInFrame are long-standing ClickHouse
	// window functions (present well below the 25.9 floor the ts_grid family
	// needs), so it carries NO version gate (AlwaysAvailable) and no
	// allow_experimental_* setting. It is registered as a chopt feature
	// purely for the boot-wired kill-switch this repo's optimization
	// features all share (one lifecycle, no per-query branch) — see
	// docs/clickhouse-optimizations.md.
	//
	// AutoSelect is true: the annotation pass carries the SAME kernels the
	// fan-out does (curr < prev for resets; curr != prev AND NOT
	// both-NaN for changes, #1489's carve-out; CounterOrDeltaPairDelta's
	// DELTA/CUMULATIVE branch for irate, with AggregationTemporality riding
	// the argMax tuple so the branch survives the annotation stage) and every
	// pair whose prev falls outside the anchor's window is excluded BY
	// CONSTRUCTION, so a shard's lagInFrame seed can never differ from route
	// A's (see internal/chplan/sliceinvariant.go's RangeWindow entry) —
	// proven bit-identical against the fan-out by dual-emit parity
	// (internal/chsql/range_window_lag_adjacency_chdb_test.go), not merely
	// asserted. It complements FeatureTSGridChanges (permanently opt-in,
	// #1721), FeatureTSGridResets, FeatureTSGridIrate, and FeatureTSGridIdelta
	// (all 25.9+ only): all four functions fall back to this improved fan-out
	// on any server their own native path does not cover — irate/idelta
	// gained their native timeSeries*ToGrid member (timeSeriesInstantRateToGrid
	// / timeSeriesInstantDeltaToGrid) in cerberus issue #2746; before that
	// this annotation pass was their ONLY non-fan-out strategy.
	FeatureLagInFrameAdjacency = "laginframe_adjacency"

	// FeatureFixedAccumulatorExtrapolated opts eligible query_range
	// rate()/increase()/delta() matrix shapes onto per-(series, anchor)
	// fixed-size aggregates (count/min/max/argMin/argMax/sumIf), retiring the
	// groupArray/arraySort/arrayFilter array-fold fan-out
	// (internal/chsql.emitWindowedArrayExtrapolatedMatrix) for those shapes
	// (cerberus issue #2760) — the FALLBACK path for the extrapolated family
	// when the native timeSeries*ToGrid grid path (ts_grid_range /
	// ts_grid_increase) isn't eligible: below the 25.9 floor, capability-
	// forbidden, or a temporality-bearing window (the native aggregate has no
	// DELTA-temporality arm, see nativeTSGridMatrixNode). This is issue
	// #2759's own direct sibling/precedent — the timeSeries*ToGrid family
	// needs the FIRST and LAST sample of each anchor's window (Prom's
	// extrapolatedRate boundary correction), which #2759's own lagInFrame
	// shape does not compute, so this is a SEPARATE registration rather than
	// a narrowing of laginframe_adjacency; its counter-reset term reuses
	// that feature's own lagInFrame kernel and validity check directly (see
	// internal/chsql/range_window_fixed_accumulator.go), without depending
	// on laginframe_adjacency being independently enabled — the two resolve
	// as SEPARATE floor-independent client-side SQL-shape optimizations,
	// composed by which fallback each Native*Lowerer wraps at the
	// cmd/cerberus wiring site (mirroring how laginframe_adjacency itself
	// narrows the changes/resets fallback there).
	//
	// Like laginframe_adjacency this is a pure SQL-SHAPE optimization —
	// argMin/argMax/uniqExact/count/lagInFrame are all long-standing
	// ClickHouse primitives present well below the 25.9 floor the ts_grid
	// family needs — so it carries NO version gate (AlwaysAvailable) and no
	// allow_experimental_* setting.
	//
	// AutoSelect is false, unlike laginframe_adjacency: the issue's own
	// proposal calls for an optcorpus A/B pass before promoting this to
	// auto-selected (the reset-correction term's summation runs in
	// ClickHouse's own non-deterministic aggregation order, unlike the
	// array-fold's strict time-ordered fold — proven algebraically
	// equivalent and verified bit-identical on the dual-emit parity corpus,
	// but not yet fielded). Reachable only via an explicit
	// CERBERUS_CH_OPTIMIZATIONS=fixed_accumulator_extrapolated listing.
	//
	// Scope: eligible for a temporality-bearing counter window too — the
	// DELTA/CUMULATIVE runtime branch and the reconstructed counter
	// zero-clamp are both decomposed into fixed accumulators, reusing
	// range_window.go's deltaMatrixLevelSource / deltaFirstValFrag
	// UNCHANGED (see internal/chsql/range_window_fixed_accumulator.go's own
	// doc, "Temporality-bearing counters"). What stays excluded is the
	// EXACT, retention-independent DELTA-prefix aggregate mechanism (issue
	// #2389, RangeWindow.DeltaPrefixAggregateInput != nil) — a narrower
	// opt-in-only population needing its own separate re-plumbing, tracked
	// at https://github.com/tsouza/cerberus/issues/2797.
	FeatureFixedAccumulatorExtrapolated = "fixed_accumulator_extrapolated"

	// FeatureSortedSlabOverTime opts eligible query_range sum_over_time() /
	// avg_over_time() matrix shapes onto a per-series sorted slab (one
	// groupArray per series), with each anchor's window sliced out of that
	// ONE array via arrayFilter instead of a fresh per-(series, anchor)
	// regroup, retiring the arrayJoin fan-out + GROUP BY (series, anchor)
	// regroup (internal/chsql.emitWindowedArrayMatrix) for those two shapes
	// (cerberus issue #2761). The regroup's per-(series, anchor) window
	// array is an UNGUARDED fan-out axis — unlike rate/increase/delta
	// (#2429) it carries no size guard at all — so peak memory scales with
	// samples * anchors; the sorted-slab shape bounds it to samples per
	// series, independent of anchor count.
	//
	// Scoped to sum_over_time / avg_over_time only, deliberately narrower
	// than the issue's own full *_over_time proposal (which also names
	// first/last/stddev/stdvar/mad_over_time): these two are the ones whose
	// byte-identical contract with the array-fold is the SIMPLEST to state
	// and verify (arraySum / arrayAvg over the sliced window fold in the
	// same left-to-right float order the array-fold's identical reducers
	// already use — see chplan.RangeWindow.SortedSlabOverTime's own doc),
	// so this cut ships the mandatory arraySlice-preserving-order form the
	// issue calls out first. min/max/count/present_over_time already skip
	// the array-fold entirely (overTimeDirectAggFrag's direct CH group
	// aggregate); first/last/stddev/stdvar/mad_over_time extending to the
	// same slab shape is tracked at
	// https://github.com/tsouza/cerberus/issues/2804.
	//
	// Like fixed_accumulator_extrapolated this is a pure SQL-SHAPE
	// optimization — groupArray/arraySort/arrayFilter/arraySum/arrayAvg are
	// all long-standing ClickHouse primitives — so it carries NO version gate
	// (AlwaysAvailable) and no allow_experimental_* setting.
	//
	// AutoSelect is false, mirroring fixed_accumulator_extrapolated: pending
	// an optcorpus A/B pass before promoting to auto-selected. Reachable
	// only via an explicit CERBERUS_CH_OPTIMIZATIONS=sorted_slab_over_time
	// listing.
	FeatureSortedSlabOverTime = "sorted_slab_over_time"

	// FeatureMapBucketedSerialization stamps
	// map_serialization_version='with_buckets' on the logs table and the
	// traces spans table's CREATE TABLE SETTINGS tail (cerberus issue #2774).
	// ClickHouse's bucketed Map serialization distributes a Map column's
	// entries across N independent sub-streams by a hash of the key, so a
	// read that only touches one key (a PromQL/LogQL/TraceQL label matcher,
	// a tempo tag_values scan) decompresses just that key's bucket instead
	// of the whole map — see
	// https://clickhouse.com/blog/clickhouse-vs-promethous-high-cardinality-part-2-cardinality-in-clickhouse.
	// It is a TABLE-level MergeTree setting (not per-column, not an ALTER
	// target): confirmed against the ClickHouse docs
	// (https://clickhouse.com/docs/sql-reference/data-types/map) that
	// max_buckets_in_map already DEFAULTS to 32 and map_buckets_strategy to
	// 'sqrt' — the two knobs the issue's proposal names as "tuned" are
	// already ClickHouse's own defaults, so this feature stamps ONLY
	// map_serialization_version and leaves both alone rather than
	// re-asserting values that would already apply.
	//
	// The read side needs NO change. Bucketing is a storage/serialization
	// detail the ClickHouse Map reader resolves internally: `m['key']` and
	// mapContains(m, 'key') already compute the same key hash to pick a
	// bucket whether or not with_buckets is active, and a full-map read
	// (`SELECT m`, mapConcat, stream-label rendering) reassembles every
	// bucket transparently. Every place cerberus reads a single Attributes
	// key today (LogQL matcherLHS, TraceQL FieldAccess, tempo
	// tag_values' mapContains/subscript scan) already emits exactly that
	// subscript/mapContains SQL shape — none of it needs to compute a
	// bucket index itself, so no internal/chsql / internal/chplan / lowering
	// change accompanies this feature. (Confirmed against the ClickHouse docs
	// page above, not merely inferred from the blog post.)
	//
	// EXCLUDES the five metrics tables. A metrics series' identity IS its
	// whole Attributes map (every catalog/metadata read and the
	// seriesProjection in internal/schema/ddl already read/hash the full
	// map), so the family's own ~2x whole-map-read penalty (see Risks below)
	// would land squarely on metrics' hottest path for zero benefit — the
	// mechanism only pays off for the SINGLE-key reads logs/traces filter
	// shapes are dominated by. internal/schema/ddl.Config therefore carries
	// LogsSettings / TracesSettings as a DIFFERENT field from the generic
	// Settings escape hatch, which still applies uniformly to every
	// auto-created table (metrics included) — the per-table application
	// machinery a setting scoped to exactly two signals needs, since
	// appendSettings splices ONE Settings continuation into every template
	// alike.
	//
	// SCOPE: new tables only. internal/schemaboot.DDLConfig folds this
	// feature's verdict into the CREATE TABLE SETTINGS tail auto-create
	// renders — it never touches an existing table. An operator upgrading an
	// existing deployment keeps the current 'basic' serialization on every
	// already-created table (byte-identical DDL, zero risk) until they
	// re-provision or run their own
	// `ALTER TABLE ... MODIFY SETTING map_serialization_version='with_buckets'`
	// (existing parts stay flat and readable; only parts written by a
	// SUBSEQUENT merge adopt the bucketed layout — the docs page above
	// confirms with_buckets is not itself an ALTER target requiring a
	// rewrite, and map_serialization_version_for_zero_level_parts can keep
	// fresh inserts flat while merges convert). Scoping to new tables avoids
	// this PR needing to drive or verify that merge-triggered conversion
	// against a live cluster; it is the safe half of the feature to land
	// first, matching this repo's own precedent of shipping a schema knob
	// auto-create renders before any migration tooling exists for it (the
	// AggregationTemporality skip index shipped the same way — see
	// renderAddTemporalityIndex's own MATERIALIZE INDEX backfill note).
	//
	// PRECONDITION (verified against the docs page): pre-26.8 with_buckets
	// does NOT preserve Map key order (order preservation via a
	// bucket_indexes metadata stream ships in 26.8+). Cerberus is safe at
	// any floor at or above this feature's own 26.4 MinVersion solely
	// because every stream-identity / series read already goes through
	// mapSort canonicalization (internal/chplan/canonical_attributes.go,
	// map_key_order_chdb_test.go) rather than trusting raw map iteration
	// order — a future read path that inspects raw map order without
	// mapSort would break under this setting regardless of version.
	//
	// VERSION FLOOR: the upstream feature landed via a backport in
	// v26.3.2.3-lts; 26.3.0/26.3.1 lack it. chopt.Version compares
	// (major, minor) only, so a "26.3" MinVersion would wrongly claim
	// 26.3.0/26.3.1 support it. 26.4 is the first minor whose EVERY patch
	// carries the feature, so it is the floor this registry — which cannot
	// express a patch-level exception — can state without lying to an
	// operator on an early 26.3 patch. An operator who knows their cluster
	// is >= 26.3.2 may still list the feature explicitly.
	//
	// RISKS (from the issue, unmitigated by this feature — a hard gate for
	// promoting past opt-in): the SAME blog source measures roughly a 2x
	// SLOWER full-map read under with_buckets. Cerberus has real full-map
	// read paths on logs/traces too — stream-label rendering, Loki
	// series/labels/detected_labels/index_stats/index_volume, tempo
	// mapConcat — so this feature is NOT auto-selected (AutoSelect=false):
	// enabling it is a deliberate operator tradeoff (single-key filters get
	// faster, whole-map reads get slower) pending a real before/after
	// benchmark on both shapes, not a version-gated pure win the auto-picker
	// can safely assume — the same conservative posture as
	// FeatureQuantilePromHistogram and FeatureColumnarResultDecode.
	FeatureMapBucketedSerialization = "map_bucketed_serialization"

	// FeatureTSGridLastOverTime opts eligible query_range matrix-mode
	// last_over_time(<v>[<range>]) shapes onto the SAME native
	// timeSeriesResampleToGridWithStaleness aggregate FeatureTSGridResample
	// already rides (also spelled timeSeriesLastToGrid) — its contract, "most
	// recent sample within the staleness window per grid point, NULL when
	// none", IS last_over_time's contract, with the matrix [range] supplying
	// the staleness parameter (chplan.RangeWindowStaleResample.Lookback) in
	// place of the bare-selector shape's fixed 5m instantLookback.
	//
	// IMPORTANT — the floor is 26.6, NOT the family's 25.9. Two real
	// correctness fixes to timeSeriesResampleToGridWithStaleness /
	// timeSeriesLastToGrid landed after 25.9: ClickHouse/ClickHouse#106504
	// ("Fix timeSeriesLastToGrid() for timestamps before start") and #106577
	// ("Fix timeSeriesLastToGrid() for out-of-window timestamps"), both
	// merged into 26.6.1. #106577 is the binding one for this shape
	// specifically: its own regression fixture is
	// `last_over_time(test[10s])[120s:15s]` — a staleness window (10s)
	// SMALLER than the grid step (15s). Pre-fix, the sparse resample
	// carry-forward only re-checked staleness for a grid cell that started
	// NULL (AggregateFunctionTimeseriesToGridSparse.h); a cell that already
	// held a direct sample skipped the check entirely, so a sample landing
	// inside grid bucket i's coarse span but stale for bucket i's own
	// [current-window, current] membership was still emitted uncorrected.
	// ts_grid_resample's own default instantLookback (5m) comfortably
	// dominates the request step in the overwhelming majority of
	// deployments, so #106577 rarely bites that shape; last_over_time's
	// staleness parameter is the caller's OWN [range] literal, which is
	// routinely smaller than or comparable to the query step — exactly the
	// regime #106577 fixes. A 25.9 floor here would auto-enable a path that
	// silently emits a wrong value on that common shape.
	//
	// AutoSelect is false: 26.6 is a brand-new floor with no fielded
	// validation yet, the same conservative posture
	// FeatureQuantilePromHistogram's 25.10 bump and
	// FeatureMapBucketedSerialization's 26.4 one took — opt-in only via
	// CERBERUS_CH_OPTIMIZATIONS=ts_grid_last_over_time until the floor earns
	// auto promotion. Shares the family's RequiresExperimentalTSGrid gate
	// (allow_experimental_time_series_aggregate_functions).
	//
	// first_over_time has no native sibling: the aggregate carries the
	// LATEST in-window sample forward, never the earliest, so it stays on
	// the fan-out unconditionally.
	FeatureTSGridLastOverTime = "ts_grid_last_over_time"

	// FeatureColumnStatistics gates the curated `ADD STATISTICS IF NOT
	// EXISTS` ALTER registry (cerberus issue #2766) that installs ClickHouse
	// column statistics on the metrics/logs/traces fact tables' highest-value
	// filter and join columns. Statistics give the query planner real
	// cardinality/selectivity estimates for PREWHERE-pushdown and
	// join-ordering, in place of cerberus's own hand-rolled static heuristic
	// (internal/chsql/prewhere.go) — zero statistics usage exists in
	// production before this feature.
	//
	// STATISTICS TYPE PER COLUMN TYPE — verified against a live ClickHouse
	// 26.5 server, NOT assumed from the issue's proposal text: `minmax` and
	// `tdigest` are numeric-only, and ClickHouse rejects ADD STATISTICS
	// outright (code 708, ILLEGAL_STATISTICS) for either on a String /
	// LowCardinality(String) column. So the String-family identity columns —
	// ServiceName, MetricName, SpanName, TraceId — carry `uniq` only (which is
	// also the semantically right stat for an equality-filtered column;
	// `minmax` exists for RANGE predicates a string equality never issues).
	// The numeric columns — AggregationTemporality (Int32), SeverityNumber
	// (UInt8) — carry `minmax, uniq`. Duration (UInt64) additionally carries
	// `tdigest` (a range predicate — a latency threshold — needs a
	// distribution estimate `minmax`/`uniq` alone cannot give). See
	// internal/schema/ddl's renderMetricsColumnStatistics /
	// renderLogsColumnStatistics / renderTracesColumnStatistics for the exact
	// per-table ALTER split this forces (a string column and a numeric column
	// can never share one ADD STATISTICS statement, since ClickHouse applies
	// one TYPE list to every listed column).
	//
	// VERSION FLOOR: 26.3, and unlike FeatureMapBucketedSerialization this one
	// needs no "round up to the next minor" caution — upstream PR #97487
	// ("Make statistics GA & automatically create minmax + uniq statistics
	// for new columns") merged as 26.3.1.276, the FIRST patch of the 26.3
	// release, so every 26.3 server carries it. The PR also renames
	// `allow_experimental_statistics` (default false) to `allow_statistics`
	// (default TRUE) and promotes `allow_statistics_optimize` — the setting
	// that actually feeds PREWHERE/join-ordering decisions — from beta to GA,
	// still defaulting on. Neither setting needs a client-side stamp the way
	// the timeSeries*ToGrid family needs
	// allow_experimental_time_series_aggregate_functions: both already
	// default to enabled on a >= 26.3 server, so this feature (like
	// map_bucketed_serialization) carries no RequiresExperimentalTSGrid-style
	// gate and no engine co-stamp.
	//
	// INSERT-OVERHEAD MITIGATION (verified, not merely assumed): the SAME PR
	// disables `materialize_statistics_on_insert` by default specifically to
	// avoid slower INSERTs — new parts do NOT compute statistics inline on
	// every insert; the values are populated by background MERGES instead.
	// This directly answers the issue's own "insert/merge overhead" risk
	// note: the overhead lands on the existing merge background pool, not the
	// synchronous insert path.
	//
	// PREWHERE REORDERING IS VERIFIED, not merely claimed: the issue itself
	// flagged as unverified "whether statistics-based condition reordering
	// hooks into cerberus's explicitly written PREWHERE clause (vs only the
	// WHERE->PREWHERE move optimizer)". Upstream RFC ClickHouse#53240 ("use
	// statistic to order prewhere conditions better") confirms
	// allow_statistics_optimize reorders an ALREADY-multi-condition PREWHERE
	// clause's own conjuncts by statistics-derived selectivity — exactly
	// cerberus's own emission shape, not only the WHERE->PREWHERE promotion
	// decision.
	//
	// NOT auto-selected (AutoSelect=false) regardless: statistics are
	// UNSUPPORTED on ClickHouse Cloud (the apply path tolerates that refusal
	// — see internal/schema/ddl.isColumnStatisticsUnsupported), and even with
	// the PREWHERE-reorder mechanism confirmed to fire, its real-world
	// MAGNITUDE on cerberus's own production query shapes is not yet
	// measured — nor is whether a resulting plan-shape change (a different
	// join build side, say) interacts with the solver's calibrated
	// fanout-guard constants the way spill settings did in #2665. Each is a
	// real-world calibration question this feature alone cannot answer, so
	// enabling it is a deliberate operator choice pending that evidence, not
	// a version-gated pure win the auto-picker can assume.
	FeatureColumnStatistics = "column_statistics"

	// FeatureClassicBucketMergeSumMap opts the aggregated classic-histogram-
	// quantile cross-series merge stage (histogram_quantile.go's
	// lowerHistogramQuantileAgg / histogram_quantile_range.go's
	// lowerHistogramQuantileClassicAggRange) onto a linear
	// sumMap(bounds, counts) + arrayCumSum reshape for the SUM fold ONLY —
	// see internal/promql/classic_bucket_merge_summap.go — retiring the
	// groupArray + per-rung arrayFilter-rescan fold
	// (classicBucketMergedLadderExpr) that classic_bucket_merge_bound.go's
	// own audited doc identifies as quadratic in (merge width x total
	// bucket-element volume). avg/min/max/count/group/stddev/stdvar/quantile
	// folds are untouched — every non-sum operator keeps the existing
	// groupArray-fold shaping unconditionally, chopt feature or not.
	//
	// sumMap is old, always-available ClickHouse functionality — there is no
	// version floor to probe, matching FeatureLagInFrameAdjacency's own
	// posture for a client-side, no-server-setting optimization.
	//
	// AutoSelect is false, UNLIKE FeatureLagInFrameAdjacency's "bit-identical
	// to the fan-out" bar. A chDB differential probe run while implementing
	// this feature found a REAL, measurable divergence from the existing
	// merge for a HETEROGENEOUS group (contributing rows reporting different
	// ExplicitBounds layouts): sumMap + arrayCumSum sums, at each union bound
	// u, every row's own sub-cumulative count over its OWN buckets <= u,
	// while reference Prometheus's sum by(le) — and the existing has-filter
	// fold that reproduces it — only sums rows whose OWN layout contains u
	// exactly. Worked/chDB-confirmed example: bounds [1,2,3]/counts [10,5,0]
	// merged with bounds [1,5]/counts [7,0] — the existing merge (plus its
	// monotonic repair) yields [17,17,17,17] at union bounds [1,2,3,5];
	// sumMap + arrayCumSum yields [17,22,22,22].
	//
	// For a HOMOGENEOUS group (every row shares one ExplicitBounds — the
	// overwhelmingly common real shape, and the one this feature's own
	// ~50x win estimate is calibrated on) the two constructions are provably
	// identical: every row carries every union bound, so the has-filter is a
	// no-op and both reduce to the same per-bound sum. The divergence is real
	// only for genuinely mismatched bucket boundaries across a group's
	// series — an already-degenerate input Prometheus itself handles poorly.
	// See https://github.com/tsouza/cerberus/issues/2817, filed to
	// investigate restricting the sumMap path to provably-homogeneous groups
	// (which would let AutoSelect move to true). Until that lands this
	// feature is reachable only by explicit
	// CERBERUS_CH_OPTIMIZATIONS=classic_bucket_merge_summap listing,
	// mirroring FeatureQuantilePromHistogram's and FeatureTSGridChanges'
	// posture for a feature with a proven, real divergence on a specific
	// input shape.
	//
	// A second, independent risk is DOCUMENTED rather than gating AutoSelect
	// further: arrayCumSum propagates a NaN forward to every higher union
	// rung once it appears, while the existing has-filter fold only poisons
	// the rungs a NaN row's own layout carries — pinned by
	// TestClassicBucketMergeSumMapDifferential's NaN case, not glossed over.
	FeatureClassicBucketMergeSumMap = "classic_bucket_merge_summap"

	// FeatureJoinSpill stamps max_bytes_before_external_join = cap/2 (the
	// SAME cap-relative arithmetic internal/engine/spill.go's unconditional
	// group_by/sort stamps use, keyed off CERBERUS_CH_QUERY_MAX_MEMORY) on
	// join-bearing query plans (cerberus issue #2779). Join memory today is
	// protected only by throwIf cardinality guards (VectorJoin's own
	// ManyToManyMatchMessage) and structural shape restrictions — neither is
	// a memory backstop — so a big hash build (PromQL many-to-many vector
	// matching, a group_left skew, the delta-prefix LEFT JOIN, a TraceQL
	// structural join) can still hit the destructive MEMORY_LIMIT_EXCEEDED
	// (code 241) abort the group_by/sort stamps were added to prevent for
	// aggregation and sort.
	//
	// EXPLICIT stamp, not the ClickHouse-native ratio default: ClickHouse
	// 26.5+ ships max_bytes_ratio_before_external_join=0.5, but a ratio
	// setting is silently ignored when no server/user memory limit is
	// configured — the exact failure mode ClickHouse#76740 documents for the
	// analogous max_bytes_ratio_before_external_group_by. cerberus does not
	// control whether an operator's ClickHouse profile sets a server-side
	// limit, so the ratio default cannot be relied on at any floor this
	// feature supports; the explicit byte stamp is unconditional the way the
	// group_by/sort stamps already are.
	//
	// VERSION FLOOR: max_bytes_before_external_join itself carries an
	// EXPERIMENTAL marker at its 26.4 introduction (the release
	// presentations.clickhouse.com/2026-release-26.4 covers it; the
	// changelog #264 anchor does not surface the entry) and is treated as
	// production-grade from 26.5, where the ratio-default sibling setting
	// ships alongside it. This registry entry pins the floor to 26.4 —
	// where the setting is FIRST available to stamp, regardless of its own
	// maturity label — and separately marks the registry Stability as
	// Experimental to keep that honestly reflected in operator-facing docs,
	// mirroring how the timeSeries*ToGrid family stays Experimental in
	// maturity while still being version-gated AutoSelect (Stability and
	// AutoSelect are deliberately decoupled — see the Feature.AutoSelect
	// doc below).
	//
	// AutoSelect is true: like the group_by/sort stamps this narrows, the
	// setting is RESULT-EQUIVALENT (spill changes only execution strategy,
	// never the rows) and strictly protective — an operation whose join
	// build stays under spillThreshold(cap) never spills, so a normal query
	// is byte-for-byte unaffected, and only a join approaching the cap
	// spills-and-completes instead of aborting. There is no downside to
	// auto-enabling a pure availability win on every server that supports
	// the setting.
	//
	// Known cost (from the issue, verified): on 26.4-26.7 configuring join
	// spill loses some post-build join optimisations (tryRerangeRightTableData,
	// FixedHashMap conversion, shared runtime filters) — a cost the
	// ratio-default path pays too, so it is not specific to the explicit
	// stamp. The fix ships in 26.8 (upstream PR #111972), NOT 26.7. This is
	// the same trade the group_by/sort spill stamps already accept — an
	// OOM abort is an availability bug, not an optimisation opportunity — so
	// it does not change the AutoSelect posture.
	FeatureJoinSpill = "join_spill"

	// FeatureArgAndMaxFusion opts two narrow chsql emission sites off the
	// `argMax(Value, TimeUnix)` + separate `max(TimeUnix)` two-aggregate
	// pairing onto ClickHouse's fused `argAndMax(Value, TimeUnix)` (a
	// single Tuple(Value, TimeUnix)-returning aggregate), destructured back
	// into the two columns via `tupleElement` in the outer SELECT (cerberus
	// issue #2764): chplan.RangeLWR.SampleTimestamp's collapse (the
	// `timestamp(<selector>)` carve-out native ToGrid can't serve —
	// internal/chsql/range_lwr.go, the pairing exists ONLY when
	// SampleTimestamp is requested) and internal/chsql/vector_join.go's
	// non-derived, non-StepAligned "latest sample" per-side join
	// aggregation. StepAligned (range-mode) and derived (range-vector-
	// operand) join arms are untouched — the StepAligned arm never pairs
	// the two aggregates at all (TimestampColumn is a plain GROUP BY key
	// there), and the derived arm has no real TimestampColumn to argMax by.
	//
	// VERSION FLOOR: 25.11, NOT the docs page's "Introduced in: v1.1.0"
	// badge — that badge is a docs-tooling artifact unrelated to any real
	// ClickHouse product release (product releases run 24.x/25.x; nothing
	// in ClickHouse versions as "1.1.0"). Verified against two independent
	// authoritative sources instead: upstream PR
	// https://github.com/ClickHouse/ClickHouse/pull/89884 ("Implement
	// argAndMin, argAndMax functions"), merged 2025-11-17, and the official
	// ClickHouse 25.11 release blog
	// (https://clickhouse.com/blog/clickhouse-release-25-11), whose
	// "argAndMin and argAndMax" section states outright "ClickHouse 25.11
	// introduces the argAndMax and argAndMin functions" — both naming the
	// same author/PR. argAndMax needs no allow_experimental_* setting: it
	// shipped as a plain new aggregate function, not behind an experimental
	// gate, so RequiresExperimentalTSGrid is false.
	//
	// EQUIVALENCE (verified, not merely assumed the way the issue's own
	// risk section flagged it): downstream code never reads WHICH row
	// argMax(Value, TimeUnix) picked among ties on TimeUnix, only the
	// selected Value and the max(TimeUnix) value itself — see
	// chplan.RangeLWRSampleTimestampColumn's consumers
	// (internal/promql/date_fns.go's timestamp() lowering,
	// internal/promql/duplicate_labelset_guard.go). max(TimeUnix) is a
	// PLAIN deterministic aggregate with no tie-break ambiguity of its own
	// — the tie-break only affects which Value argMax happens to pair with
	// a TIED max(TimeUnix), and that ambiguity is identical whether the
	// picking is done by two separate aggregates or by one fused
	// argAndMax, because ClickHouse's argAndMax is documented to compute
	// the identical "value corresponding to max(val)" selection argMax
	// does — it is a packaging of the same underlying mechanism, not a
	// different one. So fusion introduces no NEW divergence in either the
	// timestamp component (never ambiguous, tie or no tie) or the value
	// component (equally ambiguous either way).
	//
	// EXPECTED WIN (the issue's own honest accounting): ClickHouse keeps
	// ONE hash-table entry per GROUP BY key regardless of aggregate count,
	// and per-group memory is dominated by the Attributes-Map group key —
	// so fusion saves one fewer aggregate state (create/update/merge/
	// serialize) and one fewer per-row comparison, roughly a third of the
	// two states' payload, NOT a halving. Small but essentially free, most
	// visible on high-cardinality instant queries and timestamp()
	// carve-outs — the fixed-size-accumulator shape
	// internal/chsql/lwr_fanout_bound.go already optimizes for.
	//
	// AutoSelect is true: a pure, version-gated, tie-invariant win with no
	// operator tradeoff to weigh (unlike FeatureColumnarResultDecode /
	// FeatureMapBucketedSerialization / FeatureColumnStatistics), so auto
	// picks it on any server that meets the 25.11 floor — mirroring the
	// timeSeries*ToGrid family's own AutoSelect posture for a
	// proven-equivalent, version-gated substitution. Stability is
	// Experimental purely on account of the underlying ClickHouse function
	// being very new (25.11, no fielded deployment history yet), the same
	// reasoning the ts_grid family's own Experimental marking uses despite
	// also being AutoSelect true.
	FeatureArgAndMaxFusion = "arg_and_max_fusion"

	// FeatureResultCache stamps use_query_cache=1 + query_cache_ttl on a
	// cerberus-eligible read path — a query whose evaluated window has fully
	// CLOSED (every range-mode window End lies before now minus the
	// deployment's configured ingest-lag horizon, and no now()/now64()
	// expression reaches the emitted SQL; see
	// internal/engine.eligibleForResultCache) — so ClickHouse's query result
	// cache can answer a dashboard's byte-identical re-issue of the same
	// query_range without re-scanning.
	//
	// The setting family (use_query_cache, query_cache_ttl) is long-stable —
	// ClickHouse shipped the query cache in 23.2, well before cerberus's own
	// 24.8 floor — so unlike condition_cache there is no version floor above
	// the supported baseline to gate on. What a version floor CANNOT catch is
	// a deployment that runs a hardened/constrained profile pinning or
	// forbidding use_query_cache, or a server whose query cache is disabled
	// entirely (query_cache_max_size_in_bytes=0): the resolver instead gates
	// on a boot capability probe (ProbeResultCacheCapability,
	// RequiresResultCacheCapability below), mirroring how
	// RequiresExperimentalTSGrid gates the native timeSeries*ToGrid family on
	// a SEPARATE probed capability rather than trusting the version floor
	// alone.
	//
	// AutoSelect is true: the eligibility gate is cerberus's own correctness
	// guard (never ClickHouse's query_cache_nondeterministic_function_handling
	// alone — that is defense in depth), so a query the gate marks eligible is
	// safe to cache on any capable server, with no operator tradeoff to weigh.
	// A deployment that wants the result cache off entirely omits it from an
	// explicit CERBERUS_CH_OPTIMIZATIONS list (or sets "off"), exactly the
	// opt-out condition_cache and every other AutoSelect feature already give
	// an operator — no separate dedicated flag is needed.
	FeatureResultCache = "result_cache"

	// FeatureLazyMaterialization stamps query_plan_optimize_lazy_materialization=1
	// + query_plan_max_limit_for_lazy_materialization=<the query's own LIMIT> on
	// a Tempo `ORDER BY Timestamp DESC LIMIT N` search shape (internal/api/tempo
	// handler.go's /search/recent and boundNewestTraces, structural_two_phase.go's
	// phase-A ranking) — see internal/engine.eligibleForLazyMaterialization.
	// ClickHouse defers fetching every non-sort-key column (SpanAttributes,
	// ResourceAttributes, Events, Links — the wide OTel span payload) until
	// AFTER the ORDER BY + LIMIT has picked the surviving rows, instead of
	// reading them for the whole scanned window and discarding most of it.
	//
	// This replaces cerberus's own hand-rolled late-materialisation rewrite
	// (formerly internal/chsql/late_mat.go, deleted alongside this feature —
	// cerberus issue #2782): that structural Project(Limit(Filter?(Scan)))
	// matcher never fired on any production query path, because every
	// production Limit construction wraps an OrderBy directly (the matcher's
	// switch only accepted Filter/Scan under Limit), and the Loki line path
	// builds no SQL Limit at all (the request limit is applied Go-side). The
	// server-side setting handles the shape the hand-rolled JOIN-back never
	// could: no second read, no RowKey registry, no degenerate all-zero
	// TraceId JOIN key.
	//
	// The knob is sized to the REQUEST's own LIMIT, never a fixed ceiling:
	// verified on a live chDB 26.5 probe that a max-limit knob BELOW the
	// query's actual LIMIT silently falls back to eager reads (no
	// LazilyReadFromMergeTree step in EXPLAIN PLAN) — a fixed constant would
	// silently stop helping the instant a caller's limit grew past it.
	//
	// VERSION FLOOR: query_plan_optimize_lazy_materialization ships in
	// ClickHouse 25.11 (https://clickhouse.com/docs/whats-new/changelog/2025#2511).
	// Stability is Experimental — the same "very new capability, no fielded
	// deployment history yet" reasoning arg_and_max_fusion's own 25.11-floor
	// entry uses — despite AutoSelect being true.
	//
	// AutoSelect is true: verified on a live chDB 26.5.1.1 probe (EXPLAIN PLAN
	// actions=1 against a seeded otel_traces-shaped table) that stamping both
	// settings is RESULT-EQUIVALENT — identical row count and column set with
	// and without the stamp, byte-identical rows, only the read order/IO
	// pattern changes (a LazilyReadFromMergeTree + JoinLazyColumnsStep pair
	// replaces the eager Expression/Filter/Sort/Limit chain reading every
	// column up front). ClickHouse's own top-N PREWHERE promotion
	// (`__topKFilter` on the sort column) fires independently of this
	// setting — confirmed present in EXPLAIN PLAN even with lazy
	// materialisation forced off (enable_analyzer=0) — so there is no
	// negative PREWHERE interaction to weigh.
	//
	// query_plan_optimize_lazy_materialization is gated behind the analyzer
	// exactly like use_query_condition_cache: forcing enable_analyzer=0 on the
	// same chDB probe made the LazilyReadFromMergeTree step disappear entirely
	// (plain eager ReadFromMergeTree instead), so internal/engine co-stamps
	// enable_analyzer=1 alongside this feature's two settings, mirroring
	// ConditionCache's settingEnableAnalyzer co-stamp — safe because the
	// analyzer is GA on every server this feature's 25.11 floor reaches (well
	// past condition_cache's own 25.3 GA baseline).
	FeatureLazyMaterialization = "lazy_materialization"
)

// AlwaysAvailable is the zero version floor for a feature that depends on no
// server-version gate at all (a purely client-side optimization such as
// columnar_result_decode). Version{} is the additive identity of AtLeast:
// every probed server version satisfies AtLeast(AlwaysAvailable), so listing
// such a feature explicitly never trips the "needs ClickHouse >=X" fail-fast,
// in either enforcing or permissive mode. It is named rather than written as a
// bare Version{} literal so the registry entry reads as an intentional "no
// version requirement" rather than a forgotten / zero-valued field.
var AlwaysAvailable = Version{Major: 0, Minor: 0}

// Stability classifies a registry feature's MATURITY (operator-facing docs /
// support expectations) — it is deliberately DECOUPLED from auto-eligibility,
// which lives on the separate Feature.AutoSelect axis. A feature can be
// Experimental in maturity yet auto-selected by version (the native
// timeSeries*ToGrid aggregates: validated result-correct + flat-memory, so auto
// picks them on capable servers while their docs stay honestly "experimental").
type Stability int

const (
	// Stable features are mature and documented as production-ready.
	Stable Stability = iota
	// Experimental features are honestly young in maturity. This says nothing
	// about whether auto picks them — that is Feature.AutoSelect.
	Experimental
)

// Feature is one registry entry: a stable id, the minimum major.minor server
// version that supports it, its stability class, an auto-eligibility flag, and a
// one-line operator-facing description.
//
// AutoSelect is the auto-eligibility axis, kept distinct from Stability
// (maturity): under the default `auto` selection a feature is enabled iff
// AutoSelect is true AND the probed server satisfies MinVersion. This lets an
// Experimental-maturity feature still be auto-enabled by version (the native
// timeSeries*ToGrid aggregates), while a feature that is a deliberate perf
// TRADEOFF rather than a version-gated win (columnar_result_decode) stays
// AutoSelect=false and is reachable only by explicit listing.
//
// Note: the per-feature ClickHouse allow_experimental_* setting NAME is NOT a
// registry field. Stamping that setting lives in the engine plan path: the
// engine inspects the post-optimize plan (planHasTSGridNative) and co-stamps
// allow_experimental_time_series_aggregate_functions=1 via
// chclient.WithTSGridSetting on exactly the queries that use the native node,
// rather than on every query merely because the feature is enabled. Carrying a
// setting name on the registry entry as well would be a dead second source of
// truth, so it is intentionally absent here.
//
// RequiresExperimentalTSGrid DOES record, data-driven rather than by hardcoded
// id, WHICH features need that experimental setting stamped — the eight native
// timeSeries*ToGrid aggregates. The resolver reads this flag to gate those
// features on the boot capability verdict (Config.Capability): a feature with
// RequiresExperimentalTSGrid is enabled only when the server both meets its
// version floor AND permits the experimental setting. Features that touch no
// experimental setting (aggregation_in_order, condition_cache,
// columnar_result_decode) leave it false and are never capability-gated.
//
// RequiresResultCacheCapability is the SAME kind of second axis for the
// result_cache feature, but gated on a DIFFERENT boot probe
// (ProbeResultCacheCapability, verifying use_query_cache/query_cache_ttl
// rather than the ts-grid experimental setting) and a DIFFERENT
// Config.ResultCacheCapability field — the two axes are independent
// verdicts about independent settings, so a server can permit one and
// forbid the other.
type Feature struct {
	ID         string
	MinVersion Version
	Stability  Stability
	AutoSelect bool
	// RequiresExperimentalTSGrid marks a feature whose native node makes the
	// engine stamp allow_experimental_time_series_aggregate_functions=1. Such a
	// feature is additionally gated on the boot capability verdict: a server that
	// forbids the setting (constrained profile / readonly user) drops it to the
	// fan-out path even when the version floor is met.
	RequiresExperimentalTSGrid bool
	// RequiresResultCacheCapability marks a feature (only result_cache today)
	// whose settings the engine stamps must be verified against the boot
	// query-result-cache capability probe rather than assumed available just
	// because the version floor is met — see the type doc above.
	RequiresResultCacheCapability bool
	Doc                           string
}

// registry is the seeded feature table. It is value data (no init-time
// mutation), so Registry can hand out a defensive copy and callers cannot
// mutate the canonical entries.
var registry = []Feature{
	{
		ID:         FeatureAggregationInOrder,
		MinVersion: Version{Major: 24, Minor: 8},
		Stability:  Stable,
		AutoSelect: true,
		Doc:        "stamp optimize_aggregation_in_order=1 when the Aggregate GROUP BY is a sort-key prefix (result-equivalent)",
	},
	{
		ID:         FeatureConditionCache,
		MinVersion: Version{Major: 25, Minor: 3},
		Stability:  Stable,
		AutoSelect: true,
		Doc:        "stamp use_query_condition_cache=1 on predicate-stable read paths (result-equivalent cache, server >= 25.3)",
	},
	{
		ID:                         FeatureTSGridRange,
		MinVersion:                 Version{Major: 25, Minor: 9},
		Stability:                  Experimental,
		AutoSelect:                 true,
		RequiresExperimentalTSGrid: true,
		Doc:                        "opt eligible rate(<counter>[<range>]) shapes onto native timeSeriesRateToGrid (experimental maturity, auto-enabled on server >= 25.9 — the left-open window fix)",
	},
	{
		ID:                         FeatureTSGridResample,
		MinVersion:                 Version{Major: 25, Minor: 9},
		Stability:                  Experimental,
		AutoSelect:                 true,
		RequiresExperimentalTSGrid: true,
		Doc:                        "opt the range-mode instant-vector staleness shape onto native timeSeriesResampleToGridWithStaleness (experimental maturity, auto-enabled on server >= 25.9 — the left-open window fix)",
	},
	{
		ID:         FeatureColumnarResultDecode,
		MinVersion: AlwaysAvailable,
		Stability:  Experimental,
		AutoSelect: false,
		Doc:        "decode the query_range matrix shape via ch-go columnar path (client-side, no version floor, opt-in only — never auto)",
	},
	{
		ID:                         FeatureTSGridChanges,
		MinVersion:                 Version{Major: 25, Minor: 9},
		Stability:                  Experimental,
		AutoSelect:                 false,
		RequiresExperimentalTSGrid: true,
		Doc:                        "opt eligible changes(<v>[<range>]) shapes onto native timeSeriesChangesToGrid (experimental maturity, server >= 25.9, opt-in only via CERBERUS_CH_OPTIMIZATIONS — the builtin diverges from reference Prometheus on NaN-adjacent windows, #1721)",
	},
	{
		ID:                         FeatureTSGridResets,
		MinVersion:                 Version{Major: 25, Minor: 9},
		Stability:                  Experimental,
		AutoSelect:                 true,
		RequiresExperimentalTSGrid: true,
		Doc:                        "opt eligible resets(<counter>[<range>]) shapes onto native timeSeriesResetsToGrid (experimental maturity, auto-enabled on server >= 25.9)",
	},
	{
		ID:                         FeatureTSGridDeriv,
		MinVersion:                 Version{Major: 25, Minor: 9},
		Stability:                  Experimental,
		AutoSelect:                 true,
		RequiresExperimentalTSGrid: true,
		Doc:                        "opt eligible deriv(<gauge>[<range>]) shapes onto native timeSeriesDerivToGrid (experimental maturity, auto-enabled on server >= 25.9)",
	},
	{
		ID:                         FeatureTSGridPredictLinear,
		MinVersion:                 Version{Major: 25, Minor: 9},
		Stability:                  Experimental,
		AutoSelect:                 true,
		RequiresExperimentalTSGrid: true,
		Doc:                        "opt eligible predict_linear(<gauge>[<range>], t) shapes (whole-second literal t) onto native timeSeriesPredictLinearToGrid (experimental maturity, auto-enabled on server >= 25.9)",
	},
	{
		ID:                         FeatureTSGridRecollapse,
		MinVersion:                 Version{Major: 25, Minor: 9},
		Stability:                  Experimental,
		AutoSelect:                 true,
		RequiresExperimentalTSGrid: true,
		Doc:                        "defer the label-shaping tower past an eligible native rate grid via the -State/-Merge combinator pair, so it runs once per raw series instead of once per row (narrows ts_grid_range; experimental maturity, auto-enabled on server >= 25.9)",
	},
	{
		ID:                         FeatureTSGridIncrease,
		MinVersion:                 Version{Major: 25, Minor: 9},
		Stability:                  Experimental,
		AutoSelect:                 true,
		RequiresExperimentalTSGrid: true,
		Doc:                        "opt eligible increase(<counter>[<range>]) shapes onto native timeSeriesRateToGrid multiplied back by the window seconds (experimental maturity, auto-enabled on server >= 25.9 — the left-open window fix)",
	},
	{
		ID:                         FeatureTSGridHistogram,
		MinVersion:                 Version{Major: 25, Minor: 9},
		Stability:                  Experimental,
		AutoSelect:                 true,
		RequiresExperimentalTSGrid: true,
		Doc:                        "opt the classic-histogram rate() window fold behind histogram_quantile onto a native timeSeriesRateToGrid ladder over the unnested le rungs (experimental maturity, auto-enabled on server >= 25.9 — floor inherited from the timeSeries*ToGrid family)",
	},
	{
		ID:         FeatureQuantilePromHistogram,
		MinVersion: Version{Major: 25, Minor: 10},
		Stability:  Experimental,
		AutoSelect: false,
		Doc:        "opt the classic histogram_quantile rank walk onto the native quantilePrometheusHistogram(phi)(le, cum) aggregate (server >= 25.10, opt-in only via CERBERUS_CH_OPTIMIZATIONS pending fielded validation of the new floor)",
	},
	{
		ID:                         FeatureTSGridDelta,
		MinVersion:                 Version{Major: 25, Minor: 9},
		Stability:                  Experimental,
		AutoSelect:                 true,
		RequiresExperimentalTSGrid: true,
		Doc:                        "opt eligible delta(<gauge>[<range>]) shapes onto native timeSeriesDeltaToGrid (experimental maturity, auto-enabled on server >= 25.9 — the left-open window fix; a chDB differential sweep proved no counter-reset correction, matching PromQL)",
	},
	{
		ID:                         FeatureTSGridIrate,
		MinVersion:                 Version{Major: 25, Minor: 9},
		Stability:                  Experimental,
		AutoSelect:                 true,
		RequiresExperimentalTSGrid: true,
		Doc:                        "opt eligible irate(<counter>[<range>]) shapes onto native timeSeriesInstantRateToGrid (experimental maturity, auto-enabled on server >= 25.9 — the left-open window fix; a chDB sweep proved trailing-pair counter-reset correction, matching PromQL)",
	},
	{
		ID:                         FeatureTSGridIdelta,
		MinVersion:                 Version{Major: 25, Minor: 9},
		Stability:                  Experimental,
		AutoSelect:                 true,
		RequiresExperimentalTSGrid: true,
		Doc:                        "opt eligible idelta(<gauge>[<range>]) shapes onto native timeSeriesInstantDeltaToGrid (experimental maturity, auto-enabled on server >= 25.9 — the left-open window fix; a chDB differential sweep proved no counter-reset correction, matching PromQL)",
	},
	{
		ID:         FeatureLagInFrameAdjacency,
		MinVersion: AlwaysAvailable,
		Stability:  Experimental,
		AutoSelect: true,
		Doc:        "opt eligible query_range changes/resets/irate/idelta shapes onto a lagInFrame/leadInFrame annotation pass with fixed-size accumulators, retiring the array-fold fan-out (client-side, no version floor, auto-enabled, bit-identical to the fan-out)",
	},
	{
		ID:         FeatureFixedAccumulatorExtrapolated,
		MinVersion: AlwaysAvailable,
		Stability:  Experimental,
		AutoSelect: false,
		Doc:        "opt eligible query_range rate/increase/delta shapes onto per-(series, anchor) fixed-size aggregates (count/min/max/argMin/argMax/sumIf), retiring the array-fold fan-out (no version floor, opt-in via CERBERUS_CH_OPTIMIZATIONS pending optcorpus A/B)",
	},
	{
		ID:         FeatureSortedSlabOverTime,
		MinVersion: AlwaysAvailable,
		Stability:  Experimental,
		AutoSelect: false,
		Doc:        "opt eligible query_range sum_over_time/avg_over_time shapes onto a per-series sorted-slab groupArray sliced per anchor, retiring the arrayJoin fan-out + per-(series, anchor) regroup (no version floor, opt-in via CERBERUS_CH_OPTIMIZATIONS pending optcorpus A/B)",
	},
	{
		ID:         FeatureMapBucketedSerialization,
		MinVersion: Version{Major: 26, Minor: 4},
		Stability:  Experimental,
		AutoSelect: false,
		Doc:        "stamp map_serialization_version='with_buckets' on new logs/traces tables only, never metrics (server >= 26.4, opt-in only via CERBERUS_CH_OPTIMIZATIONS — read side is transparent, but full-map reads get ~2x slower, so auto never picks it)",
	},
	{
		ID:                         FeatureTSGridLastOverTime,
		MinVersion:                 Version{Major: 26, Minor: 6},
		Stability:                  Experimental,
		AutoSelect:                 false,
		RequiresExperimentalTSGrid: true,
		Doc:                        "opt eligible last_over_time(<v>[<range>]) shapes onto the native timeSeriesResampleToGridWithStaleness aggregate (ts_grid_resample's), [range] as staleness (experimental, server >= 26.6 — PRs #106504/#106577, opt-in via CERBERUS_CH_OPTIMIZATIONS)",
	},
	{
		ID:         FeatureColumnStatistics,
		MinVersion: Version{Major: 26, Minor: 3},
		Stability:  Experimental,
		AutoSelect: false,
		Doc:        "curated ADD STATISTICS registry on metrics/logs/traces filter+join columns for PREWHERE/join-ordering (server >= 26.3, opt-in via CERBERUS_CH_OPTIMIZATIONS — unsupported on ClickHouse Cloud, tolerated; auto never picks it pending real-world calibration)",
	},
	{
		ID:         FeatureClassicBucketMergeSumMap,
		MinVersion: AlwaysAvailable,
		Stability:  Experimental,
		AutoSelect: false,
		Doc:        "opt the classic-histogram-quantile cross-series merge SUM fold onto sumMap+arrayCumSum, retiring the groupArray + per-rung fold (no version floor, opt-in via CERBERUS_CH_OPTIMIZATIONS — heterogeneous bucket layouts diverge, #2817)",
	},
	{
		ID:         FeatureJoinSpill,
		MinVersion: Version{Major: 26, Minor: 4},
		Stability:  Experimental,
		AutoSelect: true,
		Doc:        "stamp max_bytes_before_external_join=cap/2 on join-bearing plans (server >= 26.4, auto-enabled) — mirrors the group_by/sort spill stamps; explicit, not the 26.5+ ratio default, silently ignored with no memory limit configured (cf. ClickHouse#76740)",
	},
	{
		ID:         FeatureArgAndMaxFusion,
		MinVersion: Version{Major: 25, Minor: 11},
		Stability:  Experimental,
		AutoSelect: true,
		Doc:        "fuse RangeLWR.SampleTimestamp's and vector_join's non-derived instant-mode argMax(Value, TimeUnix) + max(TimeUnix) pair into one argAndMax(Value, TimeUnix) (server >= 25.11, auto-enabled — tie-invariant, proven-equivalent substitution)",
	},
	{
		ID:                            FeatureResultCache,
		MinVersion:                    Version{Major: 24, Minor: 8},
		Stability:                     Stable,
		AutoSelect:                    true,
		RequiresResultCacheCapability: true,
		Doc:                           "stamp use_query_cache=1 + query_cache_ttl on cerberus-eligible fully-closed read paths (result cache, boot-probed knob availability, server >= 24.8)",
	},
	{
		ID:         FeatureLazyMaterialization,
		MinVersion: Version{Major: 25, Minor: 11},
		Stability:  Experimental,
		AutoSelect: true,
		Doc:        "stamp query_plan_optimize_lazy_materialization=1 + query_plan_max_limit_for_lazy_materialization=<request LIMIT> on Tempo's ORDER BY Timestamp DESC LIMIT N search shapes (server >= 25.11, auto-enabled — result-equivalent, chDB-verified)",
	},
}

// Registry returns a copy of the seeded feature registry
// (aggregation_in_order, condition_cache, ts_grid_range, ts_grid_resample,
// columnar_result_decode, ts_grid_changes, ts_grid_resets, ts_grid_deriv,
// ts_grid_predict_linear, ts_grid_recollapse, ts_grid_increase,
// ts_grid_histogram, quantile_prom_histogram, ts_grid_delta, ts_grid_irate,
// ts_grid_idelta, laginframe_adjacency, fixed_accumulator_extrapolated,
// sorted_slab_over_time, map_bucketed_serialization, ts_grid_last_over_time,
// column_statistics, classic_bucket_merge_summap, join_spill,
// arg_and_max_fusion, result_cache, lazy_materialization). The copy keeps
// the canonical entries immutable from the caller's side. Exposed so tests
// can enumerate the gates and the docs generator can render the table.
func Registry() []Feature {
	out := make([]Feature, len(registry))
	copy(out, registry)
	return out
}

// featureByID returns the registry entry for id, or ok=false when id is not a
// known feature (the typo-guard case the resolver turns into a fatal error).
func featureByID(id string) (Feature, bool) {
	for _, f := range registry {
		if f.ID == id {
			return f, true
		}
	}
	return Feature{}, false
}
