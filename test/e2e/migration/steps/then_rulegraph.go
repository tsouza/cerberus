package steps

import (
	"bytes"
	"fmt"

	"github.com/cucumber/godog"

	"github.com/tsouza/cerberus/internal/migrate"
	"github.com/tsouza/cerberus/test/e2e/migration/lib"
)

// ruleGraphGolden is the committed graph each archetype's build is compared
// against; ruleGraphRerun is the second build a determinism check writes.
const (
	ruleGraphGolden = "rulegraph.json"
	ruleGraphRerun  = "rulegraph.rerun.json"
)

// registerRuleGraphSteps binds the MIG-04 steps.
func (w *World) registerRuleGraphSteps(ctx *godog.ScenarioContext) {
	ctx.Step(`^the operator builds the recording-rule dependency graph offline$`, w.whenBuildRuleGraph)
	ctx.Step(`^each rule graph matches its committed golden byte for byte$`, w.thenRuleGraphMatchesGolden)
	ctx.Step(`^every recorded series is marked either consumed or orphan$`, w.thenRecordedSeriesAreClassified)
	ctx.Step(`^every consumed series lists the consumers that keep it materialised$`, w.thenConsumedSeriesListConsumers)
	ctx.Step(`^every rule input the builder could not parse is counted with a stated reason$`, w.thenRuleGraphCountsSkips)
	ctx.Step(`^building the graph from the same inputs a second time produces identical bytes$`, w.thenRuleGraphIsReproducible)
}

// whenBuildRuleGraph links each archetype's recorded series to the consumers
// that keep them materialised.
func (w *World) whenBuildRuleGraph() error {
	if err := w.requireArchetypes("the rule graph"); err != nil {
		return err
	}
	for _, a := range w.archetypes {
		data, err := w.buildRuleGraph(a, ruleGraphGolden)
		if err != nil {
			return err
		}
		var g migrate.RuleGraph
		if err := lib.DecodeArtifact(data, &g); err != nil {
			return fmt.Errorf("archetype %s: %w", a, err)
		}
		w.ruleGraphRaw[a], w.ruleGraph[a] = data, g
	}
	return nil
}

// buildRuleGraph runs one `migrate rulegraph` over an archetype's rule files
// and harvested corpus, writing to outName in the scenario workspace.
func (w *World) buildRuleGraph(archetype, outName string) ([]byte, error) {
	corpus, err := w.corpusPathFor(archetype)
	if err != nil {
		return nil, err
	}
	in := inputFor(archetype)
	return w.runArtifact(archetype, outName,
		"migrate", "rulegraph", "--rules", in.rules, "--corpus", corpus, "--json")
}

// thenRuleGraphMatchesGolden compares each graph against its committed golden,
// having first established that the archetype declares recorded series at all.
func (w *World) thenRuleGraphMatchesGolden() error {
	return w.eachRuleGraph(func(a string, g migrate.RuleGraph) error {
		if g.Counts.Recorded == 0 {
			return fmt.Errorf("archetype %s declares no recorded series, so its graph decides nothing", a)
		}
		if g.SchemaVersion != migrate.RuleGraphVersion {
			return fmt.Errorf("archetype %s emitted rule-graph version %d, this build reads %d",
				a, g.SchemaVersion, migrate.RuleGraphVersion)
		}
		return lib.AssertGolden(w.goldenPath(a, ruleGraphGolden), w.ruleGraphRaw[a])
	})
}

// thenRecordedSeriesAreClassified asserts every recorded series carries one of
// the two verdicts and that the headline tally partitions them. A series left
// unclassified is one the operator cannot decide about: keep materialising it,
// or drop it.
func (w *World) thenRecordedSeriesAreClassified() error {
	return w.eachRuleGraph(func(a string, g migrate.RuleGraph) error {
		var consumed, orphan int
		for _, n := range g.Recorded {
			switch n.Status {
			case migrate.StatusConsumed:
				consumed++
			case migrate.StatusOrphan:
				orphan++
			default:
				return fmt.Errorf("archetype %s: recorded series %q carries status %q, which is neither %q nor %q",
					a, n.Name, n.Status, migrate.StatusConsumed, migrate.StatusOrphan)
			}
			if n.Name == "" || n.Source == "" {
				return fmt.Errorf("archetype %s: a recorded node names %q from %q", a, n.Name, n.Source)
			}
		}
		if len(g.Recorded) != g.Counts.Recorded {
			return fmt.Errorf("archetype %s lists %d recorded series but counts %d", a, len(g.Recorded), g.Counts.Recorded)
		}
		if consumed != g.Counts.Consumed || orphan != g.Counts.Orphan {
			return fmt.Errorf("archetype %s marks %d consumed / %d orphan but counts %d / %d",
				a, consumed, orphan, g.Counts.Consumed, g.Counts.Orphan)
		}
		if consumed+orphan != g.Counts.Recorded {
			return fmt.Errorf("archetype %s: %d consumed + %d orphan does not equal %d recorded",
				a, consumed, orphan, g.Counts.Recorded)
		}
		return nil
	})
}

