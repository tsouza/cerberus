package regression

import (
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
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

// handAuthoredMarker records, inside the gate itself, a committed data file
// that sits in generated territory but is maintained by hand. Writing the
// classification next to the rule is what keeps it a decision someone made
// rather than a gap nobody noticed — and a record counts only when it STATES
// the reason a bad merge of that file would fail loudly, so the section can
// never decay into a bare list of paths the gate has agreed to ignore.
const handAuthoredMarker = "# hand-authored: "

// handAuthoredContinuation is the prefix of a comment line that continues the
// previous hand-authored record's reason. The records are wrapped prose, so the
// reason routinely starts on the line below the path; a continuation is
// indented past the `# ` every other comment in the file uses.
const handAuthoredContinuation = "#  "

// reasonPunctuation is trimmed off a parsed reason before it is checked for
// emptiness — a record whose stated reason is nothing but the em dash that
// introduces it has stated nothing.
const reasonPunctuation = " —-"

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

// logqlReferenceVerdictsEnv and traceqlReferenceVerdictsEnv are the env gates
// that re-probe every LogQL / TraceQL surface symbol through the AGPL reference
// parser and rewrite the checked-in verdict ledger. They need the `agpl_oracle`
// build tag, which is why each has its own gate rather than riding
// CERBERUS_UPDATE_INVENTORY.
const (
	logqlReferenceVerdictsEnv   = "CERBERUS_UPDATE_LOGQL_REFERENCE_VERDICTS=1"
	traceqlReferenceVerdictsEnv = "CERBERUS_UPDATE_TRACEQL_REFERENCE_VERDICTS=1"
)

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
	{"test/surface-parity/logql-reference-verdicts.json", logqlReferenceVerdictsEnv},
	{"test/surface-parity/traceql-reference-verdicts.json", traceqlReferenceVerdictsEnv},

	{"test/rejection-parity/divergence-ceiling.json", inventoryUpdateEnv},

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
// artefacts. They are a SECOND, independent signal, and no longer the load
// bearing one: a substring list of naming families protects only artefacts whose
// author happened to pick one of these words, so it caught neither
// test/rejection-parity/divergence-ceiling.json nor the two AGPL-oracle
// reference-verdict ledgers (#1868). The classification that fails closed is the
// territory inversion below; this list stays because it reaches artefacts
// OUTSIDE any generated directory, which the inversion by construction does not.
// A family name on the DIRECTORY counts — see looksGenerated.
var generatedNameMarkers = []string{"baseline", "inventory", "catalogue", ".pin."}

// generatedDataExtensions restrict both nets to DATA files. The same words
// appear in the names of the Go and Node sources that PRODUCE these artefacts
// (compat-baseline-sync.mjs, inventory.go, catalogue.go); those are hand-written
// code, and code is line-mergeable.
var generatedDataExtensions = []string{".json", ".txt", ".sql"}

// generatorSourceExtensions are the languages cerberus writes its generators in.
// A generator is found by reading source, not by compiling it, so the
// build-tagged ones (`agpl_oracle`, `chdb`, `migration`) are seen by this gate
// exactly like any other file — which matters, because two of the artefacts the
// name net missed are written from behind `agpl_oracle`.
var generatorSourceExtensions = []string{".go", ".mjs", ".ts", ".js"}

// updateModeWriteCall matches the two file-write calls cerberus's generators
// make. A generated artefact exists because something wrote it; that is the fact
// the classification hangs on, rather than on what the artefact was named.
var updateModeWriteCall = regexp.MustCompile(`os\.WriteFile\(|writeFileSync\(`)

// updateModeGate matches the SHOUTING env-var or flag name a regeneration mode
// is gated on — CERBERUS_UPDATE_INVENTORY, GOLDEN_UPDATE, MIGRATION_UPDATE_GOLDENS,
// UPDATE_CARDINALITY_BASELINE, COVERAGE_UPDATE_FLOORS, REGENERATE and their
// siblings all carry one of these three words. Pairing it with a write call is
// what separates a generator from the many tests that write a scratch file into
// a t.TempDir().
var updateModeGate = regexp.MustCompile(`[A-Z0-9_]*(?:UPDATE|REGEN|GOLDEN)[A-Z0-9_]*`)

// updateModeWriters returns every tracked source file that gates a file write on
// a regeneration mode. These are the generators, read off the tree rather than
// listed by hand: adding one is how a new generated artefact comes into
// existence, and the directory it sits in becomes generated territory the moment
// it lands.
func updateModeWriters(t *testing.T, tracked []string) []string {
	t.Helper()

	var writers []string
	for _, file := range tracked {
		if !hasExtension(file, generatorSourceExtensions) {
			continue
		}
		body, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(file)))
		if err != nil {
			t.Fatalf("read %s: %v. It is tracked, so it must be readable from the repository root "+
				"this gate resolves against.", file, err)
		}
		if updateModeWriteCall.Match(body) && updateModeGate.Match(body) {
			writers = append(writers, file)
		}
	}

	if len(writers) == 0 {
		t.Fatalf("no tracked source file pairs a file write with a regeneration-mode gate. Every "+
			"generated artefact in this repository is written by one, so an empty set means the "+
			"scan stopped reading the tree — check that this gate runs with the repository root "+
			"at %s.", repoRoot)
	}

	return writers
}

