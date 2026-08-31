package engine

import (
	"context"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
)

// fixedNow is the reference "now" every result-cache eligibility test anchors
// against, so the closed/live-edge boundary is pinned to an exact instant
// rather than a moving time.Now().
var fixedNow = time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

// testIngestLag is the ingest-lag horizon used throughout: a query window
// whose End is before fixedNow.Add(-testIngestLag) (11:55:00) is closed;
// anything from 11:55:00 onward is live-edge.
const testIngestLag = 5 * time.Minute

// stepGrid builds a minimal range-mode chplan.GridCarrier — a StepGrid with
// a positive Step — for eligibility tests that don't care about the rest of
// the plan shape.
func stepGrid(start, end time.Time) chplan.Node {
	return &chplan.StepGrid{Start: start, End: end, Step: time.Minute}
}

func TestEligibleForResultCache_ClosedWindow_Eligible(t *testing.T) {
	// End = 11:50:00, well before the 11:55:00 threshold.
	plan := stepGrid(fixedNow.Add(-2*time.Hour), fixedNow.Add(-10*time.Minute))
	if !eligibleForResultCache(plan, fixedNow, testIngestLag) {
		t.Error("closed window (End 10m before now, 5m ingest lag): want eligible")
	}
}

func TestEligibleForResultCache_LiveEdgeWindow_NotEligible(t *testing.T) {
	// End = 11:59:00, inside the last 5 minutes (the ingest-lag horizon):
	// rows for this window may still be arriving.
	plan := stepGrid(fixedNow.Add(-2*time.Hour), fixedNow.Add(-1*time.Minute))
	if eligibleForResultCache(plan, fixedNow, testIngestLag) {
		t.Error("live-edge window (End 1m before now, 5m ingest lag): want NOT eligible")
	}
}

func TestEligibleForResultCache_BoundaryIsExclusive(t *testing.T) {
	// End is EXACTLY at the threshold (now - ingestLag): the check is
	// End.Before(threshold), so an End equal to the threshold is NOT closed —
	// a query evaluated one instant later would see that exact millisecond
	// slide inside the live-edge zone, so the boundary must be conservative.
	threshold := fixedNow.Add(-testIngestLag)
	plan := stepGrid(fixedNow.Add(-2*time.Hour), threshold)
	if eligibleForResultCache(plan, fixedNow, testIngestLag) {
		t.Error("End exactly at the ingest-lag threshold: want NOT eligible (exclusive boundary)")
	}
}

func TestEligibleForResultCache_ZeroTimeSentinel_NotEligible(t *testing.T) {
	// A zero-value End is the codebase-wide "resolve at emit time" sentinel:
	// nativeGridTimeBoundFrag/timeOrNowFrag render a bare now()/now64() SQL
	// call for it, so this window is NOT fixed at all — it is exactly the
	// live-edge shape this gate exists to exclude, regardless of how early
	// the zero value compares numerically.
	plan := stepGrid(fixedNow.Add(-2*time.Hour), time.Time{})
	if eligibleForResultCache(plan, fixedNow, testIngestLag) {
		t.Error("zero-time End (now()-relative sentinel): want NOT eligible")
	}
	planZeroStart := stepGrid(time.Time{}, fixedNow.Add(-10*time.Minute))
	if eligibleForResultCache(planZeroStart, fixedNow, testIngestLag) {
		t.Error("zero-time Start: want NOT eligible")
	}
}

func TestEligibleForResultCache_NoGridCarrier_NotEligible(t *testing.T) {
	// A bare Scan has no eval grid at all — conservative fail-closed. This is
	// also the shape a metadata-only plan takes, though the metadata routes
	// never reach this function in production (they dispatch through
	// chclient.Client.Query/QueryStrings directly, never through the engine's
	// execute seam — see eligibleForResultCache's own doc).
	plan := chplan.Node(&chplan.Scan{Table: "otel_metrics_sum"})
	if eligibleForResultCache(plan, fixedNow, testIngestLag) {
		t.Error("plan with no GridCarrier: want NOT eligible")
	}
}

func TestEligibleForResultCache_InstantModeCarrier_NotEligible(t *testing.T) {
	// Step == 0 is instant mode: EvalGrid's Start/End carry no request-grid
	// meaning at all (see chplan.GridCarrier's own doc), so a plan whose only
	// carrier is instant-mode has nothing that proves a closed window.
	plan := &chplan.StepGrid{Start: fixedNow.Add(-time.Hour), End: fixedNow, Step: 0}
	if eligibleForResultCache(plan, fixedNow, testIngestLag) {
		t.Error("instant-mode-only carrier: want NOT eligible")
	}
}

func TestEligibleForResultCache_MultipleCarriers_AllMustBeClosed(t *testing.T) {
	// A plan reaching two range-mode carriers (e.g. a binary op over two
	// differently-offset range vectors) is eligible only when EVERY carrier's
	// window is closed — the latest End anywhere in the plan governs.
	closed := stepGrid(fixedNow.Add(-2*time.Hour), fixedNow.Add(-10*time.Minute))
	liveEdge := stepGrid(fixedNow.Add(-2*time.Hour), fixedNow.Add(-1*time.Minute))
	plan := &chplan.Filter{
		Input:     closed,
		Predicate: &chplan.ScalarSubquery{Input: liveEdge},
	}
	if eligibleForResultCache(plan, fixedNow, testIngestLag) {
		t.Error("one closed + one live-edge carrier (via a subquery-embedded plan): want NOT eligible")
	}
}

