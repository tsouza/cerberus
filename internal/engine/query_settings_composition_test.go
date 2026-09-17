package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// settingsContributor is ONE rule of the shared dispatch seam, run in
// isolation: only its own flag is on, and only its own stamps land on the
// returned ctx. The composition test below compares the union of every
// contributor's isolated map against what applySharedQuerySettings actually
// stamps, which is how a last-write-wins collision on one key between two
// rules becomes visible — chclient.WithQuerySetting overwrites silently, so
// the composed map alone can never show which rule lost.
type settingsContributor struct {
	name  string
	apply func(ctx context.Context, plan chplan.Node, memCap int64, rules SettingsRules) context.Context
}

// settingsRuleFlag names one boolean SettingsRules flag and knows how to set
// it on a copy, so the test can enumerate EVERY flag combination rather than
// the handful the per-rule tests pick by hand.
type settingsRuleFlag struct {
	name string
	set  func(r *SettingsRules)
}

// compositionRuleFlags is every operator-facing SettingsRules switch. The
// contributor list below is derived from it, so a flag added to SettingsRules
// without a row here fails TestSharedQuerySettings_EveryFlagIsEnumerated.
var compositionRuleFlags = []settingsRuleFlag{
	{"OptimizeAggregationInOrder", func(r *SettingsRules) { r.OptimizeAggregationInOrder = true }},
	{"ConditionCache", func(r *SettingsRules) { r.ConditionCache = true }},
	{"JoinSpill", func(r *SettingsRules) { r.JoinSpill = true }},
	{"ExpHistogramTwoLevel", func(r *SettingsRules) { r.ExpHistogramTwoLevel = true }},
	{"TraceIDBitmapFilter", func(r *SettingsRules) { r.TraceIDBitmapFilter = true }},
	{"LogCommentShape", func(r *SettingsRules) { r.LogCommentShape = true }},
	{"ResultCache", func(r *SettingsRules) { r.ResultCache = true }},
	{"LazyMaterialization", func(r *SettingsRules) { r.LazyMaterialization = true }},
	{"QueryWorkload", func(r *SettingsRules) { r.QueryWorkload = "cerberus_queries" }},
}

// compositionBaseRules is the flag-free SettingsRules every enumerated
// combination starts from: the default schema (so the schema-reading
// predicates run against live sort keys), a fixed clock and a closed-window
// horizon (so the result-cache predicate is decidable), and a TTL.
func compositionBaseRules() SettingsRules {
	return SettingsRules{
		Metrics:              schema.DefaultOTelMetrics(),
		Traces:               schema.DefaultOTelTraces(),
		Logs:                 schema.DefaultOTelLogs(),
		Now:                  func() time.Time { return fixedNow },
		ResultCacheIngestLag: testIngestLag,
		ResultCacheTTL:       10 * time.Minute,
	}
}

// onlyFlag returns compositionBaseRules with exactly one flag set.
func onlyFlag(f settingsRuleFlag) SettingsRules {
	r := compositionBaseRules()
	f.set(&r)
	return r
}

// compositionContributors lists every rule applySharedQuerySettings composes,
// each runnable on its own. The unconditional / feature-gated bounds are
// called directly; every SettingsRules flag runs through apply with ONLY that
// flag on, on a rules value whose other flags are all off, so the map it
// returns is that one rule's contribution and nothing else.
func compositionContributors() []settingsContributor {
	contributors := []settingsContributor{
		{"spill", func(ctx context.Context, _ chplan.Node, memCap int64, _ SettingsRules) context.Context {
			return applySpillSettings(ctx, memCap)
		}},
		{"join_spill", func(ctx context.Context, plan chplan.Node, memCap int64, rules SettingsRules) context.Context {
			return applyJoinSpillSettings(ctx, plan, memCap, rules.JoinSpill)
		}},
		{"compare_memory_bound", func(ctx context.Context, plan chplan.Node, memCap int64, _ SettingsRules) context.Context {
			return applyCompareMemoryBound(ctx, plan, memCap)
		}},
		{"native_histogram_analyzer_fix", func(ctx context.Context, plan chplan.Node, _ int64, _ SettingsRules) context.Context {
			return applyNativeHistogramAnalyzerFix(ctx, planHasNativeHistogramAnalyzerHazard(plan))
		}},
		{"sorted_slab_memory_bound", func(ctx context.Context, plan chplan.Node, _ int64, _ SettingsRules) context.Context {
			return applySortedSlabOverTimeMemoryBound(ctx, plan)
		}},
		{"exp_histogram_two_level", func(ctx context.Context, plan chplan.Node, _ int64, rules SettingsRules) context.Context {
			return applyExpHistogramTwoLevelBound(ctx, plan, rules.ExpHistogramTwoLevel)
		}},
	}
	for _, f := range compositionRuleFlags {
		f := f
		switch f.name {
		case "JoinSpill", "ExpHistogramTwoLevel":
			// Applied by the dedicated bound above, not by SettingsRules.apply.
			continue
		}
		contributors = append(contributors, settingsContributor{
			name: "rules." + f.name,
			apply: func(ctx context.Context, plan chplan.Node, _ int64, rules SettingsRules) context.Context {
				if !flagIsSet(rules, f) {
					return ctx
				}
				return onlyFlag(f).apply(ctx, plan)
			},
		})
	}
	return contributors
}

