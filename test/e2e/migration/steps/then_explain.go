package steps

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/cucumber/godog"

	"github.com/tsouza/cerberus/test/e2e/migration/lib"
)

// explainGolden is the committed report each archetype's explain is compared
// against; explainRerun is the second report a determinism check writes.
const (
	explainGolden = "explain.txt"
	explainRerun  = "explain.rerun.txt"
)

// registerExplainSteps binds the MIG-05 steps.
func (w *World) registerExplainSteps(ctx *godog.ScenarioContext) {
	ctx.Step(`^the operator explains the corpus offline$`, w.whenExplain)
	ctx.Step(`^each explain report matches its committed golden byte for byte$`, w.thenExplainMatchesGolden)
	ctx.Step(`^every explained query yields either ClickHouse SQL or a named unsupported symbol$`, w.thenExplainYieldsExactlyOne)
	ctx.Step(`^every query that yields SQL names the ClickHouse tables it would read$`, w.thenExplainNamesTables)
	ctx.Step(`^every unsupported entry names the corpus source it came from$`, w.thenExplainNamesSources)
	ctx.Step(`^explaining the same corpus a second time produces identical bytes$`, w.thenExplainIsReproducible)
	ctx.Step(`^explaining the corpus contacts no network endpoint$`, w.thenExplainRanOffline)
}

// whenExplain dry-runs every tagged archetype's corpus through the read
// pipeline, with no database in the loop.
func (w *World) whenExplain() error {
	if err := w.requireArchetypes("the explain report"); err != nil {
		return err
	}
	for _, a := range w.archetypes {
		data, err := w.explainArchetype(a, explainGolden)
		if err != nil {
			return err
		}
		rep, err := parseExplainReport(data)
		if err != nil {
			return fmt.Errorf("archetype %s: %w", a, err)
		}
		w.explainRaw[a], w.explain[a] = data, rep
	}
	return nil
}

// explainArchetype runs one `migrate explain` over an archetype's harvested
// corpus, writing to outName in the scenario workspace.
func (w *World) explainArchetype(archetype, outName string) ([]byte, error) {
	corpus, err := w.corpusPathFor(archetype)
	if err != nil {
		return nil, err
	}
	return w.runArtifact(archetype, outName, "migrate", "explain", "--corpus", corpus)
}

// thenExplainMatchesGolden compares each report against its committed golden
// and asserts the report's own headline tally agrees with the blocks it
// carries, so a truncated report cannot pass by matching a truncated golden.
func (w *World) thenExplainMatchesGolden() error {
	return w.eachExplain(func(a string, rep explainReport) error {
		if len(rep.Queries) == 0 {
			return fmt.Errorf("archetype %s explained no query", a)
		}
		if rep.HeaderQueries != len(rep.Queries) || rep.HeaderSkipped != len(rep.Skipped) {
			return fmt.Errorf("archetype %s: the report announces %d queries / %d skipped but carries %d / %d",
				a, rep.HeaderQueries, rep.HeaderSkipped, len(rep.Queries), len(rep.Skipped))
		}
		if want := len(w.corpus[a].Queries); len(rep.Queries) != want {
			return fmt.Errorf("archetype %s explained %d of the corpus's %d queries", a, len(rep.Queries), want)
		}
		return lib.AssertGolden(w.goldenPath(a, explainGolden), w.explainRaw[a])
	})
}

