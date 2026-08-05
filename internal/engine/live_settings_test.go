package engine

import (
	"context"
	"sync"
	"testing"

	"github.com/tsouza/cerberus/internal/schema"
)

// Layer 2 — the per-query settings rules an engine evaluates are LIVE state.
// They are derived from the ClickHouse capability set, and that set is
// re-resolved while the process runs, so a rule that became available on an
// upgraded server has to reach the execute seam without a restart.

// TestSettings_FallsBackToTheWiredRulesUntilSwapped — an engine nobody has
// swapped reads exactly the rules it was constructed with, so the live seam is
// invisible until it is used.
func TestSettings_FallsBackToTheWiredRulesUntilSwapped(t *testing.T) {
	t.Parallel()

	e := &Engine{Settings: defaultRules()}
	if got := e.settings(); !got.OptimizeAggregationInOrder || !got.LogCommentShape {
		t.Fatalf("settings() = %+v; want the wired rules", got)
	}
	if got := (&Engine{}).settings(); got.OptimizeAggregationInOrder || got.ConditionCache || got.LogCommentShape {
		t.Errorf("settings() on a bare engine = %+v; want the zero rules", got)
	}
}

// TestSetSettings_ChangesWhatAQueryStamps is the activation gate: swapping the
// rules must change the settings a dispatched query carries, not merely the
// value of a field. A seam that stores the new rules but leaves the execute
// path reading the boot copy passes every structural check while doing
// nothing, which is exactly the failure this pins.
func TestSetSettings_ChangesWhatAQueryStamps(t *testing.T) {
	t.Parallel()

	plan := aggOverScan("otel_metrics_sum", "MetricName")
	// Boot posture: a server below the feature's floor resolves the rule OUT,
	// so the engine stamps nothing.
	e := &Engine{Settings: SettingsRules{Metrics: schema.DefaultOTelMetrics()}}

	before := e.settings().apply(context.Background(), plan)
	if got := settingValue(before, settingOptimizeAggregationInOrder); got != nil {
		t.Fatalf("before the swap: setting = %v; want absent", got)
	}

	// The re-probe finds an upgraded server and installs the rules it now allows.
	e.SetSettings(defaultRules())

	after := e.settings().apply(context.Background(), plan)
	if got := settingValue(after, settingOptimizeAggregationInOrder); got != 1 {
		t.Errorf("after the swap: setting = %v; want 1 — the swapped rules never reached the execute seam", got)
	}
	if got := settingValue(after, settingLogComment); got == nil {
		t.Errorf("after the swap: %s absent; want the plan shape id", settingLogComment)
	}
}

// TestSetSettings_SwapIsReversible — a re-probe against a downgraded (or
// rolled-back) server must be able to take a rule away again, or a transient
// upgrade would pin the process to settings its server no longer supports.
func TestSetSettings_SwapIsReversible(t *testing.T) {
	t.Parallel()

	plan := aggOverScan("otel_metrics_sum", "MetricName")
	e := &Engine{Settings: defaultRules()}

	e.SetSettings(SettingsRules{Metrics: schema.DefaultOTelMetrics()})
	ctx := e.settings().apply(context.Background(), plan)
	if got := settingValue(ctx, settingOptimizeAggregationInOrder); got != nil {
		t.Errorf("after the rollback: setting = %v; want absent", got)
	}
}

// TestSetSettings_ConcurrentWithReads — the re-probe goroutine swaps while
// request goroutines read. Under -race this fails on any non-atomic seam, and
// every read must observe a whole rules value rather than a mixture of two.
func TestSetSettings_ConcurrentWithReads(t *testing.T) {
	t.Parallel()

	e := &Engine{Settings: defaultRules()}
	const rounds = 200

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			if i%2 == 0 {
				e.SetSettings(defaultRules())
			} else {
				e.SetSettings(SettingsRules{Metrics: schema.DefaultOTelMetrics()})
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			// LogCommentShape rides with OptimizeAggregationInOrder in both
			// values above, so a torn read would show them disagreeing.
			got := e.settings()
			if got.OptimizeAggregationInOrder != got.LogCommentShape {
				t.Errorf("torn rules read: %+v", got)
				return
			}
		}
	}()
	wg.Wait()
}