// flagIsSet reports whether f is on in rules by setting it on a zero value
// and comparing the one field that changed — it keeps the contributor table
// free of a second hand-written per-flag reader.
func flagIsSet(rules SettingsRules, f settingsRuleFlag) bool {
	var probe SettingsRules
	f.set(&probe)
	switch f.name {
	case "OptimizeAggregationInOrder":
		return rules.OptimizeAggregationInOrder == probe.OptimizeAggregationInOrder
	case "ConditionCache":
		return rules.ConditionCache == probe.ConditionCache
	case "JoinSpill":
		return rules.JoinSpill == probe.JoinSpill
	case "ExpHistogramTwoLevel":
		return rules.ExpHistogramTwoLevel == probe.ExpHistogramTwoLevel
	case "TraceIDBitmapFilter":
		return rules.TraceIDBitmapFilter == probe.TraceIDBitmapFilter
	case "LogCommentShape":
		return rules.LogCommentShape == probe.LogCommentShape
	case "ResultCache":
		return rules.ResultCache == probe.ResultCache
	case "LazyMaterialization":
		return rules.LazyMaterialization == probe.LazyMaterialization
	case "QueryWorkload":
		return rules.QueryWorkload == probe.QueryWorkload
	}
	panic("flagIsSet: unknown flag " + f.name)
}

// compositionPlans is the plan-shape table: one plan per predicate the seam
// reads, plus the plans where two predicates match at once — which is where
// two rules can reach for the same setting key.
func compositionPlans() []struct {
	name string
	plan chplan.Node
} {
	// A closed range grid: End sits before fixedNow - testIngestLag, so the
	// result-cache predicate is satisfied on any carrier that wears it.
	closedStart, closedEnd := fixedNow.Add(-2*time.Hour), fixedNow.Add(-10*time.Minute)
	return []struct {
		name string
		plan chplan.Node
	}{
		{"bare scan", routeBTestPlan()},
		{"filter over scan", filterScan("otel_metrics_sum")},
		{"aggregate on sort-key prefix", aggOverScan("otel_metrics_sum", "MetricName")},
		{"closed-window range over filter", &chplan.RangeWindow{
			Input: filterScan("otel_metrics_sum"), Start: closedStart, End: closedEnd, Step: time.Minute,
		}},
		{"trace-id equality", traceIDEqualityFilter()},
		{"limit over order-by", tempoSearchRecentPlan(20)},
		{"compare", compareOverScan()},
		{"sorted slab", &chplan.RangeWindow{Input: filterScan("otel_metrics_sum"), SortedSlabOverTime: true}},
		{"vector join", &chplan.VectorJoin{Left: filterScan("otel_metrics_sum"), Right: filterScan("otel_metrics_gauge")}},
		{"exp-histogram window", expHistogramWindowPlan()},
		{"native histogram quantile over filter", &chplan.HistogramQuantileNative{
			Input: &chplan.RangeBucketFanout{Input: filterScan("otel_metrics_exponential_histogram")},
		}},
		{"exp-histogram value-fn fan-out over filter", &chplan.RangeBucketFanout{
			Input: &chplan.Filter{
				Input: &chplan.Scan{
					Table: "otel_metrics_exponential_histogram",
					Roles: []chplan.Column{{Name: "Scale", Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldScale}},
				},
				Predicate: &chplan.LitString{V: "x"},
			},
			AggFuncs: []chplan.AggFunc{{
				Fn:    chplan.FnArgMax,
				Args:  []chplan.Expr{&chplan.ColumnRef{Name: "Scale"}, &chplan.ColumnRef{Name: "TimeUnix"}},
				Alias: "Scale",
			}},
		}},
		{"limit over order-by over native histogram", &chplan.Limit{
			Count: 20,
			Input: &chplan.OrderBy{
				Input: &chplan.HistogramQuantileNative{
					Input: &chplan.RangeBucketFanout{Input: filterScan("otel_metrics_exponential_histogram")},
				},
				Keys: []chplan.OrderKey{{Expr: &chplan.ColumnRef{Name: "Value"}, Desc: true}},
			},
		}},
		{"closed-window native histogram", &chplan.HistogramQuantileNative{
			Input: &chplan.RangeBucketFanout{
				Input: filterScan("otel_metrics_exponential_histogram"),
				Start: closedStart, End: closedEnd, Step: time.Minute,
			},
		}},
	}
}

