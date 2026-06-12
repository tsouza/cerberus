//go:build chdb

package profile

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tsouza/cerberus/test/spec"
)

// FindExecutableFixtures walks every *.txtar under specDir (recursively)
// and returns the paths of fixtures that are executable round-trip
// fixtures — those declaring `seed:` + `expected_rows:` + a non-empty
// `sql:`. Non-executable fixtures (text-only goldens) are excluded:
// without a seed there is nothing to profile.
//
// Paths are returned sorted for deterministic profiling order.
func FindExecutableFixtures(specDir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(specDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".txtar") {
			return nil
		}
		c, lerr := spec.Load(path)
		if lerr != nil {
			return fmt.Errorf("load %s: %w", path, lerr)
		}
		prep, ok, perr := spec.PrepareRoundTrip(c)
		if perr != nil {
			// A malformed fixture is surfaced, not swallowed — the
			// nightly lane should see corpus corruption loudly.
			return fmt.Errorf("prepare %s: %w", path, perr)
		}
		if !ok || prep == nil {
			return nil
		}
		out = append(out, path)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// FixtureID derives the "<head>/<name>" identity used in Record.Fixture
// from a fixture path and the spec root. e.g. specDir="test/spec",
// path="test/spec/promql/lwr_range_rate.txtar" → "promql/lwr_range_rate".
// Falls back to the basename (sans .txtar) when path is not under specDir.
func FixtureID(specDir, path string) string {
	rel, err := filepath.Rel(specDir, path)
	if err != nil {
		rel = filepath.Base(path)
	}
	return strings.TrimSuffix(filepath.ToSlash(rel), ".txtar")
}

// ProfileCorpus profiles every executable fixture under specDir and
// returns the records, sorted descending by fan factor. Each fixture is
// loaded, prepared through the shared spec seam, and profiled against the
// shared chDB session. A per-fixture seed/exec error is captured in the
// fixture's Record.Err rather than aborting the whole corpus.
func ProfileCorpus(specDir string) ([]Record, error) {
	paths, err := FindExecutableFixtures(specDir)
	if err != nil {
		return nil, err
	}

	p, err := NewProfiler()
	if err != nil {
		return nil, err
	}
	defer p.Close()

	recs := make([]Record, 0, len(paths))
	for _, path := range paths {
		c, lerr := spec.Load(path)
		if lerr != nil {
			recs = append(recs, Record{
				Fixture: FixtureID(specDir, path),
				Err:     fmt.Sprintf("load: %v", lerr),
			})
			continue
		}
		prep, ok, perr := spec.PrepareRoundTrip(c)
		if perr != nil || !ok || prep == nil {
			recs = append(recs, Record{
				Fixture: FixtureID(specDir, path),
				Err:     fmt.Sprintf("prepare: ok=%v err=%v", ok, perr),
			})
			continue
		}
		recs = append(recs, p.ProfileFixture(FixtureID(specDir, path), prep))
	}

	SortByFanFactor(recs)
	return recs, nil
}
