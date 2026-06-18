package config

import (
	"sort"
	"strings"
	"testing"
)

// TestEnvDocsCoverAllKeys asserts the documentation metadata (envDocs) and the
// loader's key set (allEnvKeys) are in 1:1 correspondence: every CERBERUS_*
// key the loader resolves has exactly one EnvDoc, and no EnvDoc documents a
// key the loader does not resolve. This is the coverage gate that makes a new
// env var with no metadata (or a stale doc entry for a removed key) fail
// `go test` BEFORE the docs/configuration.md drift gate runs in CI - so the
// generated reference can never silently omit a real knob.
func TestEnvDocsCoverAllKeys(t *testing.T) {
	keySet := make(map[string]bool, len(allEnvKeys))
	for _, k := range allEnvKeys {
		if keySet[k] {
			t.Errorf("allEnvKeys contains duplicate key %q", k)
		}
		keySet[k] = true
	}

	docSet := make(map[string]bool, len(envDocs))
	for _, d := range envDocs {
		if docSet[d.Key] {
			t.Errorf("envDocs contains duplicate entry for %q", d.Key)
		}
		docSet[d.Key] = true
	}

	// Every loader key must be documented.
	var missing []string
	for k := range keySet {
		if !docSet[k] {
			missing = append(missing, k)
		}
	}
	// No doc may reference a key the loader does not resolve.
	var extra []string
	for k := range docSet {
		if !keySet[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("envDocs is missing %d key(s) the loader resolves (add them to envDocs):\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
	if len(extra) > 0 {
		t.Errorf("envDocs documents %d key(s) the loader does NOT resolve (remove them or add to allEnvKeys):\n  %s",
			len(extra), strings.Join(extra, "\n  "))
	}
}

// TestEnvDocsFieldsPopulated asserts every EnvDoc carries the hand-authored
// fields the generated table needs (Type, Group, Desc) - an empty field would
// render a broken table cell. Key emptiness is implicitly covered by the
// coverage test above (an empty key cannot match a real loader key).
func TestEnvDocsFieldsPopulated(t *testing.T) {
	for _, d := range envDocs {
		if strings.TrimSpace(d.Type) == "" {
			t.Errorf("%s: empty Type", d.Key)
		}
		if strings.TrimSpace(d.Group) == "" {
			t.Errorf("%s: empty Group", d.Key)
		}
		if strings.TrimSpace(d.Desc) == "" {
			t.Errorf("%s: empty Desc", d.Key)
		}
		// A pipe in a single-cell Desc would break the markdown table.
		if strings.Contains(d.Desc, "|") {
			t.Errorf("%s: Desc contains a raw '|' which breaks the table cell", d.Key)
		}
		if strings.Contains(d.Desc, "\n") {
			t.Errorf("%s: Desc must be single-line", d.Key)
		}
	}
}

// TestEnvDocGroupsBijection asserts the section list (envDocGroups) and the
// groups actually used by envDocs are in 1:1 correspondence: every EnvDoc.Group
// is declared in envDocGroups, and every declared group owns at least one
// EnvDoc. A group with no keys (or a key with an undeclared group) would make
// the generator error, so catching it here keeps the failure on `go test`.
func TestEnvDocGroupsBijection(t *testing.T) {
	declared := make(map[string]bool, len(envDocGroups))
	for _, g := range envDocGroups {
		if declared[g.Name] {
			t.Errorf("envDocGroups declares duplicate group %q", g.Name)
		}
		declared[g.Name] = true
	}

	used := make(map[string]bool)
	for _, d := range envDocs {
		used[d.Group] = true
		if !declared[d.Group] {
			t.Errorf("%s: group %q is not declared in envDocGroups", d.Key, d.Group)
		}
	}
	for name := range declared {
		if !used[name] {
			t.Errorf("envDocGroups declares group %q which no EnvDoc uses", name)
		}
	}
}

// TestDocDefaultsCoverAllKeys asserts DocDefaults returns a rendered default
// for every loader key, so the generator never emits a blank Default cell.
func TestDocDefaultsCoverAllKeys(t *testing.T) {
	defaults := DocDefaults()
	if len(defaults) != len(allEnvKeys) {
		t.Errorf("DocDefaults returned %d entries; want %d", len(defaults), len(allEnvKeys))
	}
	for _, k := range allEnvKeys {
		v, ok := defaults[k]
		if !ok {
			t.Errorf("DocDefaults missing key %q", k)
			continue
		}
		if strings.TrimSpace(v) == "" {
			t.Errorf("DocDefaults[%q] is empty", k)
		}
	}
}
