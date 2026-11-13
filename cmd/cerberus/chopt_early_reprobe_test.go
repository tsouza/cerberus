package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chopt"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/config"
	"github.com/tsouza/cerberus/internal/engine"
	"github.com/tsouza/cerberus/internal/schema"
)

// Builds either side of a rolling rollback the docs support: the fleet ran
// 26.8, and one replica rolls back to 26.6.
var (
	rollbackFromBuild = chopt.Version{Major: 26, Minor: 8, Patch: 10, Build: 6}
	rollbackToBuild   = chopt.Version{Major: 26, Minor: 6, Patch: 8, Build: 7}
)

// rejectionBurst is how many queries a dashboard refresh lands on the
// rolled-back replica before the early probe runs.
const rejectionBurst = 64

// expHistogramRateQuantilePlan is the plan of an instant
// histogram_quantile over rate() of an exponential histogram: the window
// enable_adaptive_aggregator=0 is stamped on, under the quantile node that
// carries the native-histogram analyzer hazard.
func expHistogramRateQuantilePlan() chplan.Node {
	return &chplan.HistogramQuantileNative{Input: &chplan.Aggregate{
		Input: &chplan.Scan{
			Table: "otel_metrics_exponential_histogram",
			Roles: []chplan.Column{
				{Name: "TimeUnix", Role: chplan.RoleTimestamp},
				{Name: "PositiveBucketCounts", Role: chplan.RoleHistogramField, HistogramField: chplan.HistogramFieldPositiveBucketCounts},
				{Name: "AggregationTemporality", Role: chplan.RoleTemporality},
			},
		},
		GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		AggFuncs: []chplan.AggFunc{
			{Fn: chplan.FnGroupArray, Args: []chplan.Expr{&chplan.ColumnRef{Name: "PositiveBucketCounts"}}, Alias: "_hq_pos_buckets"},
			{Fn: chplan.FnGroupArray, Args: []chplan.Expr{&chplan.ColumnRef{Name: "TimeUnix"}}, Alias: "_hq_ts_list"},
			{Fn: chplan.FnAny, Args: []chplan.Expr{&chplan.ColumnRef{Name: "AggregationTemporality"}}, Alias: "_hq_temporality"},
		},
	}}
}

// stampedSettings is what the engine stamps on plan under the rules set
// implies for cfg.
func stampedSettings(cfg config.Config, set chopt.EnabledSet, plan chplan.Node) map[string]any {
	return engine.ProbeQuerySettings(plan, settingsRules(cfg, set), 0, chsql.SchemaStrategies{}).Settings
}

