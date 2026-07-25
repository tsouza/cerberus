package steps

import (
	"fmt"
	"os"
	"sort"

	"github.com/tsouza/cerberus/internal/migrate"
	"github.com/tsouza/cerberus/internal/migrategate"
)

// GateFold is one archetype's whole offline chain folded into a single cutover
// decision: harvest → classify → rule graph → gate. It carries the raw decision
// bytes (compared against the archetype's committed golden), the decoded
// decision, and the status the gate left with.
type GateFold struct {
	Archetype string
	Raw       []byte
	Decision  migrategate.Decision
	ExitCode  int
	RuleGraph migrate.RuleGraph
	Classify  migrate.Classification
}

// CommittedArchetypes lists every archetype directory the harness ships, sorted.
// The fold walks THIS list rather than a hand-maintained one, so an archetype
// added without a committed decision fails the fold instead of sitting in the
// tree unread.
func CommittedArchetypes(root string) ([]string, error) {
	dir := harnessPath(root, archetypeDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("migration harness: read the archetype fixtures at %s: %w", dir, err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("migration harness: no archetype fixtures under %s", dir)
	}
	sort.Strings(out)
	return out, nil
}

// GoldenGatePath is the committed decision one archetype's fold is compared
// against.
func GoldenGatePath(root, archetype string) string {
	return harnessPath(root, archetypeDir, archetype, expectedDir, gateGolden)
}

// FoldGate runs one archetype's offline chain and returns the decision. work is
// a caller-owned scratch directory; every intermediate artifact is written
// under it, so the repository tree is never touched.
//
// The chain deliberately runs WITHOUT a parity artifact: producing one needs a
// live reference backend, which no offline tier has. The resulting decision is
// therefore the honest one for an operator who has run only the offline blocks,
// and each archetype's committed golden pins exactly which stages object.
func FoldGate(root, bin, work, archetype string) (GateFold, error) {
	w := NewWorld(root, bin)
	w.work = work
	w.archetypes = []string{archetype}
	w.corpusRaw, w.corpus = map[string][]byte{}, map[string]migrate.Corpus{}
	w.classifyRaw, w.classify = map[string][]byte{}, map[string]migrate.Classification{}
	w.ruleGraphRaw, w.ruleGraph = map[string][]byte{}, map[string]migrate.RuleGraph{}

	if err := w.loadCorpora(); err != nil {
		return GateFold{}, err
	}
	if err := w.whenClassify(); err != nil {
		return GateFold{}, err
	}
	if err := w.whenBuildRuleGraph(); err != nil {
		return GateFold{}, err
	}
	run, err := w.runGate(archetype, gateGolden)
	if err != nil {
		return GateFold{}, err
	}
	return GateFold{
		Archetype: archetype,
		Raw:       run.raw,
		Decision:  run.decision,
		ExitCode:  run.exitCode,
		RuleGraph: w.ruleGraph[archetype],
		Classify:  w.classify[archetype],
	}, nil
}
