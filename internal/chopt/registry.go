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
	// #1721) and FeatureTSGridResets (25.9+ only): irate/idelta have no
	// native timeSeries*ToGrid member at all, and changes/resets fall back to
	// this improved fan-out on any server the native path does not cover.
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
	Doc                        string
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
		Doc:        "opt eligible query_range rate/increase/delta shapes onto per-(series, anchor) fixed-size aggregates (count/min/max/argMin/argMax/sumIf), retiring the array-fold fan-out (client-side, no version floor, opt-in only via CERBERUS_CH_OPTIMIZATIONS pending optcorpus A/B)",
	},
}

// Registry returns a copy of the seeded feature registry
// (aggregation_in_order, condition_cache, ts_grid_range, ts_grid_resample,
// columnar_result_decode, ts_grid_changes, ts_grid_resets, ts_grid_deriv,
// ts_grid_predict_linear, ts_grid_recollapse, ts_grid_increase,
// ts_grid_histogram, quantile_prom_histogram, laginframe_adjacency,
// fixed_accumulator_extrapolated). The copy keeps the canonical entries
// immutable from the caller's side. Exposed so tests can enumerate the gates
// and the docs generator can render the table.
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
