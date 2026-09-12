package regression

import (
	buildconstraint "go/build/constraint"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// agplOracleLane is one CI lane that RUNS agpl_oracle-tagged code.
//
// There is more than one such lane because the tag composes: a file needing
// only the reference parser runs in the tag's own lane, while one that also
// needs the chDB session runs under the synthetic `chdb_agpl_oracle` tag.
//
// WHAT THIS DESCRIBES CHANGED (#3182). The inventory used to be a list of
// directory-PREFIX strings, matched with strings.HasPrefix and first-lane-wins,
// with the `tags` field never consulted at all. Three things followed:
//
//   - A file was "covered" by a lane whose tag set could not build it.
//     test/property/logql_test.go is `//go:build chdb_agpl_oracle`, and the
//     prefix `test/property/` attributed it to agpl-oracle.yml — which passes
//     `-tags agpl_oracle` only, and never names ./test/property/... at all.
//     agpl-oracle.yml's own header says that file compiles OUT there. Its real
//     lane, property.yml, was not in the inventory.
//   - Later lanes' scopes were dead. First-lane-wins meant `internal/` and
//     `test/spec/parityoracle/` shadowed `internal/traceql/` and `test/spec/`,
//     so those entries could never be reached — ordering decided attribution,
//     not intent.
//   - `//go:build !chdb_agpl_oracle` was counted as an oracle file. It builds
//     when the tag is ABSENT, so it needs no oracle lane at all.
//
// Both halves are derived from the toolchain now rather than pattern-matched.
// The build constraint is parsed and EVALUATED against each lane's tag set, and
// the packages a lane reaches come from `go list` under those same tags — which
// is also the only way to see that package `test/spec` is reached by the
// roundtrip lane as a TEST dependency of ./test/spec/<ql>/..., something no
// prefix over its directory could express.
type agplOracleLane struct {
	// name is how the lane is described in a failure message.
	name string

	// source is the file carrying the invocation. Its text must contain the
	// tag set and every package pattern below, so this inventory cannot
	// describe a lane that does not exist — which is what the entry claiming
	// chdb.yml ran `-tags chdb,agpl_oracle,chdb_agpl_oracle` over `test/spec/`
	// and `internal/traceql/` was: no such invocation is written anywhere.
	source string

	// tags is the build-tag set the lane passes to `go test`.
	tags []string

	// packages are the `./…/...` patterns the lane's `go test` names.
	packages []string

	// runFilter is the lane's `-run` regex, empty when it runs everything. A
	// lane that filters runs only SOME tests of the packages it names, so a
	// _test.go file it compiles but never selects is not covered by it.
	runFilter string

	// tagsSource is the file that must carry the tag set, when it is not
	// `source`. The round-trip matrix is the case: its tags are a workflow
	// matrix value and its `go test` is assembled in a script.
	tagsSource string

	// evidence is what must appear literally in `source` to prove the lane's
	// packages are really named there. Defaults to `packages`; the round-trip
	// lanes need it because the script interpolates the query language into
	// the pattern (`./test/spec/${ql}/...`), so the resolved pattern this
	// inventory reasons about is not the string on disk.
	evidence []string
}

// agplOracleLanes must stay in step with the invocations it names; each lane's
// `source` is asserted to contain its tags and packages verbatim.
var agplOracleLanes = []agplOracleLane{
	{
		name:      "agpl-oracle.yml → just test-agpl-oracle (internal/logql)",
		source:    "just/test.just",
		tags:      []string{"agpl_oracle"},
		packages:  []string{"./internal/logql/..."},
		runFilter: `^(TestAGPLOracle_|TestJSONPathParse_MatchesLokiJSONExpr$|TestParseExprPermissive_MatchAllAccepted$|TestPattern_)`,
	},
	{
		name:   "agpl-oracle.yml → just test-agpl-oracle (oracles)",
		source: "just/test.just",
		tags:   []string{"agpl_oracle"},
		packages: []string{
			"./test/agpl_oracle/...",
			"./test/spec/parityoracle/logql/...",
			"./test/spec/parityoracle/traceql/...",
			"./test/property/oracle/logql/...",
		},
	},
	{
		name:      "agpl-oracle.yml → just test-agpl-oracle (surface parity)",
		source:    "just/test.just",
		tags:      []string{"agpl_oracle"},
		packages:  []string{"./test/surface-parity/..."},
		runFilter: `^Test(LogQL|TraceQL)ReferenceVerdictsAreCurrent$`,
	},
	{
		name:      "agpl-oracle.yml → just test-agpl-oracle (loki pipeline error)",
		source:    "just/test.just",
		tags:      []string{"agpl_oracle"},
		packages:  []string{"./internal/api/loki/..."},
		runFilter: `^TestPipelineErrorMessageMatchesUpstream$`,
	},
	{
		name:     "property.yml → just property",
		source:   "just/test.just",
		tags:     []string{"chdb", "agpl_oracle", "chdb_agpl_oracle"},
		packages: []string{"./test/property/..."},
	},
	{
		name:     "chdb.yml → fixed LogQL integration suite",
		source:   ".github/workflows/chdb.yml",
		tags:     []string{"chdb", "agpl_oracle", "chdb_agpl_oracle"},
		packages: []string{"./test/integration/logql/..."},
	},
	{
		name:      "chdb.yml → parity exemption liveness helpers",
		source:    ".github/workflows/chdb.yml",
		tags:      []string{"chdb", "agpl_oracle", "chdb_agpl_oracle"},
		packages:  []string{"./test/spec"},
		runFilter: `^Test(ExemptionVerdict|CompareAgainstReferenceClassifiesOnlyAnswerMismatch|ConflictingDuplicateTimestampRefusal|OrderSensitiveSelectionPermutation|LimitKReferenceAnswerChangesWithSeriesSchedule|SynthesizedParity|SynthesizedParityRequiresExactlyOneQuerySection|TempoSpanIdentityErrorClassification)$`,
	},
	{
		// The TXTAR round-trip matrix. Its `go test` is built in
		// chdb-roundtrip.mjs (it needs an explicit per-process -timeout a
		// `run:` line cannot carry), so the package patterns are asserted
		// against that script and the tag set against the workflow's matrix.
		name:       "chdb.yml → TXTAR round-trip matrix (logql)",
		source:     ".github/scripts/chdb-roundtrip.mjs",
		tagsSource: ".github/workflows/chdb.yml",
		tags:       []string{"chdb", "agpl_oracle", "chdb_agpl_oracle"},
		packages:   []string{"./test/spec/logql/...", "./internal/logql/..."},
		evidence:   []string{"./test/spec/${ql}/...", "./internal/${ql}/..."},
	},
	{
		name:       "chdb.yml → TXTAR round-trip matrix (traceql)",
		source:     ".github/scripts/chdb-roundtrip.mjs",
		tagsSource: ".github/workflows/chdb.yml",
		tags:       []string{"chdb", "agpl_oracle", "chdb_agpl_oracle"},
		packages:   []string{"./test/spec/traceql/...", "./internal/traceql/..."},
		evidence:   []string{"./test/spec/${ql}/...", "./internal/${ql}/..."},
	},
}

// oracleTagRe matches the agpl_oracle tag FAMILY: the tag itself and the
// synthetic composite tags that imply it (`chdb_agpl_oracle`).
//
// Deliberately NOT word-anchored. `\bagpl_oracle\b` looks tighter and is
// wrong: `_` is a word character, so there is no word boundary between `chdb_`
// and `agpl_oracle`, and every chdb_agpl_oracle file would be skipped as "not
// an oracle file" — the whole population this gate exists to cover, silently
// absent. Caught by neutralising a lane and watching nothing fail.
var oracleTagRe = regexp.MustCompile(`agpl_oracle`)

// buildConstraintOf returns the parsed //go:build expression of a Go file, and
// whether it had one.
func buildConstraintOf(t *testing.T, path string) (buildconstraint.Expr, bool) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	for _, line := range strings.SplitN(string(data), "\n", 50) {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "package ") {
			return nil, false
		}
		if !buildconstraint.IsGoBuild(trimmed) {
			continue
		}
		expr, err := buildconstraint.Parse(trimmed)
		if err != nil {
			t.Fatalf("%s: unparseable build constraint %q: %v", path, trimmed, err)
		}
		return expr, true
	}
	return nil, false
}

