package chclient

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tsouza/cerberus/internal/chopt"
)

// TestQuerySettings_ConditionCacheOverride pins the client-wide
// use_query_condition_cache override: absent while off (the server default and
// the engine's per-query rule decide), stamped to 0 while on — overriding a
// per-query 1 an engine rule attached — and shared by every ForHead view.
func TestQuerySettings_ConditionCacheOverride(t *testing.T) {
	t.Parallel()

	def, registry := buildBreakers(false, 0, 0, 0, nil)
	c := &Client{br: def, breakers: registry, conditionCacheOverride: new(atomic.Uint32)}
	view := c.ForHead(HeadLoki)
	engineStamped := WithQuerySetting(context.Background(), SettingUseQueryConditionCache, 1)

	if _, ok := c.querySettings(context.Background())[SettingUseQueryConditionCache]; ok {
		t.Errorf("override off: %s stamped on a query that did not ask for it", SettingUseQueryConditionCache)
	}
	if got := c.querySettings(engineStamped)[SettingUseQueryConditionCache]; got != 1 {
		t.Errorf("override off: %s = %v; want the engine's per-query 1", SettingUseQueryConditionCache, got)
	}

	c.SetQueryConditionCacheOverride(chopt.DefectGated)
	for name, client := range map[string]*Client{"client": c, "view": view} {
		for ctxName, ctx := range map[string]context.Context{"bare": context.Background(), "engine-stamped": engineStamped} {
			if got := client.querySettings(ctx)[SettingUseQueryConditionCache]; got != 0 {
				t.Errorf("override on, %s, %s query: %s = %v; want 0", name, ctxName, SettingUseQueryConditionCache, got)
			}
		}
	}

	c.SetQueryConditionCacheOverride(chopt.DefectAbsent)
	if _, ok := view.querySettings(context.Background())[SettingUseQueryConditionCache]; ok {
		t.Errorf("override lifted: the view still stamps %s", SettingUseQueryConditionCache)
	}
}

// TestAdmitDispatch_RefusesOnlyTheUnsafeMixedFleet pins the refusal check
// every data-plane dispatch passes: a query the engine marked refused, and
// every query while the condition-cache override is refused, fail with
// ErrUnsafeMixedFleet naming the remedy; a gated or absent override and an
// unmarked query pass.
func TestAdmitDispatch_RefusesOnlyTheUnsafeMixedFleet(t *testing.T) {
	t.Parallel()

	def, registry := buildBreakers(false, 0, 0, 0, nil)
	c := &Client{br: def, breakers: registry, conditionCacheOverride: new(atomic.Uint32)}
	view := c.ForHead(HeadTempo)
	marked := WithDispatchRefusal(context.Background(), "shape defect x needs setting y")

	for _, g := range []chopt.DefectGate{chopt.DefectAbsent, chopt.DefectGated} {
		c.SetQueryConditionCacheOverride(g)
		if err := view.admitDispatch(context.Background()); err != nil {
			t.Errorf("override %d: unmarked query refused: %v", g, err)
		}
		err := view.admitDispatch(marked)
		if !errors.Is(err, ErrUnsafeMixedFleet) || !strings.Contains(err.Error(), "shape defect x needs setting y") {
			t.Errorf("override %d: marked query: err = %v; want ErrUnsafeMixedFleet carrying the engine's reason", g, err)
		}
	}

	c.SetQueryConditionCacheOverride(chopt.DefectRefused)
	err := view.admitDispatch(context.Background())
	if !errors.Is(err, ErrUnsafeMixedFleet) {
		t.Fatalf("override refused: err = %v; want ErrUnsafeMixedFleet", err)
	}
	for _, want := range []string{SettingUseQueryConditionCache, chopt.QueryConditionCacheSettingSince.String()} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("override refused: error %q does not name %q", err, want)
		}
	}
	if _, ok := c.querySettings(context.Background())[SettingUseQueryConditionCache]; ok {
		t.Errorf("override refused: %s stamped; a refused fleet must never be sent the setting", SettingUseQueryConditionCache)
	}
}

// TestQueryOpen_RefusedBeforeDialing pins that the refusal sits on the
// dispatch seam: a refused query against an unreachable address fails with
// ErrUnsafeMixedFleet, not with the dial error a dispatch would have met.
func TestQueryOpen_RefusedBeforeDialing(t *testing.T) {
	t.Parallel()

	c, err := New(Config{Addr: "127.0.0.1:1", Database: "otel"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	marked := WithDispatchRefusal(context.Background(), "shape defect x")
	if _, err := c.QueryStrings(marked, "SELECT 1"); !errors.Is(err, ErrUnsafeMixedFleet) {
		t.Errorf("engine-refused query: err = %v; want ErrUnsafeMixedFleet", err)
	}
	c.SetQueryConditionCacheOverride(chopt.DefectRefused)
	if _, err := c.ForHead(HeadLoki).QueryStrings(context.Background(), "SELECT 1"); !errors.Is(err, ErrUnsafeMixedFleet) {
		t.Errorf("refused override: err = %v; want ErrUnsafeMixedFleet", err)
	}
	c.SetQueryConditionCacheOverride(chopt.DefectAbsent)
	if _, err := c.QueryStrings(context.Background(), "SELECT 1"); err == nil || errors.Is(err, ErrUnsafeMixedFleet) {
		t.Errorf("unrefused query: err = %v; want the dial error", err)
	}
}