// thenExplainYieldsExactlyOne asserts each query landed on exactly one outcome.
// A block with neither is a query the preview said nothing about; a block with
// both would mean the report claims a query was rejected and emitted at once.
func (w *World) thenExplainYieldsExactlyOne() error {
	var emitted, rejected int
	if err := w.eachExplain(func(a string, rep explainReport) error {
		for _, q := range rep.Queries {
			switch {
			case q.SQL != "" && q.Unsupported != "":
				return fmt.Errorf("archetype %s: %s is reported as both emitted and unsupported", a, q.Source)
			case q.SQL != "":
				emitted++
			case q.Unsupported != "":
				rejected++
			default:
				return fmt.Errorf("archetype %s: %s yielded neither SQL nor a rejecting symbol", a, q.Source)
			}
			if q.Expr == "" {
				return fmt.Errorf("archetype %s: the block for %s carries no expression", a, q.Source)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if emitted == 0 {
		return fmt.Errorf("no tagged archetype emitted SQL for a single query; %v explain nothing", w.archetypes)
	}
	if rejected == 0 {
		return fmt.Errorf(
			"no tagged archetype had a query rejected, so the unsupported branch was never exercised; %v explain clean",
			w.archetypes,
		)
	}
	return nil
}

// thenExplainNamesTables asserts every emitted query names the physical tables
// it would read, that the list is sorted (the report is diffed in review), and
// that each name is one cerberus actually reads for some signal — a table name
// nobody recognises is a preview an operator cannot provision against.
func (w *World) thenExplainNamesTables() error {
	known := map[string]struct{}{}
	for _, t := range signalTables() {
		known[t] = struct{}{}
	}
	return w.eachExplain(func(a string, rep explainReport) error {
		for _, q := range rep.Queries {
			if q.SQL == "" {
				if len(q.Tables) > 0 {
					return fmt.Errorf("archetype %s: rejected query %s still names tables %v", a, q.Source, q.Tables)
				}
				continue
			}
			if len(q.Tables) == 0 {
				return fmt.Errorf("archetype %s: %s emitted SQL but names no table", a, q.Source)
			}
			if !sort.StringsAreSorted(q.Tables) {
				return fmt.Errorf("archetype %s: %s names tables out of order: %v", a, q.Source, q.Tables)
			}
			for _, t := range q.Tables {
				if _, ok := known[t]; !ok {
					return fmt.Errorf("archetype %s: %s reads %q, which is not one of cerberus's signal tables %v",
						a, q.Source, t, signalTables())
				}
			}
		}
		return nil
	})
}

// thenExplainNamesSources asserts every rejected entry is traceable back to the
// corpus entry it came from. Without the source an operator is told something is
// unsupported but not which rule or panel to rewrite.
func (w *World) thenExplainNamesSources() error {
	return w.eachExplain(func(a string, rep explainReport) error {
		sources := w.corpusSources(a)
		for _, q := range rep.Queries {
			if q.Unsupported == "" {
				continue
			}
			if !hasProvenancePrefix(q.Source) {
				return fmt.Errorf("archetype %s: rejected entry names %q, which is neither a rule nor a dashboard", a, q.Source)
			}
			if _, ok := sources[q.Source]; !ok {
				return fmt.Errorf("archetype %s: rejected entry names source %q, which is not in the corpus it explained",
					a, q.Source)
			}
		}
		return nil
	})
}

// thenExplainIsReproducible re-explains the same corpus and asserts the second
// report is byte-identical.
func (w *World) thenExplainIsReproducible() error {
	return w.eachExplain(func(a string, _ explainReport) error {
		again, err := w.explainArchetype(a, explainRerun)
		if err != nil {
			return err
		}
		if !bytes.Equal(w.explainRaw[a], again) {
			return fmt.Errorf("archetype %s explained %d bytes the first time and %d the second; the preview is not reproducible",
				a, len(w.explainRaw[a]), len(again))
		}
		return nil
	})
}

// thenExplainRanOffline asserts the preview ran with every HTTP route closed
// and still emitted SQL, so "no database in the loop" is a property of the run
// rather than a claim about the tool.
func (w *World) thenExplainRanOffline() error {
	if err := lib.RequireOffline(lib.OfflineEnv()); err != nil {
		return err
	}
	return w.eachExplain(func(a string, rep explainReport) error {
		for _, q := range rep.Queries {
			if q.SQL != "" {
				return nil
			}
		}
		return fmt.Errorf("archetype %s emitted no SQL at all under the offline environment", a)
	})
}

// eachExplain runs fn over every tagged archetype's report, failing when a When
// never produced one.
func (w *World) eachExplain(fn func(archetype string, rep explainReport) error) error {
	if err := w.requireArchetypes("the explain assertion"); err != nil {
		return err
	}
	for _, a := range w.archetypes {
		rep, ok := w.explain[a]
		if !ok {
			return fmt.Errorf("archetype %s has no explain report; the scenario must explain first", a)
		}
		if err := fn(a, rep); err != nil {
			return err
		}
	}
	return nil
}