// joinCall matches a path-join expression in either language a generator here
// is written in: Go's `filepath.Join(` and Node's `path.join(`. The argument
// list is captured whole and split by resolveJoin; `[^()]*` stops the match at
// the first nested call, which is deliberate — an argument this gate cannot
// read as a constant is an argument it must not guess at.
var joinCall = regexp.MustCompile(`(?:filepath|path)\.[Jj]oin\(([^()]*)\)`)

// stringConstant matches a `name = "value"` binding — a Go `const`/`var` inside
// or outside a group, and a JS `const`, all of which share this shape. Values
// carrying an escape are skipped rather than half-decoded: a destination this
// gate is not certain of is one it must not claim.
var stringConstant = regexp.MustCompile(`(\w+)\s*=\s*"([^"\\]*)"`)

// stringLiteralArg matches a join argument written inline rather than named.
var stringLiteralArg = regexp.MustCompile(`^"([^"\\]*)"$`)

// writerDestinations resolves the directories an update-mode writer writes INTO,
// for the writers whose destination is not their own directory.
//
// A generator almost always writes next to itself, and for those this adds
// nothing. One does not: test/oracle/inventory/promql_test.go holds
// `inventoryDir = "../../e2e/grafana/ql-inventory"` and writes the PromQL
// feature inventory there. Its destination is territory today only because
// rostered artefacts already sit in it — so the hole is live-shaped but not yet
// live, and a NEW generator writing its FIRST artefact into a NEW directory
// would land in no territory at all and be caught only if its filename happened
// to match one of the four surviving legacy markers. That fragility is #1868
// exactly, one level up.
//
// The resolution is deliberately narrow, and the narrowness is the feature. It
// follows a join whose every argument resolves to a string constant in the same
// file and whose LAST argument names a data file. It does not follow a
// directory that is merely mentioned, a local variable, a function result, or a
// join it cannot read end to end. A looser rule that resolved paths by
// proximity pulls test/e2e/grafana/dashboards and
// test/e2e/grafana/compose/dashboards — eight hand-written Grafana dashboards —
// into territory as pure noise, and a gate that cries wolf is a gate that gets
// ignored, which costs more than the hole it was widened to close. The existing
// false-positive rate is zero and TestTerritoryFollowsTheDestination pins it
// there in both directions.
func writerDestinations(t *testing.T, writers []string) []string {
	t.Helper()

	var destinations []string
	for _, writer := range writers {
		body, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(writer)))
		if err != nil {
			t.Fatalf("read %s: %v. It was just read as an update-mode writer, so it must still "+
				"be readable from the repository root this gate resolves against.", writer, err)
		}

		constants := make(map[string]string)
		for _, m := range stringConstant.FindAllStringSubmatch(string(body), -1) {
			constants[m[1]] = m[2]
		}

		for _, m := range joinCall.FindAllStringSubmatch(string(body), -1) {
			dir, ok := resolveJoin(m[1], constants)
			if !ok {
				continue
			}
			dest := path.Join(path.Dir(writer), dir)
			// A join resolving above the repository root cannot name a tracked
			// directory, so there is nothing there to classify.
			if dest == ".." || strings.HasPrefix(dest, "../") {
				continue
			}
			destinations = append(destinations, dest)
		}
	}

	return destinations
}