func TestEligibleForResultCache_NowExprPresent_NotEligible(t *testing.T) {
	// A closed window is not enough on its own: a literal now()/now64()
	// FuncCall anywhere in the plan's Expr tree — the shape PromQL's time()
	// builtin lowers to before the range-mode anchor-ref rewrite runs — must
	// also block eligibility, independent of the grid check.
	plan := &chplan.Filter{
		Input:     stepGrid(fixedNow.Add(-2*time.Hour), fixedNow.Add(-10*time.Minute)),
		Predicate: &chplan.FuncCall{Fn: chplan.FnNow64, Args: []chplan.Expr{&chplan.LitInt{V: chplan.NanoScale}}},
	}
	if eligibleForResultCache(plan, fixedNow, testIngestLag) {
		t.Error("closed window but a now64() expression present: want NOT eligible")
	}
}

func TestPlanHasNowExpr(t *testing.T) {
	closed := stepGrid(fixedNow.Add(-2*time.Hour), fixedNow.Add(-10*time.Minute))
	if planHasNowExpr(closed) {
		t.Error("plain StepGrid with no Expr fields: want no now() expression found")
	}

	withNow := &chplan.Filter{
		Input:     &chplan.Scan{Table: "otel_metrics_sum"},
		Predicate: &chplan.FuncCall{Fn: chplan.FnNow, Args: nil},
	}
	if !planHasNowExpr(withNow) {
		t.Error("Filter.Predicate carrying a bare now() FuncCall: want found")
	}

	// Nested inside a Binary, and inside a ScalarSubquery-embedded plan's own
	// Filter — both must still be found, proving the walk is exhaustive over
	// sub-expressions AND expr-embedded plan subtrees, not just top-level
	// Expr slots.
	nestedInBinary := &chplan.Filter{
		Input: &chplan.Scan{Table: "otel_metrics_sum"},
		Predicate: &chplan.Binary{
			Op:    chplan.OpSub,
			Left:  &chplan.ColumnRef{Name: "TimeUnix"},
			Right: &chplan.FuncCall{Fn: chplan.FnNow64, Args: []chplan.Expr{&chplan.LitInt{V: chplan.NanoScale}}},
		},
	}
	if !planHasNowExpr(nestedInBinary) {
		t.Error("now64() nested inside a Binary: want found")
	}

	nestedInSubquery := &chplan.Filter{
		Input: &chplan.Scan{Table: "otel_metrics_sum"},
		Predicate: &chplan.ScalarSubquery{Input: &chplan.Filter{
			Input:     &chplan.Scan{Table: "otel_metrics_sum"},
			Predicate: &chplan.FuncCall{Fn: chplan.FnNow, Args: nil},
		}},
	}
	if !planHasNowExpr(nestedInSubquery) {
		t.Error("now() nested inside a ScalarSubquery-embedded plan: want found")
	}
}

// resultCacheRules returns SettingsRules with ResultCache ON, a fixed clock,
// and the configured ingest lag / ttl.
func resultCacheRules() SettingsRules {
	return SettingsRules{
		ResultCache:          true,
		ResultCacheIngestLag: testIngestLag,
		ResultCacheTTL:       10 * time.Minute,
		Now:                  func() time.Time { return fixedNow },
	}
}

func TestApply_StampsResultCache_OnClosedWindow(t *testing.T) {
	plan := stepGrid(fixedNow.Add(-2*time.Hour), fixedNow.Add(-10*time.Minute))
	ctx := resultCacheRules().apply(context.Background(), plan)
	if got := settingValue(ctx, chclient.SettingUseQueryCache); got != 1 {
		t.Errorf("ResultCache on + closed window: %s = %v; want 1", chclient.SettingUseQueryCache, got)
	}
	if got := settingValue(ctx, chclient.SettingQueryCacheTTL); got != int64(600) {
		t.Errorf("ResultCache on + closed window: %s = %v; want 600 (10m)", chclient.SettingQueryCacheTTL, got)
	}
}

func TestApply_ResultCache_OffByDefault(t *testing.T) {
	plan := stepGrid(fixedNow.Add(-2*time.Hour), fixedNow.Add(-10*time.Minute))
	off := SettingsRules{Now: func() time.Time { return fixedNow }}.apply(context.Background(), plan)
	if got := settingValue(off, chclient.SettingUseQueryCache); got != nil {
		t.Errorf("ResultCache off: %s = %v; want absent", chclient.SettingUseQueryCache, got)
	}
}

func TestApply_ResultCache_NotStampedOnLiveEdgeWindow(t *testing.T) {
	plan := stepGrid(fixedNow.Add(-2*time.Hour), fixedNow.Add(-1*time.Minute))
	ctx := resultCacheRules().apply(context.Background(), plan)
	if got := settingValue(ctx, chclient.SettingUseQueryCache); got != nil {
		t.Errorf("ResultCache on + live-edge window: %s = %v; want absent (conservative gate)", chclient.SettingUseQueryCache, got)
	}
}

func TestApply_ResultCache_NotStampedWithoutIngestLagMargin(t *testing.T) {
	// A window that just closed (End 1 second before now) is NOT eligible
	// under a 5-minute ingest lag, confirming the lag — not merely "End is in
	// the past" — governs eligibility.
	plan := stepGrid(fixedNow.Add(-2*time.Hour), fixedNow.Add(-time.Second))
	ctx := resultCacheRules().apply(context.Background(), plan)
	if got := settingValue(ctx, chclient.SettingUseQueryCache); got != nil {
		t.Errorf("window closed 1s ago under a 5m ingest lag: %s = %v; want absent", chclient.SettingUseQueryCache, got)
	}
}