// TestSharedQuerySettings_EveryKeyResolvesToOneValue enumerates every
// SettingsRules flag combination over every plan shape and, for each, runs
// every rule of the seam in isolation. It fails when two rules write the same
// ClickHouse setting key with different values on the same query — the
// last-write-wins collision chclient.WithQuerySetting cannot report — and
// when the composed map differs from the union of the contributions (a rule
// dropped from, or duplicated in, applySharedQuerySettings).
//
// The collision it was written for: on a native-histogram plan carrying a
// Filter, applyNativeHistogramAnalyzerFix stamps enable_analyzer=0 while the
// condition-cache rule co-stamped enable_analyzer=1, and the rule ran last —
// so the measured 5x analyzer fix never reached ClickHouse on any deployment
// whose server resolved the condition_cache feature in (>= 25.3, the
// default). The same collision sat on the lazy-materialisation co-stamp.
func TestSharedQuerySettings_EveryKeyResolvesToOneValue(t *testing.T) {
	t.Parallel()

	contributors := compositionContributors()
	plans := compositionPlans()
	combos := 1 << len(compositionRuleFlags)

	for _, p := range plans {
		p := p
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			for mask := 0; mask < combos; mask++ {
				rules := compositionBaseRules()
				var on []string
				for i, f := range compositionRuleFlags {
					if mask&(1<<i) != 0 {
						f.set(&rules)
						on = append(on, f.name)
					}
				}
				label := strings.Join(on, "+")
				if label == "" {
					label = "no flags"
				}

				// writers[key] = "contributor=value" for every contributor
				// that stamped key; a key with two distinct values is the
				// collision.
				writers := map[string]map[string]string{}
				union := map[string]any{}
				for _, c := range contributors {
					got := chclient.QuerySettingsFromContext(c.apply(context.Background(), p.plan, testQueryMemoryCap, rules))
					for k, v := range got {
						if writers[k] == nil {
							writers[k] = map[string]string{}
						}
						writers[k][c.name] = fmt.Sprint(v)
						union[k] = v
					}
				}
				for key, byRule := range writers {
					values := map[string][]string{}
					for rule, v := range byRule {
						values[v] = append(values[v], rule)
					}
					if len(values) > 1 {
						t.Errorf("%s / [%s]: %s is written with %d distinct values — %s; the last rule to run wins silently",
							p.name, label, key, len(values), describeWriters(values))
					}
				}

				composed := chclient.QuerySettingsFromContext(
					applySharedQuerySettings(context.Background(), p.plan, testQueryMemoryCap, rules),
				)
				if len(composed) != len(union) {
					t.Errorf("%s / [%s]: composed map has %d keys, the union of contributions has %d\n composed: %v\n union:    %v",
						p.name, label, len(composed), len(union), composed, union)
				}
				for k, v := range union {
					if got, ok := composed[k]; !ok || fmt.Sprint(got) != fmt.Sprint(v) {
						t.Errorf("%s / [%s]: composed[%s] = %v, a contributor wrote %v", p.name, label, k, got, v)
					}
				}
			}
		})
	}
}