// resolveJoin turns a join's argument list into the directory part it names,
// reporting false unless every argument resolves to a string constant and the
// last one names a data file. Requiring the filename is what separates a write
// to a destination from the many joins that merely walk a directory.
func resolveJoin(args string, constants map[string]string) (string, bool) {
	fields := strings.Split(args, ",")
	if len(fields) < 2 {
		return "", false
	}

	resolved := make([]string, 0, len(fields))
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if m := stringLiteralArg.FindStringSubmatch(field); m != nil {
			resolved = append(resolved, m[1])

			continue
		}
		value, ok := constants[field]
		if !ok {
			return "", false
		}
		resolved = append(resolved, value)
	}

	if !hasExtension(resolved[len(resolved)-1], generatedDataExtensions) {
		return "", false
	}

	return path.Join(resolved[:len(resolved)-1]...), true
}

// generatedTerritory is the set of directories in which a committed data file
// has to be classified. It is DERIVED, never listed:
//
//   - the directory of every update-mode writer, because a generator almost
//     always writes next to itself;
//   - every directory a writer writes INTO when that is not its own, so the one
//     generator that aims elsewhere carries its destination with it;
//   - the directory of every rostered artefact, so a generated artefact's
//     siblings are policed by its presence;
//   - the directory of every hand-authored record, for the same reason from the
//     other side.
//
// The inversion is the point. The name net asks "is this file named like an
// artefact?" and lets everything else through; territory asks "is this file
// sitting where artefacts are made?" and lets NOTHING through unclassified. A
// new artefact dropped next to its generator is therefore loud on arrival
// whatever it is called, which is exactly what divergence-ceiling.json was not
// (#1868).
func generatedTerritory(
	roster []generatedArtifact,
	records map[string]string,
	writers []string,
	destinations []string,
) map[string]struct{} {
	territory := make(map[string]struct{})
	for _, writer := range writers {
		territory[path.Dir(writer)] = struct{}{}
	}
	for _, destination := range destinations {
		territory[destination] = struct{}{}
	}
	for _, artifact := range roster {
		territory[path.Dir(artifact.path)] = struct{}{}
	}
	for recorded := range records {
		territory[path.Dir(recorded)] = struct{}{}
	}

	return territory
}

// hasExtension reports whether a slash-separated path's basename ends in one of
// the given extensions.
func hasExtension(file string, extensions []string) bool {
	base := strings.ToLower(path.Base(file))
	for _, ext := range extensions {
		if strings.HasSuffix(base, ext) {
			return true
		}
	}

	return false
}

// handAuthoredRecords parses the hand-authored section of `.gitattributes` into
// path -> stated reason, folding each record's wrapped continuation lines into
// the reason. A record that names a path without arguing for it is rejected: an
// entry with no reason is an expected-failure list wearing a comment's clothes,
// and the classification only means something while every "not generated" is a
// position someone took.
func handAuthoredRecords(t *testing.T, attrBody string) map[string]string {
	t.Helper()

	lines := strings.Split(attrBody, "\n")
	records := make(map[string]string)
	for i, line := range lines {
		rest, found := strings.CutPrefix(line, handAuthoredMarker)
		if !found {
			continue
		}

		recorded, reason, _ := strings.Cut(strings.TrimSpace(rest), " ")
		for _, cont := range lines[i+1:] {
			if !strings.HasPrefix(cont, handAuthoredContinuation) {
				break
			}
			reason += " " + strings.TrimSpace(strings.TrimPrefix(cont, "#"))
		}

		reason = strings.Trim(reason, reasonPunctuation)
		if reason == "" {
			t.Errorf("%s records %s as hand-authored but states no reason. A record with no reason "+
				"is an exemption, not a classification: say what regenerates nothing here and why a "+
				"bad merge of it fails loudly instead of silently.", gitattributesPath, recorded)
		}
		records[recorded] = reason
	}

	return records
}

