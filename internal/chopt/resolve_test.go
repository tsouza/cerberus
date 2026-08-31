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
	// On 25.9 the stable features (aggregation_in_order 24.8, condition_cache
	// 25.3) plus ELEVEN of the twelve 25.9-floored ts_grid_* features
	// (ts_grid_range, ts_grid_increase, ts_grid_resample, ts_grid_resets,
	// ts_grid_deriv, ts_grid_predict_linear, ts_grid_recollapse,
	// ts_grid_histogram, ts_grid_delta, ts_grid_irate, ts_grid_idelta) are
	// AutoSelect=true and supported. 25.9 is the first release whose
	// timeSeries*ToGrid window is left-open (PR #86588), so it is the native
	// floor for the whole family (deriv/predict_linear shipped at 25.8 but are
	// registry-pinned to the shared 25.9 floor).
	// columnar_result_decode and ts_grid_changes are both AutoSelect=false so
	// auto never picks either: columnar_result_decode is a perf tradeoff,
	// ts_grid_changes diverges from reference Prometheus on NaN-adjacent windows
	// (#1721) and is opt-in only via CERBERUS_CH_OPTIMIZATIONS=ts_grid_changes.
	// Capability=Available is the happy-path boot verdict (the server permits
	// the experimental setting); ResultCacheCapability=Available is the same
	// happy-path verdict for the SEPARATE result-cache probe, so result_cache
	// (MinVersion 24.8, met here) also joins the auto set.
	set, _, err := Resolve(Config{Optimizations: "auto", Capability: CapabilityAvailable, ResultCacheCapability: CapabilityAvailable}, v(25, 9))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertSet(t, set, FeatureAggregationInOrder, FeatureConditionCache, FeatureLagInFrameAdjacency,
		FeatureTSGridRange, FeatureTSGridIncrease, FeatureTSGridResample, FeatureTSGridResets, FeatureTSGridDelta,
		FeatureTSGridDeriv, FeatureTSGridPredictLinear, FeatureTSGridRecollapse,
		FeatureTSGridHistogram, FeatureTSGridIrate, FeatureTSGridIdelta, FeatureResultCache)
	if set.Has(FeatureColumnarResultDecode) {
		t.Errorf("auto on 25.9 enabled %q; want it off (opt-in only)", FeatureColumnarResultDecode)
	}
	if set.Has(FeatureTSGridChanges) {
		t.Errorf("auto on 25.9 enabled %q; want it off (opt-in only, #1721)", FeatureTSGridChanges)
	}
}

func TestResolve_Auto_NativeAggregatesOffBelow259(t *testing.T) {
	// On 25.8 (the compose / prod-floor substrate) NONE of the native
	// timeSeries*ToGrid aggregates auto-enable: the family floor is 25.9 (the
	// left-open window fix, PR #86588). Only the stable features remain.
	set, _, err := Resolve(Config{Optimizations: "auto", Capability: CapabilityAvailable, ResultCacheCapability: CapabilityAvailable}, v(25, 8))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertSet(t, set, FeatureAggregationInOrder, FeatureConditionCache, FeatureLagInFrameAdjacency, FeatureResultCache)
	for _, off := range []string{
		FeatureTSGridRange, FeatureTSGridIncrease, FeatureTSGridResample, FeatureTSGridChanges, FeatureTSGridResets,
		FeatureTSGridDeriv, FeatureTSGridPredictLinear, FeatureTSGridRecollapse,
		FeatureTSGridHistogram, FeatureTSGridDelta, FeatureTSGridIrate, FeatureTSGridIdelta,
	} {
		if set.Has(off) {
			t.Errorf("auto on 25.8 enabled %q; want it off (native floor is 25.9)", off)
		}
	}
}

func TestResolve_Auto_EmptySelectionDefaultsToAuto(t *testing.T) {
	set, _, err := Resolve(Config{Optimizations: "", Capability: CapabilityAvailable, ResultCacheCapability: CapabilityAvailable}, v(25, 9))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertSet(t, set, FeatureAggregationInOrder, FeatureConditionCache, FeatureLagInFrameAdjacency,
		FeatureTSGridRange, FeatureTSGridIncrease, FeatureTSGridResample, FeatureTSGridResets, FeatureTSGridDelta,
		FeatureTSGridDeriv, FeatureTSGridPredictLinear, FeatureTSGridRecollapse,
		FeatureTSGridHistogram, FeatureTSGridIrate, FeatureTSGridIdelta, FeatureResultCache)
	if set.Has(FeatureTSGridChanges) {
		t.Errorf("empty-selection-defaults-to-auto enabled %q; want it off (opt-in only, #1721)", FeatureTSGridChanges)
	}
}

