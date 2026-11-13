package chopt

import "testing"

// TestDroppableSettings_TableIsConsistent pins what the retry relies on: each
// entry belongs to a registered feature whose floor is at or above the
// release that first knows the setting, so the feature never resolves in
// against a server that would reject it; the setting is unknown to the
// supported floor, or nothing could reject it; and no entry is a gate
// setting, which is stamped against a wrong-result defect and must never be
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
		if _, gate := GateSettingByName(s.Name); gate {
			t.Errorf("%s: a gate setting must never be droppable", s.Name)
		}
		if got, ok := LookupDroppableSetting(s.Name); !ok || got != s {
			t.Errorf("LookupDroppableSetting(%s) = %+v, %v; want %+v", s.Name, got, ok, s)
		}
	}
	if _, ok := LookupDroppableSetting(SettingUseQueryConditionCache); ok {
		t.Errorf("LookupDroppableSetting(%s) found a gate setting", SettingUseQueryConditionCache)
	}
}