// TestNoGeneratedArtifactEscapesTheMergeGate walks every tracked file and fails
// on a committed data file that is UNCLASSIFIED: one sitting in generated
// territory, or named like a generated artefact, that is neither `-merge` gated
// and rostered nor recorded as hand-authored with a reason. Without this the
// gate decays the moment someone adds the next artefact: the roster above would
// still pass while the new file merges silently, which is the shape of the bug
// rather than a fix for it.
//
// The two nets fail in opposite directions on purpose. The name net is
// permissive — everything it does not recognise is allowed through — which is
// how divergence-ceiling.json and the two reference-verdict ledgers went
// unprotected under four naming families that happened not to describe them
// (#1868). Territory is the closed half: inside a directory where artefacts are
// made, a data file is unclassified until someone classifies it, so the default
// for a new file is loud rather than silent.
func TestNoGeneratedArtifactEscapesTheMergeGate(t *testing.T) {
	t.Parallel()

	attrBody := readGitattributes(t)
	records := handAuthoredRecords(t, attrBody)

	roster := rosterArtifacts(t)
	rostered := make(map[string]struct{}, len(roster))
	for _, artifact := range roster {
		rostered[artifact.path] = struct{}{}
	}

	tracked := trackedFiles(t)
	writers := updateModeWriters(t, tracked)
	territory := generatedTerritory(roster, records, writers, writerDestinations(t, writers))

	var candidates []string
	for _, file := range tracked {
		if !hasExtension(file, generatedDataExtensions) {
			continue
		}
		if _, inTerritory := territory[path.Dir(file)]; !inTerritory && !looksGenerated(file) {
			continue
		}
		candidates = append(candidates, file)
	}
	states := mergeAttrs(t, candidates)

	scanned := make(map[string]struct{}, len(candidates))
	for _, file := range candidates {
		scanned[file] = struct{}{}

		gated := states[file] == mergeAttrUnset
		if _, ok := rostered[file]; ok {
			// The per-artefact test above reports an ungated roster entry in
			// full; repeating it here would only duplicate the failure.
			continue
		}

		if gated {
			t.Errorf("%s is `-merge` gated in %s but is missing from generatedArtifacts. The "+
				"roster is what pins the regeneration command next to the rule, so an entry "+
				"only in %s leaves the conflict undiagnosable. Add it, with the command that "+
				"regenerates it.", file, gitattributesPath, gitattributesPath)

			continue
		}

		if _, recorded := records[file]; !recorded {
			t.Errorf("%s is an unclassified data file in generated territory (%s holds a "+
				"generator or a known generated artefact). If a command regenerates it, gate it "+
				"`-merge` in %s and add it to generatedArtifacts — an ungated generated baseline "+
				"is how PR #1422's silent blend got in. If it really is hand-maintained, say so "+
				"in %s with a %q%s line and the reason a bad merge of it would fail loudly.",
				file, path.Dir(file), gitattributesPath, gitattributesPath, handAuthoredMarker, file)
		}
	}

	for _, artifact := range roster {
		if _, ok := scanned[artifact.path]; !ok {
			t.Fatalf("the classification scan never reached %s, which this gate's own roster names "+
				"as a generated artefact. Every rostered artefact is a data file in generated "+
				"territory by construction, so missing one means the scan is not reading the tree "+
				"it polices — check that this test runs with the repository root at %s.",
				artifact.path, repoRoot)
		}
	}
}