// satisfiedBy evaluates a build constraint against a tag set.
func satisfiedBy(expr buildconstraint.Expr, tags []string) bool {
	set := make(map[string]bool, len(tags))
	for _, tg := range tags {
		set[tg] = true
	}
	return expr.Eval(func(tag string) bool { return set[tag] })
}

// mentionsOracleTag reports whether a constraint names any agpl_oracle-family
// tag at all.
func mentionsOracleTag(expr buildconstraint.Expr) bool {
	return oracleTagRe.MatchString(expr.String())
}

// goListPackages returns the import paths `go list` yields for the patterns
// under a tag set. `withTestDeps` adds the test-dependency closure, which is
// how a package compiled only because another lane's TEST binary links it —
// package `test/spec` under ./test/spec/<ql>/... — becomes visible.
func goListPackages(t *testing.T, tags, patterns []string, withTestDeps bool) map[string]bool {
	t.Helper()
	args := []string{"list", "-tags", strings.Join(tags, ",")}
	if withTestDeps {
		args = append(args, "-test", "-deps")
	}
	args = append(args, patterns...)

	cmd := exec.Command("go", args...)
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go %s: %v", strings.Join(args, " "), err)
	}
	set := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		// `go list -test` emits synthetic ".test" and "[…]" package names too;
		// the base import path is what a file's package maps to.
		if line == "" || strings.HasSuffix(line, ".test") {
			continue
		}
		if i := strings.Index(line, " ["); i >= 0 {
			line = line[:i]
		}
		set[line] = true
	}
	if len(set) == 0 {
		t.Fatalf("go %s listed no packages — the deriver is broken, not the tree", strings.Join(args, " "))
	}
	return set
}

