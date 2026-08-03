package regression

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// golangci-lint analyses ONE build configuration per invocation. That single
// fact is why a lint gate can report green over a tree it never read: a run
// that sets no tags cannot see a `//go:build chdb` file, and `./...` reports
// clean over what is left. The green then reads as "the tree is clean" when it
// means "the default build is clean" — 120 chdb files, the agpl_oracle
// differentials, the integration and e2e suites and the migration tiers all
// sit outside it, along with the discipline linters that are supposed to apply
// project-wide.
//
// The fix is two passes: one under the tag union in `.golangci.yml`, one under
// the plain build configuration. That pair is TOTAL only because of a property
// of this tree — every `//go:build` line is a single term, positive or negated
// — so the pins below establish the property first and then hold every
// invocation site to it. A compound constraint arriving later would slip
// between the two passes silently, which is why it fails here rather than
// being tolerated.

const (
	golangciConfigPath = repoRoot + "/.golangci.yml"
	lefthookPath       = repoRoot + "/lefthook.yml"
)

// buildConstraintLine matches the `//go:build` line's constraint expression.
// Only the FIRST such line in a file counts: the directive is only a build
// constraint above the package clause, and a later one is a comment.
var buildConstraintLine = regexp.MustCompile(`(?m)^//go:build (.+)$`)

// singleTermConstraint matches the one constraint shape the two-pass scheme
// covers: a lone tag, or a lone negated tag.
var singleTermConstraint = regexp.MustCompile(`^(!?)([a-z0-9_]+)$`)

// skippedDirs are directories with no Go the lint gate is responsible for.
// `test/oracle` is a nested module with its own go.mod, exercised by its own
// CI step; the rest hold no compilable input.
var skippedDirs = map[string]bool{
	".git":          true,
	".claude":       true,
	"node_modules":  true,
	"test/oracle":   true,
	"build":         true,
	"dist":          true,
	"bin":           true,
	"vendor":        true,
	"scratchpad":    true,
	"testdata-temp": true,
}

// constraint is one file's build constraint.
type constraint struct {
	file    string
	term    string
	negated bool
}

