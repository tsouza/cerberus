package regression

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// This file pins the `.gitattributes` gate that stops git from three-way
// line-merging a GENERATED artefact.
//
// PR #1422 merged test/perf/cardinality-baseline.json — a sorted array of
// per-fixture records — with NO conflict. Both sides had inserted entries at
// nearby offsets, git's line-based merge shifted the record boundaries, and
// fixture names ended up paired with the next record's metrics: the whole
// traceql block came out off by one, and so did the promql
// histogram_quantile_classic_* block from the other side. The result parsed as
// valid JSON, had a plausible length, and every entry in the blended region was
// wrong — so the cardinality ratchet, whose entire job is catching perf
// regressions, went on reporting green while measuring nothing real.
//
// A flagged conflict is loud and gets regenerated. This class is dangerous
// precisely because it is NOT a conflict. `-merge` converts the silent case
// into the loud one: git falls back to the binary merge driver, refuses to
// blend, and leaves the path conflicted — so the only correct resolution, take
// the merged code and re-run the generator, is the only one available.

// gitattributesPath is the gate, relative to this package's directory.
const gitattributesPath = repoRoot + "/.gitattributes"

// mergeAttrUnset is what `git check-attr merge -- <path>` reports for a path
// marked `-merge`. Git spells "this attribute is explicitly turned off" as
// `unset`; `unspecified` means no rule matched the path at all, which is the
// ungated default this gate exists to move files off.
const mergeAttrUnset = "unset"

// checkAttrSeparator splits `git check-attr`'s `<path>: <attr>: <state>` output.
// A path may itself contain a colon, so the state is read from the LAST
// separator rather than by splitting into a fixed field count.
const checkAttrSeparator = ": "

// handAuthoredMarker records, inside the gate itself, a file that is NAMED like
// a generated artefact but is maintained by hand. Writing the exemption next to
// the rule is what keeps the classification a decision someone made rather than
// a gap nobody noticed.
const handAuthoredMarker = "# hand-authored: "

// generatedArtifact is one committed file written by a regeneration command
// rather than by hand, together with the distinctive fragment of that command
// which `.gitattributes` must carry. Pinning the fragment is what keeps the gate
// self-documenting: whoever first hits one of these conflicts reads the
// resolution off the file that caused it.
type generatedArtifact struct {
	// path is repo-root-relative, in the form `git ls-files` prints.
	path string
	// regen is a substring of the regeneration instruction that
	// `.gitattributes` must carry.
	regen string
}

// migrationGoldenRecipe regenerates every Tier-0 migration golden. It refuses
// to run when CI is set, so only a local run repairs these. `just
// update-golden` chains it (see TestUpdateGoldenChainsMigrationGolden), but the
// instruction names this recipe directly: a resolver fixing one stale artifact
// wants the command that regenerates exactly it, not the umbrella that also
// rewrites every TXTAR fixture and both perf baselines.
const migrationGoldenRecipe = "just migration-golden"

// inventoryUpdateEnv is the env gate that rewrites each feature/surface
// inventory in place from the current parser surface.
const inventoryUpdateEnv = "CERBERUS_UPDATE_INVENTORY=1"

// crawlInventorySpec is the Playwright spec that rewrites the Grafana surface
// inventories; it needs the named stack healthy, so the resolution is heavier
// than a `go test` and the gate says so.
const crawlInventorySpec = "npx playwright test crawl/crawl.spec.ts"

// parityBaselineSync is the ONLY entry here that is not a local command. It
// rewrites a head's entry from that head's compat-cases.json RUN ARTEFACT,
// which only a compatibility job against a live reference backend produces —
// naming the script rather than a recipe is the point: it tells the resolver
// that re-running something locally is not on the table.
const parityBaselineSync = "compat-baseline-sync.mjs"