const modulePath = "github.com/tsouza/cerberus"

// testFuncRe finds top-level Test function names in a _test.go file.
var testFuncRe = regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]*)\s*\(`)

// runFilterSelects reports whether a lane's -run regex would select at least
// one test in the file. `go test -run` matches each slash-separated element
// against the pattern unanchored, so an unanchored pattern is a substring
// match on the test name.
func runFilterSelects(t *testing.T, filter, path string) bool {
	t.Helper()
	re, err := regexp.Compile(filter)
	if err != nil {
		t.Fatalf("lane -run filter %q does not compile: %v", filter, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	for _, m := range testFuncRe.FindAllStringSubmatch(string(data), -1) {
		if re.MatchString(m[1]) {
			return true
		}
	}
	return false
}

// TestAGPLOracleLaneInventoryMatchesItsInvocations asserts the inventory above
// describes invocations that actually exist. Without this the table is
// free-floating prose: the entry it replaced claimed chdb.yml ran
// `-tags chdb,agpl_oracle,chdb_agpl_oracle` over `test/spec/` and
// `internal/traceql/`, and no such `go test` is written anywhere in the repo.
func TestAGPLOracleLaneInventoryMatchesItsInvocations(t *testing.T) {
	for _, lane := range agplOracleLanes {
		body := readFileString(t, filepath.Join(repoRoot, lane.source))

		tagsSource := lane.tagsSource
		if tagsSource == "" {
			tagsSource = lane.source
		}
		tagsBody := body
		if tagsSource != lane.source {
			tagsBody = readFileString(t, filepath.Join(repoRoot, tagsSource))
		}
		tagSet := strings.Join(lane.tags, ",")
		if !strings.Contains(tagsBody, tagSet) {
			t.Errorf("lane %q: %s contains no `%s` tag set — the inventory describes an invocation that does not exist",
				lane.name, tagsSource, tagSet)
		}

		evidence := lane.evidence
		if evidence == nil {
			evidence = lane.packages
		}
		for _, pkg := range evidence {
			if !strings.Contains(body, pkg) {
				t.Errorf("lane %q: %s never names %s", lane.name, lane.source, pkg)
			}
		}
		if lane.runFilter != "" && !strings.Contains(body, lane.runFilter) {
			t.Errorf("lane %q: %s does not carry the -run filter %q", lane.name, lane.source, lane.runFilter)
		}
	}
}

// TestAGPLOracleTagSetCoverage asserts that every agpl_oracle-gated file in the
// root module is RUN by some CI lane whose TAG SET can actually build it (the
// "hold the tag set to the lanes" regression of #1610, made load-bearing in
// #3182).
func TestAGPLOracleTagSetCoverage(t *testing.T) {
	absRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		t.Fatalf("filepath.Abs: %v", err)
	}

	files := findGoFilesWithBuildConstraints(t, absRoot)
	if len(files) == 0 {
		t.Fatal("no build-constrained Go files found in the root module — the scan is broken, not the tree")
	}

	// Per lane: the packages it TESTS, and the wider closure it COMPILES.
	type laneSets struct {
		tested   map[string]bool
		compiled map[string]bool
	}
	sets := make([]laneSets, len(agplOracleLanes))
	for i, lane := range agplOracleLanes {
		sets[i] = laneSets{
			tested:   goListPackages(t, lane.tags, lane.packages, false),
			compiled: goListPackages(t, lane.tags, lane.packages, true),
		}
	}

	gated := 0
	for _, rel := range files {
		expr, ok := buildConstraintOf(t, filepath.Join(absRoot, rel))
		if !ok || !mentionsOracleTag(expr) {
			continue
		}
		// A constraint satisfied with NO tags builds in the default lane and
		// needs no oracle lane. `//go:build !chdb_agpl_oracle` is exactly this,
		// and the old substring test counted it as needing coverage.
		if satisfiedBy(expr, nil) {
			continue
		}
		gated++

		pkg := modulePath + "/" + filepath.ToSlash(filepath.Dir(rel))
		isTest := strings.HasSuffix(rel, "_test.go")

		var covered bool
		var why []string
		for i, lane := range agplOracleLanes {
			if !satisfiedBy(expr, lane.tags) {
				why = append(why, "  "+lane.name+": tag set cannot build it")
				continue
			}
			if isTest {
				// A _test.go file is RUN only if its own package is in the
				// lane's tested set — being a compiled dependency is not
				// execution.
				if !sets[i].tested[pkg] {
					why = append(why, "  "+lane.name+": does not test "+pkg)
					continue
				}
				if lane.runFilter != "" && !runFilterSelects(t, lane.runFilter, filepath.Join(absRoot, rel)) {
					why = append(why, "  "+lane.name+": -run filter selects no test in this file")
					continue
				}
			} else if !sets[i].compiled[pkg] {
				why = append(why, "  "+lane.name+": does not compile "+pkg)
				continue
			}
			covered = true
			break
		}

		if !covered {
			t.Errorf("%s is agpl_oracle-gated but RUN by no CI lane:\n%s", rel, strings.Join(why, "\n"))
		}
	}

	if gated == 0 {
		t.Fatal("no agpl_oracle-gated file was found — either the tag was removed everywhere " +
			"(update this test) or the constraint parser is broken")
	}
	t.Logf("agpl_oracle tag set: %d gated file(s) across %d lane(s) — all covered", gated, len(agplOracleLanes))
}

// findGoFilesWithBuildConstraints walks the module root for Go files carrying
// any //go:build line. Nested modules and vendored trees are skipped: they are
// built by their own toolchain invocation, not this module's lanes.
func findGoFilesWithBuildConstraints(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (name == "vendor" || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			if _, statErr := os.Stat(filepath.Join(path, "go.mod")); statErr == nil && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") {
			return nil
		}
		if _, ok := buildConstraintOf(t, path); ok {
			rel, _ := filepath.Rel(root, path)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
	return out
}
