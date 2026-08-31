package engine

import (
	"context"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// settingOptimizeAggregationInOrder is the ClickHouse setting that lets the
// aggregator consume rows in sort order and emit each group as soon as its
// key block is exhausted, instead of building a full hash table. It is
// RESULT-EQUIVALENT: it changes only the execution strategy, never the rows.
// It has existed since well before cerberus's CH 24.8 floor, so stamping it
// is version-safe.
const settingOptimizeAggregationInOrder = "optimize_aggregation_in_order"

// settingUseQueryConditionCache is the ClickHouse setting that turns on the
// query condition cache: the server caches, per data part, which granules a
// WHERE predicate already selected, so a later query with the SAME predicate
// skips re-evaluating it on the cached parts. It is RESULT-EQUIVALENT (a
// cache, not a result rewrite) and lands in ClickHouse 25.3, gated behind the
// analyzer. cerberus stamps it only when the condition_cache feature resolved
// in (server >= 25.3) AND the read path is predicate-stable; below 25.3 the
// feature is absent from the resolved set, so ConditionCache is false and this
// is never stamped (version-safe fallback to no-op).
const settingUseQueryConditionCache = "use_query_condition_cache"

// settingEnableAnalyzer turns on ClickHouse's new query analyzer. The query
// condition cache is gated behind the analyzer, so cerberus co-stamps
// enable_analyzer=1 wherever it stamps use_query_condition_cache=1 to ensure
// the cache is honored even if an operator disabled the analyzer at the
// server/profile level. It is RESULT-EQUIVALENT (an execution-planner choice,
// not a result rewrite) and the analyzer is GA on every server the
// condition_cache feature resolves on (>= 25.3), so co-stamping is version-safe.
const settingEnableAnalyzer = "enable_analyzer"

// settingLogComment is ClickHouse's free-form per-query annotation. When set
// it is copied verbatim into system.query_log.log_comment, letting operators
// GROUP BY a cerberus-assigned shape id. Free-form and ignored by execution,
// so stamping it is version-safe and result-neutral.
const settingLogComment = "log_comment"

// SettingsRules holds the DARK-by-default, plan-shape-gated per-query
// ClickHouse settings rules the engine evaluates at the execute seam. The
// zero value applies NOTHING: with both flags false the ctx is returned
// unchanged, so wiring SettingsRules is byte-neutral until an operator opts
// in via the CERBERUS_* flags.
//
// Both rules are safe on ClickHouse 24.8 (cerberus's min floor):
// optimize_aggregation_in_order is a long-standing result-equivalent
// execution knob, and log_comment is a free-form annotation. Neither adopts
// a 25.x feature.
type SettingsRules struct {
	// OptimizeAggregationInOrder, when true, stamps
	// optimize_aggregation_in_order=1 on queries whose post-optimize plan
	// has an Aggregate GROUP BY that is a genuine bare-column PREFIX of the
	// scanned table's sorting key (see eligibleForAggregationInOrder). The
	// setting is result-equivalent and off by default, so this is doubly
	// safe, but the eligibility check is still conservative: when anything
	// about the plan shape is unclear it does NOT stamp.
	OptimizeAggregationInOrder bool

	// ConditionCache, when true, stamps use_query_condition_cache=1 on a
	// predicate-stable read path so ClickHouse's query condition cache can skip
	// re-evaluating an already-seen WHERE predicate on cached parts. It is
	// driven by the condition_cache registry feature, which only resolves in on
	// server >= 25.3; below that the feature is absent from the resolved set,
	// so this flag is false and nothing is stamped (24.8-safe no-op). The cache
	// is result-equivalent, so this is safe whenever it fires; the eligibility
	// check (predicateStableForConditionCache) is still conservative.
	ConditionCache bool

	// JoinSpill, when true, stamps max_bytes_before_external_join=cap/2 on a
	// join-bearing plan (see planHasJoin in spill.go) so a large hash build
	// spills to disk instead of aborting with MEMORY_LIMIT_EXCEEDED (code
	// 241). Driven by the join_spill registry feature, which only resolves
	// in on server >= 26.4; below that the feature is absent from the
	// resolved set, so this flag is false and nothing is stamped (a no-op on
	// every server too old to carry the setting). Applied via
	// applyJoinSpillSettings rather than inside apply below, because unlike
	// OptimizeAggregationInOrder/ConditionCache it also needs the live
	// per-query memory cap — see engine.go's execContext.
	JoinSpill bool

	// LogCommentShape, when true, stamps log_comment with a compact cerberus
	// shape id (planShapeID) carrying the emit-root node kind plus key
	// modifiers and NEVER any literal values, so operators with query_log
	// enabled can cluster by normalized_query_hash + log_comment.
	LogCommentShape bool

	// ResultCache, when true, stamps use_query_cache=1 + query_cache_ttl on a
	// query whose evaluated windows have all fully CLOSED (see
	// eligibleForResultCache) so ClickHouse's query result cache can answer a
	// dashboard's byte-identical re-issue of the same query_range without
	// re-scanning. Driven by the result_cache registry feature, which is
	// AutoSelect but gated on the boot result-cache capability probe (a
	// hardened/constrained profile, or a server-disabled query cache, drops
	// this to false regardless of version — see chopt.FeatureResultCache).
	// The eligibility check is cerberus's own correctness guard, not merely a
	// convenience: it is the ONLY thing standing between "cache a fully-closed
	// historical panel" and "cache a live-edge tail forever" — see the doc on
	// eligibleForResultCache for exactly what it verifies.
	ResultCache bool

	// ResultCacheIngestLag is the deployment's configured
	// CERBERUS_RESULT_CACHE_INGEST_LAG: how far behind wall-clock now the
	// ingest pipeline may still be writing rows for. eligibleForResultCache
	// requires a query's window End to be strictly before now minus this
	// horizon before ResultCache is ever stamped.
	ResultCacheIngestLag time.Duration

	// ResultCacheTTL is the deployment's configured CERBERUS_RESULT_CACHE_TTL,
	// stamped as query_cache_ttl (seconds) alongside use_query_cache=1.
	ResultCacheTTL time.Duration

	// Now returns the current time for eligibleForResultCache's window-closed
	// check. Nil (the production default) uses time.Now(); tests inject a
	// fixed clock so the closed/live-edge boundary can be pinned exactly.
	Now func() time.Time

	// Metrics / Traces / Logs are the schema instances whose SortingKeyPrefix
	// the aggregation-in-order eligibility check reads to map a scanned table
	// name to its bare-column sort-key prefix. They mirror the same schema the
	// query heads read; a renamed table simply fails to match and the setting
	// is not stamped (fail-safe).
	Metrics schema.Metrics
	Traces  schema.Traces
	Logs    schema.Logs
}

// enabledOpts returns the ids of the optimization rules currently enabled on
// the SettingsRules, sorted, for the corpus reconciler to record alongside a
// dispatched query's shape-id. It reports the rules that COULD fire for a
// query (the resolved EnabledSet membership), not which actually fired on a
// given plan; that keeps the recorded opts stable per cerberus process and
// lets the corpus attribute observed cost to the active optimization posture.
func (r SettingsRules) enabledOpts() []string {
	var opts []string
	if r.OptimizeAggregationInOrder {
		opts = append(opts, "aggregation_in_order")
	}
	if r.ConditionCache {
		opts = append(opts, "condition_cache")
	}
	if r.JoinSpill {
		opts = append(opts, "join_spill")
	}
	if r.ResultCache {
		opts = append(opts, "result_cache")
	}
	return opts
}

// now returns r.Now() when set, else time.Now() — see the Now field's doc.
func (r SettingsRules) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// apply layers the enabled settings rules onto ctx for plan. Each rule that
// fires writes through chclient.WithQuerySetting so they accumulate on the
// one per-request settings map. With both flags off, ctx is returned
// unchanged.
func (r SettingsRules) apply(ctx context.Context, plan chplan.Node) context.Context {
	if r.OptimizeAggregationInOrder && r.eligibleForAggregationInOrder(plan) {
		ctx = chclient.WithQuerySetting(ctx, settingOptimizeAggregationInOrder, 1)
	}
	if r.ConditionCache && predicateStableForConditionCache(plan) {
		ctx = chclient.WithQuerySetting(ctx, settingUseQueryConditionCache, 1)
		// The condition cache is gated behind the analyzer; co-stamp
		// enable_analyzer=1 so the cache is honored even if an operator
		// disabled the analyzer. Result-equivalent and version-safe on the
		// >= 25.3 servers this rule resolves on.
		ctx = chclient.WithQuerySetting(ctx, settingEnableAnalyzer, 1)
	}
	if r.LogCommentShape {
		if id := planShapeID(plan); id != "" {
			ctx = chclient.WithQuerySetting(ctx, settingLogComment, id)
		}
	}
	if r.ResultCache && eligibleForResultCache(plan, r.now(), r.ResultCacheIngestLag) {
		ctx = chclient.WithResultCacheSetting(ctx, int64(r.ResultCacheTTL.Seconds()))
	}
	return ctx
}

// settingMaxThreads caps the number of concurrent ClickHouse read threads for a
// query. Each read thread holds its own column read buffer, so on S3-backed
// storage (large buffers) uncapped parallelism multiplies buffer RAM. It is
// RESULT-EQUIVALENT: it changes only how many lanes run concurrently, never the
// rows produced.
const settingMaxThreads = "max_threads"

// compareMaxThreads bounds the read parallelism of a TraceQL compare() query to
// keep it under the per-query memory budget on wide-attribute / S3-backed scans.
// compare() reads the wide ResourceAttributes / SpanAttributes Map columns; on
// S3-backed parts every read thread allocates its own large read buffer, so the
// concurrent buffers — not the GROUP BY hash table — are what push the query
// over budget once the aggregation already spills. Validated on prod ClickHouse
// 26.6: external-group-by spill at half the cap PLUS this thread cap completes a
// previously-OOMing compare() under a 2 GiB per-query cap (spill@1GiB + threads=4).
const compareMaxThreads = 4

// applyCompareMemoryBound stamps the two memory-bounding settings a TraceQL
// compare() query needs to stay under the per-query memory budget on
// wide-attribute, S3-backed scans. It fires ONLY when plan contains a
// chplan.MetricsCompare node, so plain queries keep full read parallelism and
// are byte-unchanged.
//
// The validated fix couples TWO result-equivalent knobs that must ride together
// for compare():
//
//   - max_bytes_before_external_group_by, sized at half the live per-query
//     memory cap via the same spillThreshold the unconditional spill uses, so
//     the compare() attribute-distribution GROUP BY spills to disk instead of
//     building its hash table unbounded in RAM.
//   - max_threads, capped at compareMaxThreads, so the concurrent S3 read
//     buffers for the wide Map columns can't multiply past the budget.
//
// Spill ALONE was proven insufficient on prod: read parallelism still peaked
// above the cap. Both knobs are RESULT-EQUIVALENT — external aggregation yields
// the same rows, and bounding threads only changes concurrency — so attaching
// them never changes the compare() result, only its peak memory.
func applyCompareMemoryBound(ctx context.Context, plan chplan.Node, maxMemory int64) context.Context {
	if !planHasMetricsCompare(plan) {
		return ctx
	}
	ctx = chclient.WithQuerySetting(ctx, settingMaxBytesBeforeExternalGroupBy, spillThreshold(maxMemory))
	ctx = chclient.WithQuerySetting(ctx, settingMaxThreads, compareMaxThreads)
	return ctx
}

// planHasMetricsCompare reports whether plan contains a chplan.MetricsCompare
// node anywhere in its tree — the lowered form of TraceQL's compare() operator.
//
// The sweep is chplan.WalkDeep so a compare() nested inside a plan subtree that
// hangs off an Expr slot still gets the spill + thread bound. Both settings are
// result-equivalent, so widening the match can only cost read parallelism on a
// query that reads the same wide Map columns; missing one is the OOM this bound
// exists to prevent.
func planHasMetricsCompare(plan chplan.Node) bool {
	found := false
	chplan.WalkDeep(plan, func(n chplan.Node) bool {
		if _, ok := n.(*chplan.MetricsCompare); ok {
			found = true
			return false
		}
		return true
	})
	return found
}

// applyNativeHistogramAnalyzerFix stamps enable_analyzer=0 on a query whose
// plan reaches the native (exponential) histogram merge/window-fold machinery
// — histogram_quantile.go's promHistogramKahanSum and the deeply-nested
// arrayFold/arrayMap/tupleElement trees it builds, shared by every
// histogram_quantile()/sum()/avg()/rate()/increase() lowering over an
// exponential histogram (histogram_quantile_native_window.go,
// histogram_native_sum.go).
//
// It exists because ClickHouse's newer query analyzer (the GA default since
// well before cerberus's 24.8 floor) has a cost on that specific shape —
// deeply nested lambda/arrayMap/arrayFold expressions — that is wildly
// superlinear relative to the OLDER analyzer, and almost entirely
// PRE-EXECUTION: `send_logs_level=trace` against a floor-pinned CH 24.8 shows
// the gap sits between `executeQuery` and the first `Planner` trace line, not
// in row processing. Measured on CH 24.8.14 with
// `histogram_quantile(0.9, sum(rate(demo_shifting_latency_exp_hist[1m])))`
// (the cerberus issue #2355 regression case), averaged over 8 runs each via
// system.query_log's OSCPUVirtualTimeMicroseconds: ~8.4s with the new
// analyzer (default) vs ~1.4s with it disabled — an 83% reduction, comfortably
// inside the compatibility harness's 10s per-query deadline where the default
// analyzer left only a ~1.5s margin under CI contention. Both setting names
// (`enable_analyzer` / `allow_experimental_analyzer`) alias the same underlying
// flag on every ClickHouse version cerberus supports, so the stamp is
// version-safe.
//
// It is deliberately NOT version-gated, and that is a measured decision rather
// than an untested convenience. #2358 spot-checked the same single-series query
// on CH 26.5 (0.57s new analyzer vs 1.2s old) and read the inversion as "a
// bounded, harmless trade on newer servers" — which invited a later removal
// once the 24.8 floor moves. Re-measuring on the shape production actually
// runs, at production cardinality, refutes that reading: the trade does not
// merely stay bounded on newer servers, it reverses. On chDB's embedded
// ClickHouse 26.x engine (the substrate every chdb-tagged lane runs on) with
// 500 distinct series under a one-label GROUP BY (the `cerb:project;agg=1;rbf`
// plan shape — histogram_quantile over rate() of a native histogram in a range
// query), three alternating rounds measured 12.2-14.5s with the new analyzer
// against 2.3-2.6s with it disabled: ~5x, in the SAME direction as the 24.8
// floor rather than against it. Peak memory moves the same way — bisecting
// max_memory_usage, both settings complete at 512 MiB and exceed 256 MiB, but
// at that failure the new analyzer had already reached 317 MiB against 286 MiB
// for the old, so it costs ~11% more, not less. EXPLAIN PLAN says why: the new
// analyzer emits MORE arrayMap nodes on this shape (391 vs 357), so its common-
// subexpression elimination is not collapsing the merge/window-fold's repeated
// array walks — the emitter's own bindings (promql's hqLet) are what do that.
//
// The 0.57s-vs-1.2s figure was real; it was just taken at a cardinality where
// the analyzer's per-call-site planning cost still dominated its per-row
// execution cost. So the standing instruction for whoever next touches this
// stamp — including on a floor bump that retires 24.8 — is: a single-series
// timing is not evidence about this shape. Re-measure at several hundred
// distinct series under a one-label GROUP BY, on the newest server in the
// supported range, and change the stamp only if THAT measurement says to.
//
// It is an execution-PLANNER choice, not a result rewrite — RESULT-EQUIVALENT
// like every other rule in this file — so it is unconditional (no CERBERUS_*
// opt-in) and always stamped when the plan shape matches, mirroring
// applyCompareMemoryBound's always-on treatment of a validated, plan-shape-
// gated fix rather than SettingsRules' operator-opt-in rules.
//
// Two OTHER levers were tried and measured flat before this one: hoisting
// promHistogramKahanSum's shared fold lambda into a query-level
// `WITH (kh_acc, kh_x) -> ... AS name` CTE referenced by all ~11 call sites
// (ClickHouse rejects resolving a LAMBDA-typed WITH alias across a subquery
// scope boundary — "Resolve identifier ... from parent scope only supported
// for constants and CTE"), and the SAME hoist scoped locally to the three
// same-statement call-site clusters (5/2/4 occurrences) that CAN share a
// local WITH — which parses and runs correctly, cuts rendered SQL by ~9%, but
// moved wall-clock/CPU time by less than run-to-run noise (measured via the
// same OSCPUVirtualTimeMicroseconds profile event). Neither reduces the
// ANALYZER's real per-call-site work; disabling the analyzer itself does.
func applyNativeHistogramAnalyzerFix(ctx context.Context, plan chplan.Node) context.Context {
	if !planHasNativeHistogramMerge(plan) {
		return ctx
	}
	return chclient.WithQuerySetting(ctx, settingEnableAnalyzer, 0)
}

// planHasNativeHistogramMerge reports whether plan contains a
// chplan.HistogramQuantileNative or chplan.HistogramProjection node anywhere
// in its tree — the two IR nodes exclusive to the exponential (native)
// histogram lowering (both carry the Scale/ZeroCount/PositiveOffset/
// NegativeOffset field set no classic-histogram or non-histogram node has),
// and the only roots internal/promql builds over histogram_quantile.go's
// shared merge/window-fold machinery. A bare native-histogram selector with no
// range function or cross-series aggregation still reaches
// HistogramQuantileNative / HistogramProjection, so this can over-match a
// cheap query — harmless, since the setting is result-equivalent either way.
//
// The sweep is chplan.WalkDeep, matching planHasMetricsCompare /
// planHasTSGridNative: a native-histogram node nested inside a scalar-binding
// subtree (an Expr slot Walk does not follow) must still be found.
func planHasNativeHistogramMerge(plan chplan.Node) bool {
	found := false
	chplan.WalkDeep(plan, func(n chplan.Node) bool {
		switch n.(type) {
		case *chplan.HistogramQuantileNative, *chplan.HistogramProjection:
			found = true
			return false
		}
		return true
	})
	return found
}

// eligibleForAggregationInOrder reports whether plan's single Aggregate has a
// GROUP BY that is a genuine bare-column prefix of its scanned table's
// sorting key. The check is deliberately conservative: it returns false on
// ANY shape it can't prove eligible, because a wrong stamp would change
// execution strategy on a query whose GROUP BY is NOT sort-key-aligned (still
// result-correct, but not the intended win, and a false signal to operators
// mining query_log).
//
// Eligibility requires ALL of:
//
//   - exactly one Aggregate node in the plan (the plain metrics/aggregation
//     shape). Zero Aggregates means nothing to order; multiple Aggregates
//     (nested second-stage rollups, compare ops) make "the" group key
//     ambiguous against a single sort key, so it bails.
//   - the Aggregate's GROUP BY is non-empty and every key is a BARE column
//     reference (chplan.ColumnRef with no Qualifier). A function-of-column
//     or a join-qualified key can't be matched against the bare-column sort
//     prefix, so any non-bare key disqualifies.
//   - the plan reads exactly ONE physical table (one Scan, no UnionTables,
//     no second Scan from a join). A union/join has no single sort key to be
//     a prefix of.
//   - the GROUP BY column-name sequence is an ordered prefix of that table's
//     schema SortingKeyPrefix.
func (r SettingsRules) eligibleForAggregationInOrder(plan chplan.Node) bool {
	agg, ok := singleAggregate(plan)
	if !ok {
		return false
	}
	groupCols, ok := bareGroupByColumns(agg)
	if !ok || len(groupCols) == 0 {
		return false
	}
	table, ok := singleScanTable(plan)
	if !ok {
		return false
	}
	sortKey := r.sortingKeyPrefixFor(table)
	return isOrderedPrefix(groupCols, sortKey)
}

// predicateStableForConditionCache reports whether plan is a read path the
// query condition cache can help: it must carry an actual WHERE predicate (a
// chplan.Filter node over a Scan) so there is a granule-selection result to
// cache and reuse on a later identical-predicate query. The cache is
// result-equivalent regardless, so this gate is purely about "is there a
// predicate worth caching"; it is deliberately conservative — a plan with no
// Filter (a bare full-table scan) gains nothing from the condition cache, so
// the setting is not stamped there. A union/multi-table plan still qualifies
// as long as it filters: the cache is keyed per data part, so it composes
// across the scanned tables without correctness risk.
//
// The whole rule is additionally gated upstream by ConditionCache, which only
// resolves in on ClickHouse >= 25.3, so this never fires on an older server.
func predicateStableForConditionCache(plan chplan.Node) bool {
	hasFilter := false
	hasScan := false
	chplan.Walk(plan, func(n chplan.Node) bool {
		switch n.(type) {
		case *chplan.Filter:
			hasFilter = true
		case *chplan.Scan:
			hasScan = true
		}
		return true
	})
	return hasFilter && hasScan
}

// singleAggregate returns the sole Aggregate in plan, or ok=false when there
// is none or more than one.
func singleAggregate(plan chplan.Node) (*chplan.Aggregate, bool) {
	var found *chplan.Aggregate
	count := 0
	chplan.Walk(plan, func(n chplan.Node) bool {
		if a, ok := n.(*chplan.Aggregate); ok {
			found = a
			count++
		}
		return true
	})
	if count != 1 {
		return nil, false
	}
	return found, true
}

// bareGroupByColumns returns the GROUP BY keys of agg as bare column names,
// in order. ok is false (and the slice nil) if ANY key is not a bare,
// unqualified chplan.ColumnRef.
func bareGroupByColumns(agg *chplan.Aggregate) (cols []string, ok bool) {
	cols = make([]string, 0, len(agg.GroupBy))
	for _, e := range agg.GroupBy {
		ref, isRef := e.(*chplan.ColumnRef)
		if !isRef || ref.Qualifier != "" {
			return nil, false
		}
		cols = append(cols, ref.Name)
	}
	return cols, true
}

// singleScanTable returns the one physical table the plan scans, or ok=false
// when there is not exactly one (zero Scans, a multi-table union, or two
// Scans from a join).
// ineligibleMarker poisons the scan count so the final count != 1
// guard rejects the plan (a union / empty-table scan has no single
// sort key to be a prefix of).
const ineligibleMarker = -1

func singleScanTable(plan chplan.Node) (table string, ok bool) {
	count := 0
	chplan.Walk(plan, func(n chplan.Node) bool {
		s, isScan := n.(*chplan.Scan)
		if !isScan {
			return true
		}
		// A union scan has no single sort key to be a prefix of.
		if len(s.UnionTables) > 0 || s.Table == "" {
			count = ineligibleMarker
			return false
		}
		table = s.Table
		count++
		return true
	})
	if count != 1 {
		return "", false
	}
	return table, true
}

// sortingKeyPrefixFor maps a scanned table name to its bare-column
// sorting-key prefix using the configured schema. An unknown table (renamed,
// or a table cerberus doesn't model the sort key for) returns nil, so the
// prefix check fails closed and the setting is not stamped.
func (r SettingsRules) sortingKeyPrefixFor(table string) []string {
	switch table {
	case r.Metrics.GaugeTable, r.Metrics.SumTable, r.Metrics.HistogramTable,
		r.Metrics.ExpHistogramTable, r.Metrics.SummaryTable:
		return r.Metrics.SortingKeyPrefix()
	case r.Traces.SpansTable:
		return r.Traces.SortingKeyPrefix()
	case r.Logs.LogsTable:
		return r.Logs.SortingKeyPrefix()
	default:
		return nil
	}
}

// eligibleForResultCache reports whether plan's evaluated windows have ALL
// fully CLOSED as of now, so ClickHouse's query result cache
// (use_query_cache) can safely serve a cached answer for it without ever
// risking a stale live-edge tail. This is cerberus's OWN correctness guard —
// not merely a convenience layered on top of ClickHouse's own
// query_cache_nondeterministic_function_handling, which the issue this
// implements (#2781) explicitly calls out as defense-in-depth rather than the
// primary gate.
//
// Eligibility requires ALL of:
//
//   - at least one range-mode (Step > 0) chplan.GridCarrier anywhere in the
//     plan — the query_range shape the result cache targets. A plan with
//     none (an instant query, or a metadata-only plan with no eval grid at
//     all — internal/api/prom's /api/v1/labels, /api/v1/series, and similar
//     metadata routes dispatch straight through chclient.Client.Query /
//     QueryStrings, never through the engine's execute seam this rule lives
//     on, so they never even reach this function) is conservatively NOT
//     eligible: there is no window to prove closed.
//   - EVERY range-mode carrier's Start and End are concrete, non-zero
//     timestamps. The zero time.Time is the codebase-wide sentinel for
//     "resolve at emit time" (chplan.checkPredictedGrid and siblings in
//     reanchor.go all test Start.IsZero() && End.IsZero() the same way) —
//     nativeGridTimeBoundFrag and timeOrNowFrag
//     (internal/chsql/range_window.go) both render a bare `now()` /
//     `now64(9)` SQL call for a zero Start/End, so a zero-time carrier's
//     window is NOT fixed at all: it resolves to whenever ClickHouse happens
//     to execute the query, which is exactly the live-edge case this gate
//     exists to exclude.
//   - EVERY range-mode carrier's End is strictly before now minus ingestLag:
//     the window has fully closed and the deployment's own ingest pipeline
//     has had time to land every row for it.
//
// Defense in depth, verifying EXHAUSTIVELY rather than assuming: even after
// the above, planHasNowExpr additionally rejects a plan carrying a literal
// now()/now64() FuncCall ANYWHERE in its Expr tree. PromQL's time() builtin
// lowers to exactly that shape (internal/promql/synthetic.go's lowerTime)
// before the range-mode synthetic-vector rewrite (rewriteAnchorRefs)
// replaces it with an anchor_ts column reference — so this second,
// independent leg catches any residual non-deterministic expression the grid
// check above cannot see, rather than trusting that every lowering path
// already runs that rewrite.
func eligibleForResultCache(plan chplan.Node, now time.Time, ingestLag time.Duration) bool {
	threshold := now.Add(-ingestLag)
	foundRangeCarrier := false
	closed := true
	chplan.WalkDeep(plan, func(n chplan.Node) bool {
		gc, ok := n.(chplan.GridCarrier)
		if !ok {
			return true
		}
		start, end, step := gc.EvalGrid()
		if step <= 0 {
			// Instant-mode carrier: Start/End carry no request-grid meaning
			// (chplan.GridCarrier's own doc), so it says nothing about
			// whether the query's window is closed.
			return true
		}
		foundRangeCarrier = true
		if start.IsZero() || end.IsZero() || !end.Before(threshold) {
			closed = false
			return false
		}
		return true
	})
	if !foundRangeCarrier || !closed {
		return false
	}
	return !planHasNowExpr(plan)
}

// planHasNowExpr reports whether plan carries a literal now()/now64()
// FuncCall anywhere in ANY node's Expr tree — the two chplan function ids
// (chplan.FnNow, chplan.FnNow64) that ever render a non-deterministic
// ClickHouse call. internal/chplan/fn.go has no other nondeterministic
// entry point reachable from PromQL/LogQL/TraceQL lowering (no rand/uuid/…
// function id exists in the registry at all), so these two exhaust the
// search — verified by inspection of chplan's Fn constant table, not merely
// assumed.
//
// The walk composes chplan.WalkDeep (every Node reachable through Children()
// AND every plan subtree embedded in an Expr slot, e.g. a ScalarSubquery)
// with chplan.InspectNodeExprs + chplan.InspectExpr (every sub-expression of
// each node's own Expr-typed fields), so it is exhaustive over every
// expression anywhere in the plan — not just the ones the GridCarrier check
// in eligibleForResultCache happens to visit.
func planHasNowExpr(plan chplan.Node) bool {
	found := false
	chplan.WalkDeep(plan, func(n chplan.Node) bool {
		if found {
			return false
		}
		chplan.InspectNodeExprs(n, func(e chplan.Expr) {
			if found {
				return
			}
			chplan.InspectExpr(e, func(sub chplan.Expr) bool {
				if found {
					return false
				}
				if fc, ok := sub.(*chplan.FuncCall); ok && (fc.Fn == chplan.FnNow || fc.Fn == chplan.FnNow64) {
					found = true
					return false
				}
				return true
			})
		})
		return true
	})
	return found
}

// isOrderedPrefix reports whether group is a non-empty ordered prefix of
// sortKey: len(group) <= len(sortKey) and group[i] == sortKey[i] for all i.
// An empty group or an empty sortKey is never a valid prefix here.
func isOrderedPrefix(group, sortKey []string) bool {
	if len(group) == 0 || len(group) > len(sortKey) {
		return false
	}
	for i := range group {
		if group[i] != sortKey[i] {
			return false
		}
	}
	return true
}