// generatedArtifacts is the canonical roster of committed generated artefacts.
// Every entry must be `-merge` gated and documented in `.gitattributes`.
var generatedArtifacts = []generatedArtifact{
	{"test/perf/cardinality-baseline.json", "just update-cardinality-baseline"},
	{"test/perf/solver-decision-baseline.json", "just update-solver-decision-baseline"},
	{"test/perf/scale-wall-baseline.json", "just update-scale-wall-baseline"},

	{"compatibility/parity-baseline.json", parityBaselineSync},
	{"compatibility/loki/upstream-skip-baseline.txt", "-regen-baseline"},

	{"test/e2e/migration/archetypes/already-otel/expected/corpus.json", migrationGoldenRecipe},
	{"test/e2e/migration/archetypes/already-otel/expected/explain.txt", migrationGoldenRecipe},
	{"test/e2e/migration/archetypes/already-otel/expected/gate.json", migrationGoldenRecipe},
	{"test/e2e/migration/archetypes/kube-prometheus-stack/expected/corpus.json", migrationGoldenRecipe},
	{"test/e2e/migration/archetypes/kube-prometheus-stack/expected/explain.txt", migrationGoldenRecipe},
	{"test/e2e/migration/archetypes/kube-prometheus-stack/expected/gate.json", migrationGoldenRecipe},
	{"test/e2e/migration/archetypes/kube-prometheus-stack/expected/rulegraph.json", migrationGoldenRecipe},
	{"test/e2e/migration/archetypes/mimir-cortex/expected/corpus.json", migrationGoldenRecipe},
	{"test/e2e/migration/archetypes/mimir-cortex/expected/explain.txt", migrationGoldenRecipe},
	{"test/e2e/migration/archetypes/mimir-cortex/expected/gate.json", migrationGoldenRecipe},
	{"test/e2e/migration/archetypes/prometheus-thanos/expected/corpus.json", migrationGoldenRecipe},
	{"test/e2e/migration/archetypes/prometheus-thanos/expected/explain.txt", migrationGoldenRecipe},
	{"test/e2e/migration/archetypes/prometheus-thanos/expected/gate.json", migrationGoldenRecipe},
	{"test/e2e/migration/archetypes/prometheus-thanos/expected/lookback.json", migrationGoldenRecipe},
	{"test/e2e/migration/archetypes/regulated-airgapped/expected/corpus.json", migrationGoldenRecipe},
	{"test/e2e/migration/archetypes/regulated-airgapped/expected/explain.txt", migrationGoldenRecipe},
	{"test/e2e/migration/archetypes/regulated-airgapped/expected/gate.json", migrationGoldenRecipe},
	{"test/e2e/migration/archetypes/saas-repatriation/expected/classify.json", migrationGoldenRecipe},
	{"test/e2e/migration/archetypes/saas-repatriation/expected/corpus.json", migrationGoldenRecipe},
	{"test/e2e/migration/archetypes/saas-repatriation/expected/explain.txt", migrationGoldenRecipe},
	{"test/e2e/migration/archetypes/saas-repatriation/expected/gate.json", migrationGoldenRecipe},
	{"test/e2e/migration/archetypes/three-signal/expected/corpus.json", migrationGoldenRecipe},
	{"test/e2e/migration/archetypes/three-signal/expected/explain.txt", migrationGoldenRecipe},
	{"test/e2e/migration/archetypes/three-signal/expected/gate.json", migrationGoldenRecipe},
	{"test/e2e/migration/archetypes/three-signal/expected/lookback.json", migrationGoldenRecipe},
	{"test/e2e/migration/archetypes/victoriametrics/expected/classify.json", migrationGoldenRecipe},
	{"test/e2e/migration/archetypes/victoriametrics/expected/corpus.json", migrationGoldenRecipe},
	{"test/e2e/migration/archetypes/victoriametrics/expected/explain.txt", migrationGoldenRecipe},
	{"test/e2e/migration/archetypes/victoriametrics/expected/gate.json", migrationGoldenRecipe},
	{"test/e2e/migration/archetypes/victoriametrics/expected/lookback.json", migrationGoldenRecipe},
	{"test/e2e/migration/expected/schema/default.sql", migrationGoldenRecipe},
	{"test/e2e/migration/expected/schema/metrics-ttl-override.sql", migrationGoldenRecipe},

	{"test/e2e/grafana/ql-inventory/promql-feature-inventory.json", inventoryUpdateEnv},
	{"test/e2e/grafana/ql-inventory/logql-feature-inventory.json", inventoryUpdateEnv},
	{"test/e2e/grafana/ql-inventory/traceql-feature-inventory.json", inventoryUpdateEnv},
	{"test/rejection-parity/catalogue.json", inventoryUpdateEnv},
	{"test/surface-parity/inventory.json", inventoryUpdateEnv},
	{"test/surface-parity/promql-reference-verdicts.json", "promql-surface-gate.mjs"},

	{"test/e2e/playwright/crawl/grafana-surface-inventory.compose.json", crawlInventorySpec},
	{"test/e2e/playwright/crawl/grafana-surface-inventory.k3d.json", crawlInventorySpec},
}