func TestResolve_Auto_VersionBoundaries(t *testing.T) {
	// The auto-selection matrix as the probed server version crosses each floor,
	// with the boot capability verdict = Available on BOTH probed axes (the
	// server permits the experimental ts-grid setting AND the result-cache
	// settings) so the version floor is the only gate in play.
	// columnar_result_decode and ts_grid_changes (both AutoSelect=false) are
	// absent from every row: columnar_result_decode carries no version floor at
	// all, and ts_grid_changes never auto-selects regardless of version because
	// the native builtin diverges from reference Prometheus on NaN-adjacent
	// windows (#1721) — auto must never select either. result_cache's own
	// 24.8 floor is met by every row here, so it is present in all of them.
	cases := []struct {
		name   string
		server Version
		want   []string
	}{
		{
			name:   "24.8 only aggregation_in_order",
			server: v(24, 8),
			want:   []string{FeatureAggregationInOrder, FeatureLagInFrameAdjacency, FeatureResultCache},
		},
		{
			name:   "25.3 adds condition_cache, no native aggregates",
			server: v(25, 3),
			want:   []string{FeatureAggregationInOrder, FeatureConditionCache, FeatureLagInFrameAdjacency, FeatureResultCache},
		},
		{
			name:   "25.6 below the 25.9 native floor (closed-window aggregates)",
			server: v(25, 6),
			want:   []string{FeatureAggregationInOrder, FeatureConditionCache, FeatureLagInFrameAdjacency, FeatureResultCache},
		},
		{
			name:   "25.8 still below the 25.9 native floor",
			server: v(25, 8),
			want:   []string{FeatureAggregationInOrder, FeatureConditionCache, FeatureLagInFrameAdjacency, FeatureResultCache},
		},
		{
			name:   "25.9 adds eleven ts_grid_* features (left-open window; ts_grid_changes stays opt-in)",
			server: v(25, 9),
			want: []string{
				FeatureAggregationInOrder, FeatureConditionCache, FeatureLagInFrameAdjacency,
				FeatureTSGridRange, FeatureTSGridIncrease, FeatureTSGridResample, FeatureTSGridResets, FeatureTSGridDelta,
				FeatureTSGridDeriv, FeatureTSGridPredictLinear, FeatureTSGridRecollapse,
				FeatureTSGridHistogram, FeatureTSGridIrate, FeatureTSGridIdelta, FeatureResultCache,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			set, warns, err := Resolve(Config{Optimizations: "auto", Capability: CapabilityAvailable, ResultCacheCapability: CapabilityAvailable}, tc.server)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			assertSet(t, set, tc.want...)
			if set.Has(FeatureColumnarResultDecode) {
				t.Error("auto selected columnar_result_decode; it is opt-in only (AutoSelect=false)")
			}
			if set.Has(FeatureTSGridChanges) {
				t.Error("auto selected ts_grid_changes; it is opt-in only (AutoSelect=false, #1721)")
			}
			if len(warns) != 0 {
				t.Errorf("auto emitted warnings %v; want none (auto is silent on version skips)", warns)
			}
		})
	}
}

