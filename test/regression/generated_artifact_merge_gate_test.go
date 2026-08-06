package regression

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// This file pins the `.gitattributes` gate that stops git from three-way
// line-merging a GENERATED artefact.
//
// PR #1422 merged the cardinality ratchet baseline — a roster of per-fixture
// records — with NO conflict. Both sides had inserted entries at
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
	{"test/perf/scale-wall-baseline.json", "just update-scale-wall-baseline"},
	{"test/coverage-floor.json", "just update-coverage-floor"},

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
	{"test/surface-parity/inventory.json", inventoryUpdateEnv},
	{"test/surface-parity/promql-reference-verdicts.json", "promql-surface-gate.mjs"},

	{"test/e2e/playwright/crawl/grafana-surface-inventory.compose.json", crawlInventorySpec},
	{"test/e2e/playwright/crawl/grafana-surface-inventory.k3d.json", crawlInventorySpec},
}

// shardExt is the suffix every shard file carries; a shard tree holds nothing
// else.
const shardExt = ".json"

// shardTree is a generated artefact stored as a DIRECTORY of one shard per
// record rather than as a single file. Sharding is itself a merge measure —
// two branches touching different records write different paths and never
// meet — but it does not retire this gate: two branches moving the SAME record
// still meet in one shard, which is the #1422 shape again at shard
// granularity.
type shardTree struct {
	// dir is the tree root, repo-root-relative.
	dir string
	// regen is the fragment of the regeneration instruction
	// `.gitattributes` must carry, exactly as for a single-file artefact.
	regen string
	// what names the tree in the failure message that fires when it is
	// empty, so the reader knows what stopped being gated.
	what string
}

// shardTrees is the roster of sharded generated artefacts. Each is expanded
// into one roster entry per shard below.
var shardTrees = []shardTree{
	// The rejection catalogue: one shard per lowering SOURCE FILE
	// (test/rejection-parity/catalogue.go's shardName owns the mapping).
	{"test/rejection-parity/catalogue", inventoryUpdateEnv, "rejection site"},
	// The two perf ratchet baselines: one shard per RECORD — a profiled
	// fixture, filed under its head, and a classified corpus query
	// (test/perf/baseline_shards_test.go owns both mappings).
	{"test/perf/cardinality-baseline", "just update-cardinality-baseline", "profiled fixture"},
	{"test/perf/solver-decision-baseline", "just update-solver-decision-baseline", "classified query"},
}

// rosterArtifacts returns the hand-written roster with every shard tree
// expanded into one entry per shard. Enumerating them beats hard-coding
// hundreds of paths that turn over whenever a fixture or a lowering guard is
// added or dropped. The net below independently reaches the shards through
// their directory name, so the two overlap here by design: this roster
// additionally pins the regeneration command, which the net cannot.
func rosterArtifacts(t *testing.T) []generatedArtifact {
	t.Helper()

	out := make([]generatedArtifact, 0, len(generatedArtifacts))
	out = append(out, generatedArtifacts...)

	for _, tree := range shardTrees {
		root := filepath.Join(repoRoot, filepath.FromSlash(tree.dir))
		before := len(out)
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(d.Name(), shardExt) {
				return nil
			}
			rel, rerr := filepath.Rel(repoRoot, path)
			if rerr != nil {
				return rerr
			}
			out = append(out, generatedArtifact{filepath.ToSlash(rel), tree.regen})

			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v. It is a directory of per-record shards; if it moved, this gate "+
				"and %s have to move with it.", tree.dir, err, gitattributesPath)
		}
		if len(out) == before {
			t.Fatalf("%s holds no %s shard. An empty shard tree would make this gate vacuous for "+
				"every %s in the tree.", tree.dir, shardExt, tree.what)
		}
	}

	return out
}

