package chopt

import (
	"slices"
	"testing"
)

// TestKnownShapeDefect_Boundaries pins each shape defect's range at the
// builds the test/chserver surveys ran: the last affected build and the first
// clean one on either side, so a range widened or narrowed by one line fails.
func TestKnownShapeDefect_Boundaries(t *testing.T) {
	tests := []struct {
		id     string
		server Version
		want   bool
	}{
		{ShapeDefectConversionConditionCache, Version{Major: 25, Minor: 2, Patch: 2, Build: 1}, false},
		{ShapeDefectConversionConditionCache, Version{Major: 25, Minor: 3, Patch: 14, Build: 14}, true},
		{ShapeDefectConversionConditionCache, Version{Major: 26, Minor: 3, Patch: 62, Build: 3}, true},
		{ShapeDefectConversionConditionCache, Version{Major: 26, Minor: 8, Patch: 36, Build: 3}, true},
		{ShapeDefectConversionConditionCache, Version{Major: 26, Minor: 9, Patch: 1, Build: 1629}, false},
		{ShapeDefectConversionConditionCache, Version{Major: 26, Minor: 10, Patch: 2, Build: 103}, false},

		{ShapeDefectNegatedConjunctionProjection, Version{Major: 25, Minor: 10, Patch: 7, Build: 6}, false},
		{ShapeDefectNegatedConjunctionProjection, Version{Major: 25, Minor: 11, Patch: 1, Build: 558}, true},
		{ShapeDefectNegatedConjunctionProjection, Version{Major: 26, Minor: 3, Patch: 17, Build: 56}, true},
		{ShapeDefectNegatedConjunctionProjection, Version{Major: 26, Minor: 9, Patch: 25, Build: 15}, true},
		{ShapeDefectNegatedConjunctionProjection, Version{Major: 26, Minor: 10, Patch: 2, Build: 103}, false},

		{ShapeDefectTraceIDProjectionRead, Version{Major: 25, Minor: 10, Patch: 7, Build: 6}, false},
		{ShapeDefectTraceIDProjectionRead, Version{Major: 25, Minor: 11, Patch: 1, Build: 558}, true},
		{ShapeDefectTraceIDProjectionRead, Version{Major: 25, Minor: 11, Patch: 9, Build: 34}, true},
		{ShapeDefectTraceIDProjectionRead, Version{Major: 25, Minor: 12, Patch: 11, Build: 4}, false},
		{ShapeDefectTraceIDProjectionRead, Version{Major: 26, Minor: 3, Patch: 17, Build: 56}, false},

		{"no_such_defect", Version{Major: 26, Minor: 3, Patch: 17, Build: 56}, false},
	}
	for _, tc := range tests {
		if got := KnownShapeDefect(tc.id, tc.server); got != tc.want {
			t.Errorf("KnownShapeDefect(%s, %s) = %v; want %v", tc.id, tc.server, got, tc.want)
		}
	}
}