// TestReprobeCHOptimizations_RollbackRejectionBurstProbesOnce is a rolling
// rollback from 26.8 to 26.6 seen from the re-probe. The set in force was
// resolved against 26.8, so the engine stamps enable_adaptive_aggregator=0 and
// withholds the native-histogram enable_analyzer=0 stamp. A burst of queries
// lands on the rolled-back node, which rejects the setting, and each
// rejection asks for an early probe through the hook chclient calls.
//
// The burst must cause exactly one probe, long before the steady interval,
// and that probe must re-resolve against 26.6: enable_adaptive_aggregator
// leaves the plan and the enable_analyzer=0 stamp comes back, because
// NativeHistogramAnalyzerFaster no longer holds for the fleet.
func TestReprobeCHOptimizations_RollbackRejectionBurstProbesOnce(t *testing.T) {
	t.Parallel()

	cfg := config.Config{
		CHOptimizations:     chopt.SelectionAuto,
		CHOptimizationsMode: chopt.Permissive,
		Schema:              schema.DefaultOTelMetrics(),
	}
	cfg.ClickHouse.Addr = unreachableAddr(t)
	cfg.ClickHouse.DialTimeout = 100 * time.Millisecond

	bootSet, _, err := chopt.Resolve(chopt.Config{Optimizations: cfg.CHOptimizations, Mode: cfg.CHOptimizationsMode}, rollbackFromBuild)
	if err != nil {
		t.Fatalf("resolve auto against %s: %v", rollbackFromBuild, err)
	}
	plan := expHistogramRateQuantilePlan()
	before := stampedSettings(cfg, bootSet, plan)
	if _, ok := before[chopt.SettingEnableAdaptiveAggregator]; !ok {
		t.Fatalf("premise: on %s the plan carries %s; got %v", rollbackFromBuild, chopt.SettingEnableAdaptiveAggregator, before)
	}
	if _, ok := before["enable_analyzer"]; ok {
		t.Fatalf("premise: on %s the enable_analyzer=0 stamp is withheld; got %v", rollbackFromBuild, before)
	}
	live := newCHOptLive(chOptResolution{Set: bootSet, ResolvedVersion: rollbackFromBuild})

	var probes atomic.Int32
	probe := func(context.Context) chclient.FleetVersions {
		probes.Add(1)
		return chclient.FleetVersions{Versions: []chopt.Version{rollbackFromBuild, rollbackToBuild}, Complete: true}
	}
	// The steady interval outlives the test: only an early request can
	// probe. The spacing is long enough that the whole burst lands before the
	// probe it asks for.
	timing := reprobeTiming{steady: time.Hour, earlySpacing: 250 * time.Millisecond}
	early := newReprobeTrigger()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		reprobeCHOptimizations(ctx, quietLogger(), cfg, live, chOptConsumers{}, timing, early, "", probe)
	}()

	var burst sync.WaitGroup
	for range rejectionBurst {
		burst.Go(func() {
			early.request(chclient.UnknownNameRejection{Kind: "setting", Name: chopt.SettingEnableAdaptiveAggregator, Retried: true})
		})
	}
	burst.Wait()

	deadline := time.Now().Add(20 * time.Second)
	for live.get().ResolvedVersion != rollbackToBuild {
		if time.Now().After(deadline) {
			t.Fatalf("no early re-probe re-resolved against the rolled-back node (probes=%d): %+v", probes.Load(), live.get())
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Room for any further probe the burst could have caused.
	time.Sleep(4 * timing.earlySpacing)
	if got := probes.Load(); got != 1 {
		t.Errorf("probes after a burst of %d rejections = %d; want exactly 1", rejectionBurst, got)
	}

	after := live.get().Set
	if after.Has(chopt.FeatureHistogramAdaptiveAggregatorOff) || after.NativeHistogramAnalyzerFaster() {
		t.Errorf("re-resolved set on a fleet holding %s: %s in=%v, NativeHistogramAnalyzerFaster=%v; want both off",
			rollbackToBuild, chopt.FeatureHistogramAdaptiveAggregatorOff, after.Has(chopt.FeatureHistogramAdaptiveAggregatorOff), after.NativeHistogramAnalyzerFaster())
	}
	stamped := stampedSettings(cfg, after, plan)
	if _, ok := stamped[chopt.SettingEnableAdaptiveAggregator]; ok {
		t.Errorf("after the early re-probe the plan still carries %s: %v", chopt.SettingEnableAdaptiveAggregator, stamped)
	}
	if got, ok := stamped["enable_analyzer"]; !ok || got != 0 {
		t.Errorf("after the early re-probe enable_analyzer = %v (present %v); want the 0 stamp back for the 26.6 node", got, ok)
	}

	cancel()
	<-done
}

// TestReprobeTrigger_RequestNeverBlocks: the hook runs on the rejected
// query's goroutine, so a full trigger drops the request instead of waiting.
func TestReprobeTrigger_RequestNeverBlocks(t *testing.T) {
	t.Parallel()
	early := newReprobeTrigger()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for range rejectionBurst {
			early.request(chclient.UnknownNameRejection{Kind: "function", Name: "argAndMax"})
		}
	}()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("request blocked with no loop draining the trigger")
	}
	if got := len(early); got != 1 {
		t.Errorf("pending requests = %d; want 1", got)
	}
}