// TestGeneratedArtifactsRefuseLineMerge asserts every generated artefact is
// `-merge` gated and that `.gitattributes` names the command that regenerates
// it. The second half is not decoration: a conflict git refuses to resolve is
// only an improvement if the person staring at it can find the generator.
func TestGeneratedArtifactsRefuseLineMerge(t *testing.T) {
	t.Parallel()

	attrBody := readGitattributes(t)

	roster := rosterArtifacts(t)
	paths := make([]string, 0, len(roster))
	for _, artifact := range roster {
		paths = append(paths, artifact.path)
	}
	states := mergeAttrs(t, paths)

	for _, artifact := range roster {
		t.Run(artifact.path, func(t *testing.T) {
			t.Parallel()

			if _, err := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(artifact.path))); err != nil {
				t.Fatalf("%s is listed as a generated artefact but is not present: %v. If it was "+
					"renamed or retired, update this roster and %s together rather than leaving "+
					"the gate pointing at nothing.", artifact.path, err, gitattributesPath)
			}

			if got := states[artifact.path]; got != mergeAttrUnset {
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
// under one of these names has to be classified here one way or the other. A
// family name on the DIRECTORY counts — see looksGenerated.
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
	roster := rosterArtifacts(t)
	rostered := make(map[string]struct{}, len(roster))
	var rosteredMatchingNet int
	for _, artifact := range roster {
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

	var candidates []string
	for _, tracked := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if looksGenerated(tracked) {
			candidates = append(candidates, tracked)
		}
	}
	states := mergeAttrs(t, candidates)

	matched := len(candidates)
	for _, tracked := range candidates {
		gated := states[tracked] == mergeAttrUnset
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
// artefact. The markers are matched against the WHOLE path, not just the
// basename: a sharded artefact carries the family name on its directory
// (test/rejection-parity/catalogue/internal__promql__lower.go.json), so a
// basename-only net would let every shard through — neither `-merge` gated nor
// recorded as hand-authored — which is the silent-blend shape this gate exists
// to prevent. The extension check stays on the basename, since only the file
// itself decides whether it is a data file.
func looksGenerated(path string) bool {
	lower := strings.ToLower(path)

	var dataFile bool
	for _, ext := range generatedDataExtensions {
		if strings.HasSuffix(filepath.Base(lower), ext) {
			dataFile = true

			break
		}
	}
	if !dataFile {
		return false
	}

	for _, marker := range generatedNameMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}

	return false
}

// TestLooksGeneratedReadsTheWholePath pins the directory half of the net. The
// net was basename-only when it landed, which meant a generated artefact whose
// family name sat on its DIRECTORY — the shape every sharded artefact takes —
// was classified as neither generated nor hand-authored, and merged silently
// with every check green. That is the #1422 failure the gate exists to make
// loud, so the distinction is pinned rather than left to the roster to cover
// incidentally.
func TestLooksGeneratedReadsTheWholePath(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"marker on the basename", "test/perf/scale-wall-baseline.json", true},
		{"marker on a nested shard directory", "test/perf/cardinality-baseline/promql/rate.json", true},
		{"marker on the directory only", "test/rejection-parity/catalogue/internal__promql__lower.go.json", true},
		{"marker on a directory further up", "test/rejection-parity/catalogue/nested/shard.json", true},
		{"marker anywhere, uppercased", "test/Rejection-Parity/CATALOGUE/Shard.JSON", true},
		{"no marker at all", "test/spec/promql/rate.json", false},
		{"marker present but not a data file", "test/rejection-parity/catalogue.go", false},
		{"marker on the directory, non-data extension", "test/rejection-parity/catalogue/shard.md", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := looksGenerated(tc.path); got != tc.want {
				t.Errorf("looksGenerated(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
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

// mergeAttrs returns the state git resolves the `merge` attribute to for every
// given path, in ONE `git check-attr` invocation. The sharded artefacts put
// well over a thousand paths through this gate, and a subprocess apiece turned
// a millisecond check into a minute of process churn; `--stdin` resolves the
// whole set against the same attribute stack in a single pass. Git writes one
// result line per input line, in input order, which is what pairs the states
// back to the paths — a path may itself contain a colon, so the state is read
// from the LAST separator rather than by splitting the line into fields.
func mergeAttrs(t *testing.T, paths []string) map[string]string {
	t.Helper()

	if len(paths) == 0 {
		return map[string]string{}
	}

	cmd := exec.Command("git", "check-attr", "merge", "--stdin")
	cmd.Dir = repoRoot
	cmd.Stdin = strings.NewReader(strings.Join(paths, "\n") + "\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git check-attr merge --stdin (%d paths): %v", len(paths), err)
	}

	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) != len(paths) {
		t.Fatalf("git check-attr merge --stdin printed %d line(s) for %d path(s); the result lines "+
			"are paired back to the paths by position, so a mismatch means every state read here "+
			"could belong to the wrong file.", len(lines), len(paths))
	}

	states := make(map[string]string, len(paths))
	for i, line := range lines {
		idx := strings.LastIndex(line, checkAttrSeparator)
		if idx < 0 {
			t.Fatalf("git check-attr merge printed %q for %s, which carries no %q separator",
				line, paths[i], checkAttrSeparator)
		}
		states[paths[i]] = line[idx+len(checkAttrSeparator):]
	}

	return states
}