// trackedFiles lists every file git tracks, slash-separated and repo-root-relative.
func trackedFiles(t *testing.T) []string {
	t.Helper()

	cmd := exec.Command("git", "ls-files")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}

	return strings.Split(strings.TrimSpace(string(out)), "\n")
}

// TestTerritoryCatchesWhatTheNameNetCannot pins the #1868 defect itself. The
// ceiling ratchet's artefact is named after neither a baseline, an inventory, a
// catalogue nor a pin, so the name net classified it as "not a generated file"
// and it sat ungated. What makes it generated is that the ratchet WRITES it
// under CERBERUS_UPDATE_INVENTORY, and that is what territory reads. Pinning
// both halves stops a future simplification from dropping the derivation back
// to naming, where the same class of file escapes again under a fifth word.
func TestTerritoryCatchesWhatTheNameNetCannot(t *testing.T) {
	t.Parallel()

	const (
		ceiling = "test/rejection-parity/divergence-ceiling.json"
		ratchet = "test/rejection-parity/divergence_ratchet_test.go"
	)

	if looksGenerated(ceiling) {
		t.Errorf("looksGenerated(%q) = true. This test exists because it is FALSE: the name net "+
			"cannot see this artefact, which is why the classification may not rest on naming.",
			ceiling)
	}

	writers := updateModeWriters(t, trackedFiles(t))
	if !slices.Contains(writers, ratchet) {
		t.Fatalf("%s is not among the %d update-mode writers this gate derives territory from, yet "+
			"it writes %s under CERBERUS_UPDATE_INVENTORY. Without it %s is in no directory the "+
			"gate polices and goes back to being silently line-merged (#1868).",
			ratchet, len(writers), ceiling, ceiling)
	}

	if _, ok := generatedTerritory(nil, nil, writers, nil)[path.Dir(ceiling)]; !ok {
		t.Errorf("%s is not generated territory even though %s writes into it. Territory derived "+
			"from the writers alone is what has to reach this directory — deriving it from the "+
			"roster instead would only ever confirm what someone already classified.",
			path.Dir(ceiling), ratchet)
	}
}

// TestTerritoryFollowsTheDestination pins #1875: territory has to follow where a
// generator WRITES, not merely where its source file sits.
//
// Both halves are load-bearing and the second is the harder one. Asserting only
// that the cross-directory destination is found would pass just as happily with
// a loose resolver that drags every path-shaped string in a writer into
// territory — and that resolver pulls in the two hand-written Grafana dashboard
// directories, at which point the gate reports on eight files nobody generates
// and starts being ignored. So this pins the delta EXACTLY: the destinations
// derived from the whole tree are precisely the one directory the one
// cross-directory writer aims at, and nothing else.
func TestTerritoryFollowsTheDestination(t *testing.T) {
	t.Parallel()

	const (
		// The one writer in the tree whose destination is not its own directory.
		crossWriter = "test/oracle/inventory/promql_test.go"
		crossDest   = "test/e2e/grafana/ql-inventory"
	)

	// Hand-written Grafana dashboards. A resolver loose enough to reach these
	// is the failure mode this test exists to reject.
	handWritten := []string{
		"test/e2e/grafana/dashboards",
		"test/e2e/grafana/compose/dashboards",
	}

	writers := updateModeWriters(t, trackedFiles(t))
	destinations := writerDestinations(t, writers)

	if !slices.Contains(destinations, crossDest) {
		t.Errorf("writerDestinations did not resolve %s, which %s writes into via its "+
			"inventoryDir/inventoryFile constants. Territory derived from the writer's own "+
			"directory alone never reaches it, so a new generator writing its first artefact "+
			"into a new directory would land in no territory at all (#1875).",
			crossDest, crossWriter)
	}

	// The destination set is derived from writers alone here — deliberately not
	// from the roster — because crossDest is already territory today by way of
	// the rostered artefacts sitting in it. Passing the roster would let this
	// test pass with the destination resolution deleted outright.
	territory := generatedTerritory(nil, nil, writers, destinations)
	if _, ok := territory[crossDest]; !ok {
		t.Errorf("%s is not generated territory derived from the writers alone. It is territory "+
			"today only because rostered artefacts happen to live in it, which is exactly the "+
			"accident this derivation must not depend on.", crossDest)
	}

	for _, dir := range handWritten {
		if _, ok := territory[dir]; ok {
			t.Errorf("%s is generated territory, but its dashboards are hand-written. The "+
				"destination resolution has been widened past the shape it is allowed to "+
				"follow (a directory constant joined to a filename constant), and the gate now "+
				"reports on files nobody generates. The false-positive rate was zero and has "+
				"to stay zero — a gate that cries wolf gets ignored, which costs more than the "+
				"hole widening it was meant to close.", dir)
		}
	}

	// The exact delta, both directions at once. A destination this gate claims
	// that no generator actually writes is a false positive regardless of which
	// directory it names, so the assertion is on the whole set rather than on a
	// list of directories someone remembered to exclude.
	unique := slices.Clone(destinations)
	slices.Sort(unique)
	unique = slices.Compact(unique)
	if !slices.Equal(unique, []string{crossDest}) {
		t.Errorf("writerDestinations resolved %v, want exactly [%s]. Every entry beyond that one "+
			"is a directory this gate would police on the strength of a string it guessed at.",
			unique, crossDest)
	}
}

