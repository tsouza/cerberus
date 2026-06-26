package chopt

import (
	"sort"
	"strings"
	"testing"
)

// v constructs a Version tersely.
func v(major, minor int) Version { return Version{Major: major, Minor: minor} }

func TestParseMode(t *testing.T) {
	cases := []struct {
		in      string
		want    Mode
		wantErr bool
	}{
		{"permissive", Permissive, false},
		{"enforcing", Enforcing, false},
		{"PERMISSIVE", Permissive, false},
		{"  enforcing  ", Enforcing, false},
		{"", Enforcing, false}, // empty resolves to the default (enforcing)
		{"strict", Enforcing, true},
	}
	for _, tc := range cases {
		got, err := ParseMode(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("ParseMode(%q) err = %v; wantErr %v", tc.in, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && got != tc.want {
			t.Errorf("ParseMode(%q) = %v; want %v", tc.in, got, tc.want)
		}
	}
}

func TestResolve_Off(t *testing.T) {
	set, warns, err := Resolve(Config{Optimizations: "off"}, v(25, 8))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(set.IDs()) != 0 {
		t.Errorf("off set = %v; want empty", set.IDs())
	}
	if len(warns) != 0 {
		t.Errorf("off warnings = %v; want none", warns)
	}
}

func TestResolve_Off_LegacyTrue_StaysEmpty(t *testing.T) {
	// "off" is the absolute kill-switch: a stale legacy
	// CERBERUS_EXPERIMENTAL_TS_GRID_RANGE=true must NOT resurrect the
	// experimental native-rate path. The new knob wins; legacy is ignored with
	// the deprecation + 'ignored' warnings (permissive), the set stays empty.
	set, warns, err := Resolve(Config{
		Optimizations: "off",
		Mode:          Permissive,
		LegacyTSGrid:  LegacyFlag{Set: true, Value: true},
	}, v(25, 6))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(set.IDs()) != 0 {
		t.Errorf("off + legacy-true set = %v; want empty (off is absolute)", set.IDs())
	}
	if set.Has(FeatureTSGridRange) {
		t.Error("legacy true resurrected ts_grid_range under off; off must be absolute")
	}
	if !hasDeprecation(warns) {
		t.Errorf("legacy set must emit deprecation warning; warns = %v", warns)
	}
	if !anyContains(warns, "ignored") {
		t.Errorf("off + legacy must warn legacy ignored; warns = %v", warns)
	}
}

func TestResolve_Off_LegacyTrue_EnforcingFatal(t *testing.T) {
	// Under enforcing, an ignored legacy flag (because off was chosen
	// explicitly) is fatal, same as legacy + an explicit list.
	_, _, err := Resolve(Config{
		Optimizations: "off",
		Mode:          Enforcing,
		LegacyTSGrid:  LegacyFlag{Set: true, Value: true},
	}, v(25, 6))
	if err == nil {
		t.Fatal("off + legacy-true under enforcing: want fatal, got nil")
	}
}

func TestResolve_Off_LegacyFalse_StaysEmpty(t *testing.T) {
	// off + legacy-false: off wins, legacy ignored (deprecation only emitted),
	// set stays empty.
	set, warns, err := Resolve(Config{
		Optimizations: "off",
		Mode:          Permissive,
		LegacyTSGrid:  LegacyFlag{Set: true, Value: false},
	}, v(25, 6))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(set.IDs()) != 0 {
		t.Errorf("off + legacy-false set = %v; want empty", set.IDs())
	}
	if !hasDeprecation(warns) {
		t.Errorf("legacy set must emit deprecation warning; warns = %v", warns)
	}
}

func TestResolve_Auto_EnablesAutoSelectByVersion(t *testing.T) {
	// On 25.8 the stable features (aggregation_in_order 24.8, condition_cache
	// 25.3) plus the 25.6-floored native aggregates (ts_grid_range,
	// ts_grid_resample) are AutoSelect=true and supported; the 25.9-floored
	// changes/resets are not yet reachable, and columnar_result_decode is
	// AutoSelect=false so auto never picks it.
	set, _, err := Resolve(Config{Optimizations: "auto"}, v(25, 8))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertSet(t, set, FeatureAggregationInOrder, FeatureConditionCache, FeatureTSGridRange, FeatureTSGridResample)
	for _, off := range []string{FeatureTSGridChanges, FeatureTSGridResets, FeatureColumnarResultDecode} {
		if set.Has(off) {
			t.Errorf("auto on 25.8 enabled %q; want it off (floor 25.9 / opt-in only)", off)
		}
	}
}

func TestResolve_Auto_EmptySelectionDefaultsToAuto(t *testing.T) {
	set, _, err := Resolve(Config{Optimizations: ""}, v(25, 8))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertSet(t, set, FeatureAggregationInOrder, FeatureConditionCache, FeatureTSGridRange, FeatureTSGridResample)
}

func TestResolve_Auto_VersionBoundaries(t *testing.T) {
	// The auto-selection matrix as the probed server version crosses each floor.
	// columnar_result_decode (AutoSelect=false) is absent from every row even
	// though it carries no version floor — auto must never select it.
	cases := []struct {
		name   string
		server Version
		want   []string
	}{
		{
			name:   "24.8 only aggregation_in_order",
			server: v(24, 8),
			want:   []string{FeatureAggregationInOrder},
		},
		{
			name:   "25.3 adds condition_cache, no native aggregates",
			server: v(25, 3),
			want:   []string{FeatureAggregationInOrder, FeatureConditionCache},
		},
		{
			name:   "25.5 still below the 25.6 native floor",
			server: v(25, 5),
			want:   []string{FeatureAggregationInOrder, FeatureConditionCache},
		},
		{
			name:   "25.6 adds rate + resample, not changes/resets",
			server: v(25, 6),
			want:   []string{FeatureAggregationInOrder, FeatureConditionCache, FeatureTSGridRange, FeatureTSGridResample},
		},
		{
			name:   "25.8 still below the 25.9 changes/resets floor",
			server: v(25, 8),
			want:   []string{FeatureAggregationInOrder, FeatureConditionCache, FeatureTSGridRange, FeatureTSGridResample},
		},
		{
			name:   "25.9 adds changes + resets — all four native aggregates",
			server: v(25, 9),
			want: []string{
				FeatureAggregationInOrder, FeatureConditionCache,
				FeatureTSGridRange, FeatureTSGridResample, FeatureTSGridChanges, FeatureTSGridResets,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			set, warns, err := Resolve(Config{Optimizations: "auto"}, tc.server)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			assertSet(t, set, tc.want...)
			if set.Has(FeatureColumnarResultDecode) {
				t.Error("auto selected columnar_result_decode; it is opt-in only (AutoSelect=false)")
			}
			if len(warns) != 0 {
				t.Errorf("auto emitted warnings %v; want none (auto is silent on version skips)", warns)
			}
		})
	}
}

func TestResolve_Auto_OldServerExcludesUnsupportedStable(t *testing.T) {
	// On 24.8 only aggregation_in_order (24.8) is supported; condition_cache
	// (25.3) is silently excluded under auto (no warning, "best available").
	set, warns, err := Resolve(Config{Optimizations: "auto"}, v(24, 8))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertSet(t, set, FeatureAggregationInOrder)
	if set.Has(FeatureConditionCache) {
		t.Error("auto enabled condition_cache on 24.8; needs 25.3")
	}
	if len(warns) != 0 {
		t.Errorf("auto skip emitted warnings %v; want none (auto is silent)", warns)
	}
}

func TestResolve_ExplicitList_SupportedEnabled(t *testing.T) {
	set, _, err := Resolve(Config{Optimizations: "aggregation_in_order,condition_cache"}, v(25, 8))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertSet(t, set, FeatureAggregationInOrder, FeatureConditionCache)
}

func TestResolve_ExplicitList_UnsupportedPermissiveWarns(t *testing.T) {
	// condition_cache on 25.0 (< 25.3): permissive -> WARN + skip, no error.
	set, warns, err := Resolve(Config{
		Optimizations: "condition_cache",
		Mode:          Permissive,
	}, v(25, 0))
	if err != nil {
		t.Fatalf("Resolve: unexpected err %v", err)
	}
	if set.Has(FeatureConditionCache) {
		t.Error("permissive enabled unsupported condition_cache")
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "condition_cache") || !strings.Contains(warns[0], "25.3") {
		t.Errorf("permissive warnings = %v; want one naming condition_cache + 25.3", warns)
	}
}

func TestResolve_ExplicitList_UnsupportedEnforcingFatal(t *testing.T) {
	_, _, err := Resolve(Config{
		Optimizations: "condition_cache",
		Mode:          Enforcing,
	}, v(25, 0))
	if err == nil {
		t.Fatal("enforcing unsupported: want fatal error, got nil")
	}
	if !strings.Contains(err.Error(), "condition_cache") || !strings.Contains(err.Error(), "25.3") {
		t.Errorf("err = %v; want it to name condition_cache + 25.3", err)
	}
}

func TestResolve_UnknownID_FatalBothModes(t *testing.T) {
	for _, mode := range []Mode{Permissive, Enforcing} {
		_, _, err := Resolve(Config{
			Optimizations: "aggregation_in_order,bogus_feature",
			Mode:          mode,
		}, v(25, 8))
		if err == nil {
			t.Fatalf("mode %v: unknown id must be fatal, got nil", mode)
		}
		if !strings.Contains(err.Error(), "bogus_feature") {
			t.Errorf("mode %v: err = %v; want it to name bogus_feature", mode, err)
		}
	}
}

func TestResolve_ExplicitTSGrid_Supported(t *testing.T) {
	// Experimental ts_grid_range IS reachable by explicit listing (25.6+).
	set, _, err := Resolve(Config{Optimizations: "ts_grid_range"}, v(25, 6))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertSet(t, set, FeatureTSGridRange)
}

func TestResolve_ColumnarResultDecode_OptInOnly(t *testing.T) {
	// columnar_result_decode is opt-in only: auto must NOT enable it, even on a
	// brand-new server, since it is a perf tradeoff (a second ch-go dial).
	autoSet, _, err := Resolve(Config{Optimizations: "auto"}, v(25, 8))
	if err != nil {
		t.Fatalf("Resolve(auto): %v", err)
	}
	if autoSet.Has(FeatureColumnarResultDecode) {
		t.Error("auto must not enable columnar_result_decode (opt-in only)")
	}
}

func TestResolve_ColumnarResultDecode_NoVersionFloor(t *testing.T) {
	// columnar_result_decode is a client-side decode with no version gate
	// (MinVersion AlwaysAvailable): listing it explicitly enables it on ANY
	// server version, in enforcing mode, with no "needs ClickHouse >=X" error.
	for _, ver := range []Version{{Major: 24, Minor: 0}, {Major: 24, Minor: 8}, {Major: 99, Minor: 9}} {
		set, _, err := Resolve(Config{Optimizations: "columnar_result_decode", Mode: Enforcing}, ver)
		if err != nil {
			t.Fatalf("Resolve(columnar_result_decode) on %s: %v", ver, err)
		}
		if !set.Has(FeatureColumnarResultDecode) {
			t.Errorf("columnar_result_decode not enabled on %s", ver)
		}
	}
}

func TestResolve_AutoPlusOptIn_UnionsBoth(t *testing.T) {
	// The headline case: "auto,columnar_result_decode" = the version-gated auto
	// set PLUS the opt-in feature, without bailing out of auto. On 25.8 the auto
	// half now includes the 25.6-floored native aggregates.
	set, _, err := Resolve(Config{Optimizations: "auto,columnar_result_decode"}, v(25, 8))
	if err != nil {
		t.Fatalf("Resolve(auto,columnar_result_decode): %v", err)
	}
	assertSet(t, set,
		FeatureAggregationInOrder, FeatureConditionCache,
		FeatureTSGridRange, FeatureTSGridResample, FeatureColumnarResultDecode)
}

func TestResolve_AutoPlusOptIn_AutoSetStillVersionGated(t *testing.T) {
	// On 24.8 the auto half drops condition_cache (needs 25.3) but keeps
	// aggregation_in_order; the opt-in (no floor) still enables. Auto stays
	// silent about its own version skips even when composed.
	set, warns, err := Resolve(Config{Optimizations: "auto,columnar_result_decode"}, v(24, 8))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertSet(t, set, FeatureAggregationInOrder, FeatureColumnarResultDecode)
	if len(warns) != 0 {
		t.Errorf("auto-skip in a composed selection emitted warnings %v; want none", warns)
	}
}

func TestResolve_AutoPlusExplicitVersionGated_EnforcingFatal(t *testing.T) {
	// An explicit version-gated id keeps its "I require this" contract even next
	// to auto: condition_cache on 25.0 under Enforcing is fatal (unlike the
	// silent skip auto alone would do).
	_, _, err := Resolve(Config{Optimizations: "auto,condition_cache", Mode: Enforcing}, v(25, 0))
	if err == nil {
		t.Fatal("auto,condition_cache on 25.0 enforcing: want fatal, got nil")
	}
	if !strings.Contains(err.Error(), "condition_cache") || !strings.Contains(err.Error(), "25.3") {
		t.Errorf("err = %v; want it to name condition_cache + 25.3", err)
	}
}

func TestResolve_OffCannotBeCombined(t *testing.T) {
	for _, sel := range []string{"auto,off", "off,columnar_result_decode", "off,auto"} {
		_, _, err := Resolve(Config{Optimizations: sel}, v(25, 8))
		if err == nil {
			t.Errorf("%q: want fatal (off is absolute), got nil", sel)
			continue
		}
		if !strings.Contains(err.Error(), "off") {
			t.Errorf("%q: err = %v; want it to mention off", sel, err)
		}
	}
}

func TestResolve_LegacyTrue_ForceEnables(t *testing.T) {
	set, warns, err := Resolve(Config{
		Optimizations: "auto",
		LegacyTSGrid:  LegacyFlag{Set: true, Value: true},
	}, v(25, 8))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !set.Has(FeatureTSGridRange) {
		t.Error("legacy true did not force-enable ts_grid_range")
	}
	if !hasDeprecation(warns) {
		t.Errorf("legacy set must emit deprecation warning; warns = %v", warns)
	}
}

func TestResolve_LegacyFalse_ForceDisables(t *testing.T) {
	// Legacy false (with no new explicit list) force-disables ts_grid_range.
	// Under the default auto on a 25.8 server ts_grid_range is now auto-selected
	// (AutoSelect=true), so this is an ACTIVE removal — the operator's escape
	// hatch back to the fan-out rate path — while still emitting the deprecation
	// notice.
	set, warns, err := Resolve(Config{
		Optimizations: "auto",
		LegacyTSGrid:  LegacyFlag{Set: true, Value: false},
	}, v(25, 8))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if set.Has(FeatureTSGridRange) {
		t.Error("legacy false did not force-disable ts_grid_range")
	}
	if !hasDeprecation(warns) {
		t.Errorf("legacy set must emit deprecation warning; warns = %v", warns)
	}
}

func TestResolve_LegacyTrue_UnsupportedPermissiveWarns(t *testing.T) {
	// Legacy true on a server below the ts_grid_range floor -> permissive WARN.
	set, warns, err := Resolve(Config{
		Optimizations: "auto",
		Mode:          Permissive,
		LegacyTSGrid:  LegacyFlag{Set: true, Value: true},
	}, v(25, 0))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if set.Has(FeatureTSGridRange) {
		t.Error("ts_grid_range enabled below 25.6")
	}
	if !hasDeprecation(warns) {
		t.Errorf("want deprecation warning; warns = %v", warns)
	}
}

func TestResolve_LegacyTrue_UnsupportedEnforcingFatal(t *testing.T) {
	_, _, err := Resolve(Config{
		Optimizations: "auto",
		Mode:          Enforcing,
		LegacyTSGrid:  LegacyFlag{Set: true, Value: true},
	}, v(25, 0))
	if err == nil {
		t.Fatal("legacy true unsupported under enforcing: want fatal, got nil")
	}
}

func TestResolve_BothLegacyAndExplicitList_NewWins(t *testing.T) {
	// New CERBERUS_CH_OPTIMIZATIONS list set AND legacy set -> new wins, legacy
	// ignored with a warning (permissive).
	set, warns, err := Resolve(Config{
		Optimizations: "aggregation_in_order",
		Mode:          Permissive,
		LegacyTSGrid:  LegacyFlag{Set: true, Value: true},
	}, v(25, 8))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertSet(t, set, FeatureAggregationInOrder)
	if set.Has(FeatureTSGridRange) {
		t.Error("legacy true leaked ts_grid_range when a new explicit list was set")
	}
	if !anyContains(warns, "ignored") {
		t.Errorf("want a 'legacy ignored' warning; warns = %v", warns)
	}
}

func TestResolve_BothLegacyAndExplicitList_EnforcingFatal(t *testing.T) {
	_, _, err := Resolve(Config{
		Optimizations: "aggregation_in_order",
		Mode:          Enforcing,
		LegacyTSGrid:  LegacyFlag{Set: true, Value: true},
	}, v(25, 8))
	if err == nil {
		t.Fatal("legacy + explicit list under enforcing: want fatal, got nil")
	}
}

func TestResolve_LegacyUnset_NoEffect(t *testing.T) {
	// Unset legacy is a no-op: the resolved set is exactly what plain auto gives
	// (so on 25.8 ts_grid_range/resample ARE present — auto-selected by version,
	// not by the legacy flag) and no deprecation notice is emitted.
	set, warns, err := Resolve(Config{
		Optimizations: "auto",
		LegacyTSGrid:  LegacyFlag{Set: false},
	}, v(25, 8))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertSet(t, set, FeatureAggregationInOrder, FeatureConditionCache, FeatureTSGridRange, FeatureTSGridResample)
	if hasDeprecation(warns) {
		t.Errorf("unset legacy must not emit deprecation; warns = %v", warns)
	}
}

func TestRegistry_SeededEntries(t *testing.T) {
	reg := Registry()
	// AutoSelect is decoupled from Stability: the native timeSeries*ToGrid
	// aggregates are Experimental maturity yet AutoSelect=true (auto picks them
	// by version), while columnar_result_decode is the lone AutoSelect=false
	// opt-in perf tradeoff.
	want := map[string]Feature{
		FeatureAggregationInOrder:   {ID: FeatureAggregationInOrder, MinVersion: v(24, 8), Stability: Stable, AutoSelect: true},
		FeatureConditionCache:       {ID: FeatureConditionCache, MinVersion: v(25, 3), Stability: Stable, AutoSelect: true},
		FeatureTSGridRange:          {ID: FeatureTSGridRange, MinVersion: v(25, 6), Stability: Experimental, AutoSelect: true},
		FeatureTSGridResample:       {ID: FeatureTSGridResample, MinVersion: v(25, 6), Stability: Experimental, AutoSelect: true},
		FeatureColumnarResultDecode: {ID: FeatureColumnarResultDecode, MinVersion: AlwaysAvailable, Stability: Experimental, AutoSelect: false},
		FeatureTSGridChanges:        {ID: FeatureTSGridChanges, MinVersion: v(25, 9), Stability: Experimental, AutoSelect: true},
		FeatureTSGridResets:         {ID: FeatureTSGridResets, MinVersion: v(25, 9), Stability: Experimental, AutoSelect: true},
	}
	if len(reg) != len(want) {
		t.Fatalf("registry has %d entries; want %d", len(reg), len(want))
	}
	for _, f := range reg {
		w, ok := want[f.ID]
		if !ok {
			t.Errorf("unexpected feature %q", f.ID)
			continue
		}
		if f.MinVersion != w.MinVersion || f.Stability != w.Stability || f.AutoSelect != w.AutoSelect {
			t.Errorf("feature %q = %+v; want minVersion/stability/autoSelect %+v", f.ID, f, w)
		}
	}
}

func assertSet(t *testing.T, set EnabledSet, want ...string) {
	t.Helper()
	got := set.IDs()
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("set = %v; want %v", got, want)
	}
}

func hasDeprecation(warns []string) bool {
	return anyContains(warns, "deprecated")
}

func anyContains(warns []string, sub string) bool {
	for _, w := range warns {
		if strings.Contains(strings.ToLower(w), strings.ToLower(sub)) {
			return true
		}
	}
	return false
}