// TestGeneratedArtifactsRefuseLineMerge asserts every generated artefact is
// `-merge` gated and that `.gitattributes` names the command that regenerates
// it. The second half is not decoration: a conflict git refuses to resolve is
// only an improvement if the person staring at it can find the generator.
func TestGeneratedArtifactsRefuseLineMerge(t *testing.T) {
	t.Parallel()

	attrBody := readGitattributes(t)

	for _, artifact := range generatedArtifacts {
		t.Run(artifact.path, func(t *testing.T) {
			t.Parallel()

			if _, err := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(artifact.path))); err != nil {
				t.Fatalf("%s is listed as a generated artefact but is not present: %v. If it was "+
					"renamed or retired, update this roster and %s together rather than leaving "+
					"the gate pointing at nothing.", artifact.path, err, gitattributesPath)
			}

			if got := mergeAttr(t, artifact.path); got != mergeAttrUnset {
				t.Errorf("%s has merge=%s, want %s. It is written by %q, so a three-way line "+
					"merge of it is never a correct resolution — and when both sides insert "+
					"records at nearby offsets git produces NO conflict, shifts the record "+
					"boundaries, and yields a file that still parses while every blended entry "+
					"is wrong (PR #1422). Add a `-merge` entry for it in %s.",
					artifact.path, got, mergeAttrUnset, artifact.regen, gitattributesPath)
			}

			if !strings.Contains(attrBody, artifact.regen) {
				t.Errorf("%s does not mention %q, the command that regenerates %s. Refusing the "+
					"merge is only half the fix: whoever hits the conflict must be able to read "+
					"the correct resolution off the gate itself instead of guessing.",
					gitattributesPath, artifact.regen, artifact.path)
			}
		})
	}
}

// generatedNameMarkers are the naming families cerberus gives its generated data
// artefacts. They are the net for artefacts added AFTER this gate landed: the
// roster above can only cover what its author knew about, so anything committed
// under one of these names has to be classified here one way or the other.
var generatedNameMarkers = []string{"baseline", "inventory", "catalogue", ".pin."}

// generatedDataExtensions restrict the net to DATA files. The same words appear
// in the names of the Go and Node sources that PRODUCE these artefacts
// (compat-baseline-sync.mjs, inventory.go, catalogue.go); those are hand-written
// code, and code is line-mergeable.
var generatedDataExtensions = []string{".json", ".txt"}

