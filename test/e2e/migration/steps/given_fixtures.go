package steps

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/cucumber/godog"
)

// registerFixtureSteps binds the Given steps that establish an archetype's
// committed inputs and the artifacts derived from them.
func (w *World) registerFixtureSteps(ctx *godog.ScenarioContext) {
	ctx.Step(`^the committed rule files and exported dashboards for each tagged archetype$`,
		w.givenCommittedFixtures)
	ctx.Step(`^the harvested corpus for each tagged archetype$`,
		w.givenHarvestedCorpus)
	ctx.Step(`^the committed rule files and the harvested corpus for each tagged archetype$`,
		w.givenFixturesAndCorpus)
}

// givenCommittedFixtures asserts every tagged archetype ships both fixture
// kinds and that neither is empty. An archetype whose rules or dashboards
// directory exists but holds nothing would harvest an empty corpus, which is
// the shape that greens a scenario while exercising nothing.
func (w *World) givenCommittedFixtures() error {
	if err := w.requireArchetypes("the committed fixtures"); err != nil {
		return err
	}
	for _, a := range w.archetypes {
		for _, sub := range []string{rulesDir, dashboardsDir} {
			dir := harnessPath(w.root, archetypeDir, a, sub)
			entries, err := os.ReadDir(dir)
			if err != nil {
				return fmt.Errorf("archetype %s has no %s fixtures at %s: %w", a, sub, dir, err)
			}
			var files int
			for _, e := range entries {
				if !e.IsDir() {
					files++
				}
			}
			if files == 0 {
				return fmt.Errorf("archetype %s has an empty %s directory at %s", a, sub, dir)
			}
		}
		expected := harnessPath(w.root, archetypeDir, a, expectedDir)
		if _, err := os.Stat(expected); err != nil {
			return fmt.Errorf("archetype %s ships no committed goldens at %s: %w", a, expected, err)
		}
	}
	return nil
}

// givenHarvestedCorpus harvests every tagged archetype's corpus, which the
// classify / explain / lookback scenarios read as their input.
func (w *World) givenHarvestedCorpus() error {
	if err := w.requireArchetypes("the harvested corpus"); err != nil {
		return err
	}
	return w.loadCorpora()
}

// givenFixturesAndCorpus establishes both: the rule files the graph reads for
// its recorded series, and the corpus it scans for consumers.
func (w *World) givenFixturesAndCorpus() error {
	if err := w.givenCommittedFixtures(); err != nil {
		return err
	}
	return w.loadCorpora()
}

// corpusPathFor returns the workspace path of an archetype's harvested corpus,
// failing when a Given did not harvest it first.
func (w *World) corpusPathFor(archetype string) (string, error) {
	if _, ok := w.corpus[archetype]; !ok {
		return "", fmt.Errorf("archetype %s has no harvested corpus; the scenario must establish one first", archetype)
	}
	path := filepath.Join(w.work, archetype, "corpus.json")
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("archetype %s: the harvested corpus is missing from the workspace: %w", archetype, err)
	}
	return path, nil
}