// TestResolve_ShapeDefectsWithholdNothing pins that a shape defect gates a
// query shape and never a feature: on a build carrying all three, every
// selection resolves the features it would resolve anyway — condition_cache
// and trace_id_bitmap_filter under auto, an explicit trace_id_projection
// under enforcing — and carries the defects.
func TestResolve_ShapeDefectsWithholdNothing(t *testing.T) {
	affected := Version{Major: 25, Minor: 11, Patch: 9, Build: 34}
	wantDefects := []string{
		ShapeDefectConversionConditionCache,
		ShapeDefectNegatedConjunctionProjection,
		ShapeDefectTraceIDProjectionRead,
	}

	auto, _, err := Resolve(Config{Optimizations: "auto", Mode: Enforcing, Capability: CapabilityAvailable}, affected)
	if err != nil {
		t.Fatalf("Resolve(auto): %v", err)
	}
	if !auto.Has(FeatureTraceIDBitmapFilter) {
		t.Errorf("auto on %s: %s withheld; a shape defect must not withhold a feature", affected, FeatureTraceIDBitmapFilter)
	}
	// 25.11 is inside condition_cache's own UnsafeBuilds (ClickHouse#105686),
	// so the condition-cache half is pinned on a 26.3 LTS build, which carries
	// ClickHouse#119168 and no build-wide defect.
	lts, _, err := Resolve(Config{Optimizations: "auto", Mode: Enforcing, Capability: CapabilityAvailable}, Version{Major: 26, Minor: 3, Patch: 17, Build: 56})
	if err != nil {
		t.Fatalf("Resolve(auto) on 26.3: %v", err)
	}
	if !lts.Has(FeatureConditionCache) || !lts.HasShapeDefect(ShapeDefectConversionConditionCache) {
		t.Errorf("auto on 26.3: condition_cache resolved = %v, ClickHouse#119168 carried = %v; want both",
			lts.Has(FeatureConditionCache), lts.HasShapeDefect(ShapeDefectConversionConditionCache))
	}
	if got := auto.ShapeDefectIDs(); !slices.Equal(got, wantDefects) {
		t.Errorf("auto on %s: ShapeDefectIDs() = %v; want %v", affected, got, wantDefects)
	}

	explicit, _, err := Resolve(Config{Optimizations: FeatureTraceIDProjection, Mode: Enforcing, Capability: CapabilityAvailable}, affected)
	if err != nil {
		t.Fatalf("Resolve(%s, enforcing) on %s: %v; a shape defect must not refuse boot", FeatureTraceIDProjection, affected, err)
	}
	if !explicit.Has(FeatureTraceIDProjection) {
		t.Errorf("explicit %s on %s: withheld", FeatureTraceIDProjection, affected)
	}

	off, _, err := Resolve(Config{Optimizations: "off", Mode: Enforcing}, affected)
	if err != nil {
		t.Fatalf("Resolve(off): %v", err)
	}
	if got := off.ShapeDefectIDs(); !slices.Equal(got, wantDefects) {
		t.Errorf("off on %s: ShapeDefectIDs() = %v; want %v (the server's defects do not depend on the selection)", affected, got, wantDefects)
	}

	clean, _, err := Resolve(Config{Optimizations: "auto", Mode: Enforcing, Capability: CapabilityAvailable}, Version{Major: 26, Minor: 10, Patch: 2, Build: 103})
	if err != nil {
		t.Fatalf("Resolve(auto) on a clean build: %v", err)
	}
	if got := clean.ShapeDefectIDs(); len(got) != 0 {
		t.Errorf("auto on a clean build: ShapeDefectIDs() = %v; want none", got)
	}
}

// TestEnabledSet_ShapeDefectsInEqual pins that a set differing only in its
// shape defects is a transition the re-probe must swap in.
func TestEnabledSet_ShapeDefectsInEqual(t *testing.T) {
	a := EnabledSet{}
	b := a.WithShapeDefects([]string{ShapeDefectConversionConditionCache})
	if a.Equal(b) {
		t.Error("sets differing only in shape defects compare equal")
	}
	if !b.Equal(a.WithShapeDefects([]string{ShapeDefectConversionConditionCache})) {
		t.Error("sets carrying the same shape defects compare unequal")
	}
	if !b.HasShapeDefect(ShapeDefectConversionConditionCache) || b.HasShapeDefect(ShapeDefectTraceIDProjectionRead) {
		t.Errorf("HasShapeDefect disagrees with WithShapeDefects: %v", b.ShapeDefectIDs())
	}
}

// TestShapeDefects_RangesStartAtOrAboveTheirSetting pins what makes stamping
// a gate safe on a mixed fleet: every build inside a defect's ranges, and
// inside condition_cache's known-unsafe ranges, accepts the setting its gate
// stamps. A node too old for the setting therefore predates the defect; it
// rejects the query instead of answering it wrong.
func TestShapeDefects_RangesStartAtOrAboveTheirSetting(t *testing.T) {
	check := func(owner, setting string, ranges []BuildRange) {
		g, ok := GateSettingByName(setting)
		if !ok {
			t.Errorf("%s: gate setting %q is not in the gate-setting table", owner, setting)
			return
		}
		if len(g.Defects) == 0 {
			t.Errorf("%s: gate setting %s names no defect", owner, setting)
		}
		for _, b := range ranges {
			if b.From.Less(g.Since) {
				t.Errorf("%s: range from %s starts below %s's first accepting release %s", owner, b.From, setting, g.Since)
			}
		}
	}
	for _, d := range ShapeDefects() {
		check(d.ID, d.Setting, d.Builds)
	}
	check(FeatureConditionCache, SettingUseQueryConditionCache, mustFeature(t, FeatureConditionCache).UnsafeBuilds)
}

// mustFeature returns the registry entry for id.
func mustFeature(t *testing.T, id string) Feature {
	t.Helper()
	for _, f := range Registry() {
		if f.ID == id {
			return f
		}
	}
	t.Fatalf("feature %s is not in the registry", id)
	return Feature{}
}
