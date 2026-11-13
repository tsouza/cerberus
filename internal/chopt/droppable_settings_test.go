package chopt

import "testing"

// TestDroppableSettings_TableIsConsistent pins what the retry relies on: each
// entry belongs to a registered feature whose floor is at or above the
// release that first knows the setting, so the feature never resolves in
// against a server that would reject it; the setting is unknown to the
// supported floor, or nothing could reject it; and an entry that shares its
// name with a gate setting is restricted to a value other than the gate's
// stamp (0), which is stamped against a wrong-result defect and must never be
// dropped.
func TestDroppableSettings_TableIsConsistent(t *testing.T) {
	t.Parallel()
	supportedFloor := Version{Major: 24, Minor: 8}
	features := map[string]Feature{}
	for _, f := range Registry() {
		features[f.ID] = f
	}
	seen := map[string]bool{}
	for _, s := range DroppableSettings() {
		if seen[s.Name] {
			t.Errorf("%s: listed twice", s.Name)
		}
		seen[s.Name] = true
		f, ok := features[s.Feature]
		if !ok {
			t.Errorf("%s: feature %q is not in the registry", s.Name, s.Feature)
			continue
		}
		if f.MinVersion.Less(s.Since) {
			t.Errorf("%s: feature %s resolves in from %s, below %s, the first release that knows the setting", s.Name, s.Feature, f.MinVersion, s.Since)
		}
		if !supportedFloor.Less(s.Since) {
			t.Errorf("%s: known since %s, at or below the supported floor %s; no node can reject it", s.Name, s.Since, supportedFloor)
		}
		if _, gate := GateSettingByName(s.Name); gate && (s.OnlyAt == nil || s.DroppableAt(gateStamp)) {
			t.Errorf("%s: a gate setting is droppable at its gate stamp %v", s.Name, gateStamp)
		}
		if got, ok := LookupDroppableSetting(s.Name); !ok || got != s {
			t.Errorf("LookupDroppableSetting(%s) = %+v, %v; want %+v", s.Name, got, ok, s)
		}
	}
	for _, g := range GateSettings() {
		if d, ok := LookupDroppableSetting(g.Name); ok && d.DroppableAt(gateStamp) {
			t.Errorf("gate setting %s is droppable at its gate stamp %v", g.Name, gateStamp)
		}
	}
}

// gateStamp is the value every gate setting is stamped at: the mechanism off.
const gateStamp = 0

// TestDroppableSetting_DroppableAt: an unrestricted entry drops at any value;
// the condition cache drops switched on and never switched off, whatever
// integer type carries the value.
func TestDroppableSetting_DroppableAt(t *testing.T) {
	t.Parallel()
	join, _ := LookupDroppableSetting(SettingMaxBytesBeforeExternalJoin)
	if !join.DroppableAt(0) || !join.DroppableAt(1<<30) {
		t.Errorf("%s: not droppable at every value", join.Name)
	}
	cache, ok := LookupDroppableSetting(SettingUseQueryConditionCache)
	if !ok {
		t.Fatalf("%s: not in the table", SettingUseQueryConditionCache)
	}
	for _, on := range []any{1, uint8(1), int64(1)} {
		if !cache.DroppableAt(on) {
			t.Errorf("%s=%v (%T): not droppable; want droppable when it only switches the cache on", cache.Name, on, on)
		}
	}
	for _, off := range []any{0, uint8(0)} {
		if cache.DroppableAt(off) {
			t.Errorf("%s=%v (%T): droppable; the gate's stamp must never be dropped", cache.Name, off, off)
		}
	}
}
