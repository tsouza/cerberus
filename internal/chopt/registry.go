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

	// FeatureTSGridInstant opts each of rate() / changes() / resets() /
	// deriv() / predict_linear()'s ALREADY-ENABLED native MATRIX strategy
	// (ts_grid_range / ts_grid_changes / ts_grid_resets / ts_grid_deriv /
	// ts_grid_predict_linear) to ALSO cover the INSTANT (single-anchor, Step
	// == 0) query shape — the alerting/recording-rule path, which today
	// always falls back to emitWindowedArrayExtrapolated's per-series
	// groupArray, unbounded on this axis for a wide lookback (`rate(m[1d])`,
	// `rate(m[30d])`; cerberus issue #2748). The emitter feeds the SAME
	// timeSeries*ToGrid aggregate a DEGENERATE one-point grid (start == end
	// == the query's single eval instant), so the flat-memory native
	// aggregation applies to the instant shape exactly as it already does to
	// the matrix one.
	//
	// It is a pure NARROWING of each function's own matrix feature, mirroring
	// FeatureTSGridRecollapse's own relationship to ts_grid_range: with a
	// function's matrix feature off there is no native strategy to extend, so
	// cmd/cerberus only consults ts_grid_instant INSIDE that function's own
	// "matrix feature enabled" branch (nativeRangeLowerers sets a
	// Native*Lowerer's Instant field to `optSet.Has(FeatureTSGridInstant)`
	// only when it is ALSO building that lowerer). This is deliberately a
	// SINGLE feature governing all five functions together — cerberus issue
	// #2748 scoped it that way — rather than five per-function siblings the
	// way changes/resets/deriv/predict_linear split out from ts_grid_range:
	// the instant arm's SQL shape and eligibility contract are identical
	// across all five (nativeTSGridInstantNode), so there is no per-function
	// divergence for a split to encode. changes() nonetheless keeps ITS OWN
	// #1721 carve-out intact: ts_grid_changes stays AutoSelect=false, and the
	// instant arm only fires when ts_grid_changes is ALSO present in
	// EnabledSet (the AND-composition above), so an operator who has not
	// opted into the buggy matrix changes() path never gets the identical
	// bug through the instant one either — the NaN-adjacent overcount lives
	// in timeSeriesChangesToGrid itself, the SAME aggregate the instant arm
	// reads.
	//
	// IMPORTANT — the floor is 26.5, HIGHER than the matrix family's shared
	// 25.9. A degenerate one-point (start == end) grid is exactly the
	// "extreme parameter" shape ClickHouse/ClickHouse#103223 (an overflow in
	// timeSeries*ToGrid's internal grid-index arithmetic for boundary
	// parameter combinations, fixed in 26.5) and #105319 (a staleness-window
	// overflow, backported through 25.8 and 26.3-26.5) both describe; a 25.9
	// floor — correct for the LEFT-OPEN WINDOW fix the matrix family rests
	// on — would auto-reach a server that still carries either overflow.
	// 26.5 is the first release verified to carry BOTH fixes. The boot
	// version probe resolves at (Major, Minor) granularity fine enough to
	// express this floor precisely (see chopt.FeatureTSGridLastOverTime's own
	// 26.6 floor for the identical mechanism), so there is no probe-precision
	// reason to fall back to a coarser number here.
	//
	// AutoSelect is false: 26.5 is a brand-new floor with no fielded
	// validation yet, the same conservative posture
	// FeatureTSGridLastOverTime's own 26.6 bump took — opt-in only via
	// CERBERUS_CH_OPTIMIZATIONS=ts_grid_instant until the floor earns auto
	// promotion. Shares the family's RequiresExperimentalTSGrid gate
	// (allow_experimental_time_series_aggregate_functions).
	//
	// increase() and delta() are OUT OF SCOPE for this feature even though
	// their own native matrix features (ts_grid_increase, ts_grid_delta)
	// already exist: cerberus issue #2748 explicitly defers their instant
	// coverage to a follow-up once each has separately earned it — a scope
	// decision, not a technical gap. No Native{Increase,Delta}Lowerer carries
	// an Instant field.
	FeatureTSGridInstant = "ts_grid_instant"

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

	// FeatureTSGridVectorAgg folds an element-wise-correct outer PromQL vector
	// aggregation (`sum`/`min`/`max`/`avg`/`count` by/without) directly into an
	// eligible native rate grid's own pre-explode per-series grid, via
	// ClickHouse's `-ForEach` combinator (sumForEach / minForEach / maxForEach
	// / avgForEach / countForEach), instead of exploding every per-series grid
	// to (series, anchor) rows FIRST and re-aggregating with a second,
	// blocking GROUP BY (cerberus issue #2763 — the second axis of the
	// ~33.5M-row hard cliff #2651 documents, internal/chsql/
	// range_bucket_grid_native_bound.go). Only the resulting
	// ALREADY-AGGREGATED per-OUTPUT-series grid is exploded, once, at the
	// very end — dropping the row count entering that explode from
	// (series x anchors) to (outSeries x anchors).
	//
	// It is a pure narrowing of FeatureTSGridRange, exactly the relationship
	// FeatureTSGridRecollapse has to it: with ts_grid_range off there is no
	// native per-series grid to fold an outer aggregation into, so
	// internal/promql only consults this feature (via RangeLowerers.VectorAgg)
	// inside code paths ts_grid_range has already activated. The registry
	// cannot express that dependency directly, so the two carry the same 25.9
	// floor and the same experimental gate and are therefore resolved
	// identically by a single probed capability verdict — the floor is
	// INHERITED from ts_grid_range, not independently derived: the `-ForEach`
	// combinator itself is AlwaysAvailable (no version floor of its own,
	// already proven in-tree by the classic-histogram bucket-array-sum path,
	// chplan.FnSumForEach), so nothing about the combine raises the floor
	// past whatever ts_grid_range already pins.
	//
	// It is a SINGLE feature governing all five outer aggregations together
	// (sum/min/max/avg/count), the same one-feature-many-functions shape
	// FeatureTSGridInstant uses for its own five range functions: the
	// eligibility contract and SQL restructuring are identical across all
	// five outer Fns (chplan.RangeWindowGridNativeVectorAgg), so there is no
	// per-Fn divergence a split into five siblings would encode. A non-
	// element-wise outer aggregation (stddev/stdvar/quantile/group/topk/
	// bottomk/count_values/limitk/limit_ratio) is OUT OF SCOPE by
	// construction — internal/promql's lowerAggregate only ever builds a
	// RangeWindowGridNativeVectorAgg for the five Fns this feature covers,
	// never a broader dispatch this registry entry would have to narrow
	// further.
	//
	// This SAME flag additionally governs a narrower composition (cerberus
	// issue #2852): rate()/increase() against a schema with a per-row
	// AggregationTemporality column always lower to a temporality-split
	// UnionAll{RangeWindowGridNative, RangeWindow} (issue #2843), never a
	// bare RangeWindowGridNative, so the plain branch above never fires for
	// them. internal/promql's lowerAggregate recognizes that UnionAll shape
	// too and folds the outer aggregation into just its CUMULATIVE
	// RangeWindowGridNative arm — but ONLY for sum/min/max there, not the
	// full five-Fn set: avg/count cannot correctly re-combine the native
	// arm's single already-reduced row with the DELTA arm's raw per-series
	// rows by re-applying the same Fn (see internal/promql's
	// nativeGridVectorAggUnionFns for the full argument). This does not
	// split the flag in two — it is the same boot-resolved verdict, still
	// consulted through the same RangeLowerers.VectorAgg field, just with a
	// per-shape Fn-set narrowing lowerAggregate itself applies.
	//
	// AutoSelect is false: unlike FeatureTSGridRecollapse (whose merge
	// exactness was checked across time-disjoint, interleaved, and
	// counter-reset-straddling regimes before shipping auto-on), this is a
	// brand-new code path with no fielded validation yet — the same
	// conservative posture FeatureTSGridInstant's own new-floor bump took.
	// Opt-in only via CERBERUS_CH_OPTIMIZATIONS=ts_grid_vector_agg until it
	// earns auto promotion. Shares the family's RequiresExperimentalTSGrid
	// gate (allow_experimental_time_series_aggregate_functions) because it
	// only ever narrows an already-experimental ts_grid_range node.
	FeatureTSGridVectorAgg = "ts_grid_vector_agg"

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
	//     rule is order-dependent for a real-vs-NaN duplicate pair, the
	//     family-wide gap cerberus tracks at
	//     https://github.com/tsouza/cerberus/issues/2798. Re-measured
	//     against a real ClickHouse at this feature's own 25.9 floor
	//     (internal/chsql's
	//     TestTSGridFamily_NaNDuplicateSurvivorIsOrderDependent_RealCH),
	//     irate INVERTS the whole-window members' direction: the finite
	//     sample survives when the NaN reaches the fold first, the NaN
	//     survives when it reaches the fold second. Unlike rate/delta,
	//     irate reduces every window to its trailing pair, so a
	//     duplicate-timestamp trailing pair is not a rare edge of a summed
	//     window but the whole answer.
	//   - The array-fold fan-out this feature displaces does NOT share that
	//     exposure, and the asymmetry is real rather than a wash: the
	//     pairs-shaped fan-out carries no dedup layer for a duplicate-ts
	//     trailing pair, but arraySort orders Float64 totally with NaN
	//     ranked greatest, so the pair it selects is a function of the
	//     sample multiset alone. Switching irate to the native aggregate
	//     therefore trades a deterministic answer for a scan-order-dependent
	//     one on that shape. internal/chsql.dedupWindowPairsByTsFrag's doc
	//     states the rule and names the tests that execute both sides.
	//
	// AutoSelect is true, and the reason is exposure rather than harmlessness.
	// The shape needed to reach the divergence is doubly degenerate — two
	// samples at one series' exact timestamp, one of them NaN — and it is
	// the FAMILY's shape, shared verbatim by the already-auto-selected
	// rate/increase/resets/deriv/predict_linear/delta siblings; nothing about
	// irate makes it likelier or worse there. The two family members that DO
	// carry AutoSelect: false carry it for divergences ordinary data reaches:
	// FeatureTSGridChanges diverges on any NaN-adjacent window with no
	// duplicate involved at all (#1721), and FeatureTSGridGroupArray is a
	// pure SQL-shape swap whose only effect would be to import this same
	// nondeterminism into paths that today do not have it (#2749). Treating
	// irate differently from its siblings would single out the member whose
	// evidence is strongest, not the risk.
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
	// doc, "Temporality-bearing counters"). This includes the EXACT,
	// retention-independent DELTA-prefix aggregate mechanism (issue #2389,
	// RangeWindow.DeltaPrefixAggregateInput != nil) — cerberus issue #2797
	// closed the gap that used to exclude that narrower, opt-in-only
	// population.
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

	// FeatureTSGridGroupArray swaps the fan-out window-assembly idiom's
	// `groupArray((ts,value))` + `arraySort` (+, at four sites, the
	// `arrayReverse`/`arrayCompact`/`arrayReverse` last-of-run dedup triple,
	// dedupWindowPairsByTsFrag) for the native
	// `timeSeriesGroupArray(ts, value)` aggregate, which sorts AND collapses
	// duplicate timestamps in one pass (cerberus issue #2749). Scoped to the
	// three plain (non-split-window) sites that already carry the dedup
	// triple: emitWindowedArrayExtrapolated's instant-path per-series array,
	// emitWindowedArrayExtrapolatedMatrix's non-temporality-split regroup
	// branch, and the fused multi-anchor slab (range_window_fused.go).
	// Downstream Frag towers (window_pairs, window_vals, counter_delta, …)
	// are unchanged either way.
	//
	// Explicitly OUT of scope, verified rather than assumed:
	//   - The split-window groupArrayIf sites (range_window.go's
	//     needsDeltaFirstLevel branch) and deltaPrefixSumFrag, which
	//     exclusively consumes that branch's output: a plain
	//     timeSeriesGroupArrayIf(ts, value, cond) DOES work (measured against
	//     a real 26.6.3 server, contradicting the "no -If variant" premise),
	//     but these sites are deliberately left on the hand-rolled idiom to
	//     keep this cut to the four sites the dedup triple already touches —
	//     tracked as a follow-up, https://github.com/tsouza/cerberus/issues/2862.
	//   - The fused multi-arm variant tower (groupArrayVariantTupleFrag):
	//     genuinely inexpressible, not just out of scope — it carries N
	//     distinct value columns per sample in one tuple array, while
	//     timeSeriesGroupArray is a fixed binary (timestamp, value)
	//     aggregate (confirmed against system.functions and by direct call).
	//   - Every site that sorts WITHOUT already deduping today (the
	//     `*_over_time` array path, holt_winters, subquery interiors, the
	//     pairs shapes irate/deriv/predict_linear/timestamp): OTel/ClickHouse
	//     ingestion can genuinely write two rows with the same (Attributes,
	//     TimeUnix) (dedupWindowPairsByTsFrag's own doc, the reason the four
	//     dedup sites exist at all), so swapping any of these would SILENTLY
	//     introduce duplicate-timestamp collapsing where the fan-out
	//     currently sums/counts/tie-breaks every duplicate — a behavior
	//     change this issue has no evidence to justify site-by-site.
	//
	// Timestamp-type precondition (measured against a real ClickHouse 26.6.3
	// server): timeSeriesGroupArray(DateTime64(9), Float64) is accepted
	// DIRECTLY and LOSSLESSLY, full nanosecond precision round-tripping, the
	// result typed exactly Array(Tuple(DateTime64(9), Float64)) — byte-
	// identical to window_pairs' existing element type — contradicting the
	// function's own documented UInt32/DateTime/UInt64 signature. No
	// toUnixTimestamp64Nano wrap-and-arrayMap-back pass is needed (and none
	// is implemented): UInt64 was tried too, for comparison, and is REJECTED
	// outright (ILLEGAL_TYPE_OF_ARGUMENT), so the documented UInt64
	// alternative is not even reachable as a fallback shape.
	//
	// NaN precondition (measured, the reason AutoSelect is false): the
	// existing dedupWindowPairsByTsFrag idiom is deterministic on a
	// duplicate-timestamp NaN — arraySort ranks NaN greatest, so it always
	// survives the last-of-run keep, independent of insertion order (both
	// orderings verified). timeSeriesGroupArray's own duplicate-timestamp
	// collapse is a running "replace current-best only when candidate >
	// current-best" fold: for finite values this converges to the same true
	// max regardless of insertion order (also verified both orderings), but
	// IEEE754 makes every comparison against NaN false, so a NaN landing
	// FIRST at a duplicate timestamp can never be replaced, and a NaN
	// landing after any non-NaN can never displace it — the surviving value
	// depends on which row a (possibly multi-threaded, multi-part) scan
	// visits first. That is not just a divergence from the fan-out's own
	// NaN-always-wins rule, it is NON-DETERMINISTIC. This codebase already
	// forced ts_grid_changes into AutoSelect: false for an analogous
	// NaN-adjacent native/fan-out divergence (#1721); this feature follows
	// the identical posture rather than risk a query whose answer can flip
	// between two runs of the same data.
	//
	// Shares the timeSeries*ToGrid family's registry gate
	// (allow_experimental_time_series_aggregate_functions) and 25.9 floor —
	// timeSeriesGroupArray itself shipped in 25.8, but the family's single
	// capability probe only ever ran against the left-open/right-closed
	// staleness-window fix (PR #86588, landed 25.9); pinning this feature to
	// 25.8 would let it auto-fire on a probe verdict that never actually
	// exercised it, the same reasoning ts_grid_deriv / ts_grid_predict_linear
	// give for their own 25.9 floor despite shipping in 25.8.
	FeatureTSGridGroupArray = "ts_grid_group_array"

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

	// FeatureDownsampleTier opts eligible LONG-RANGE / low-resolution
	// irate() / idelta() / last_over_time() query_range shapes (step >= the
	// tier's fixed 5-minute bucket, range a positive integer multiple of the
	// bucket, grid aligned to the bucket boundary — see
	// internal/promql/lower_strategy.go's eligibility check) onto the
	// operator-provisioned downsampled long-range tier
	// (schema.DownsampleTierTable): TWO independent materialized views
	// (internal/schema/ddl's renderDownsampleTierView from the Sum table,
	// renderDownsampleTierGaugeView from the Gauge table — cerberus issue
	// #2858) folding raw samples into a SHARED persisted
	// timeSeriesLastTwoSamples aggregate state per bucket, read back via
	// timeSeriesLastTwoSamplesMerge + finalizeAggregation instead of
	// scanning full-resolution raw rows (cerberus issue #2751; a range
	// spanning several buckets merges them and re-filters to the exact
	// window — issue #2857). The Gauge-sourced MV feeds last_over_time()
	// ONLY — a gauge has no counter-reset semantics for irate()/idelta(),
	// see internal/promql/lower.go's attachDownsampleTierArm — and is
	// eligible only over an UNAMBIGUOUS Gauge-table metric-name resolution,
	// the same restriction an unsuffixed COUNTER metric already has for
	// irate()/idelta() (schema.Metrics.TablesForUnknownName's own doc).
	//
	// THIS IS A FUNDAMENTALLY DIFFERENT MECHANISM from the rest of the
	// timeSeries*ToGrid family above: those are stateless read-time
	// function swaps over the SAME raw table; this reads an
	// operator-provisioned, PERSISTED table that must be created
	// (internal/schema/ddl, gated on this same verdict — see
	// schema.DownsampleTierTable's doc) and, for history predating its own
	// MV, separately backfilled (`cerberus schema downsample-tier-backfill`,
	// internal/downsampletier) before it answers anything for that older
	// range.
	//
	// HARD SCOPE BOUNDARY (cerberus issue #2751's own "Scope constraint",
	// non-negotiable — verified empirically against a real ClickHouse
	// instance, see internal/chsql's downsample-tier chDB test): retaining
	// only the two newest raw samples per bucket makes a counter reset
	// between samples the bucket DISCARDS undetectable. rate() / increase()
	// / delta() over that state can silently underestimate an unbounded
	// amount — a cerberus-COMPOSED failure mode, not documented ClickHouse
	// behavior — so this feature's lowering (internal/promql/
	// lower_strategy.go's DownsampleTier{Irate,Idelta,LastOverTime}Lowerer)
	// is wired ONLY for irate() / idelta() / last_over_time(): PromQL
	// defines all three as functions of EXACTLY the last two (or one, for
	// last_over_time) samples in the window, so a state that retains
	// exactly those two samples is exact by construction — verified
	// empirically, including a counter reset landing ON the retained
	// trailing pair (irate correctly reset-corrects; idelta correctly does
	// not, matching Prometheus's funcIrate / funcIdelta). rate() /
	// increase() / delta() have and will have NO DownsampleTier lowering —
	// there is no Fallback wrapper for them the way NativeRateLowerer wraps
	// FanoutRateLowerer for ts_grid_range; they are structurally absent
	// from cmd/cerberus's nativeRangeLowerers wiring for this feature.
	//
	// Floor 25.9, the SAME floor and the SAME
	// allow_experimental_time_series_aggregate_functions gate
	// (RequiresExperimentalTSGrid) the rest of the family shares — the
	// function itself (timeSeriesLastTwoSamples, its -State/-Merge
	// combinators, and finalizeAggregation over its state) is available
	// from 25.6, but this repo pins every timeSeries*ToGrid-family
	// consumer to the family's own 25.9 floor rather than each function's
	// individual introduction version (see FeatureTSGridRange's own doc):
	// this feature shares the family's boot capability probe and its
	// experimental-setting gate, so it can never actually be enabled below
	// 25.9 regardless.
	//
	// AutoSelect is false, unlike most of the family: this is a genuine
	// operator decision (new ongoing storage/compute cost, a PERSISTED
	// EXPERIMENTAL aggregate state in an AggregatingMergeTree that a
	// ClickHouse upgrade changing the state's wire format could strand —
	// see the issue's own "Risks" section) with no safe-by-default
	// posture, never AutoSelect where a raw read already fits budget.
	// Opt-in only via CERBERUS_CH_OPTIMIZATIONS=downsample_tier.
	FeatureDownsampleTier = "downsample_tier"

	// FeatureExplainEstimate gates using ClickHouse's own `EXPLAIN ESTIMATE`
	// (cerberus issue #2787) as an ADVISORY input to the solver's fan-out
	// factor K and to the per-rung admission learner's priors — a NO-EXECUTION
	// scan estimator (parts / marks / rows after index analysis, available
	// since ClickHouse 21.9) run once per distinct plan SHAPE for a
	// ModeAuto-eligible candidate, never on every request (internal/engine's
	// explain_estimate_wiring.go caches the result per shape and skips the
	// round trip entirely once the route memo or the per-rung admission
	// learner already holds a verdict for that shape — the exact "no new live
	// round-trip on every per-rung request" constraint per_rung_admission.go
	// itself was built to avoid).
	//
	// VERSION FLOOR: registered AlwaysAvailable, purely for rollout / kill
	// switch — not a real version gate. EXPLAIN ESTIMATE has been part of
	// ClickHouse's EXPLAIN grammar since 21.9, well below cerberus's own 24.8
	// floor (docs/toolchain.md), so every server this codebase supports
	// already answers it.
	//
	// NOT auto-selected (AutoSelect=false), mirroring FeatureColumnStatistics'
	// own posture: the signal is advisory (never a correctness gate — see
	// planner.go's own doc on how it clamps K) and its real-world value on
	// cerberus's own production query mix is a calibration question this
	// feature alone cannot answer, so enabling it is a deliberate operator
	// choice pending that evidence, not a version-gated pure win.
	FeatureExplainEstimate = "explain_estimate"

	// FeatureCardinalityProbe gates a SECOND, independent advisory pre-flight
	// (cerberus issue #2788, extended to more carrier kinds and an uncapped
	// reading by issue #2840) that complements FeatureExplainEstimate: a
	// bounded REAL aggregate — `count()`, `uniqUpTo(100)(...)`, and
	// `uniqCombined64(...)` — over a ModeAuto candidate's already-pruned
	// scan window, answering the distinct-series fan-out question EXPLAIN
	// ESTIMATE's marks-level upper bound cannot. Recognises five
	// [chplan.GridCarrier] kinds (RangeWindow, RangeWindowGridNative,
	// RangeBucketFanout, RangeBucketGridNative, RangeLWR) — see
	// internal/engine/cardinality_probe_wiring.go's own top-level doc for
	// the full carrier-kind list and its scope-narrowing rationale. Gated
	// identically to FeatureExplainEstimate: once
	// per distinct (plan shape, metric) pair for a candidate whose baseline
	// classification reached the cost-grid section, never on every request
	// (internal/engine's cardinality_probe_wiring.go — same round-trip
	// constraint per_rung_admission.go's own doc requires, and the same
	// route-memo / per-rung-admission skip narrowing
	// explain_estimate_wiring.go established first).
	//
	// VERSION FLOOR: registered AlwaysAvailable — uniqUpTo / uniqCombined are
	// old, universally-available ClickHouse aggregate functions (unlike
	// EXPLAIN ESTIMATE, this is not even a metadata-analysis feature; it is
	// an ordinary bounded aggregate query), so there is no real version gate
	// to express.
	//
	// NOT auto-selected (AutoSelect=false), same posture and same reasoning
	// as FeatureExplainEstimate: advisory only, real-world value on
	// cerberus's own production query mix is a calibration question pending
	// operator opt-in evidence — and unlike EXPLAIN ESTIMATE this probe DOES
	// execute against real data, so enabling it by default would add
	// unmeasured query volume without an operator's explicit choice.
	FeatureCardinalityProbe = "cardinality_probe"

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
	// to the fan-out" bar — but no longer because the two constructions
	// disagree. They agreed only for HOMOGENEOUS groups when this feature
	// first shipped: a chDB differential probe found that sumMap over
	// per-BUCKET counts followed by arrayCumSum along the union sums, at each
	// union bound u, every row's own sub-cumulative count over its OWN
	// buckets <= u, while reference Prometheus's sum by(le) — and the
	// has-filter fold that reproduces it — sums only the rows whose OWN
	// layout contains u. Cerberus issue #2817 closed that at the source: each
	// ROW now cumulates over its own buckets BEFORE the key-wise sumMap, so
	// the merged rung is "sum over the rows carrying u of that row's
	// cumulative count at u" — the fold's own definition, for any mix of
	// layouts — and both ladders go through the same monotonic repair. The
	// equivalence is pinned by a row-level chDB differential against the
	// fold's `b <= u` reading, by end-to-end differentials over heterogeneous
	// and partially-overlapping layouts, and by a Prometheus-parity-enrolled
	// spec fixture on a heterogeneous seed.
	//
	// What keeps AutoSelect false is now a MEASUREMENT question, tracked by
	// https://github.com/tsouza/cerberus/issues/2923: this feature's own
	// ~50x figure is an estimate taken against the superseded construction,
	// and [maxClassicBucketMergeCostUnits]'s guard — calibrated against the
	// fold's per-rung rescan — over-rejects a path whose cost is linear in
	// total bucket volume. Auto-selecting before both are settled would move
	// the default path onto an unmeasured cost model and a ceiling that does
	// not describe it. Until then the feature is reachable by explicit
	// CERBERUS_CH_OPTIMIZATIONS=classic_bucket_merge_summap listing.
	//
	// The NaN asymmetry #2756 documented as a second, accepted risk is gone
	// with the same change: arrayCumSum now runs over ONE ROW's own buckets,
	// the same reach the fold's arraySum gives a NaN, and the repair layer
	// both paths share answers a poisoned rung identically. See
	// internal/promql/classic_bucket_merge_summap.go's header.
	FeatureClassicBucketMergeSumMap = "classic_bucket_merge_summap"

	// FeatureTraceIDProjection gates the curated `ADD PROJECTION IF NOT
	// EXISTS proj_trace_id (SELECT TraceId, _part_offset ORDER BY TraceId)`
	// ALTER (cerberus issue #2767) on the otel_traces and otel_logs tables.
	// Neither table's own ORDER BY carries TraceId — otel_traces sorts
	// (ServiceName, SpanName, Timestamp) and otel_logs sorts
	// (toStartOfFiveMinutes(Timestamp), ServiceName, Timestamp) — so a
	// trace-by-id lookup or a logs<->traces correlation hop has no primary-key
	// locality on either side; today it is served only by the idx_trace_id
	// bloom_filter skip index (trace_id_index_probe_chdb_test.go's bar),
	// which grants probabilistic GRANULARITY-coarse pruning, not exact row
	// addressing. proj_trace_id stores only the sort key plus the
	// _part_offset pointer back into the base part — a lightweight
	// "secondary index" projection, not a full column copy — so ClickHouse
	// can resolve a TraceId predicate to exact rows once the query optimizer
	// (>= 25.11, see FeatureTraceIDBitmapFilter) or an explicit query targets
	// it.
	//
	// VERSION FLOOR: 25.5 — the first release accepting `_part_offset` inside
	// a normal projection's SELECT list (upstream PR #78429, "Projection
	// Index Step 1: Support _part_offset in normal projections"); ADD
	// PROJECTION with this shape is rejected outright below it. cerberus's
	// supported floor is 24.8 (versions.yaml), so unlike every projection in
	// metricCatalogProjections (unconditional today) this ALTER needs the
	// version-conditional DDL gate internal/schema/ddl.Config.
	// TraceIDProjectionEnabled threads — the boot resolver here supplies the
	// verdict the SAME way it already does for FeatureColumnStatistics
	// (internal/schemaboot.DDLConfig, cmd/cerberus's chOptResolution),
	// reusing the SAME probed-version detection rather than adding a second
	// one.
	//
	// BOTH otel_traces and otel_logs carry the projection, not traces alone:
	// the issue's own motivation names logs<->traces correlation, not just
	// trace-by-id, and TraceIDIndexProbe (trace_id_index_probe_chdb_test.go)
	// already requires BOTH sides index-served for Consistent()==true —
	// otel_logs' ORDER BY has no more TraceId locality than otel_traces'
	// does, so scoping the projection to traces alone would leave the logs
	// side of every correlation hop on the bloom filter.
	//
	// NOT auto-selected (AutoSelect=false), mirroring FeatureColumnStatistics'
	// posture on a fresh floor-raising DDL feature: the MATERIALIZE
	// PROJECTION backfill cost for existing parts and the steady-state
	// merge-time maintenance cost on new parts are real (if the issue's own
	// Risks section correctly calls them "tiny") but unmeasured at production
	// data volumes beyond this PR's own synthetic benchmark — an operator
	// opt-in via CERBERUS_CH_OPTIMIZATIONS=trace_id_projection until that
	// evidence accumulates, exactly the posture column_statistics and
	// map_bucketed_serialization already took on their own new floors.
	FeatureTraceIDProjection = "trace_id_projection"

	// FeatureLokiCatalogMV gates the curated `CREATE MATERIALIZED VIEW ...
	// REFRESH EVERY 5 MINUTE ... TO loki_label_catalog AS SELECT ...` DDL
	// (cerberus issue #2770) that maintains a small per-label-key
	// cardinality catalog table on top of the logs table, refreshed on a
	// schedule instead of computed per request.
	// `/loki/api/v1/detected_labels` (internal/api/loki/detected_labels.go)
	// serves from the catalog when eligible — a request that carries no
	// LogQL selector, i.e. a datasource-open probe — and falls back to its
	// existing per-request server-side GROUP BY otherwise; the fallback
	// path is untouched and permanent, not a transitional shim. A selector
	// is left on the fallback path deliberately: the catalog is unkeyed by
	// stream, so a selector-scoped request (Grafana Logs Drilldown's
	// per-service view) cannot be answered from it without either
	// service-keying the catalog or narrowing eligibility further — the
	// issue calls for the SIMPLER, more conservative rule, which this is.
	//
	// VERSION FLOOR: 24.10 — upstream PR #70550 ("Refreshable materialized
	// views: minor fixes + GA") removed the
	// `allow_experimental_refreshable_materialized_view` flag requirement
	// in the 24.10 release, making `CREATE MATERIALIZED VIEW ... REFRESH
	// EVERY ...` a plain, unflagged DDL statement. Below 24.10 the CREATE is
	// rejected outright (or requires the experimental flag cerberus never
	// sets), so — like FeatureTraceIDProjection and FeatureColumnStatistics
	// on their own newer floors — this needs the version-conditional DDL
	// gate in internal/schema/ddl.Config (LokiLabelCatalogEnabled) rather
	// than rendering unconditionally.
	//
	// The catalog is DELIBERATELY a full-window aggregate (last 24h,
	// bounding the per-refresh scan cost), computed server-side over every
	// row in that window — NOT a sampled peek. Its cardinality estimates
	// therefore do NOT reproduce the peek-based path's numbers bit-for-bit,
	// which is intentional: unlike every OTHER estimate this endpoint
	// family emits (deliberately matched to upstream Loki's peek-based HLL
	// sketches so the compat harness diffs clean), the catalog path is a
	// different computation over a different, larger data window by
	// design — exempted from that parity requirement, not merely
	// undertested against it.
	//
	// NOT auto-selected (AutoSelect=false), mirroring FeatureTraceIDProjection's
	// posture on its own fresh floor-raising DDL feature: the refresh's
	// steady-state scan cost against a real 24h window of production log
	// volume is unmeasured beyond this feature's own synthetic benchmark, so
	// enabling it is an operator opt-in via
	// CERBERUS_CH_OPTIMIZATIONS=loki_catalog_mv pending that evidence.
	FeatureLokiCatalogMV = "loki_catalog_mv"

	// FeatureTempoTagCatalogMV gates the curated `CREATE MATERIALIZED VIEW
	// ... REFRESH EVERY 5 MINUTE ... TO tempo_tag_catalog AS SELECT ...`
	// DDL (cerberus issue #2771, the Tempo sibling of FeatureLokiCatalogMV
	// above) that maintains a per-(scope, tag-key) top-values catalog over
	// the traces table's resource / span / event / link attribute
	// families, refreshed on a schedule instead of scanned per request.
	// `/api/v2/search/tags` and `/api/search/tag/{name}/values`
	// (internal/api/tempo/search_tags.go, search_tag_values.go) serve from
	// the catalog when eligible — see internal/api/tempo/tag_catalog.go's
	// eligibility rule — and fall back to the existing live attribute-map
	// scan otherwise; the fallback path is untouched and permanent, exactly
	// as FeatureLokiCatalogMV's own doc describes for its sibling.
	//
	// The catalog covers resource, span, event, and link scopes — the
	// event/link (Nested Array(Map)) arms were added by cerberus issue
	// #2850 after measuring their refresh cost against the same traces
	// table (see the DDL render function's own SCOPE COVERAGE doc for the
	// numbers); only instrumentation scope stays on the live path
	// unconditionally, because the upstream schema carries no such column
	// by default. Filtered tag/tag-value lookups (the V2 `q=<TraceQL>`
	// narrowing parameter) also stay on the live path unconditionally: the
	// catalog has no way to answer "values on spans matching this
	// predicate" without evaluating the predicate per row, which is
	// exactly the scan this feature exists to avoid.
	//
	// VERSION FLOOR: 24.10 — same upstream PR #70550 floor
	// FeatureLokiCatalogMV cites; see that constant's doc for the
	// evidence, which applies identically here (same REFRESH EVERY DDL
	// shape, same server-side gate).
	//
	// NOT auto-selected (AutoSelect=false), mirroring FeatureLokiCatalogMV's
	// posture: the refresh's steady-state scan cost against a real traces
	// table is unmeasured beyond this feature's own synthetic benchmark, so
	// enabling it is an operator opt-in via
	// CERBERUS_CH_OPTIMIZATIONS=tempo_tag_catalog_mv pending that evidence.
	FeatureTempoTagCatalogMV = "tempo_tag_catalog_mv"

	// FeatureExpHistogramMergeSumMap opts the across-series exponential
	// (native) histogram merge stage — both of its call sites,
	// histogram_native_sum.go's expHistogramGroupMergedInstant (instant
	// mode) and lowerExpHistogramSumOrAvgRange (range/query_range mode,
	// cerberus issue #3027) — onto a two-pass sumMap-keyed reshape, see
	// internal/promql/exp_histogram_merge_summap.go. Every shape is
	// eligible: instant or range mode, any by()/without() grouping
	// (cerberus issue #2865), SUM or AVG fold (cerberus issue #2866) —
	// see promql.NativeExpHistogramMergeLowerer's own doc.
	//
	// sumMap is old, always-available ClickHouse functionality — no
	// version floor to probe, matching FeatureClassicBucketMergeSumMap's
	// own posture.
	//
	// AutoSelect is false. Real ClickHouse 26.6 measurements taken for
	// issue #2757, against the actual emitted SQL, found this design wins
	// 13-43x on memory at realistic OTel-SDK-default bucket width (~160)
	// once row count reaches the hundreds-to-thousands range — but is
	// roughly PARITY at a single series and costs MORE memory than the
	// existing fold for a SINGLE series carrying an unusually wide
	// individual bucket layout (width 1,280 and up), because the new
	// design's own reconstruction step is width^2 in the worst case,
	// independent of row count (see exp_histogram_merge_summap.go's
	// header for the full measured table). Its own budget guard is now
	// real-ClickHouse-calibrated per shape — single-group, multi-group
	// instant, and multi-group range mode each have their OWN ceiling
	// (exp_histogram_merge_summap_bound.go) — but the proven single-series-
	// wide-layout regression above is a real, permanent property of this
	// design, not a calibration gap, so this feature stays reachable only
	// by explicit CERBERUS_CH_OPTIMIZATIONS=exp_histogram_merge_summap
	// listing, mirroring FeatureClassicBucketMergeSumMap's own posture for
	// a feature with a proven, real regression on a specific input shape.
	FeatureExpHistogramMergeSumMap = "exp_histogram_merge_summap"

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

	// FeatureTraceIDBitmapFilter stamps
	// min_table_rows_to_use_projection_index=0 (internal/engine's
	// settingMinTableRowsToUseProjectionIndex) on a query plan carrying a
	// TraceId-keyed predicate or join (cerberus issue #2767) — a top-level
	// equality (the Tempo trace-by-id GET), a flat or subquery membership
	// test (the /api/search root-lookup and structure-tab top-N gate), or a
	// chplan.StructuralJoin's recursive closure (every TraceQL structural
	// query). See internal/engine.eligibleForTraceIDBitmapFilter for the
	// exact plan shapes matched.
	//
	// VERSION FLOOR: 25.11 — upstream PR #81021 ("Projection Index Step 3:
	// ... allow using projections (that use SELECT of _part_offset and a
	// different ORDER BY) as a secondary index. When enabled, certain query
	// predicates can be used to read from projection parts and generate
	// bitmaps to filter rows efficiently during the PREWHERE stage"), the
	// changelog's own "when enabled" phrasing. The gate the changelog names
	// is NOT a single boolean toggle — verified against the PR's own diff of
	// src/Core/Settings.cpp / SettingsChangesHistory.cpp, not assumed from
	// the changelog prose — but the pair of UInt64 thresholds the same PR
	// introduces alongside it: min_table_rows_to_use_projection_index (only
	// consider the projection index once the scanned table clears this row
	// count; default 1,000,000) and max_projection_rows_to_use_projection_index
	// (only apply it once the estimated projection read is under this row
	// count; default 1,000,000, left untouched — a TraceId point lookup's
	// estimated projection read is a handful of rows regardless of table
	// size, so the default never blocks it). A real production otel_traces/
	// otel_logs table clears the row-count default on its own, but a small
	// or synthetic table — every chdb-tagged test fixture included — does
	// not, which would silently leave the bitmap path unreachable exactly
	// where this issue's own EXPLAIN acceptance test needs to prove it fires;
	// stamping the threshold to 0 makes the path deterministic regardless of
	// table size rather than relying on production scale to clear a default
	// that was never meant to gate correctness.
	//
	// AutoSelect is true: the stamp is RESULT-EQUIVALENT (it only widens
	// which physical path the optimizer is allowed to consider; a table
	// carrying no proj_trace_id projection at all is unaffected either way)
	// and strictly protective in the same sense FeatureJoinSpill and
	// FeatureArgAndMaxFusion are — there is no server on which lowering this
	// threshold produces a different query result, only a different (never
	// worse, per ClickHouse's own cost-based projection selection) physical
	// plan. Independent of FeatureTraceIDProjection: a server can satisfy
	// this floor (25.11) without satisfying that one (25.5) only if it
	// regressed version, which preflight already rejects, and the stamp is a
	// harmless no-op on any table that carries no proj_trace_id projection.
	FeatureTraceIDBitmapFilter = "trace_id_bitmap_filter"

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
	// The two guards are INDEPENDENT and both required, which cerberus issue
	// #2895 established the hard way. cerberus's closed-window gate decides
	// which windows are stale-safe to cache; ClickHouse decides which
	// STATEMENTS its cache can vouch for, and it vetoes any query containing a
	// function it classifies as non-deterministic. Those are different
	// questions, so an eligible window is NOT by itself a cacheable statement:
	// arrayJoin — how every non-native range lowering fans samples across the
	// step grid — is one of the functions ClickHouse vetoes. Treating its
	// query_cache_nondeterministic_function_handling as mere defense in depth
	// was the error: its server default is `throw`, so the veto FAILED those
	// queries rather than merely leaving them uncached, until the stamp began
	// co-sending `ignore` (see chclient.WithResultCacheSetting).
	//
	// AutoSelect is true: with the veto costing a cache miss instead of the
	// query, a query BOTH gates admit is safe to cache on any capable server
	// and one either gate declines simply runs uncached, so there is no
	// operator tradeoff to weigh. A deployment that wants the result cache off
	// entirely omits it from an explicit CERBERUS_CH_OPTIMIZATIONS list (or
	// sets "off"), exactly the opt-out condition_cache and every other
	// AutoSelect feature already give an operator — no separate dedicated flag
	// is needed.
	FeatureResultCache = "result_cache"

	// FeatureLazyMaterialization stamps query_plan_optimize_lazy_materialization=1
	// + query_plan_max_limit_for_lazy_materialization=<the query's own LIMIT> on
	// any plan carrying an `ORDER BY Timestamp DESC LIMIT N` (or ASC) shape — see
	// internal/engine.EligibleForLazyMaterialization, which is head-agnostic (it
	// matches the chplan shape, not the query language). Tempo's handler.go
	// builds it directly for /search/recent, boundNewestTraces, and
	// structural_two_phase.go's phase-A ranking; since cerberus issue #2829,
	// Loki's LogQL lowering (internal/logql/lower.go's maybePushLogLineLimit)
	// also emits it for the request `limit`, for every log-line pipeline shape
	// proven incapable of dropping a row in Go after SQL executes
	// (pipelineCanDropRowsInGo).
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

	// FeatureFullTextIndex flips internal/schema/ddl.Config.TextIndexEnabled,
	// which swaps the `idx_lower_body` skip index on the logs table's CREATE
	// branch from `tokenbf_v1(32768, 3, 0)` to `TYPE text(tokenizer =
	// 'splitByNonAlpha')` — the SAME index name, over the SAME `lower(Body)`
	// expression, in the SAME upstream-template mutually-exclusive branch
	// HasFullTextSearch already selects (cerberus issue #2773). On an
	// EXISTING table (which already carries the tokenbf branch from an
	// earlier boot), the DDL apply path additionally installs a
	// SEPARATELY-named `idx_body_text` text index via idempotent `ADD INDEX
	// IF NOT EXISTS` — see renderAddBodyTextIndex's doc comment for why a
	// second name, not an in-place type swap, is the only additive
	// (crash-safe, no MATERIALIZE-losing DROP) upgrade path available to a
	// render-time-only DDL layer with no live system.data_skipping_indexes
	// read.
	//
	// VERSION FLOOR: 26.2 — the release the `enable_full_text_index` setting
	// (gating table-level acceptance of `TYPE text(...)`) flips from
	// default-OFF (26.1: DB::Exception SUPPORT_IS_DISABLED unless a caller
	// stamps the setting itself) to default-ON, i.e. the text index's actual
	// GA floor, not merely its earliest experimental availability. Verified
	// directly against a probed 26.1.12 vs 26.2.19 pair (`SELECT value,
	// changed FROM system.settings WHERE name = 'enable_full_text_index'`:
	// 26.1 reports "0", 26.2 reports "1"), not assumed from the changelog
	// prose.
	//
	// NOT auto-selected (AutoSelect=false), mirroring FeatureColumnStatistics
	// and FeatureTraceIDProjection's posture on their own new floors: the
	// MATERIALIZE INDEX backfill cost for existing parts and the steady-state
	// index-maintenance cost on new parts (a second body index alongside the
	// untouched legacy tokenbf one — see the doc comment above on why this
	// PR does not retire it) are real but unmeasured at production data
	// volumes beyond this PR's own synthetic benchmark — an operator opt-in
	// via CERBERUS_CH_OPTIMIZATIONS=full_text_index until that evidence
	// accumulates.
	FeatureFullTextIndex = "full_text_index"

	// FeatureTextIndexLineFilter opts internal/chsql's LogQL line-filter
	// emitter (exprLineContent) onto an ANDed per-token `lower(Body) LIKE
	// '%tok%'` prefilter ahead of the UNCHANGED, always-kept
	// `position(Body, ?) > 0` / `match(Body, ?)` row predicate (cerberus
	// issue #2773) — a STRICT-SUPERSET skip-index hint, never a replacement:
	// every token substring of a literal that is itself a substring of Body
	// is trivially also a substring of lower(Body) once both sides are
	// lowered, so the prefilter can only prune granules the row predicate
	// would have rejected anyway, never admit a false positive past the row
	// filter. Scoped to non-negated filters only (`|=`, and `|~` when the
	// pattern round-trips through regexp/syntax as a single OpLiteral, i.e.
	// it is a regex only in name) — a superset prefilter has no sound
	// dual for a NEGATED "must not contain" predicate, so `!=`/`!~` and
	// structurally regex patterns are passed through byte-identical to
	// today. See chplan.LineContent.TextIndexPrefilter's doc for the full
	// eligibility shape.
	//
	// VERSION FLOOR: 26.4 — the release introducing
	// use_text_index_like_evaluation_by_dictionary_scan (default-on),
	// the setting family that lets a text index answer a LIKE '%needle%'
	// predicate by dictionary scan instead of a row-by-row match. Verified
	// directly: `SELECT name FROM system.settings WHERE name ILIKE
	// '%text_index_like%'` returns 0 rows on a probed 26.2.19 server and 3
	// rows (use_text_index_like_evaluation_by_dictionary_scan,
	// text_index_like_min_pattern_length=4,
	// text_index_like_max_postings_to_read=50) on 26.4.5. This feature's
	// floor is thus STRICTLY ABOVE FeatureFullTextIndex's own 26.2 — a
	// server can satisfy one without the other, and this feature is INERT
	// (byte-identical SQL) on any table that carries no text index at all,
	// so listing it without full_text_index is a harmless no-op, not a
	// fatal combination.
	//
	// NOT auto-selected: depends on an independently opt-in DDL feature
	// (full_text_index) actually having been applied and backfilled — auto-
	// enabling the row-filter rewrite ahead of that leaves every emitted
	// LIKE conjunct evaluating the SAME unindexed lower(Body) scan the row
	// predicate already pays for, a pure (if small) per-row LIKE-evaluation
	// tax with no pruning to offset it.
	FeatureTextIndexLineFilter = "text_index_line_filter"

	// FeatureTraceIDExternalTable pushes the /api/search structural
	// two-phase orchestrator's phase-A TraceId result set onto phase B as a
	// native-protocol external (temporary) table instead of splicing it as a
	// literal `TraceId IN (...)` list (restrictStructural, issue #2783).
	// CLIENT-SIDE and version-independent (AlwaysAvailable): clickhouse-go/v2
	// external tables are ordinary native-protocol functionality with no CH
	// version floor, so — like FeatureColumnarResultDecode — this carries no
	// MinVersion gate, only the boot-wired chopt on/off switch the "register
	// as chopt feature, resolve at boot, one lifecycle" rule requires even
	// for a floor-less optimization.
	//
	// The switch is BYTE-SIZE-gated, not width-gated: restrictStructural only
	// takes this path once the literal splice's estimated total bytes (summed
	// across every closure scan site the restriction reaches) crosses a
	// threshold sized against chsql's own emitted-SQL bound
	// (internal/chsql/emit_size_bound.go's 262144-byte maxEmittedSQLBytes,
	// itself ClickHouse's max_query_size default). Below the threshold the
	// literal splice is cheaper (no external-table round trip) and, per this
	// issue's own EXPLAIN indexes=1 verification against a real ClickHouse
	// server, prunes idx_trace_id IDENTICALLY to the external-table form at
	// every closure size tested — the switch exists purely to avoid the
	// literal splice's SQL-text-size failure axis (megabyte-scale statements,
	// chsql's ErrEmittedSQLTooLarge, or a bare max_query_size SYNTAX_ERROR),
	// never to fix a pruning gap.
	//
	// NOT auto-selected (AutoSelect=false): native-protocol-only (the
	// resolver has no way to see Config.Protocol, so cmd/cerberus's own
	// wiring additionally requires Protocol==clickhouse.Native — see
	// buildExternalTraceIDPush), and the production win at today's
	// MaxSearchLimit=1000 is real but unmeasured at scale beyond this issue's
	// own synthetic corpus — an operator opt-in via
	// CERBERUS_CH_OPTIMIZATIONS=trace_id_external_table, mirroring
	// FeatureTraceIDProjection's posture on a fresh, real-CH-server-only
	// mechanism.
	FeatureTraceIDExternalTable = "trace_id_external_table"

	// FeatureTSGridTagGroups opts the instant-mode duplicate-labelset guard's
	// collapsing Aggregate (internal/promql/duplicate_labelset_guard.go —
	// guardNameDropCollision, the name-dropping half; guardLabelRewriteCollision
	// is a documented follow-up, see below) onto grouping by a deduplicated
	// UInt64 id (timeSeriesTagsToGroup(Attributes)) instead of the raw
	// Map(String,String) Attributes column, rehydrating Attributes via
	// timeSeriesGroupToTags only in the wrapping output projection (cerberus
	// issue #2750 — the grouping-keys-only slice of the tag-group family; label
	// ops (by/without/label_replace/label_join/group_left via the purpose-built
	// tag functions) are explicit follow-up, tracked on #2750 itself per the
	// issue's own "grouping keys first, label ops later" staging).
	//
	// SITE CHOICE: guardNameDropCollision is the single most self-contained
	// grouping site in the pipeline that groups on the raw Attributes Map —
	// one file, no entanglement with the out-of-scope label-rewrite/group_left
	// towers, and (checked directly against internal/api/prom/lang.go) reached
	// ONLY from the instant-mode `/api/v1/query` path, which — unlike
	// query_range — never routes through the solver's route-B sharding
	// (internal/solver; classify()/IsSliceInvariant gate route B on Step > 0).
	// That matters concretely: timeSeriesTagsToGroup/timeSeriesGroupToTags are
	// STATEFUL, backed by a per-query ContextTimeSeriesTagsCollector (confirmed
	// by reading the ClickHouse source, src/Functions/TimeSeries/*.cpp) — a
	// group id has NO meaning outside the single query execution that produced
	// it. Route B runs K per-shard ClickHouse queries and concatenates their
	// cursors in Go WITHOUT re-aggregating on any key the shards produced
	// (internal/api/prom/lang.go, internal/api/prom/handler.go's "K per-shard
	// cursors... concatenated, NOT merged" comment), so a route-B-sharded
	// grouping site would need its OWN per-shard rehydration story before any
	// group id could cross a shard boundary — a route-B-sharded grouping site
	// is deliberately out of scope for cerberus issue #2750's own grouping-keys
	// slice; this PR picks a site that structurally cannot reach route B at
	// all, rather than shipping an unverified assumption about cross-shard id
	// reuse. Route-B-sharded grouping sites remain open follow-up work,
	// tracked at https://github.com/tsouza/cerberus/issues/2880.
	//
	// VERSION FLOOR — verified against real ClickHouse 26.1-alpine and
	// 26.2-alpine servers (fresh containers, default config, no
	// allow_experimental_* setting of any kind): timeSeriesTagsToGroup and
	// timeSeriesGroupToTags both work on 26.1 (the issue's own claimed core
	// floor); timeSeriesThrowDuplicateSeriesIf does NOT exist on 26.1
	// (UNKNOWN_FUNCTION) and DOES exist on 26.2 — confirming the issue's "floor
	// 26.1 core; 26.2 specifically for timeSeriesThrowDuplicateSeriesIf" claim
	// exactly. This feature's own MinVersion is pinned to the HIGHER 26.2,
	// not 26.1, even though this PR does not yet wire
	// timeSeriesThrowDuplicateSeriesIf into the guard's HAVING (that stays the
	// existing throwIf(uniqExact(MetricName) > 1, ...) — the issue's own point
	// that "the Aggregate is also the collapse fix" and the throw-message
	// mechanism is a SEPARATE, independently-verifiable change from the
	// grouping-key swap this feature makes) — one feature id must not
	// silently widen its own effective floor when a follow-up change adopts
	// the throw, without a version bump an operator can see, so the id is
	// pinned to what the FAMILY needs once the throw is adopted, not merely
	// what this change's own diff touches.
	//
	// NO EXPERIMENTAL GATE — RequiresExperimentalTSGrid is deliberately false.
	// The issue flagged the docs show no experimental gate for this family
	// (unlike the timeSeries*ToGrid MATRIX/aggregate family, which stamps
	// allow_experimental_time_series_aggregate_functions) and asked for
	// empirical verification rather than trusting that absence: confirmed —
	// every call above ran on a stock container with no settings applied
	// beyond CLICKHOUSE_USER/PASSWORD, and system.functions lists the whole
	// timeSeriesTagsToGroup/GroupToTags/IdToTags/IdToGroup/StoreTags/
	// ThrowDuplicateSeriesIf family as ordinary (non-experimental) functions on
	// both 26.1 and 26.2.
	//
	// ORDER-INDEPENDENCE — verified directly (the reason this feature can
	// obviate mapSort canonicalisation for THIS grouping site specifically,
	// without weakening it): timeSeriesTagsToGroup(map('a','1','b','2')) and
	// timeSeriesTagsToGroup(map('b','2','a','1')) return the IDENTICAL group id
	// — the function keys on the tag SET, not the Map's iteration order, unlike
	// a raw positional Map comparison (canonical_series_keys.go's whole reason
	// for existing). canonical_series_keys.go / canonical_attributes.go are
	// UNCHANGED by this feature: they still canonicalise every other Map
	// comparison and join in the pipeline this PR does not touch.
	//
	// ALIAS-SCOPING CONSTRAINT — verified directly, and the reason the
	// emission is a wrapping chplan.Project rather than one flat Aggregate:
	// ClickHouse rejects referencing a GROUP BY SELECT-list alias from inside
	// an aggregate function's argument in the SAME SELECT
	// (UNKNOWN_IDENTIFIER), and separately rejects re-deriving the grouping
	// expression a second time inside an aggregate argument
	// (ILLEGAL_AGGREGATION — the analyzer matches the repeated sub-expression
	// against the GROUP BY key and misclassifies the whole aggregate call).
	// The only shape that measured correctly is two query levels: an inner
	// Aggregate that groups on timeSeriesTagsToGroup(Attributes) and carries
	// only the group id plus the existing any(Value)/any(Timestamp)/carry
	// payload (Attributes itself is dropped, never re-read), and an outer
	// Project that reads the group id back as a REAL column from that
	// subquery and calls timeSeriesGroupToTags on it to rehydrate Attributes —
	// exactly "rehydrating Maps only in the final output projection" the issue
	// proposed, not a stylistic choice.
	//
	// AutoSelect is false — and, unlike every other opt-in feature's "no
	// fielded validation yet" posture, this one has a MEASURED reason, not
	// just a cautious default: a real ClickHouse 26.2 A/B (2M rows, the same
	// two SQL shapes this feature emits, FORMAT Null to isolate server-side
	// cost) at both a low (100 distinct label sets) and a high (200,000
	// distinct label sets) group cardinality found the UInt64-group-id path
	// CONSISTENTLY SLOWER in wall clock than the Map GROUP BY it replaces —
	// roughly 2-3x at low cardinality (666-1083ms vs 265-305ms) and ~50%
	// at high cardinality (1358-1506ms vs 881-957ms) — while peak memory was
	// a MIXED result, not a clean win: ~1.8x HIGHER at low cardinality
	// (29.7-34.9MB vs 19.6MB) and only ~10-20% lower at high cardinality
	// (411-413MB vs 462-520MB). The per-query ContextTimeSeriesTagsCollector
	// backing timeSeriesTagsToGroup/timeSeriesGroupToTags (see this feature's
	// own doc above) carries real bookkeeping cost that, at this grouping
	// site's row/cardinality shape, outweighs the O(1)-integer-comparison
	// win the issue's "single biggest CPU/memory lever" framing predicted —
	// this PR's own site was chosen for being the most self-contained
	// worked example the issue names (see this feature's SITE CHOICE note),
	// not for being the highest-cardinality one; a wider, hotter GROUP BY
	// elsewhere in the pipeline remains the open question for realizing the
	// win the issue actually promises, tracked at
	// https://github.com/tsouza/cerberus/issues/2880 — cerberus issue #2750
	// itself stays open, not closed, for exactly this reason. Until a site is
	// found where the measured numbers turn around,
	// AutoSelect must stay false regardless of how many fielded runs pass —
	// the mechanism (grouping-key correctness, round-trip fidelity, version
	// floor) is proven; the performance case for THIS site is not, and the
	// measurement above is why, not merely absence of evidence. Reachable
	// only via an explicit CERBERUS_CH_OPTIMIZATIONS=ts_tag_groups listing
	// (or "auto,ts_tag_groups").
	FeatureTSGridTagGroups = "ts_tag_groups"
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
		ID:                         FeatureTSGridVectorAgg,
		MinVersion:                 Version{Major: 25, Minor: 9},
		Stability:                  Experimental,
		AutoSelect:                 false,
		RequiresExperimentalTSGrid: true,
		Doc:                        "fold an element-wise-correct sum/min/max/avg/count by/without into an eligible native rate grid via -ForEach, exploding only the aggregated per-output-series grid once (narrows ts_grid_range; experimental, server >= 25.9, opt-in — #2763)",
	},
	{
		ID:                         FeatureTSGridInstant,
		MinVersion:                 Version{Major: 26, Minor: 5},
		Stability:                  Experimental,
		AutoSelect:                 false,
		RequiresExperimentalTSGrid: true,
		Doc:                        "extend rate/changes/resets/deriv/predict_linear's native matrix strategy to the instant shape via a one-point grid (narrows ts_grid_range/changes/resets/deriv/predict_linear; server >= 26.5, #103223/#105319; opt-in)",
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
		Doc: "opt the classic histogram_quantile rank walk onto the native quantilePrometheusHistogram(phi)(le, cum) aggregate (server >= 25.10, opt-in " +
			"only via CERBERUS_CH_OPTIMIZATIONS — real-CH measurement found memory crosses above the classic walk's around 18k-22k series, so operators " +
			"should keep a single histogram_quantile() call under ~15,000 series when opting in; see #2790)",
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
		ID:                         FeatureTSGridGroupArray,
		MinVersion:                 Version{Major: 25, Minor: 9},
		Stability:                  Experimental,
		AutoSelect:                 false,
		RequiresExperimentalTSGrid: true,
		Doc:                        "swap groupArray+arraySort(+dedup) window assembly for native timeSeriesGroupArray at sites that already dedup (server >= 25.9, opt-in — native collapse is order-dependent on a NaN duplicate, so auto never picks it)",
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
		ID:                         FeatureDownsampleTier,
		MinVersion:                 Version{Major: 25, Minor: 9},
		Stability:                  Experimental,
		AutoSelect:                 false,
		RequiresExperimentalTSGrid: true,
		Doc:                        "route eligible irate()/idelta()/last_over_time() shapes onto the downsampled long-range tier (server >= 25.9, opt-in via CERBERUS_CH_OPTIMIZATIONS — new persisted state, provision + backfill first)",
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
		Doc:        "opt the classic-histogram-quantile cross-series merge SUM fold onto a sumMap over per-row cumulative counts, retiring the groupArray + per-rung fold (no version floor, opt-in via CERBERUS_CH_OPTIMIZATIONS — auto pending a recalibrated cost model, #2923)",
	},
	{
		ID:         FeatureExpHistogramMergeSumMap,
		MinVersion: AlwaysAvailable,
		Stability:  Experimental,
		AutoSelect: false,
		Doc:        "opt the instant, single-group, SUM-fold exponential-histogram cross-series merge onto a two-pass sumMap-keyed reshape (no version floor, opt-in via CERBERUS_CH_OPTIMIZATIONS — regresses a single wide-layout series, #2757)",
	},
	{
		ID:         FeatureJoinSpill,
		MinVersion: Version{Major: 26, Minor: 4},
		Stability:  Experimental,
		AutoSelect: true,
		Doc:        "stamp max_bytes_before_external_join=cap/2 on join-bearing plans (server >= 26.4, auto-enabled) — mirrors the group_by/sort spill stamps; explicit, not the 26.5+ ratio default, silently ignored with no memory limit configured (cf. ClickHouse#76740)",
	},
	{
		ID:         FeatureTraceIDProjection,
		MinVersion: Version{Major: 25, Minor: 5},
		Stability:  Experimental,
		AutoSelect: false,
		Doc:        "curated ADD PROJECTION proj_trace_id (TraceId, _part_offset) on otel_traces/otel_logs for exact-row trace lookups (server >= 25.5, opt-in via CERBERUS_CH_OPTIMIZATIONS — backfill/merge cost unmeasured at production volume pending real-world calibration)",
	},
	{
		ID:         FeatureLokiCatalogMV,
		MinVersion: Version{Major: 24, Minor: 10},
		Stability:  Experimental,
		AutoSelect: false,
		Doc:        "REFRESH EVERY 5 MINUTE materialized view for /detected_labels' selector-less requests (server >= 24.10, opt-in via CERBERUS_CH_OPTIMIZATIONS — refresh scan cost unmeasured at production volume pending real-world calibration)",
	},
	{
		ID:         FeatureTempoTagCatalogMV,
		MinVersion: Version{Major: 24, Minor: 10},
		Stability:  Experimental,
		AutoSelect: false,
		Doc:        "REFRESH EVERY 5 MINUTE materialized view for /search/tags and /search/tag/{name}/values' unfiltered resource+span lookups (server >= 24.10, opt-in via CERBERUS_CH_OPTIMIZATIONS — refresh scan cost unmeasured at production volume pending real-world calibration)",
	},
	{
		ID:         FeatureTraceIDBitmapFilter,
		MinVersion: Version{Major: 25, Minor: 11},
		Stability:  Experimental,
		AutoSelect: true,
		Doc:        "stamp min_table_rows_to_use_projection_index=0 on TraceId-keyed predicates/joins so the projection-index bitmap PREWHERE path (server >= 25.11) is reachable regardless of table size (result-equivalent, auto-enabled)",
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
		Doc: "stamp query_plan_optimize_lazy_materialization=1 + query_plan_max_limit_for_lazy_materialization=<request LIMIT> on any Limit(OrderBy(...)) plan shape — Tempo's search " +
			"paths and Loki's log-line limit pushdown (server >= 25.11, auto-enabled — result-equivalent, chDB-verified)",
	},
	{
		ID:         FeatureExplainEstimate,
		MinVersion: AlwaysAvailable,
		Stability:  Experimental,
		AutoSelect: false,
		Doc:        "advisory EXPLAIN ESTIMATE pre-flight for solver K clamping and per-rung admission priors (no version floor — available since 21.9 — opt-in via CERBERUS_CH_OPTIMIZATIONS; auto never picks it pending real-world calibration)",
	},
	{
		ID:         FeatureCardinalityProbe,
		MinVersion: AlwaysAvailable,
		Stability:  Experimental,
		AutoSelect: false,
		Doc:        "advisory bounded count()/uniqUpTo(100)/uniqCombined64 cardinality pre-probe complementing explain_estimate's marks-level estimate with real distinct-series fan-out, across five GridCarrier kinds (no version floor; opt-in via CERBERUS_CH_OPTIMIZATIONS; auto never picks it pending real-world calibration)",
	},
	{
		ID:         FeatureFullTextIndex,
		MinVersion: Version{Major: 26, Minor: 2},
		Stability:  Experimental,
		AutoSelect: false,
		Doc:        "swap idx_lower_body from tokenbf_v1 to TYPE text on CREATE, plus an additive idx_body_text on existing tables (server >= 26.2 — verified GA floor, opt-in via CERBERUS_CH_OPTIMIZATIONS — backfill/maintenance cost unmeasured at production volume)",
	},
	{
		ID:         FeatureTextIndexLineFilter,
		MinVersion: Version{Major: 26, Minor: 4},
		Stability:  Experimental,
		AutoSelect: false,
		Doc:        "prepend an ANDed per-token LIKE strict-superset prefilter ahead of the unchanged row predicate for non-negated LogQL line filters (server >= 26.4 — verified LIKE-via-text-index floor, opt-in via CERBERUS_CH_OPTIMIZATIONS, inert without full_text_index)",
	},
	{
		ID:         FeatureTraceIDExternalTable,
		MinVersion: AlwaysAvailable,
		Stability:  Experimental,
		AutoSelect: false,
		Doc:        "push the /api/search two-phase phase-A TraceId set as a native-protocol external table instead of a literal IN list above a byte threshold (no version floor, native-protocol, opt-in via CERBERUS_CH_OPTIMIZATIONS -- EXPLAIN-verified idx_trace_id parity, #2783)",
	},
	{
		ID:         FeatureTSGridTagGroups,
		MinVersion: Version{Major: 26, Minor: 2},
		Stability:  Experimental,
		AutoSelect: false,
		Doc:        "group the instant-mode duplicate-labelset guard's name-drop collapse on a UInt64 tag-group id (timeSeriesTagsToGroup), not the raw Attributes Map, rehydrating via timeSeriesGroupToTags in the projection (server >= 26.2, no experimental gate, opt-in -- #2750)",
	},
}

// Registry returns a copy of the seeded feature registry
// (aggregation_in_order, condition_cache, ts_grid_range, ts_grid_resample,
// columnar_result_decode, ts_grid_changes, ts_grid_resets, ts_grid_deriv,
// ts_grid_predict_linear, ts_grid_instant, ts_grid_recollapse, ts_grid_increase,
// ts_grid_histogram, quantile_prom_histogram, ts_grid_delta, ts_grid_irate,
// ts_grid_idelta, laginframe_adjacency, fixed_accumulator_extrapolated,
// sorted_slab_over_time, map_bucketed_serialization, ts_grid_last_over_time,
// column_statistics, classic_bucket_merge_summap, exp_histogram_merge_summap,
// join_spill, trace_id_projection, loki_catalog_mv, tempo_tag_catalog_mv,
// trace_id_bitmap_filter, arg_and_max_fusion, result_cache,
// lazy_materialization, explain_estimate, cardinality_probe,
// full_text_index, text_index_line_filter, trace_id_external_table,
// ts_tag_groups). The copy
// keeps the canonical entries immutable from the caller's side. Exposed so
// tests can enumerate the gates and the docs generator can render the
// table.
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
