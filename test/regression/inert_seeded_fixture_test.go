package regression

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/test/spec"
)

// specCorpusDir is the TXTAR corpus root, relative to test/regression.
const specCorpusDir = "../spec"

// aboveChDBFloorSection is the ONE marker that lets a seeded fixture be
// non-executable on the chDB substrate. It is a dedicated section rather
// than a reuse of the `experimental_ts_grid_*` opt-ins: those are
// CAPABILITY switches (they select a native lowerer), not exclusions —
// all 15 fixtures carrying one round-trip fine, so keying the gate off
// them would blanket-exempt a set with nothing to exempt while still
// admitting inert fixtures that carry no such section.
const aboveChDBFloorSection = "above_chdb_floor"

// The marker body must answer both questions the reviewer would ask, as
// `key: value` lines. Anything less is prose that drifts.
const (
	floorFeatureKey   = "feature:"
	floorValidatedKey = "validated:"
)

// TestNoInertSeededFixtures fails when a TXTAR fixture carries a
// non-empty `-- seed --` but cannot execute, without declaring why.
//
// The class it pins: a seeded fixture with no `-- expected_rows --`
// section is INERT everywhere. spec.RoundTripSections.IsRoundTrip
// requires both, the chDB runner returns early when it is false, and the
// GOLDEN_UPDATE=1 rewrite sits downstream of that early return — so the
// section can never be created by regeneration either. Such a fixture
// contributes a `-- sql --` shape golden and nothing else while LOOKING
// like round-trip coverage. Eleven fixtures were in that state — four
// whose header prose claimed a ClickHouse version floor and seven that
// claimed nothing — and ALL ELEVEN turned out to execute on the pinned
// chDB substrate once bootstrapped. That is precisely why the absence of
// an `-- expected_rows --` section must never be read as "presumably
// gated": here it was wrong eleven times out of eleven, and one of the
// seven was hiding a TraceQL emitter bug (spanset set-ops over
// trace-granularity arms emitted SQL ClickHouse always rejected).
//
// The rule is symmetric and carries no allow-list — it enumerates the
// corpus and asks each fixture the same two questions:
//
//   - seeded and NOT executable → must carry the marker section.
//   - seeded and executable     → must NOT carry it (a stale marker is a
//     claim of non-executability that the fixture itself disproves).
//
// A fixture legitimately above the chDB version floor states the CH
// feature and where it IS validated instead, in the fixture, so the
// exclusion travels with the thing excluded rather than living in a list
// somewhere else.
func TestNoInertSeededFixtures(t *testing.T) {
	t.Parallel()

	paths := corpusFixtures(t)
	if len(paths) == 0 {
		t.Fatalf("no fixtures found under %s — the gate would pass vacuously", specCorpusDir)
	}

	for _, path := range paths {
		id := fixtureID(path)
		c, err := spec.Load(path)
		if err != nil {
			t.Errorf("%s: load: %v", id, err)
			continue
		}
		rt, err := spec.LoadRoundTrip(c)
		if err != nil {
			t.Errorf("%s: load round-trip sections: %v", id, err)
			continue
		}
		if strings.TrimSpace(rt.Seed) == "" {
			// Text-only golden. Nothing was promised, nothing is owed.
			continue
		}

		executable := rt.IsRoundTrip() && strings.TrimSpace(rt.SQL) != ""
		marker, hasMarker := c.Section(aboveChDBFloorSection)

		for _, problem := range seededFixtureProblems(executable, hasMarker, marker) {
			t.Errorf("%s: %s", id, problem)
		}
	}
}