// thenConsumedSeriesListConsumers asserts each consumed series names the
// queries that keep it materialised, and that every named consumer is one the
// graph actually scanned. The edge list is the operator's work order: it is
// what has to keep being produced after cutover.
func (w *World) thenConsumedSeriesListConsumers() error {
	var consumed int
	if err := w.eachRuleGraph(func(a string, g migrate.RuleGraph) error {
		scanned := map[string]struct{}{}
		for _, c := range g.Consumers {
			scanned[c.Source] = struct{}{}
		}
		for _, n := range g.Recorded {
			if n.Status != migrate.StatusConsumed {
				if len(n.Consumers) > 0 {
					return fmt.Errorf("archetype %s: orphan series %q lists consumers %v", a, n.Name, n.Consumers)
				}
				continue
			}
			consumed++
			if len(n.Consumers) == 0 {
				return fmt.Errorf("archetype %s: consumed series %q lists no consumer", a, n.Name)
			}
			for _, src := range n.Consumers {
				if _, ok := scanned[src]; !ok {
					return fmt.Errorf("archetype %s: series %q names consumer %q, which the graph never scanned (it scanned %v)",
						a, n.Name, src, sortedKeys(scanned))
				}
				if !hasProvenancePrefix(src) {
					return fmt.Errorf("archetype %s: series %q names consumer %q, which is neither a rule nor a dashboard",
						a, n.Name, src)
				}
			}
		}
		for _, c := range g.Consumers {
			if len(c.References) == 0 {
				return fmt.Errorf("archetype %s: consumer %s is listed but references no recorded series", a, c.Source)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if consumed == 0 {
		return fmt.Errorf(
			"no tagged archetype has a consumed recorded series, so the edge list was never exercised; %v record only orphans",
			w.archetypes,
		)
	}
	return nil
}

// thenRuleGraphCountsSkips asserts every input the builder could not use is
// listed with a reason and included in the headline tally, and that the tagged
// set actually exercises that path. A silently dropped consumer could leave a
// truly-needed series marked orphan, which is the unsafe direction.
func (w *World) thenRuleGraphCountsSkips() error {
	var skipped int
	if err := w.eachRuleGraph(func(a string, g migrate.RuleGraph) error {
		if len(g.Skipped) != g.Counts.Skipped {
			return fmt.Errorf("archetype %s lists %d skipped inputs but counts %d", a, len(g.Skipped), g.Counts.Skipped)
		}
		skipped += len(g.Skipped)
		return requireStatedReasons(a, "rule-graph skipped", g.Skipped)
	}); err != nil {
		return err
	}
	if skipped == 0 {
		return fmt.Errorf(
			"no tagged archetype fed the graph an input it could not use, so the skip accounting was never exercised; %v parse cleanly",
			w.archetypes,
		)
	}
	return nil
}

// thenRuleGraphIsReproducible re-builds the graph from the same inputs and
// asserts the second build is byte-identical.
func (w *World) thenRuleGraphIsReproducible() error {
	return w.eachRuleGraph(func(a string, _ migrate.RuleGraph) error {
		again, err := w.buildRuleGraph(a, ruleGraphRerun)
		if err != nil {
			return err
		}
		if !bytes.Equal(w.ruleGraphRaw[a], again) {
			return fmt.Errorf("archetype %s built %d bytes the first time and %d the second; the graph is not reproducible",
				a, len(w.ruleGraphRaw[a]), len(again))
		}
		return nil
	})
}

// eachRuleGraph runs fn over every tagged archetype's graph, failing when a
// When never built one.
func (w *World) eachRuleGraph(fn func(archetype string, g migrate.RuleGraph) error) error {
	if err := w.requireArchetypes("the rule-graph assertion"); err != nil {
		return err
	}
	for _, a := range w.archetypes {
		g, ok := w.ruleGraph[a]
		if !ok {
			return fmt.Errorf("archetype %s has no rule graph; the scenario must build one first", a)
		}
		if err := fn(a, g); err != nil {
			return err
		}
	}
	return nil
}
