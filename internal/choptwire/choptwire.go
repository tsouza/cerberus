// Package choptwire turns a resolved ClickHouse-optimization EnabledSet into
// the two things the query stack is configured with from it: the polymorphic
// PromQL range-lowering dispatch table, and the per-query engine settings
// rules.
//
// It exists because both compositions have exactly two callers that must
// agree and could not share code: cmd/cerberus's boot path, where they are
// `package main` and therefore unimportable, and internal/chopttest, which
// every real-ClickHouse activation test wires its handler through. The test
// side carried reviewed COPIES of both formulas, so the perf smoke and
// nightly lanes, the solver-decision ratchet and the API integration tests
// all validated the copies rather than the production wiring: a feature
// wired in one and forgotten in the other passed every activation test and
// shipped inert, which is precisely the failure those lanes exist to prevent
// (cerberus issue #3186).
//
// Only the CAPABILITY-decided fields live here. The operator-configured ones
// cmd/cerberus reads off config.Config — the log-comment shape, the
// result-cache ingest lag and TTL, the query workload — are not capability
// decisions, so they are overlaid by the caller that has a config to read
// them from rather than being given a hidden default here.
package choptwire

import (
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// RangeLowerers builds the BOOT-WIRED polymorphic lowering dispatch table
// for the ClickHouse-native timeSeries*ToGrid family from the resolved
// optimization EnabledSet. The feature/version decision is made HERE, ONCE, at
// boot (optSet was produced by the single boot-time version probe in
// resolveCHOptimizations) and is the ONLY place the feature is read: each field
// is wired to a CONCRETE non-nil strategy —
//
//	rate      = enabled ? NativeRateLowerer{Fallback: rateFallback} : rateFallback
//	increase  = enabled ? NativeIncreaseLowerer{Fallback: increaseFallback} : increaseFallback
//	delta     = enabled ? NativeDeltaLowerer{Fallback: deltaFallback} : deltaFallback
//	  // where rateFallback/increaseFallback/deltaFallback are each
//	  // fixedAccumEnabled ? FixedAccumulator*Lowerer{Fallback: Fanout*Lowerer{}} : Fanout*Lowerer{}
//	staleness = enabled ? NativeStalenessLowerer{Fallback: FanoutStalenessLowerer{}} : FanoutStalenessLowerer{}
//	changes   = enabled ? NativeChangesLowerer{Fallback: changesFallback} : changesFallback
//	resets    = enabled ? NativeResetsLowerer{Fallback: resetsFallback} : resetsFallback
//	irate     = enabled ? NativeIrateLowerer{Fallback: irateFallback} : irateFallback
//	idelta    = enabled ? NativeIdeltaLowerer{Fallback: ideltaFallback} : ideltaFallback
//	  // where changesFallback/resetsFallback/irateFallback/ideltaFallback are each
//	  // laginframeEnabled ? LagAdjacency*Lowerer{Fallback: Fanout*Lowerer{}} : Fanout*Lowerer{}
//	deriv     = enabled ? NativeDerivLowerer{Fallback: FanoutDerivLowerer{}} : FanoutDerivLowerer{}
//	predict   = enabled ? NativePredictLinearLowerer{Fallback: FanoutPredictLinearLowerer{}} : FanoutPredictLinearLowerer{}
//	classicHq = enabled ? NativeClassicHistogramWindowLowerer{Fallback: Fanout…{}} : Fanout…{}
//	rankWalk  = enabled ? NativeQuantileRankWalkLowerer{} : FanoutQuantileRankWalkLowerer{}
//	lastOverTime = enabled ? NativeLastOverTimeLowerer{Fallback: FanoutLastOverTimeLowerer{}} : FanoutLastOverTimeLowerer{}
//	  // downsampleTierEnabled ? DownsampleTier{Irate,Idelta,LastOverTime}Lowerer{Fallback: <above>} : <above> unchanged
//	overTime  = sortedSlabEnabled ? SortedSlabOverTimeLowerer{Fallback: FanoutOverTimeLowerer{}} : FanoutOverTimeLowerer{}
//
// rankWalk (quantile_prom_histogram) is not a timeSeries*ToGrid member — it
// wraps ClickHouse's separate quantilePrometheusHistogram aggregate (floor
// 25.10) — and carries no embedded Fallback because it has no shape-based
// fallback to embed (see promql.QuantileRankWalkLowerer's own doc); it is
// resolved here, in this same table, only because RangeLowerers is the one
// seam every classic-histogram-quantile lowering already threads.
//
// overTime (sum_over_time/avg_over_time, issue #2761) is likewise not a
// timeSeries*ToGrid member and carries no native competitor, but unlike
// rankWalk it DOES embed a Fallback (the plain fan-out) — its sorted-slab
// decomposition is a shape-gated SQL rewrite with its own eligibility guard
// (sortedSlabOverTimeEligible), the same posture fixed_accumulator_extrapolated
// has for rate/increase/delta, not an always-applicable aggregate swap like
// rankWalk's.
//
// The fan-out impl is the concrete DEFAULT (never nil), and the native impl
// embeds it as the fallback for shapes it cannot handle. The features are
// independent, so the table composes per-function — native rate can be on while
// native staleness / changes / resets / deriv / predict_linear are off, and vice
// versa (the whole family shares the 25.9 floor, but each member is probed and
// resolved on its own). The per-query
// lowering then
// dispatches through this table as a plain interface method call: NO
// feature/version read, NO nil/presence check.
//
// ts_grid_recollapse is the one NON-independent knob: it defers the
// label-shaping tower past the native rate grid, so it only means anything
// inside the ts_grid_range branch. The registry has no inter-feature dependency
// mechanism, so that narrowing is expressed HERE — reading it only where a
// native rate lowerer is being built makes "recollapse without a native grid"
// unrepresentable rather than merely unlikely.
//
// laginframe_adjacency (issue #2759) is a second non-independent knob, but in
// the opposite direction from recollapse: it narrows changes/resets/irate/
// idelta's FALLBACK (the arm a shape-ineligible or sub-25.9 server lands on),
// not their native arm. It was irate/idelta's ONLY non-fan-out strategy until
// issue #2746 gave them their own native timeSeriesInstantRateToGrid /
// timeSeriesInstantDeltaToGrid competitor, so all four of changes / resets /
// irate / idelta now share the identical three-tier Native{Fallback:
// LagAdjacency{Fallback: Fanout{}}} composition. Unlike every ts_grid_*
// feature it carries no version floor (chopt.AlwaysAvailable) and no
// experimental-setting gate, so it composes with the version-gated native
// features purely via which Fallback each Native*Lowerer embeds.
//
// fixed_accumulator_extrapolated (issue #2760) is laginframe_adjacency's own
// sibling for the extrapolated family: it narrows rate/increase/delta's
// FALLBACK the same way — delta gained its own native ts_grid_delta
// competitor (issue #2745), so as of that feature all three of rate /
// increase / delta share the identical three-tier
// Native{Fallback: FixedAccumulator{Fallback: Fanout{}}} composition. Same
// AlwaysAvailable floor, same no-experimental-setting posture, same "narrows
// the fallback, never competes with the native arm" shape.
//
// ts_grid_instant (issue #2748) narrows the NATIVE arm itself rather than the
// fallback — the opposite direction from recollapse/laginframe/fixed-
// accumulator above. Each of rate/changes/resets/deriv/predict_linear's
// Native*Lowerer gains an Instant field, set to tsGridInstant ONLY inside
// that same function's own "matrix feature enabled" branch, so the instant
// arm can never fire for a function whose matrix arm is off. increase() and
// delta() are out of scope (see chopt.FeatureTSGridInstant's own doc) and
// carry no Instant field.
//
// ts_grid_group_array (issue #2749) is a third non-swappable emission-detail
// bit, mirroring VectorAgg's own posture: it lives on RangeLowerers itself
// (promql.RangeLowerers.NativeGroupArray) rather than on any single
// Native*Lowerer, and is set unconditionally below because rate/increase/
// delta's fanout lowering reads it directly off the SAME chplan.RangeWindow
// node regardless of which of those three functions' own Native /
// FixedAccumulator / Fanout tiers produced it.
func RangeLowerers(optSet chopt.EnabledSet) promql.RangeLowerers {
	var l promql.RangeLowerers
	// arg_and_max_fusion (issue #2764) is a plain emission-detail bit, not
	// a swappable strategy — see RangeLowerers.ArgAndMaxFusion's own doc.
	// It feeds BOTH the VectorJoin site (read directly off this table by
	// internal/promql/binary.go) and the RangeLWR site (its own copy on
	// FanoutStalenessLowerer below, threaded either directly or as the
	// native staleness lowerer's embedded Fallback).
	argAndMaxFusion := optSet.Has(chopt.FeatureArgAndMaxFusion)
	l.ArgAndMaxFusion = argAndMaxFusion
	// ts_grid_vector_agg (issue #2763) is a pure narrowing of ts_grid_range,
	// but — unlike ts_grid_recollapse — it lives on RangeLowerers itself
	// rather than on NativeRateLowerer (or any single Native*Lowerer),
	// because lowerAggregate consults it at a LATER lowering stage than any
	// range-function Lowerer: only after a range function has already
	// lowered its input, and only when that input turns out to be a
	// *chplan.RangeWindowGridNative. Setting it unconditionally here (never
	// nested inside `if optSet.Has(chopt.FeatureTSGridRange)`) is therefore
	// safe rather than a narrowing gap: the field is inert whenever no
	// native grid node exists for lowerAggregate to fold into.
	l.VectorAgg = optSet.Has(chopt.FeatureTSGridVectorAgg)
	// ts_grid_group_array (issue #2749) is, like VectorAgg immediately
	// above, a plain narrowing bit rather than a per-function Lowerer swap:
	// it never changes WHICH of rate/increase/delta's own Native /
	// FixedAccumulator / Fanout tiers fires, only how that tier's array-fold
	// fallback assembles its per-series sample array. Set unconditionally
	// here for the same reason VectorAgg is — it lives on RangeLowerers
	// itself, not on any single Native*Lowerer.
	l.NativeGroupArray = optSet.Has(chopt.FeatureTSGridGroupArray)
	// ts_grid_instant (issue #2748) is a pure narrowing of each of
	// rate/changes/resets/deriv/predict_linear's own matrix feature — it is
	// consulted ONLY inside each function's own "matrix feature enabled"
	// branch below, exactly like ts_grid_recollapse narrows ts_grid_range,
	// so it can never make a Native*Lowerer's instant arm reachable on its
	// own.
	tsGridInstant := optSet.Has(chopt.FeatureTSGridInstant)
	// fixed_accumulator_extrapolated (issue #2760) layers BENEATH
	// rate/increase/delta's own native ts_grid strategy, exactly like
	// laginframe_adjacency layers inside changes/resets below: it is the
	// improved fan-out a shape-ineligible, sub-25.9, or temporality-bearing
	// window falls back to, never a competitor to the native path.
	var rateFallback promql.RateLowerer = promql.FanoutRateLowerer{}
	var increaseFallback promql.IncreaseLowerer = promql.FanoutIncreaseLowerer{}
	var deltaFallback promql.DeltaLowerer = promql.FanoutDeltaLowerer{}
	if optSet.Has(chopt.FeatureFixedAccumulatorExtrapolated) {
		rateFallback = promql.FixedAccumulatorRateLowerer{Fallback: rateFallback}
		increaseFallback = promql.FixedAccumulatorIncreaseLowerer{Fallback: increaseFallback}
		deltaFallback = promql.FixedAccumulatorDeltaLowerer{Fallback: deltaFallback}
	}
	if optSet.Has(chopt.FeatureTSGridRange) {
		l.Rate = promql.NativeRateLowerer{
			Fallback:   rateFallback,
			Recollapse: optSet.Has(chopt.FeatureTSGridRecollapse),
			Instant:    tsGridInstant,
		}
	} else {
		l.Rate = rateFallback
	}
	if optSet.Has(chopt.FeatureTSGridIncrease) {
		l.Increase = promql.NativeIncreaseLowerer{Fallback: increaseFallback}
	} else {
		l.Increase = increaseFallback
	}
	if optSet.Has(chopt.FeatureTSGridResample) {
		l.Staleness = promql.NativeStalenessLowerer{Fallback: promql.FanoutStalenessLowerer{ArgAndMaxFusion: argAndMaxFusion}}
	} else {
		l.Staleness = promql.FanoutStalenessLowerer{ArgAndMaxFusion: argAndMaxFusion}
	}
	// laginframe_adjacency (issue #2759) layers BENEATH changes/resets' own
	// native ts_grid strategy, exactly like ts_grid_recollapse layers inside
	// ts_grid_range above: it is the improved fan-out a shape-ineligible or
	// sub-25.9 server falls back to, never a competitor to the native path.
	var changesFallback promql.ChangesLowerer = promql.FanoutChangesLowerer{}
	var resetsFallback promql.ResetsLowerer = promql.FanoutResetsLowerer{}
	if optSet.Has(chopt.FeatureLagInFrameAdjacency) {
		changesFallback = promql.LagAdjacencyChangesLowerer{Fallback: changesFallback}
		resetsFallback = promql.LagAdjacencyResetsLowerer{Fallback: resetsFallback}
	}
	if optSet.Has(chopt.FeatureTSGridChanges) {
		l.Changes = promql.NativeChangesLowerer{Fallback: changesFallback, Instant: tsGridInstant}
	} else {
		l.Changes = changesFallback
	}
	if optSet.Has(chopt.FeatureTSGridResets) {
		l.Resets = promql.NativeResetsLowerer{Fallback: resetsFallback, Instant: tsGridInstant}
	} else {
		l.Resets = resetsFallback
	}
	// irate/idelta gained their own native timeSeries*ToGrid member
	// (timeSeriesInstantRateToGrid / timeSeriesInstantDeltaToGrid, cerberus
	// issue #2746): laginframe_adjacency now layers BENEATH that native
	// strategy exactly like it does for changes/resets above, rather than
	// being their only non-fan-out strategy the way issue #2759 originally
	// left it.
	var irateFallback promql.IrateLowerer = promql.FanoutIrateLowerer{}
	var ideltaFallback promql.IdeltaLowerer = promql.FanoutIdeltaLowerer{}
	if optSet.Has(chopt.FeatureLagInFrameAdjacency) {
		irateFallback = promql.LagAdjacencyIrateLowerer{Fallback: irateFallback}
		ideltaFallback = promql.LagAdjacencyIdeltaLowerer{Fallback: ideltaFallback}
	}
	if optSet.Has(chopt.FeatureTSGridIrate) {
		l.Irate = promql.NativeIrateLowerer{Fallback: irateFallback}
	} else {
		l.Irate = irateFallback
	}
	if optSet.Has(chopt.FeatureTSGridIdelta) {
		l.Idelta = promql.NativeIdeltaLowerer{Fallback: ideltaFallback}
	} else {
		l.Idelta = ideltaFallback
	}
	if optSet.Has(chopt.FeatureTSGridDeriv) {
		l.Deriv = promql.NativeDerivLowerer{Fallback: promql.FanoutDerivLowerer{}, Instant: tsGridInstant}
	} else {
		l.Deriv = promql.FanoutDerivLowerer{}
	}
	if optSet.Has(chopt.FeatureTSGridPredictLinear) {
		l.PredictLinear = promql.NativePredictLinearLowerer{Fallback: promql.FanoutPredictLinearLowerer{}, Instant: tsGridInstant}
	} else {
		l.PredictLinear = promql.FanoutPredictLinearLowerer{}
	}
	if optSet.Has(chopt.FeatureTSGridDelta) {
		l.Delta = promql.NativeDeltaLowerer{Fallback: deltaFallback}
	} else {
		l.Delta = deltaFallback
	}
	// The anchor-injection window-slide mechanism (#2408 follow-up, #2493)
	// was removed by #2511's root-cause investigation: its anchor-injection
	// UNION structurally requires the per-series canonical-bound subquery to
	// be inlined into BOTH UNION arms (real rows' JOIN and the sentinel
	// arm's FROM), and this codebase's chsql architecture only single-
	// evaluates a genuinely scalar CTE (QueryBuilder.WithScalar) — a
	// relational CTE re-inlines, and re-scans, at every reference
	// (QueryBuilder.With's own doc). Real EXPLAIN PLAN / EXPLAIN PIPELINE
	// evidence against chDB confirmed 3 independent ReadFromMergeTree scans
	// of the base table for one window-slide query where the fan-out needs
	// one, plus the shared LIMIT+throwIf resource-bound pattern doubling
	// whatever that count is — the real root cause of the ~5x-more-bytes
	// regression #2511 measured. No fix avoids this without either
	// reintroducing the O(rows^2)-per-series blow-up an earlier review
	// already rejected, or a per-row scalar-map lookup against thousands of
	// series (unvalidated and very likely slower still). Combined with the
	// mechanism's own real-world win being marginal at the modal dashboard
	// shape (1.12x at 5m/1m, per #2408's own Task-1 spike), fan-out is kept
	// as the sole classic-histogram range-window path below the native
	// rate ladder.
	if optSet.Has(chopt.FeatureTSGridHistogram) {
		l.ClassicHistogram = promql.NativeClassicHistogramWindowLowerer{
			Fallback: promql.FanoutClassicHistogramWindowLowerer{},
		}
	} else {
		l.ClassicHistogram = promql.FanoutClassicHistogramWindowLowerer{}
	}
	// quantile_prom_histogram has no shape-based fallback (see
	// promql.QuantileRankWalkLowerer's own doc), so the native strategy is
	// wired directly with no embedded Fallback field.
	if optSet.Has(chopt.FeatureQuantilePromHistogram) {
		l.QuantileRankWalk = promql.NativeQuantileRankWalkLowerer{}
	} else {
		l.QuantileRankWalk = promql.FanoutQuantileRankWalkLowerer{}
	}
	// ts_grid_last_over_time rides the SAME native strategy shape as every
	// other independent family member (Native{Fallback: Fanout{}}) — it has
	// no narrowing/narrowed sibling knob of its own (no recollapse-style
	// dependent, no laginframe/fixed-accumulator-style improved fallback).
	if optSet.Has(chopt.FeatureTSGridLastOverTime) {
		l.LastOverTime = promql.NativeLastOverTimeLowerer{Fallback: promql.FanoutLastOverTimeLowerer{}}
	} else {
		l.LastOverTime = promql.FanoutLastOverTimeLowerer{}
	}
	// downsample_tier (cerberus issue #2751) WRAPS whatever irate/idelta/
	// last_over_time strategy was just resolved above — it is a genuinely
	// different mechanism (an operator-provisioned, pre-populated table, not
	// a stateless read-time function swap over the raw table), so it sits
	// as an outer layer: an eligible shape routes to the tier, everything
	// else falls through to the family's own best-available raw-scan
	// strategy (native/laginframe/fanout) unchanged. See
	// chopt.FeatureDownsampleTier's own doc for why rate()/increase()/
	// delta() have no such wrapping at all.
	if optSet.Has(chopt.FeatureDownsampleTier) {
		l.Irate = promql.DownsampleTierIrateLowerer{Fallback: l.Irate}
		l.Idelta = promql.DownsampleTierIdeltaLowerer{Fallback: l.Idelta}
		l.LastOverTime = promql.DownsampleTierLastOverTimeLowerer{Fallback: l.LastOverTime}
	}
	// sorted_slab_over_time (issue #2761, widened to first_over_time /
	// stddev_over_time / stdvar_over_time / mad_over_time by issue #2804)
	// has no native timeSeries*ToGrid competitor: this whole function set's
	// only non-fan-out arm is the sorted-slab decomposition itself, so it is
	// wired directly with no embedded native layer above it (mirroring
	// quantile_prom_histogram's posture just above, though this strategy
	// DOES still embed its own Fallback — the plain fan-out — the way
	// fixed_accumulator_extrapolated does for rate/increase/delta). One
	// lowerer wiring covers the whole set: which PromQL function names
	// reach it at all is decided upstream, purely by AST dispatch, in
	// internal/promql/lower.go's `lowerRangeVectorCallFanout` switch — see
	// that switch's own doc for why last_over_time deliberately never
	// reaches this lowerer despite sharing its shape-eligibility check.
	if optSet.Has(chopt.FeatureSortedSlabOverTime) {
		l.OverTime = promql.SortedSlabOverTimeLowerer{Fallback: promql.FanoutOverTimeLowerer{}}
	} else {
		l.OverTime = promql.FanoutOverTimeLowerer{}
	}
	// classic_bucket_merge_summap (issue #2756) has no version floor to
	// probe. #2817 closed its original correctness blocker; issue #2923's
	// real-ClickHouse re-measurement against the resulting (post-#2817)
	// construction then found its real cost within ~1% of the fold's, not
	// the estimated ~50x win — so AutoSelect stays false, a measured
	// negative result rather than an open question. See
	// promql.NativeClassicBucketMergeLowerer's own doc and
	// classic_bucket_merge_summap.go's header.
	if optSet.Has(chopt.FeatureClassicBucketMergeSumMap) {
		l.ClassicBucketMerge = promql.NativeClassicBucketMergeLowerer{
			Fallback: promql.FanoutClassicBucketMergeLowerer{},
		}
	} else {
		l.ClassicBucketMerge = promql.FanoutClassicBucketMergeLowerer{}
	}
	// exp_histogram_merge_summap (issue #2757) has no version floor to
	// probe, but ships AutoSelect: false: it now covers every shape —
	// instant AND range mode (cerberus issue #3027), any by()/without()
	// grouping (#2865), SUM or AVG fold (#2866) — each with its own
	// real-ClickHouse-calibrated budget guard rather than a reuse of the
	// classic fold's rows-dominated one — see
	// promql.NativeExpHistogramMergeLowerer's own doc.
	if optSet.Has(chopt.FeatureExpHistogramMergeSumMap) {
		l.ExpHistogramMerge = promql.NativeExpHistogramMergeLowerer{}
	} else {
		l.ExpHistogramMerge = promql.FanoutExpHistogramMergeLowerer{}
	}
	return l
}

// SettingsRules builds the CAPABILITY-decided half of the per-query
// engine.SettingsRules from a resolved EnabledSet, plus the three schema
// instances the eligibility checks need to map a scanned table name to its
// sort-key prefix (passing the zero value would silently make the
// aggregation-in-order and trace-id-bitmap-filter rules unable to fire).
//
// The OPERATOR-configured fields are deliberately absent: LogCommentShape,
// ResultCacheIngestLag, ResultCacheTTL and QueryWorkload come off
// config.Config, not off the EnabledSet, so the caller that has a config
// overlays them on the returned value. A caller without one — an integration
// test — leaves them at their zero values rather than inheriting a hidden
// default it never asked for.
//
// Note on ResultCache: it rides the EnabledSet faithfully, but a set resolved
// without chclient.ProbeResultCacheCapability keeps result_cache out by
// construction (chopt's RequiresResultCacheCapability gate reads Capability's
// conservative zero value). A caller that measures per-query cost must assert
// that for itself — a served-from-cache repeat costs almost nothing and would
// silently hollow out a max-of-N memory measurement.
func SettingsRules(set chopt.EnabledSet, metrics schema.Metrics, traces schema.Traces, logs schema.Logs) engine.SettingsRules {
	return engine.SettingsRules{
		OptimizeAggregationInOrder: set.Has(chopt.FeatureAggregationInOrder),
		ConditionCache:             set.Has(chopt.FeatureConditionCache),
		JoinSpill:                  set.Has(chopt.FeatureJoinSpill),
		TraceIDBitmapFilter:        set.Has(chopt.FeatureTraceIDBitmapFilter),
		ResultCache:                set.Has(chopt.FeatureResultCache),
		LazyMaterialization:        set.Has(chopt.FeatureLazyMaterialization),
		Metrics:                    metrics,
		Traces:                     traces,
		Logs:                       logs,
	}
}
