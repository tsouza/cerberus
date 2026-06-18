package engine

import (
	"context"

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

	// LogCommentShape, when true, stamps log_comment with a compact cerberus
	// shape id (planShapeID) carrying the emit-root node kind plus key
	// modifiers and NEVER any literal values, so operators with query_log
	// enabled can cluster by normalized_query_hash + log_comment.
	LogCommentShape bool

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
	return opts
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
	return ctx
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
func singleScanTable(plan chplan.Node) (table string, ok bool) {
	count := 0
	chplan.Walk(plan, func(n chplan.Node) bool {
		s, isScan := n.(*chplan.Scan)
		if !isScan {
			return true
		}
		// A union scan has no single sort key to be a prefix of.
		if len(s.UnionTables) > 0 || s.Table == "" {
			count = -1 // poison: force ineligible
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
