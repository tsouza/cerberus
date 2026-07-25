package steps

import (
	"bytes"
	"fmt"

	"github.com/cucumber/godog"

	"github.com/tsouza/cerberus/internal/migrate"
	"github.com/tsouza/cerberus/test/e2e/migration/lib"
)

// corpusGolden is the committed corpus each archetype's harvest is compared
// against.
const corpusGolden = "corpus.json"

// rerunSuffix names the second artifact a determinism check writes, so the two
// runs are compared as two files rather than a file with itself.
const rerunSuffix = "corpus.rerun.json"

// registerHarvestSteps binds the MIG-01 steps.
func (w *World) registerHarvestSteps(ctx *godog.ScenarioContext) {
	ctx.Step(`^the operator harvests the query corpus for each archetype$`, w.whenHarvest)
	ctx.Step(`^each corpus matches its committed golden byte for byte$`, w.thenCorpusMatchesGolden)
	ctx.Step(`^each corpus names the rule or dashboard each query came from$`, w.thenCorpusNamesProvenance)
	ctx.Step(`^no corpus repeats an identical expression from an identical source$`, w.thenCorpusHasNoExactDuplicate)
	ctx.Step(`^each corpus accounts for every input it dropped with a stated reason$`, w.thenCorpusAccountsForDrops)
	ctx.Step(`^harvesting the same inputs a second time produces identical bytes$`, w.thenHarvestIsReproducible)
	ctx.Step(`^harvesting the corpus contacts no network endpoint$`, w.thenHarvestRanOffline)
}

// whenHarvest harvests every tagged archetype's corpus from its committed rule
// files and exported dashboards.
func (w *World) whenHarvest() error {
	if err := w.requireArchetypes("the harvest"); err != nil {
		return err
	}
	return w.loadCorpora()
}

// thenCorpusMatchesGolden compares each harvested corpus against its committed
// golden byte for byte, having first established that the harvest produced
// queries at all — an empty corpus must never match an empty golden into a
// green scenario.
func (w *World) thenCorpusMatchesGolden() error {
	return w.eachCorpus(func(a string, c migrate.Corpus) error {
		if len(c.Queries) == 0 {
			return fmt.Errorf("archetype %s harvested no queries", a)
		}
		if c.Version != migrate.CorpusVersion {
			return fmt.Errorf("archetype %s harvested corpus version %d, this build reads %d",
				a, c.Version, migrate.CorpusVersion)
		}
		return lib.AssertGolden(w.goldenPath(a, corpusGolden), w.corpusRaw[a])
	})
}

// thenCorpusNamesProvenance asserts every harvested query carries the rule or
// dashboard panel it came from. Provenance is what makes the corpus a plan an
// operator can act on rather than a bag of strings.
func (w *World) thenCorpusNamesProvenance() error {
	return w.eachCorpus(func(a string, c migrate.Corpus) error {
		for _, q := range c.Queries {
			if !hasProvenancePrefix(q.Source) {
				return fmt.Errorf("archetype %s: query %q names no rule or dashboard (source %q)", a, q.Expr, q.Source)
			}
			if q.Kind == "" || q.Lang == "" {
				return fmt.Errorf("archetype %s: query from %s carries kind %q and language %q", a, q.Source, q.Kind, q.Lang)
			}
		}
		return nil
	})
}

// thenCorpusHasNoExactDuplicate asserts the corpus counts each query site once.
// A rule block copy-pasted inside one group yields two identical entries at
// harvest time; counting it twice inflates every downstream tally the cutover
// decision rests on.
func (w *World) thenCorpusHasNoExactDuplicate() error {
	return w.eachCorpus(func(a string, c migrate.Corpus) error {
		seen := make(map[migrate.CorpusQuery]struct{}, len(c.Queries))
		for _, q := range c.Queries {
			if _, dup := seen[q]; dup {
				return fmt.Errorf("archetype %s repeats %q from %s", a, q.Expr, q.Source)
			}
			seen[q] = struct{}{}
		}
		return nil
	})
}

// thenCorpusAccountsForDrops asserts every dropped input is named with a stated
// reason, and that the tagged set as a whole actually exercises the drop path.
// Without the second half a corpus set that drops nothing would pass this step
// while never reading the accounting it claims to check.
func (w *World) thenCorpusAccountsForDrops() error {
	var dropped int
	if err := w.eachCorpus(func(a string, c migrate.Corpus) error {
		dropped += len(c.Skipped)
		return requireStatedReasons(a, "skipped", c.Skipped)
	}); err != nil {
		return err
	}
	if dropped == 0 {
		return fmt.Errorf(
			"no tagged archetype dropped a single input, so the drop accounting was never exercised; %v carry no unusable input",
			w.archetypes,
		)
	}
	return nil
}

// thenHarvestIsReproducible re-harvests the same committed inputs and asserts
// the second run is byte-identical to the first. A corpus whose ordering
// wandered between runs could not be diffed in review, and every golden in this
// lane would be a coin flip.
func (w *World) thenHarvestIsReproducible() error {
	return w.eachCorpus(func(a string, _ migrate.Corpus) error {
		again, err := w.harvestArchetype(a, rerunSuffix)
		if err != nil {
			return err
		}
		if !bytes.Equal(w.corpusRaw[a], again) {
			return fmt.Errorf("archetype %s harvested %d bytes the first time and %d the second; the corpus is not reproducible",
				a, len(w.corpusRaw[a]), len(again))
		}
		return nil
	})
}

// thenHarvestRanOffline asserts the harvest ran with every HTTP route closed
// and still produced its corpus. Offline is proven by CONSTRUCTION here — the
// child had no proxy to reach and no bypass list to escape through — rather
// than by trusting the tool's own claim not to have dialled out.
func (w *World) thenHarvestRanOffline() error {
	if err := lib.RequireOffline(lib.OfflineEnv()); err != nil {
		return err
	}
	return w.eachCorpus(func(a string, c migrate.Corpus) error {
		if len(c.Queries) == 0 {
			return fmt.Errorf("archetype %s produced nothing under the offline environment", a)
		}
		return nil
	})
}

// eachCorpus runs fn over every tagged archetype's harvested corpus, failing
// when a Given never harvested one.
func (w *World) eachCorpus(fn func(archetype string, c migrate.Corpus) error) error {
	if err := w.requireArchetypes("the corpus assertion"); err != nil {
		return err
	}
	for _, a := range w.archetypes {
		c, ok := w.corpus[a]
		if !ok {
			return fmt.Errorf("archetype %s has no harvested corpus; the scenario must harvest one first", a)
		}
		if err := fn(a, c); err != nil {
			return err
		}
	}
	return nil
}