// treeConstraints walks the root module for `//go:build` lines. Reading the
// first line only mirrors the toolchain: a directive below the package clause
// is not a constraint.
func treeConstraints(t *testing.T) []constraint {
	t.Helper()

	var out []constraint
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(repoRoot, path)
		if relErr != nil {
			return relErr
		}
		if d.IsDir() {
			if skippedDirs[d.Name()] || skippedDirs[filepath.ToSlash(rel)] {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		buf, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		m := buildConstraintLine.FindSubmatch(buf)
		if m == nil {
			return nil
		}
		expr := strings.TrimSpace(string(m[1]))
		parts := singleTermConstraint.FindStringSubmatch(expr)
		if parts == nil {
			t.Errorf("%s carries the build constraint %q, which is not a single term. The lint gate "+
				"runs exactly two passes — the tag union in %s, and the plain build configuration — "+
				"and that pair covers every file only while each constraint is one tag or one negated "+
				"tag. A compound constraint is satisfied by neither pass, so this file would be "+
				"analysed by nothing while the gate still reported green. Split it, or add the third "+
				"build configuration it needs to every invocation site and widen this pin.",
				filepath.ToSlash(rel), expr, golangciConfigPath)
			return nil
		}
		out = append(out, constraint{
			file:    filepath.ToSlash(rel),
			term:    parts[2],
			negated: parts[1] == "!",
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", repoRoot, err)
	}
	if len(out) == 0 {
		t.Fatalf("no `//go:build` line found under %s — the walk is reading the wrong tree, so every "+
			"assertion below would pass over an empty set", repoRoot)
	}
	return out
}

// configuredBuildTags reads the tag union out of `.golangci.yml`.
func configuredBuildTags(t *testing.T) []string {
	t.Helper()

	var doc struct {
		Run struct {
			BuildTags []string `yaml:"build-tags"`
		} `yaml:"run"`
	}
	if err := yaml.Unmarshal([]byte(readFileString(t, golangciConfigPath)), &doc); err != nil {
		t.Fatalf("parse %s: %v", golangciConfigPath, err)
	}
	return doc.Run.BuildTags
}

// justVariable reads a `NAME := "value"` assignment out of the Justfile.
func justVariable(t *testing.T, name string) string {
	t.Helper()

	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `\s*:=\s*"([^"]*)"`)
	m := re.FindStringSubmatch(readFileString(t, repoRoot+"/Justfile"))
	if m == nil {
		t.Fatalf("Justfile declares no %s variable", name)
	}
	return m[1]
}

// TestEveryBuildConstraintIsASingleTerm establishes the premise the two-pass
// lint scheme rests on. treeConstraints reports a compound constraint as a
// failure, so this names the invariant rather than letting it surface under
// whichever other test happened to walk the tree first.
func TestEveryBuildConstraintIsASingleTerm(t *testing.T) {
	t.Parallel()

	treeConstraints(t)
}

func TestGolangciBuildTagsCoverEveryTagInTheTree(t *testing.T) {
	t.Parallel()

	constraints := treeConstraints(t)

	var inTree []string
	for _, c := range constraints {
		if !c.negated && !slices.Contains(inTree, c.term) {
			inTree = append(inTree, c.term)
		}
	}
	slices.Sort(inTree)

	configured := configuredBuildTags(t)
	sorted := slices.Clone(configured)
	slices.Sort(sorted)
	if !slices.Equal(configured, sorted) {
		t.Errorf("%s lists build-tags out of order: %v. Keep it sorted so an addition is a one-line "+
			"diff rather than a re-read of the whole list.", golangciConfigPath, configured)
	}

	for _, tag := range inTree {
		if !slices.Contains(configured, tag) {
			var files []string
			for _, c := range constraints {
				if c.term == tag && !c.negated {
					files = append(files, c.file)
				}
			}
			t.Errorf("build tag %q constrains %d file(s) — e.g. %s — and is absent from %s's "+
				"`run.build-tags`. golangci-lint analyses one build configuration, so those files are "+
				"invisible to every linter while `./...` still reports green over the rest.",
				tag, len(files), files[0], golangciConfigPath)
		}
	}

	for _, tag := range configured {
		if !slices.Contains(inTree, tag) {
			t.Errorf("%s declares build tag %q, which constrains no file in the tree. A stale tag is "+
				"not harmless: it is the residue of code that moved or went away, and it makes the "+
				"list stop being a readable statement of what the gate analyses. Delete it.",
				golangciConfigPath, tag)
		}
	}
}

func TestUntaggedLintPassRunsUnderAnInertTag(t *testing.T) {
	t.Parallel()

	inert := justVariable(t, "LINT_UNTAGGED_BUILD")

	for _, c := range treeConstraints(t) {
		if c.term == inert {
			t.Errorf("%s constrains on %q, the tag the untagged lint pass uses to CLEAR the union in "+
				"%s. A file that reacts to it makes that pass a third build configuration rather than "+
				"the plain one, so the `!chdb` / `!chaos_sleep` stubs it exists to cover stop being "+
				"covered. Rename the variable, or drop the constraint.",
				c.file, inert, golangciConfigPath)
		}
	}

	if slices.Contains(configuredBuildTags(t), inert) {
		t.Errorf("%s lists %q in `run.build-tags`. That is the tag the second pass passes in order to "+
			"replace the union — listing it here makes the two passes the same build configuration, "+
			"and the negated-constraint stubs go unanalysed by both.", golangciConfigPath, inert)
	}

	if ci := readFileString(t, ciWorkflowPath); !strings.Contains(ci, "--build-tags "+inert) {
		t.Errorf("%s never passes `--build-tags %s`. The Justfile and the workflow have to name the "+
			"same inert tag, or CI's second pass runs a build configuration nobody pinned.",
			ciWorkflowPath, inert)
	}
}

// golangciRunLine matches an invocation of the linter in a recipe or hook.
var golangciRunLine = regexp.MustCompile(`(?m)^\s*golangci-lint run (.+)$`)

func TestEveryLintSiteRunsBothBuildConfigurations(t *testing.T) {
	t.Parallel()

	inert := justVariable(t, "LINT_UNTAGGED_BUILD")

	// The `lint` recipe is the definition of "run the lint gate", so it is the
	// one site that has to spell both passes out.
	recipe := justRecipeBody(t, readFileString(t, repoRoot+"/Justfile"), "lint")
	runs := golangciRunLine.FindAllStringSubmatch(recipe, -1)
	if len(runs) != 2 {
		t.Fatalf("the Justfile `lint` recipe invokes golangci-lint %d time(s); it needs exactly two — "+
			"one under the tag union in %s, one under the plain build configuration. Recipe:\n%s",
			len(runs), golangciConfigPath, recipe)
	}
	tagged, untagged := strings.TrimSpace(runs[0][1]), strings.TrimSpace(runs[1][1])
	if strings.Contains(tagged, "--build-tags") {
		t.Errorf("the Justfile `lint` recipe's first pass overrides the tag union with %q. The union "+
			"lives in %s so there is one place to read it; a flag here silently shadows it.",
			tagged, golangciConfigPath)
	}
	if !strings.Contains(untagged, "--build-tags {{LINT_UNTAGGED_BUILD}}") {
		t.Errorf("the Justfile `lint` recipe's second pass is %q, which does not clear the tag union "+
			"with `--build-tags {{LINT_UNTAGGED_BUILD}}`. Both passes then analyse the same build "+
			"configuration and the negated-constraint stubs are analysed by neither.", untagged)
	}

	// CI runs the linter through the official action rather than the recipe, so
	// its two steps are pinned against the same pair.
	ci := readFileString(t, ciWorkflowPath)
	if got := strings.Count(ci, "golangci/golangci-lint-action@"); got != 2 {
		t.Errorf("%s uses golangci-lint-action %d time(s); the gate is two build configurations, so "+
			"it needs two steps. One step means CI lints a subset of what `just lint` does and the "+
			"required check disagrees with the hook.", ciWorkflowPath, got)
	}
	if !strings.Contains(ci, "args: ./...") {
		t.Errorf("%s has no golangci-lint step running bare `args: ./...` — the pass that takes the "+
			"tag union from %s.", ciWorkflowPath, golangciConfigPath)
	}
	if !strings.Contains(ci, "args: --build-tags "+inert+" ./...") {
		t.Errorf("%s has no golangci-lint step running `args: --build-tags %s ./...` — the pass that "+
			"covers the negated-constraint stubs.", ciWorkflowPath, inert)
	}

	// pre-push exists to fail locally whatever CI would fail. Delegating to the
	// recipe is what keeps it from drifting back to a single pass.
	hook := readFileString(t, lefthookPath)
	if !strings.Contains(hook, "just lint") {
		t.Errorf("%s does not run `just lint`. A bare `golangci-lint run ./...` there analyses one "+
			"build configuration, so a push passes the hook and fails the required check on exactly "+
			"the tagged files the hook could not see.", lefthookPath)
	}
}
