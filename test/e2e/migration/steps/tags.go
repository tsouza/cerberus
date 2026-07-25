package steps

import (
	"fmt"
	"os"
	"sort"
	"strings"

	messages "github.com/cucumber/messages/go/v21"
)

// archetypeTagPrefix marks a scenario tag naming an archetype fixture directory
// the scenario reads. It is the same vocabulary the enumerator projects into
// the coverage ratchet's JSON, so a tag typo surfaces in both places.
const archetypeTagPrefix = "@archetype:"

// archetypeDir is the fixture root, relative to the harness tree, holding one
// directory per archetype.
const archetypeDir = "archetypes"

// ArchetypesOf returns the archetype names a scenario's tags select, sorted and
// deduplicated. Tags carrying any other vocabulary (the story id, the tier) are
// ignored here — the enumerator is what reports an unrecognised one.
func ArchetypesOf(tags []*messages.PickleTag) []string {
	seen := make(map[string]struct{}, len(tags))
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		if tag == nil {
			continue
		}
		name, ok := strings.CutPrefix(tag.Name, archetypeTagPrefix)
		if !ok || name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// requireArchetypeFixtures fails when a scenario's `@archetype:` tag names a
// fixture directory that is not there. The tags are load-bearing input — every
// "for each tagged archetype" step loops over exactly this list — so a tag
// pointing at nothing would silently shrink a scenario to a loop over no
// archetypes and pass while asserting nothing.
func requireArchetypeFixtures(root string, archetypes []string) error {
	for _, name := range archetypes {
		dir := harnessPath(root, archetypeDir, name)
		info, err := os.Stat(dir)
		if err != nil {
			return fmt.Errorf("archetype %q has no fixture directory at %s: %w", name, dir, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("archetype %q resolves to %s, which is not a directory", name, dir)
		}
	}
	return nil
}