// TestResolveJoinReadsOnlyWhatItCanProve pins the resolver's contract directly.
//
// TestTerritoryFollowsTheDestination pins the DELTA over the real tree, which is
// the assertion that matters — but it can only exercise the guards the tree
// happens to trip. The tree holds exactly one cross-directory writer and its two
// joins both end in a data filename, so the guard that rejects a join NOT ending
// in one is never reached there: deleting it leaves that test green. A guard no
// test can fail is not a guard, so the contract is pinned here where every
// rejection reason is reachable.
func TestResolveJoinReadsOnlyWhatItCanProve(t *testing.T) {
	t.Parallel()

	constants := map[string]string{
		"inventoryDir":  "../../e2e/grafana/ql-inventory",
		"inventoryFile": "promql-feature-inventory.json",
		"nested":        "sub",
		"goldenDir":     "testdata",
		"sourceFile":    "lower.go",
		"noExtension":   "README",
	}

	for _, tc := range []struct {
		name string
		args string
		want string
		ok   bool
	}{
		{
			name: "a directory constant joined to a data filename is a destination",
			args: "inventoryDir, inventoryFile",
			want: "../../e2e/grafana/ql-inventory",
			ok:   true,
		},
		{
			name: "an inline filename literal resolves like a named one",
			args: `inventoryDir, "promql-feature-inventory.json"`,
			want: "../../e2e/grafana/ql-inventory",
			ok:   true,
		},
		{
			name: "every leading argument contributes to the directory",
			args: "goldenDir, nested, inventoryFile",
			want: "testdata/sub",
			ok:   true,
		},
		{
			name: "a join not ending in a data file is a directory walk, not a destination",
			args: "goldenDir, nested",
			ok:   false,
		},
		{
			name: "a join ending in source rather than data is not a destination",
			args: "goldenDir, sourceFile",
			ok:   false,
		},
		{
			name: "a join ending in an extensionless name is not a destination",
			args: "goldenDir, noExtension",
			ok:   false,
		},
		{
			name: "an argument that is not a constant is never guessed at",
			args: "dir, inventoryFile",
			ok:   false,
		},
		{
			name: "a single argument names no directory at all",
			args: "inventoryFile",
			ok:   false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, ok := resolveJoin(tc.args, constants)
			if ok != tc.ok {
				t.Fatalf("resolveJoin(%q) ok = %v, want %v. Reading a join this gate cannot prove "+
					"the shape of is how a hand-written directory becomes policed territory; "+
					"refusing one it can prove is how a real generated artefact stays unclassified.",
					tc.args, ok, tc.ok)
			}
			if ok && got != tc.want {
				t.Errorf("resolveJoin(%q) = %q, want %q", tc.args, got, tc.want)
			}
		})
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