// TestResolve_Auto_JoinSpill_VersionBoundaries pins join_spill's own 26.4
// floor across below/at/above, independent of TestResolve_Auto_VersionBoundaries
// above (which stops at 25.9 and would otherwise need every AutoSelect=true
// feature re-enumerated up to 26.4 just to add this one row). AutoSelect=true
// like the ts_grid_* family, so `auto` alone — no explicit listing needed —
// is enough to pick it up once the server clears the floor.
func TestResolve_Auto_JoinSpill_VersionBoundaries(t *testing.T) {
	cases := []struct {
		name   string
		server Version
		want   bool
	}{
		{"26.3 below the 26.4 floor", v(26, 3), false},
		{"26.4 at the floor (setting first exists to stamp)", v(26, 4), true},
		{"26.5 above the floor (ratio-default sibling ships here too)", v(26, 5), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			set, warns, err := Resolve(Config{Optimizations: "auto", Capability: CapabilityAvailable, ResultCacheCapability: CapabilityAvailable}, tc.server)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got := set.Has(FeatureJoinSpill); got != tc.want {
				t.Errorf("server %v: join_spill enabled = %v; want %v", tc.server, got, tc.want)
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
	// ResultCacheCapability=Available keeps result_cache's own capability axis
	// clean (its 24.8 floor is met here), so this stays a pure version-floor
	// test for condition_cache.
	set, warns, err := Resolve(Config{Optimizations: "auto", ResultCacheCapability: CapabilityAvailable}, v(24, 8))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertSet(t, set, FeatureAggregationInOrder, FeatureLagInFrameAdjacency, FeatureResultCache)
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
	// Experimental ts_grid_range IS reachable by explicit listing (25.9+) when
	// the server also permits the experimental setting (Capability=Available).
	set, _, err := Resolve(Config{Optimizations: "ts_grid_range", Capability: CapabilityAvailable}, v(25, 9))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertSet(t, set, FeatureTSGridRange)
}

func TestResolve_TSGridLastOverTime_ExplicitSupported(t *testing.T) {
	// ts_grid_last_over_time IS reachable by explicit listing (26.6+) when the
	// server also permits the experimental setting.
	set, _, err := Resolve(Config{Optimizations: "ts_grid_last_over_time", Capability: CapabilityAvailable}, v(26, 6))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertSet(t, set, FeatureTSGridLastOverTime)
}

func TestResolve_TSGridLastOverTime_BelowFloorEnforcingFatal(t *testing.T) {
	// 26.5 (this repo's own chdb_substrate, versions.yaml) sits one minor below
	// the 26.6 floor — the two upstream correctness fixes
	// (ClickHouse/ClickHouse#106504, #106577) this feature's own registry doc
	// names are absent there, so explicit + enforcing must refuse to run.
	_, _, err := Resolve(Config{
		Optimizations: "ts_grid_last_over_time",
		Mode:          Enforcing,
		Capability:    CapabilityAvailable,
	}, v(26, 5))
	if err == nil {
		t.Fatal("explicit ts_grid_last_over_time on 26.5 under enforcing: want fatal, got nil")
	}
	if !strings.Contains(err.Error(), "ts_grid_last_over_time") || !strings.Contains(err.Error(), "26.6") {
		t.Errorf("err = %v; want it to name ts_grid_last_over_time + 26.6", err)
	}
}

func TestResolve_TSGridLastOverTime_OptInOnly(t *testing.T) {
	// AutoSelect is false (a brand-new floor with no fielded validation yet,
	// mirroring quantile_prom_histogram / map_bucketed_serialization): auto
	// must not enable it even on a server well past its 26.6 floor.
	set, _, err := Resolve(Config{Optimizations: "auto", Capability: CapabilityAvailable}, v(26, 6))
	if err != nil {
		t.Fatalf("Resolve(auto): %v", err)
	}
	if set.Has(FeatureTSGridLastOverTime) {
		t.Error("auto enabled ts_grid_last_over_time; it is opt-in only (AutoSelect=false, new 26.6 floor)")
	}
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
	// set PLUS the opt-in feature, without bailing out of auto. On 25.9 the auto
	// half includes eleven of the twelve 25.9-floored ts_grid_* features
	// (ts_grid_changes stays opt-in-only, #1721, and is absent even from the
	// explicit union here since it was not itself listed).
	set, _, err := Resolve(Config{Optimizations: "auto,columnar_result_decode", Capability: CapabilityAvailable, ResultCacheCapability: CapabilityAvailable}, v(25, 9))
	if err != nil {
		t.Fatalf("Resolve(auto,columnar_result_decode): %v", err)
	}
	assertSet(t, set,
		FeatureAggregationInOrder, FeatureConditionCache, FeatureLagInFrameAdjacency,
		FeatureTSGridRange, FeatureTSGridIncrease, FeatureTSGridResample, FeatureTSGridResets, FeatureTSGridDelta,
		FeatureTSGridDeriv, FeatureTSGridPredictLinear, FeatureTSGridRecollapse,
		FeatureTSGridHistogram, FeatureTSGridIrate, FeatureTSGridIdelta, FeatureResultCache,
		FeatureColumnarResultDecode)
}

func TestResolve_AutoPlusOptIn_AutoSetStillVersionGated(t *testing.T) {
	// On 24.8 the auto half drops condition_cache (needs 25.3) but keeps
	// aggregation_in_order; the opt-in (no floor) still enables. Auto stays
	// silent about its own version skips even when composed.
	// ResultCacheCapability=Available keeps result_cache's own capability axis
	// clean, so it resolves in on its 24.8 floor rather than adding a spurious
	// capability-block warning to this version-skip test.
	set, warns, err := Resolve(Config{Optimizations: "auto,columnar_result_decode", ResultCacheCapability: CapabilityAvailable}, v(24, 8))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertSet(t, set, FeatureAggregationInOrder, FeatureLagInFrameAdjacency, FeatureColumnarResultDecode, FeatureResultCache)
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
		Capability:    CapabilityAvailable,
		LegacyTSGrid:  LegacyFlag{Set: true, Value: true},
	}, v(25, 9))
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
	// Under the default auto on a 25.9 server ts_grid_range is auto-selected
	// (AutoSelect=true, floor met), so this is an ACTIVE removal — the operator's
	// escape hatch back to the fan-out rate path — while still emitting the
	// deprecation notice.
	set, warns, err := Resolve(Config{
		Optimizations: "auto",
		Capability:    CapabilityAvailable,
		LegacyTSGrid:  LegacyFlag{Set: true, Value: false},
	}, v(25, 9))
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
		t.Error("ts_grid_range enabled below 25.9")
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
	// (so on 25.9 ts_grid_range/resample ARE present — auto-selected by version,
	// not by the legacy flag; ts_grid_changes stays absent — it is opt-in only,
	// #1721) and no deprecation notice is emitted.
	set, warns, err := Resolve(Config{
		Optimizations: "auto",
		Capability:    CapabilityAvailable,
		LegacyTSGrid:  LegacyFlag{Set: false},
	}, v(25, 9))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertSet(t, set, FeatureAggregationInOrder, FeatureConditionCache, FeatureLagInFrameAdjacency,
		FeatureTSGridRange, FeatureTSGridIncrease, FeatureTSGridResample, FeatureTSGridResets, FeatureTSGridDelta,
		FeatureTSGridDeriv, FeatureTSGridPredictLinear, FeatureTSGridRecollapse,
		FeatureTSGridHistogram, FeatureTSGridIrate, FeatureTSGridIdelta)
	if hasDeprecation(warns) {
		t.Errorf("unset legacy must not emit deprecation; warns = %v", warns)
	}
}

func TestRegistry_SeededEntries(t *testing.T) {
	reg := Registry()
	// AutoSelect is decoupled from Stability: the native timeSeries*ToGrid
	// aggregates are Experimental maturity yet mostly AutoSelect=true (auto
	// picks them by version). Two features are AutoSelect=false, opt-in only:
	// columnar_result_decode (a perf tradeoff) and ts_grid_changes (the native
	// builtin diverges from reference Prometheus on NaN-adjacent windows,
	// #1721).
	// RequiresExperimentalTSGrid marks the twelve native timeSeries*ToGrid
	// features (the ten aggregates — rate, increase, resample, changes,
	// resets, deriv, predict_linear, delta, irate, idelta — plus the
	// ts_grid_recollapse shape knob that rides on the rate one and
	// ts_grid_histogram); the stable/client-side features leave it false.
	// RequiresResultCacheCapability marks only result_cache, gated on its OWN
	// boot probe (see capability.go / resolve.go).
	want := map[string]Feature{
		FeatureAggregationInOrder:           {ID: FeatureAggregationInOrder, MinVersion: v(24, 8), Stability: Stable, AutoSelect: true, RequiresExperimentalTSGrid: false},
		FeatureConditionCache:               {ID: FeatureConditionCache, MinVersion: v(25, 3), Stability: Stable, AutoSelect: true, RequiresExperimentalTSGrid: false},
		FeatureTSGridRange:                  {ID: FeatureTSGridRange, MinVersion: v(25, 9), Stability: Experimental, AutoSelect: true, RequiresExperimentalTSGrid: true},
		FeatureTSGridIncrease:               {ID: FeatureTSGridIncrease, MinVersion: v(25, 9), Stability: Experimental, AutoSelect: true, RequiresExperimentalTSGrid: true},
		FeatureTSGridResample:               {ID: FeatureTSGridResample, MinVersion: v(25, 9), Stability: Experimental, AutoSelect: true, RequiresExperimentalTSGrid: true},
		FeatureColumnarResultDecode:         {ID: FeatureColumnarResultDecode, MinVersion: AlwaysAvailable, Stability: Experimental, AutoSelect: false, RequiresExperimentalTSGrid: false},
		FeatureTSGridChanges:                {ID: FeatureTSGridChanges, MinVersion: v(25, 9), Stability: Experimental, AutoSelect: false, RequiresExperimentalTSGrid: true},
		FeatureTSGridResets:                 {ID: FeatureTSGridResets, MinVersion: v(25, 9), Stability: Experimental, AutoSelect: true, RequiresExperimentalTSGrid: true},
		FeatureTSGridDeriv:                  {ID: FeatureTSGridDeriv, MinVersion: v(25, 9), Stability: Experimental, AutoSelect: true, RequiresExperimentalTSGrid: true},
		FeatureTSGridPredictLinear:          {ID: FeatureTSGridPredictLinear, MinVersion: v(25, 9), Stability: Experimental, AutoSelect: true, RequiresExperimentalTSGrid: true},
		FeatureTSGridInstant:                {ID: FeatureTSGridInstant, MinVersion: v(26, 5), Stability: Experimental, AutoSelect: false, RequiresExperimentalTSGrid: true},
		FeatureTSGridRecollapse:             {ID: FeatureTSGridRecollapse, MinVersion: v(25, 9), Stability: Experimental, AutoSelect: true, RequiresExperimentalTSGrid: true},
		FeatureTSGridHistogram:              {ID: FeatureTSGridHistogram, MinVersion: v(25, 9), Stability: Experimental, AutoSelect: true, RequiresExperimentalTSGrid: true},
		FeatureQuantilePromHistogram:        {ID: FeatureQuantilePromHistogram, MinVersion: v(25, 10), Stability: Experimental, AutoSelect: false, RequiresExperimentalTSGrid: false},
		FeatureTSGridDelta:                  {ID: FeatureTSGridDelta, MinVersion: v(25, 9), Stability: Experimental, AutoSelect: true, RequiresExperimentalTSGrid: true},
		FeatureTSGridIrate:                  {ID: FeatureTSGridIrate, MinVersion: v(25, 9), Stability: Experimental, AutoSelect: true, RequiresExperimentalTSGrid: true},
		FeatureTSGridIdelta:                 {ID: FeatureTSGridIdelta, MinVersion: v(25, 9), Stability: Experimental, AutoSelect: true, RequiresExperimentalTSGrid: true},
		FeatureLagInFrameAdjacency:          {ID: FeatureLagInFrameAdjacency, MinVersion: AlwaysAvailable, Stability: Experimental, AutoSelect: true, RequiresExperimentalTSGrid: false},
		FeatureFixedAccumulatorExtrapolated: {ID: FeatureFixedAccumulatorExtrapolated, MinVersion: AlwaysAvailable, Stability: Experimental, AutoSelect: false, RequiresExperimentalTSGrid: false},
		FeatureSortedSlabOverTime:           {ID: FeatureSortedSlabOverTime, MinVersion: AlwaysAvailable, Stability: Experimental, AutoSelect: false, RequiresExperimentalTSGrid: false},
		FeatureMapBucketedSerialization:     {ID: FeatureMapBucketedSerialization, MinVersion: v(26, 4), Stability: Experimental, AutoSelect: false, RequiresExperimentalTSGrid: false},
		FeatureTSGridLastOverTime:           {ID: FeatureTSGridLastOverTime, MinVersion: v(26, 6), Stability: Experimental, AutoSelect: false, RequiresExperimentalTSGrid: true},
		FeatureColumnStatistics:             {ID: FeatureColumnStatistics, MinVersion: v(26, 3), Stability: Experimental, AutoSelect: false, RequiresExperimentalTSGrid: false},
		FeatureClassicBucketMergeSumMap:     {ID: FeatureClassicBucketMergeSumMap, MinVersion: AlwaysAvailable, Stability: Experimental, AutoSelect: false, RequiresExperimentalTSGrid: false},
		FeatureExpHistogramMergeSumMap:      {ID: FeatureExpHistogramMergeSumMap, MinVersion: AlwaysAvailable, Stability: Experimental, AutoSelect: false, RequiresExperimentalTSGrid: false},
		FeatureJoinSpill:                    {ID: FeatureJoinSpill, MinVersion: v(26, 4), Stability: Experimental, AutoSelect: true, RequiresExperimentalTSGrid: false},
		FeatureTraceIDProjection:            {ID: FeatureTraceIDProjection, MinVersion: v(25, 5), Stability: Experimental, AutoSelect: false, RequiresExperimentalTSGrid: false},
		FeatureTraceIDBitmapFilter:          {ID: FeatureTraceIDBitmapFilter, MinVersion: v(25, 11), Stability: Experimental, AutoSelect: true, RequiresExperimentalTSGrid: false},
		FeatureArgAndMaxFusion:              {ID: FeatureArgAndMaxFusion, MinVersion: v(25, 11), Stability: Experimental, AutoSelect: true, RequiresExperimentalTSGrid: false},
		FeatureResultCache:                  {ID: FeatureResultCache, MinVersion: v(24, 8), Stability: Stable, AutoSelect: true, RequiresResultCacheCapability: true},
		FeatureLazyMaterialization:          {ID: FeatureLazyMaterialization, MinVersion: v(25, 11), Stability: Experimental, AutoSelect: true, RequiresExperimentalTSGrid: false},
		FeatureExplainEstimate:              {ID: FeatureExplainEstimate, MinVersion: AlwaysAvailable, Stability: Experimental, AutoSelect: false, RequiresExperimentalTSGrid: false},
		FeatureCardinalityProbe:             {ID: FeatureCardinalityProbe, MinVersion: AlwaysAvailable, Stability: Experimental, AutoSelect: false, RequiresExperimentalTSGrid: false},
		FeatureFullTextIndex:                {ID: FeatureFullTextIndex, MinVersion: v(26, 2), Stability: Experimental, AutoSelect: false, RequiresExperimentalTSGrid: false},
		FeatureTextIndexLineFilter:          {ID: FeatureTextIndexLineFilter, MinVersion: v(26, 4), Stability: Experimental, AutoSelect: false, RequiresExperimentalTSGrid: false},
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
		if f.MinVersion != w.MinVersion || f.Stability != w.Stability || f.AutoSelect != w.AutoSelect ||
			f.RequiresExperimentalTSGrid != w.RequiresExperimentalTSGrid || f.RequiresResultCacheCapability != w.RequiresResultCacheCapability {
			t.Errorf("feature %q = %+v; want minVersion/stability/autoSelect/requiresExperimentalTSGrid/requiresResultCacheCapability %+v", f.ID, f, w)
		}
	}
}

func TestResolve_Auto_CapabilityForbidden_DropsNativeKeepsStable(t *testing.T) {
	// A 25.9 server (every native floor met) whose boot verdict is FORBIDDEN:
	// auto drops all twelve native ts_grid_* features and keeps the
	// non-experimental stable ones (aggregation_in_order, condition_cache).
	// Eleven of the twelve are AutoSelect=true and each emits a boot WARN
	// naming the experimental setting + the fan-out fallback (auto is silent
	// on version skips, but NOT on a capability block — the operator should
	// see a working deployment lost the native path). ts_grid_changes is the
	// twelfth: it is AutoSelect=false (opt-in only, #1721), so auto never
	// even considers it — it is absent from the resolved set for that reason
	// alone, independent of the capability verdict, and produces no WARN of
	// its own. ResultCacheCapability=Available keeps the SEPARATE result-cache
	// probe clean — this test is about the ts-grid axis alone, and the two
	// axes are independent (a server can forbid one setting while permitting
	// the other) — so result_cache resolves in normally alongside the other
	// non-experimental stable features.
	set, warns, err := Resolve(Config{Optimizations: "auto", Capability: CapabilityForbidden, ResultCacheCapability: CapabilityAvailable}, v(25, 9))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertSet(t, set, FeatureAggregationInOrder, FeatureConditionCache, FeatureLagInFrameAdjacency, FeatureResultCache)
	for _, native := range []string{
		FeatureTSGridRange, FeatureTSGridIncrease, FeatureTSGridResample, FeatureTSGridChanges, FeatureTSGridResets,
		FeatureTSGridDeriv, FeatureTSGridPredictLinear, FeatureTSGridRecollapse,
		FeatureTSGridHistogram, FeatureTSGridDelta, FeatureTSGridIrate, FeatureTSGridIdelta,
	} {
		if set.Has(native) {
			t.Errorf("auto enabled %q on a capability-forbidden server; want it dropped to fan-out", native)
		}
	}
	if len(warns) != 11 {
		t.Fatalf("want one WARN per capability-dropped AutoSelect=true native feature (11, excludes ts_grid_changes which is opt-in only); got %d: %v", len(warns), warns)
	}
	for _, w := range warns {
		if !strings.Contains(w, "allow_experimental_time_series_aggregate_functions") || !strings.Contains(w, "fan-out") {
			t.Errorf("capability WARN %q must name the experimental setting + the fan-out fallback", w)
		}
	}
}

func TestResolve_Auto_CapabilityUnreachable_DropsNative(t *testing.T) {
	// An inconclusive (UNREACHABLE) verdict is conservative — identical to
	// FORBIDDEN for selection: auto drops the native family and keeps stable.
	// ResultCacheCapability=Available isolates this to the ts-grid axis alone.
	set, _, err := Resolve(Config{Optimizations: "auto", Capability: CapabilityUnreachable, ResultCacheCapability: CapabilityAvailable}, v(25, 9))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertSet(t, set, FeatureAggregationInOrder, FeatureConditionCache, FeatureLagInFrameAdjacency, FeatureResultCache)
	for _, native := range []string{FeatureTSGridRange, FeatureTSGridResample, FeatureTSGridChanges, FeatureTSGridResets} {
		if set.Has(native) {
			t.Errorf("auto enabled %q on an unreachable-capability server; want it dropped", native)
		}
	}
}

func TestResolve_Auto_CapabilityUnknown_DropsNative(t *testing.T) {
	// The zero-value verdict (canary never ran / not threaded in) is conservative:
	// the native family stays off so a caller that forgets to probe can never
	// silently re-enable the experimental path.
	set, _, err := Resolve(Config{Optimizations: "auto"}, v(25, 9))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertSet(t, set, FeatureAggregationInOrder, FeatureConditionCache, FeatureLagInFrameAdjacency)
}

func TestResolve_ExplicitTSGrid_CapabilityForbidden_EnforcingFatal(t *testing.T) {
	// An explicit ts_grid_range on a version-capable (25.9) server that FORBIDS
	// the setting behaves exactly like an explicit feature on a too-old server:
	// enforcing -> FATAL. The error names the feature + the experimental setting.
	_, _, err := Resolve(Config{
		Optimizations: "ts_grid_range",
		Mode:          Enforcing,
		Capability:    CapabilityForbidden,
	}, v(25, 9))
	if err == nil {
		t.Fatal("explicit ts_grid_range on a capability-forbidden server under enforcing: want fatal, got nil")
	}
	if !strings.Contains(err.Error(), "ts_grid_range") || !strings.Contains(err.Error(), "allow_experimental_time_series_aggregate_functions") {
		t.Errorf("err = %v; want it to name ts_grid_range + the experimental setting", err)
	}
}

func TestResolve_ExplicitTSGrid_CapabilityUnreachable_EnforcingDegradesNotFatal(t *testing.T) {
	// An explicit ts_grid_range on a version-capable (25.9) server whose boot
	// canary was INCONCLUSIVE (Unreachable) must NOT be fatal under enforcing --
	// unlike a FORBIDDEN verdict. The probe could not reach a verdict, so cerberus
	// mirrors the version probe's connectivity fallback: degrade to fan-out with a
	// WARN, never crash. (Before this fix the canary itself spuriously returned
	// Unreachable on every healthy server, and this enforcing+explicit path turned
	// that into a fatal that crashed boot.)
	set, warns, err := Resolve(Config{
		Optimizations: "ts_grid_range",
		Mode:          Enforcing,
		Capability:    CapabilityUnreachable,
	}, v(25, 9))
	if err != nil {
		t.Fatalf("explicit ts_grid_range + Unreachable under enforcing must degrade, not fatal; got err %v", err)
	}
	if set.Has(FeatureTSGridRange) {
		t.Error("inconclusive verdict must drop ts_grid_range to fan-out, not enable it")
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "ts_grid_range") || !strings.Contains(warns[0], "inconclusive") {
		t.Errorf("warns = %v; want one naming ts_grid_range + the inconclusive probe", warns)
	}
}

func TestResolve_ExplicitTSGrid_CapabilityUnknown_EnforcingDegradesNotFatal(t *testing.T) {
	// The zero-value verdict (canary never ran / not threaded in) is inconclusive
	// too: an explicit request under enforcing degrades with a WARN rather than a
	// fatal, exactly like Unreachable.
	set, warns, err := Resolve(Config{
		Optimizations: "ts_grid_range",
		Mode:          Enforcing,
		Capability:    CapabilityUnknown,
	}, v(25, 9))
	if err != nil {
		t.Fatalf("explicit ts_grid_range + Unknown under enforcing must degrade, not fatal; got err %v", err)
	}
	if set.Has(FeatureTSGridRange) {
		t.Error("unknown verdict must drop ts_grid_range to fan-out, not enable it")
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "inconclusive") {
		t.Errorf("warns = %v; want one naming the inconclusive probe", warns)
	}
}

func TestResolve_LegacyTrue_CapabilityUnreachable_EnforcingDegradesNotFatal(t *testing.T) {
	// The legacy force-enable follows the same inconclusive-is-not-fatal rule as
	// an explicit list: a 25.9-capable server with an Unreachable canary degrades
	// to fan-out with a WARN under enforcing, not a fatal.
	set, warns, err := Resolve(Config{
		Optimizations: "auto",
		Mode:          Enforcing,
		Capability:    CapabilityUnreachable,
		LegacyTSGrid:  LegacyFlag{Set: true, Value: true},
	}, v(25, 9))
	if err != nil {
		t.Fatalf("legacy true + Unreachable under enforcing must degrade, not fatal; got err %v", err)
	}
	if set.Has(FeatureTSGridRange) {
		t.Error("inconclusive verdict must drop the legacy force-enable to fan-out")
	}
	if !anyContains(warns, "inconclusive") {
		t.Errorf("warns = %v; want one naming the inconclusive probe", warns)
	}
}

func TestResolve_ExplicitTSGrid_CapabilityForbidden_PermissiveWarns(t *testing.T) {
	// Same shape under permissive: WARN + skip, no error, feature absent.
	set, warns, err := Resolve(Config{
		Optimizations: "ts_grid_range",
		Mode:          Permissive,
		Capability:    CapabilityForbidden,
	}, v(25, 9))
	if err != nil {
		t.Fatalf("Resolve: unexpected err %v", err)
	}
	if set.Has(FeatureTSGridRange) {
		t.Error("permissive enabled ts_grid_range on a capability-forbidden server")
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "ts_grid_range") || !strings.Contains(warns[0], "allow_experimental_time_series_aggregate_functions") {
		t.Errorf("permissive warns = %v; want one naming ts_grid_range + the experimental setting", warns)
	}
}

func TestResolve_ExplicitTSGrid_VersionTooOld_NotMaskedByCapability(t *testing.T) {
	// When the server is BOTH too old AND would forbid the setting, the
	// version floor is reported first (the most specific cause), not the
	// capability block. Enforcing on 25.0 -> fatal naming the 25.9 floor.
	_, _, err := Resolve(Config{
		Optimizations: "ts_grid_range",
		Mode:          Enforcing,
		Capability:    CapabilityForbidden,
	}, v(25, 0))
	if err == nil {
		t.Fatal("explicit ts_grid_range on 25.0 under enforcing: want fatal, got nil")
	}
	if !strings.Contains(err.Error(), "25.9") {
		t.Errorf("err = %v; want it to report the version floor (25.9), not the capability block", err)
	}
}

func TestResolve_ExplicitNonExperimental_CapabilityForbidden_StillEnabled(t *testing.T) {
	// A capability-forbidden verdict only gates the experimental ts_grid family.
	// Non-experimental features (condition_cache) are untouched and resolve
	// normally even under enforcing on a forbidden server.
	set, _, err := Resolve(Config{
		Optimizations: "condition_cache",
		Mode:          Enforcing,
		Capability:    CapabilityForbidden,
	}, v(25, 8))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertSet(t, set, FeatureConditionCache)
}

// TestResolve_ResultCache_AutoDropsOnForbiddenCapability_KeepsTSGridAxisIndependent
// pins result_cache's own capability axis: a server whose result-cache probe
// comes back FORBIDDEN drops result_cache from the auto set while the
// UNRELATED ts-grid axis (here left Available) resolves in its own features
// normally — the two axes never leak into each other.
func TestResolve_ResultCache_AutoDropsOnForbiddenCapability_KeepsTSGridAxisIndependent(t *testing.T) {
	set, warns, err := Resolve(Config{
		Optimizations:         "auto",
		Capability:            CapabilityAvailable,
		ResultCacheCapability: CapabilityForbidden,
	}, v(25, 9))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if set.Has(FeatureResultCache) {
		t.Error("auto enabled result_cache on a capability-forbidden server; want it dropped")
	}
	if !set.Has(FeatureTSGridRange) || !set.Has(FeatureConditionCache) {
		t.Errorf("result_cache's own forbidden capability leaked into the ts-grid/stable axes; set = %v", set.IDs())
	}
	if !anyContains(warns, "use_query_cache") {
		t.Errorf("warns = %v; want one naming use_query_cache", warns)
	}
}

// TestResolve_ResultCache_AutoDropsOnUnreachableOrUnknownCapability pins the
// conservative treatment of an inconclusive result-cache probe verdict,
// mirroring the ts-grid axis's own Unreachable/Unknown handling.
func TestResolve_ResultCache_AutoDropsOnUnreachableOrUnknownCapability(t *testing.T) {
	for _, capability := range []Capability{CapabilityUnreachable, CapabilityUnknown} {
		t.Run(capability.String(), func(t *testing.T) {
			set, _, err := Resolve(Config{Optimizations: "auto", ResultCacheCapability: capability}, v(25, 9))
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if set.Has(FeatureResultCache) {
				t.Errorf("auto enabled result_cache on capability=%v; want it dropped (conservative)", capability)
			}
		})
	}
}

// TestResolve_ExplicitResultCache_Supported pins the explicit-list path: a
// server that meets the 24.8 floor and permits the setting enables
// result_cache when it is listed explicitly (not just under auto).
func TestResolve_ExplicitResultCache_Supported(t *testing.T) {
	set, _, err := Resolve(Config{Optimizations: "result_cache", ResultCacheCapability: CapabilityAvailable}, v(24, 8))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	assertSet(t, set, FeatureResultCache)
}

// TestResolve_ExplicitResultCache_CapabilityForbidden_EnforcingFatal mirrors
// TestResolve_ExplicitTSGrid_CapabilityForbidden_EnforcingFatal for the
// result-cache axis: a definitive Forbidden verdict on an explicitly
// requested feature is fatal under enforcing.
func TestResolve_ExplicitResultCache_CapabilityForbidden_EnforcingFatal(t *testing.T) {
	_, _, err := Resolve(Config{
		Optimizations:         "result_cache",
		Mode:                  Enforcing,
		ResultCacheCapability: CapabilityForbidden,
	}, v(25, 9))
	if err == nil {
		t.Fatal("explicit result_cache on a capability-forbidden server under enforcing: want fatal, got nil")
	}
	if !strings.Contains(err.Error(), "result_cache") || !strings.Contains(err.Error(), "use_query_cache") {
		t.Errorf("err = %v; want it to name result_cache + use_query_cache", err)
	}
}

// TestResolve_ExplicitResultCache_CapabilityUnreachable_EnforcingDegradesNotFatal
// mirrors the ts-grid axis's own inconclusive-is-not-fatal rule: an explicit
// request whose probe could not reach a verdict degrades with a WARN under
// enforcing instead of crashing boot.
func TestResolve_ExplicitResultCache_CapabilityUnreachable_EnforcingDegradesNotFatal(t *testing.T) {
	set, warns, err := Resolve(Config{
		Optimizations:         "result_cache",
		Mode:                  Enforcing,
		ResultCacheCapability: CapabilityUnreachable,
	}, v(25, 9))
	if err != nil {
		t.Fatalf("explicit result_cache + Unreachable under enforcing must degrade, not fatal; got err %v", err)
	}
	if set.Has(FeatureResultCache) {
		t.Error("inconclusive verdict must drop result_cache, not enable it")
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "result_cache") || !strings.Contains(warns[0], "inconclusive") {
		t.Errorf("warns = %v; want one naming result_cache + the inconclusive probe", warns)
	}
}

func TestResolve_LegacyTrue_CapabilityForbidden_EnforcingFatal(t *testing.T) {
	// The legacy CERBERUS_EXPERIMENTAL_TS_GRID_RANGE force-enable is itself a
	// RequiresExperimentalTSGrid request, so a forbidden server makes it fatal
	// under enforcing exactly as a too-old server does.
	_, _, err := Resolve(Config{
		Optimizations: "auto",
		Mode:          Enforcing,
		Capability:    CapabilityForbidden,
		LegacyTSGrid:  LegacyFlag{Set: true, Value: true},
	}, v(25, 9))
	if err == nil {
		t.Fatal("legacy true on a capability-forbidden server under enforcing: want fatal, got nil")
	}
	if !strings.Contains(err.Error(), "allow_experimental_time_series_aggregate_functions") {
		t.Errorf("err = %v; want it to name the experimental setting", err)
	}
}

// TestResolve_Auto_RecollapseTracksTSGridRange pins the dependency the registry
// cannot express as a field: ts_grid_recollapse is a NARROWING of ts_grid_range
// (it defers the label-shaping tower past that feature's aggregate), so under
// auto the two must resolve as a unit. They share a 25.9 floor and the same
// experimental-setting gate precisely so this holds, and it is asserted in BOTH
// directions across every gate that can drop one of them — a floor drift or a
// changed RequiresExperimentalTSGrid on either entry would otherwise silently
// leave the deferred-shaping shape enabled with no native grid to defer past
// (or a native grid that never gets the hoist).
func TestResolve_Auto_RecollapseTracksTSGridRange(t *testing.T) {
	cases := []struct {
		name       string
		server     Version
		capability Capability
		want       bool
	}{
		{name: "25.9 available enables both", server: v(25, 9), capability: CapabilityAvailable, want: true},
		{name: "26.5 available enables both", server: v(26, 5), capability: CapabilityAvailable, want: true},
		{name: "25.8 below the shared floor drops both", server: v(25, 8), capability: CapabilityAvailable, want: false},
		{name: "25.6 below the shared floor drops both", server: v(25, 6), capability: CapabilityAvailable, want: false},
		{name: "capability forbidden drops both", server: v(25, 9), capability: CapabilityForbidden, want: false},
		{name: "capability unreachable drops both", server: v(25, 9), capability: CapabilityUnreachable, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			set, _, err := Resolve(Config{Optimizations: "auto", Capability: tc.capability}, tc.server)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got := set.Has(FeatureTSGridRange); got != tc.want {
				t.Errorf("Has(%q) = %v; want %v", FeatureTSGridRange, got, tc.want)
			}
			if got := set.Has(FeatureTSGridRecollapse); got != tc.want {
				t.Errorf("Has(%q) = %v; want %v", FeatureTSGridRecollapse, got, tc.want)
			}
		})
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

// The periodic re-resolution swaps the live set only when Equal reports a
// genuine transition, so both of Equal's arms decide whether a running server
// churns its optimizer state and its logs. Each arm is pinned on its own: a
// size difference, and a same-size set whose ids differ.
func TestEnabledSetEqual(t *testing.T) {
	resolve := func(selection string, server Version) EnabledSet {
		t.Helper()
		set, _, err := Resolve(Config{Optimizations: selection, Capability: CapabilityAvailable}, server)
		if err != nil {
			t.Fatalf("Resolve(%q, %v): %v", selection, server, err)
		}
		return set
	}

	cases := []struct {
		name string
		a, b EnabledSet
		want bool
	}{
		{
			name: "same selection on the same server re-probes to the same set",
			a:    resolve("aggregation_in_order,condition_cache", v(25, 8)),
			b:    resolve("aggregation_in_order,condition_cache", v(25, 8)),
			want: true,
		},
		{
			name: "both empty",
			a:    resolve("off", v(25, 9)),
			b:    resolve("off", v(25, 8)),
			want: true,
		},
		{
			name: "different sizes",
			a:    resolve("aggregation_in_order,condition_cache", v(25, 8)),
			b:    resolve("aggregation_in_order", v(25, 8)),
			want: false,
		},
		{
			// Same cardinality, disjoint ids: the length check passes and only
			// the membership loop can tell these apart.
			name: "same size, different ids",
			a:    resolve("aggregation_in_order", v(25, 8)),
			b:    resolve("condition_cache", v(25, 8)),
			want: false,
		},
		{
			name: "empty versus non-empty",
			a:    resolve("off", v(25, 8)),
			b:    resolve("aggregation_in_order", v(25, 8)),
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.a.Equal(tc.b); got != tc.want {
				t.Errorf("a.Equal(b) = %v; want %v (a=%v b=%v)", got, tc.want, tc.a.IDs(), tc.b.IDs())
			}
			// Equality is symmetric, and the loop only ever walks the receiver:
			// checking one direction alone would miss a subset-shaped bug.
			if got := tc.b.Equal(tc.a); got != tc.want {
				t.Errorf("b.Equal(a) = %v; want %v (a=%v b=%v)", got, tc.want, tc.a.IDs(), tc.b.IDs())
			}
		})
	}
}

func TestModeString(t *testing.T) {
	if got := Enforcing.String(); got != "enforcing" {
		t.Errorf("Enforcing.String() = %q; want %q", got, "enforcing")
	}
	if got := Permissive.String(); got != "permissive" {
		t.Errorf("Permissive.String() = %q; want %q", got, "permissive")
	}
}