// TestNoGeneratedArtifactEscapesTheMergeGate walks every tracked file and fails
// on one that is named like a generated artefact yet is neither `-merge` gated
// nor recorded as hand-authored in the gate. Without this the gate decays the
// moment someone adds the next baseline: the roster above would still pass while
// the new file merges silently, which is the shape of the bug rather than a fix
// for it.
func TestNoGeneratedArtifactEscapesTheMergeGate(t *testing.T) {
	t.Parallel()

	attrBody := readGitattributes(t)

	// The net is name-based, so it sees only the subset of the roster that is
	// actually NAMED like a generated artefact — the migration goldens are
	// gated by path pattern instead and never match it. Counting that subset
	// here is what makes the reachability check below compare like with like.
	rostered := make(map[string]struct{}, len(generatedArtifacts))
	var rosteredMatchingNet int
	for _, artifact := range generatedArtifacts {
		rostered[artifact.path] = struct{}{}
		if looksGenerated(artifact.path) {
			rosteredMatchingNet++
		}
	}

	cmd := exec.Command("git", "ls-files")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}

	var matched int
	for _, tracked := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if !looksGenerated(tracked) {
			continue
		}
		matched++

		gated := mergeAttr(t, tracked) == mergeAttrUnset
		if _, ok := rostered[tracked]; ok {
			if !gated {
				// The per-artefact test above reports this in full; counting it
				// here too would only duplicate the failure.
				continue
			}

			continue
		}

		if gated {
			t.Errorf("%s is `-merge` gated in %s but is missing from generatedArtifacts. The "+
				"roster is what pins the regeneration command next to the rule, so an entry "+
				"only in %s leaves the conflict undiagnosable. Add it, with the command that "+
				"regenerates it.", tracked, gitattributesPath, gitattributesPath)

			continue
		}

		if !strings.Contains(attrBody, handAuthoredMarker+tracked) {
			t.Errorf("%s is named like a generated artefact but is neither `-merge` gated nor "+
				"recorded as hand-authored in %s. If a command regenerates it, gate it and add "+
				"it to generatedArtifacts — an ungated generated baseline is how PR #1422's "+
				"silent blend got in. If it really is hand-maintained, say so there with a "+
				"%q%s line and the reason a bad merge of it would fail loudly.",
				tracked, gitattributesPath, handAuthoredMarker, tracked)
		}
	}

	if matched < rosteredMatchingNet {
		t.Fatalf("the generated-name net matched only %d tracked files, fewer than the %d rostered "+
			"artefacts whose own names match it. The net is no longer reaching the tree it "+
			"polices — check that this test runs with the repository root at %s.",
			matched, rosteredMatchingNet, repoRoot)
	}
}

// looksGenerated reports whether a tracked path is named like a generated data
// artefact.
func looksGenerated(path string) bool {
	name := strings.ToLower(filepath.Base(path))

	var dataFile bool
	for _, ext := range generatedDataExtensions {
		if strings.HasSuffix(name, ext) {
			dataFile = true

			break
		}
	}
	if !dataFile {
		return false
	}

	for _, marker := range generatedNameMarkers {
		if strings.Contains(name, marker) {
			return true
		}
	}

	return false
}

// readGitattributes loads the gate, failing loudly if it has gone missing —
// without it every generated baseline is back to being silently line-merged.
func readGitattributes(t *testing.T) string {
	t.Helper()

	buf, err := os.ReadFile(gitattributesPath)
	if err != nil {
		t.Fatalf("read %s: %v. This file IS the gate.", gitattributesPath, err)
	}

	return string(buf)
}

// mergeAttr returns the state git resolves the `merge` attribute to for path.
func mergeAttr(t *testing.T, path string) string {
	t.Helper()

	cmd := exec.Command("git", "check-attr", "merge", "--", path)
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git check-attr merge -- %s: %v", path, err)
	}

	line := strings.TrimSpace(string(out))
	idx := strings.LastIndex(line, checkAttrSeparator)
	if idx < 0 {
		t.Fatalf("git check-attr merge -- %s printed %q, which carries no %q separator",
			path, line, checkAttrSeparator)
	}

	return line[idx+len(checkAttrSeparator):]
}
