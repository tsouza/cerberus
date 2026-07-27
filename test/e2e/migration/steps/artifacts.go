package steps

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tsouza/cerberus/internal/migrate"
	"github.com/tsouza/cerberus/test/e2e/migration/lib"
)

// The fixture sub-directories every archetype carries and the directory its
// committed goldens live in.
const (
	rulesDir      = "rules"
	dashboardsDir = "dashboards"
	expectedDir   = "expected"
)

// ruleGlob matches the rule files inside an archetype. It is passed to the CLI
// verbatim, which expands it itself, so the artifact's provenance strings name
// the individual files rather than the pattern.
const ruleGlob = "*.yaml"

// provenancePrefixes are the two forms `cerberus migrate harvest` stamps onto
// every query it emits: the rule it came from, or the dashboard panel target it
// came from. A corpus entry carrying neither has lost the provenance the whole
// corpus exists to record.
var provenancePrefixes = []string{"rule:", "dashboard:"}

// workspaceDirMode is the permission the per-archetype artifact directory gets
// inside the scenario workspace.
const workspaceDirMode = 0o755

// archetypeInput is one archetype's fixture inputs, expressed relative to the
// repository root. The commands run with the repository root as their working
// directory, so these relative paths are what lands in every artifact's
// provenance strings — which is what makes a committed golden portable between
// a developer's checkout and a CI runner.
type archetypeInput struct {
	name       string
	rules      string
	dashboards string
}

// inputFor resolves one archetype's fixture inputs.
func inputFor(name string) archetypeInput {
	base := filepath.Join(lib.HarnessDir, archetypeDir, name)
	return archetypeInput{
		name:       name,
		rules:      filepath.ToSlash(filepath.Join(base, rulesDir, ruleGlob)),
		dashboards: filepath.ToSlash(filepath.Join(base, dashboardsDir)),
	}
}

// goldenPath resolves one of an archetype's committed goldens.
func (w *World) goldenPath(archetype, name string) string {
	return harnessPath(w.root, archetypeDir, archetype, expectedDir, name)
}

// workDir resolves — creating it on first use — this scenario's workspace
// directory for one archetype: where its artifacts land, and where the
// cerberus.yaml a config-file-driven command reads is written.
func (w *World) workDir(archetype string) (string, error) {
	if w.work == "" {
		return "", fmt.Errorf("migration harness: no scenario workspace; the Before hook did not run")
	}
	dir := filepath.Join(w.work, archetype)
	if err := os.MkdirAll(dir, workspaceDirMode); err != nil {
		return "", fmt.Errorf("migration harness: create the workspace for %s: %w", archetype, err)
	}
	return dir, nil
}

// workPath resolves a path for an artifact this scenario is about to write. The
// suffix distinguishes a re-run of the same command, so a determinism check
// compares two files rather than a file with itself.
func (w *World) workPath(archetype, name string) (string, error) {
	dir, err := w.workDir(archetype)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// requireArchetypes fails when a step that loops over the scenario's tagged
// archetypes has none to loop over. Without it, a scenario whose `@archetype:`
// tags were dropped would run every Then over an empty list and pass while
// asserting nothing.
func (w *World) requireArchetypes(step string) error {
	if len(w.archetypes) == 0 {
		return fmt.Errorf("%s: the scenario selected no archetypes; an %s tag is what feeds this step",
			step, strings.TrimSuffix(archetypeTagPrefix, ":"))
	}
	return nil
}

// runArtifact runs one `cerberus migrate` block for an archetype, writing its
// artifact to outName inside the scenario workspace, and returns the bytes it
// wrote. A non-zero exit is a failure naming the command and its stderr: every
// Tier-0 block except the gate is expected to succeed, and the gate has its own
// step that captures the exit code as data.
func (w *World) runArtifact(archetype, outName string, args ...string) ([]byte, error) {
	out, err := w.workPath(archetype, outName)
	if err != nil {
		return nil, err
	}
	res, err := w.run(nil, append(args, "--out", out)...)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("migration harness: `cerberus %s` for %s exited %d: %s",
			strings.Join(args, " "), archetype, res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	// Assert against the environment this exact command actually ran under,
	// not a freshly re-derived one — see World.envUsed.
	if err := lib.RequireOffline(res.Env); err != nil {
		return nil, err
	}
	w.envUsed[archetype] = res.Env
	return lib.ReadArtifact(out)
}

// harvestArchetype runs `migrate harvest` over one archetype's committed rule
// files and dashboards, writing the corpus to outName in the workspace.
func (w *World) harvestArchetype(archetype, outName string) ([]byte, error) {
	in := inputFor(archetype)
	return w.runArtifact(archetype, outName, "migrate", "harvest", "--rules", in.rules, "--dashboards", in.dashboards)
}

// loadCorpora harvests every tagged archetype and decodes each corpus, so a
// Given can establish the corpus a later step reads without duplicating the
// harvest wiring.
func (w *World) loadCorpora() error {
	for _, a := range w.archetypes {
		data, err := w.harvestArchetype(a, "corpus.json")
		if err != nil {
			return err
		}
		var c migrate.Corpus
		if err := lib.DecodeArtifact(data, &c); err != nil {
			return fmt.Errorf("archetype %s: %w", a, err)
		}
		if len(c.Queries) == 0 {
			return fmt.Errorf("archetype %s harvested an empty corpus; a fixture that yields no query cannot exercise anything", a)
		}
		w.corpusRaw[a], w.corpus[a] = data, c
	}
	return nil
}

// corpusSources returns the set of provenance strings an archetype's corpus
// carries, so a later artifact's own source strings can be checked against the
// corpus they claim to come from.
func (w *World) corpusSources(archetype string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, q := range w.corpus[archetype].Queries {
		out[q.Source] = struct{}{}
	}
	return out
}

// hasProvenancePrefix reports whether a source string names a rule or a
// dashboard panel target, which are the only two provenance forms the harvester
// emits.
func hasProvenancePrefix(source string) bool {
	for _, p := range provenancePrefixes {
		if strings.HasPrefix(source, p) {
			return true
		}
	}
	return false
}

// requireStatedReasons asserts that every dropped entry names both what was
// dropped and why. A drop with a blank reason is an input that vanished without
// the operator ever learning it existed.
func requireStatedReasons(archetype, list string, entries []migrate.SkippedEntry) error {
	for _, e := range entries {
		if strings.TrimSpace(e.Source) == "" {
			return fmt.Errorf("archetype %s: a %s entry names no source: %+v", archetype, list, e)
		}
		if strings.TrimSpace(e.Reason) == "" {
			return fmt.Errorf("archetype %s: %s entry %q states no reason", archetype, list, e.Source)
		}
	}
	return nil
}