// describeWriters renders {value: [rules]} deterministically for a failure
// message.
func describeWriters(values map[string][]string) string {
	parts := make([]string, 0, len(values))
	for v, rules := range values {
		sort.Strings(rules)
		parts = append(parts, fmt.Sprintf("%s by %s", v, strings.Join(rules, ",")))
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}

// TestSharedQuerySettings_AnalyzerFixWinsOverCoStamps pins the outcome the
// enumeration above only reports as a collision: on a native-histogram plan
// the composed ctx carries enable_analyzer=0 whatever else is enabled, and
// the two analyzer-gated rules (condition cache, lazy materialisation) stamp
// nothing — they are inert under enable_analyzer=0 anyway, so a stamp would
// only misreport the query's posture in system.query_log.
func TestSharedQuerySettings_AnalyzerFixWinsOverCoStamps(t *testing.T) {
	t.Parallel()

	rules := compositionBaseRules()
	rules.ConditionCache = true
	rules.LazyMaterialization = true
	plan := &chplan.Limit{
		Count: 20,
		Input: &chplan.OrderBy{
			Input: &chplan.HistogramQuantileNative{
				Input: &chplan.RangeBucketFanout{Input: filterScan("otel_metrics_exponential_histogram")},
			},
			Keys: []chplan.OrderKey{{Expr: &chplan.ColumnRef{Name: "Value"}, Desc: true}},
		},
	}

	got := chclient.QuerySettingsFromContext(applySharedQuerySettings(context.Background(), plan, testQueryMemoryCap, rules))
	if v := got[settingEnableAnalyzer]; v != 0 {
		t.Errorf("enable_analyzer = %v, want 0: the native-histogram analyzer fix must win over the condition-cache and lazy-materialisation co-stamps", v)
	}
	for _, key := range []string{settingUseQueryConditionCache, settingQueryPlanOptimizeLazyMaterialization, settingQueryPlanMaxLimitForLazyMaterialization} {
		if v, ok := got[key]; ok {
			t.Errorf("%s = %v stamped on a plan running under enable_analyzer=0; the setting is analyzer-gated and inert there", key, v)
		}
	}

	// The same rules on a plan WITHOUT the hazard still fire — the skip is
	// keyed on the hazard, not on the flags.
	got = chclient.QuerySettingsFromContext(applySharedQuerySettings(context.Background(), tempoSearchRecentPlan(20), testQueryMemoryCap, rules))
	if v := got[settingEnableAnalyzer]; v != 1 {
		t.Errorf("no hazard: enable_analyzer = %v, want 1 (the co-stamp)", v)
	}
	if v := got[settingUseQueryConditionCache]; v != 1 {
		t.Errorf("no hazard: use_query_condition_cache = %v, want 1", v)
	}
	if v := got[settingQueryPlanOptimizeLazyMaterialization]; v != 1 {
		t.Errorf("no hazard: query_plan_optimize_lazy_materialization = %v, want 1", v)
	}
}

// TestSharedQuerySettings_EveryFlagIsEnumerated keeps compositionRuleFlags
// honest: every exported switch on SettingsRules that enabledOpts reports, or
// that apply reads, must have a row, or the enumeration silently stops
// covering it.
func TestSharedQuerySettings_EveryFlagIsEnumerated(t *testing.T) {
	t.Parallel()

	all := compositionBaseRules()
	for _, f := range compositionRuleFlags {
		f.set(&all)
	}
	wantOpts := all.enabledOpts()
	seen := map[string]bool{}
	for _, f := range compositionRuleFlags {
		for _, opt := range onlyFlag(f).enabledOpts() {
			seen[opt] = true
		}
	}
	for _, opt := range wantOpts {
		if !seen[opt] {
			t.Errorf("enabledOpts reports %q but no single compositionRuleFlags row produces it", opt)
		}
	}
	// LogCommentShape has no enabledOpts id; it is covered by its row above.
	if len(compositionRuleFlags) != len(wantOpts)+1 {
		t.Errorf("compositionRuleFlags has %d rows, enabledOpts reports %d ids (+1 for LogCommentShape): a SettingsRules flag is missing a row",
			len(compositionRuleFlags), len(wantOpts))
	}
}