// seededFixtureProblems is the whole rule, as a pure function so every
// quadrant is exercised by TestSeededFixtureProblems rather than only the
// quadrants the corpus happens to occupy today. An empty result means the
// fixture is fine.
func seededFixtureProblems(executable, hasMarker bool, marker string) []string {
	switch {
	case executable && hasMarker:
		return []string{fmt.Sprintf(
			"carries `-- %s --` but round-trips on chDB — the marker claims the fixture "+
				"cannot execute here and it demonstrably can. Delete the section.",
			aboveChDBFloorSection,
		)}
	case executable:
		return nil
	case !hasMarker:
		return []string{fmt.Sprintf(
			"has a non-empty `-- seed --` but is NOT executable, and declares no reason. "+
				"A seeded fixture with no `-- expected_rows --` is inert in every lane and "+
				"regeneration cannot create the section. Either bootstrap it — add "+
				"`-- expected_rows --` with `[]` and run `just update-golden` to fill the real "+
				"cell — or, if the query needs a ClickHouse feature above the chDB floor, add:\n"+
				"\t-- %s --\n\t%s <ClickHouse function/version>\n\t%s <lane that proves it instead>",
			aboveChDBFloorSection, floorFeatureKey, floorValidatedKey,
		)}
	default:
		return floorMarkerProblems(marker)
	}
}

// floorMarkerProblems reports what an above_chdb_floor body is missing.
// An empty result means the marker is well-formed.
func floorMarkerProblems(body string) []string {
	var problems []string
	for _, key := range []string{floorFeatureKey, floorValidatedKey} {
		if strings.TrimSpace(valueForKey(body, key)) == "" {
			problems = append(problems, fmt.Sprintf(
				"`-- %s --` is missing a non-empty `%s` line — state it so the exclusion is reviewable",
				aboveChDBFloorSection, key,
			))
		}
	}
	return problems
}

// TestSeededFixtureProblems exercises every quadrant of the rule
// directly. The corpus currently occupies only the "executable, no
// marker" quadrant — every seeded fixture round-trips on the pinned chDB
// substrate — so without this table the marker branches would be
// unexercised code that silently rots into an always-pass escape hatch.
func TestSeededFixtureProblems(t *testing.T) {
	t.Parallel()

	const goodMarker = "feature: timeSeriesFooToGrid (ClickHouse 99.9)\n" +
		"validated: compatibility/prometheus against a >= 99.9 server\n"

	cases := []struct {
		name       string
		executable bool
		hasMarker  bool
		marker     string
		wantN      int
	}{
		{name: "executable, no marker — real coverage", executable: true, wantN: 0},
		{name: "executable with a stale marker", executable: true, hasMarker: true, marker: goodMarker, wantN: 1},
		{name: "inert and undeclared", wantN: 1},
		{name: "inert, fully declared", hasMarker: true, marker: goodMarker, wantN: 0},
		{name: "inert, empty marker body", hasMarker: true, wantN: 2},
		{
			name: "inert, feature named but no validation lane", hasMarker: true,
			marker: "feature: timeSeriesFooToGrid\n", wantN: 1,
		},
		{
			name: "inert, keys present but valueless", hasMarker: true,
			marker: "feature:\nvalidated:   \n", wantN: 2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := seededFixtureProblems(tc.executable, tc.hasMarker, tc.marker)
			if len(got) != tc.wantN {
				t.Fatalf("got %d problem(s), want %d: %v", len(got), tc.wantN, got)
			}
		})
	}
}

// valueForKey returns the text after the first line starting with key.
func valueForKey(body, key string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, key); ok {
			return after
		}
	}
	return ""
}

// corpusFixtures returns every *.txtar under the spec corpus, sorted.
func corpusFixtures(t *testing.T) []string {
	t.Helper()

	var out []string
	err := filepath.WalkDir(specCorpusDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".txtar") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", specCorpusDir, err)
	}
	sort.Strings(out)
	return out
}

// fixtureID renders "<head>/<name>" for error messages.
func fixtureID(path string) string {
	rel, err := filepath.Rel(specCorpusDir, path)
	if err != nil {
		rel = filepath.Base(path)
	}
	return strings.TrimSuffix(filepath.ToSlash(rel), ".txtar")
}
