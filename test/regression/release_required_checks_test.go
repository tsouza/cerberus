package regression

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// On the MAINTENANCE publish path (`release/*.x`) there is no pull request, so
// there is no branch protection either: release.yml's preflight is the ONLY
// gate between a push and a published artifact. That makes two silent failure
// modes possible, and both look identical to a healthy release — green, quiet,
// and shipped:
//
//   - A branch-protection lane missing from RELEASE_REQUIRED_CHECKS is
//     certified by ABSENCE. The preflight is observation-derived: it waits for
//     the check-runs it was told to expect and ignores the rest, so a lane that
//     never ran contributes zero problems.
//   - A lane NAMED in the required set whose workflow has no `release/*.x`
//     push trigger can never post a check-run there. The preflight waits out
//     its window and aborts every maintenance release — the opposite failure,
//     equally invisible until a hotfix is actually needed.
//
// These pins close both: totality against a checked-in copy of the branch
// protection contexts, and trigger-coverage against the workflow files.

// branchProtectionContexts is the required status-check set on `main`. The
// live source of truth is
//
//	gh api repos/tsouza/cerberus/branches/main/protection \
//	  --jq '.required_status_checks.contexts[]'
//
// and this copy exists so the totality assertion below has something to be
// total OVER. Adding it here without wiring it into release.yml fails
// immediately.
//
// This copy and the live setting cannot be compared from here — that endpoint
// needs repo-admin rights, which no test token carries — so the comparison
// runs where a token that does exist: `pinnedProtectionDrift` in
// .github/scripts/release-gate-drift.mjs asserts set EQUALITY between this
// slice and the live contexts, on the scheduled drift lane. Both halves matter
// and both have bitten. A context enforced live but missing here is certified
// by absence on the maintenance path. A context named here but not enforced
// live is worse, because nothing in the tree can see it: the lane keeps posting
// green on every PR while contributing nothing to the merge decision, and every
// gate that reads this slice — including the release preflight — agrees with
// itself about a configuration that is not in force. `pr-body` spent time in
// exactly that state.
//
// The relation is a SUBSET, not equality: RELEASE_REQUIRED_CHECKS also names
// release-ONLY gates that no PR ever waits on — `migration-e2e` (no
// `pull_request:` trigger at all) and the substrate lanes `compose-smoke` /
// `dashboard` / `profile`, which boot compose stacks, k3d clusters and a chDB
// corpus walk and so short-circuit to a green no-op on an ordinary PR. Those
// were the longest PR lanes by a wide margin and bought nothing a PR could act
// on faster than the merge commit could; they moved to the release gate rather
// than disappearing.
var branchProtectionContexts = []string{
	"CodeQL",
	"chart-validate",
	"check",
	"compatibility/loki",
	"compatibility/prometheus",
	"compatibility/prometheus-forced-route",
	"compatibility/tempo",
	"coverage",
	"forbid-deferral",
	"forbid-skip",
	"lint",
	"mutation",
	"perf-guards",
	"pr-body",
	"probe",
	"property (PromQL + LogQL + TraceQL, rapid N=500)",
	"roundtrip (logql)",
	"roundtrip (promql)",
	"roundtrip (traceql)",
	"strict-scan",
}

const workflowsDir = "../../.github/workflows"

// maintenanceBranchPattern is the push-trigger glob every workflow owning a
// release-required check must carry, so the check-run exists on the
// maintenance publish path where no PR is opened.
const maintenanceBranchPattern = "release/*.x"

func TestReleasePreflightCoversEveryBranchProtectionContext(t *testing.T) {
	t.Parallel()

	job := workflowJobBody(t, readFileString(t, releaseWorkflowPath), preflightJob)
	required := requiredChecksFromPreflight(t, job)
	informational := informationalChecksFromPreflight(t, job)

	inRequired := map[string]bool{}
	for _, name := range required {
		inRequired[name] = true
	}

	for _, ctx := range branchProtectionContexts {
		if inRequired[ctx] {
			continue
		}
		degated := false
		for _, prefix := range informational {
			if strings.HasPrefix(ctx, prefix) {
				degated = true
				break
			}
		}
		if !degated {
			t.Errorf("branch-protection context %q appears in neither RELEASE_REQUIRED_CHECKS nor "+
				"RELEASE_INFORMATIONAL_CHECKS of %s job %q. On the maintenance path there is no PR "+
				"and therefore no branch protection, so an unlisted lane is certified by absence: "+
				"the preflight ignores what it was not told to expect and the release publishes on "+
				"silence. Either require it, or de-gate it: add it to RELEASE_INFORMATIONAL_CHECKS "+
				"AND give it a row in the %q table of %s — TestDeGatedLanesAreDocumentedWithAReason "+
				"holds those two in sync, so the reason is enforced rather than requested.",
				ctx, releaseWorkflowPath, preflightJob, deGatedLanesHeading, operationsDocPath)
		}
	}
}

func TestReleaseRequiredChecksAllHaveAMaintenanceTrigger(t *testing.T) {
	t.Parallel()

	job := workflowJobBody(t, readFileString(t, releaseWorkflowPath), preflightJob)
	required := requiredChecksFromPreflight(t, job)
	owners := workflowCheckOwners(t)

	for _, name := range required {
		owner, ok := owners.lookup(name)
		if !ok {
			t.Errorf("RELEASE_REQUIRED_CHECKS names %q, but no job in %s declares that check name. "+
				"The preflight would wait out its whole window for a check-run that nothing can post, "+
				"then abort the release.", name, workflowsDir)
			continue
		}
		if !owner.pushesOn("main") {
			t.Errorf("check %q is owned by %s, which has no push trigger on `main`; the preflight "+
				"requires it on the main publish path", name, owner.file)
		}
		if !owner.pushesOn(maintenanceBranchPattern) {
			t.Errorf("check %q is owned by %s, which has no `%s` push trigger. A hotfix pushed to a "+
				"maintenance line would never produce this check-run, so every maintenance release "+
				"would stall in the preflight and abort. Add the trigger, or move the check to "+
				"RELEASE_INFORMATIONAL_CHECKS with a reason.",
				name, owner.file, maintenanceBranchPattern)
		}
	}
}

const (
	// operationsDocPath is where an operator reads the publish path, and so
	// where the de-gating decisions have to be legible.
	operationsDocPath = "../../docs/operations.md"
	// deGatedLanesHeading is the section holding the lane -> reason table.
	deGatedLanesHeading = "#### De-gated lanes on the publish path"
)

// deGatedLanesFromDocs reads the lane -> reason table under
// deGatedLanesHeading. Rows are `| `lane` | reason |`; the header and its
// separator are skipped, and the scan stops at the next heading.
func deGatedLanesFromDocs(t *testing.T) map[string]string {
	t.Helper()

	doc := readFileString(t, operationsDocPath)
	_, after, found := strings.Cut(doc, deGatedLanesHeading+"\n")
	if !found {
		t.Fatalf("%s has no %q section; that is where a de-gated lane's reason is written",
			operationsDocPath, deGatedLanesHeading)
	}
	section, _, _ := strings.Cut(after, "\n#")

	lanes := map[string]string{}
	for _, line := range strings.Split(section, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) != 2 {
			t.Fatalf("%s: %q table row has %d cells, want 2 (lane, reason):\n\t%s",
				operationsDocPath, deGatedLanesHeading, len(cells), line)
		}
		lane := strings.Trim(strings.TrimSpace(cells[0]), "`")
		reason := strings.TrimSpace(cells[1])
		if lane == "Lane" || strings.HasPrefix(lane, "---") {
			continue // header row and its separator
		}
		lanes[lane] = reason
	}
	return lanes
}

// TestDeGatedLanesAreDocumentedWithAReason makes the "de-gate it with a
// written reason" instruction enforceable. RELEASE_INFORMATIONAL_CHECKS is a
// bare list of names: on its own it can de-gate a branch-protection lane
// silently, and the only rationale would be a YAML comment no operator reads.
// This pins the two together — the documented set must be exactly the de-gated
// set, and every row must carry a reason.
func TestDeGatedLanesAreDocumentedWithAReason(t *testing.T) {
	t.Parallel()

	job := workflowJobBody(t, readFileString(t, releaseWorkflowPath), preflightJob)
	informational := informationalChecksFromPreflight(t, job)
	if len(informational) == 0 {
		t.Fatalf("parsed no RELEASE_INFORMATIONAL_CHECKS out of %s job %q",
			releaseWorkflowPath, preflightJob)
	}
	documented := deGatedLanesFromDocs(t)

	for _, lane := range informational {
		reason, ok := documented[lane]
		if !ok {
			t.Errorf("%s de-gates %q in RELEASE_INFORMATIONAL_CHECKS, but %s's %q table does not "+
				"say why. A lane that stops gating a publish needs its reason where an operator "+
				"reads the release pipeline, not only in a workflow comment.",
				releaseWorkflowPath, lane, operationsDocPath, deGatedLanesHeading)
			continue
		}
		if reason == "" {
			t.Errorf("%s: %q row for de-gated lane %q has an empty reason cell",
				operationsDocPath, deGatedLanesHeading, lane)
		}
	}

	degated := map[string]bool{}
	for _, lane := range informational {
		degated[lane] = true
	}
	for lane := range documented {
		if !degated[lane] {
			t.Errorf("%s's %q table documents %q, but RELEASE_INFORMATIONAL_CHECKS in %s does not "+
				"list it. Either the lane is gating again — delete the row — or the de-gating was "+
				"dropped from the preflight and the doc now describes something untrue.",
				operationsDocPath, deGatedLanesHeading, lane, releaseWorkflowPath)
		}
	}
}

// TestNoJustfileRecipePushesAReleaseTag pins the deletion of `just
// release-tag`. release.yml's raw-tag trigger is retired, and
// release-version-gate.mjs decides whether to publish by asking whether
// `v<appVersion>` already exists — so pre-creating the tag by hand does not
// START a release, it permanently CANCELS one. The gate sees the tag, sets
// publish=false, and goreleaser / publish / chart-release all skip. No job
// fails and nothing ships, which is why this needs a pin rather than a
// convention: the failure mode is a silent success.
func TestNoJustfileRecipePushesAReleaseTag(t *testing.T) {
	t.Parallel()

	recipes := justRecipes(t, readFileString(t, "../../Justfile"))
	if len(recipes) == 0 {
		t.Fatal("parsed no recipes out of the Justfile")
	}
	const consequence = "Tags are created BY release.yml after it publishes. A hand-made tag makes " +
		"the version gate see the version as already released, so the release is " +
		"skipped in silence — every job green, nothing shipped."
	for name, body := range recipes {
		if strings.HasPrefix(name, releaseTagRecipePrefix) {
			t.Errorf("Justfile declares recipe %q. There is deliberately no tag-cutting recipe: "+
				consequence, name)
		}
		for _, line := range strings.Split(body, "\n") {
			if why := tagCuttingCommand(line); why != "" {
				t.Errorf("Justfile recipe %q %s:\n\t%s\n"+consequence,
					name, why, strings.TrimSpace(line))
			}
		}
	}
}

// releaseTagRecipePrefix is the recipe name (and any variant of it) that must
// never come back — see the comment above the test.
const releaseTagRecipePrefix = "release-tag"

var (
	// gitTagReadOnly are the `git tag` sub-flags that only READ tags. Any
	// other `git tag` invocation creates or deletes one.
	gitTagReadOnly = map[string]bool{
		"--list": true, "-l": true, "--points-at": true, "--contains": true,
		"--no-contains": true, "--merged": true, "--no-merged": true,
		"--verify": true, "--sort": true, "-n": true,
	}
	// gitPushTagFlags publish tags without ever naming one.
	gitPushTagFlags = map[string]bool{"--tags": true, "--follow-tags": true}
	// tagRefspec matches a pushed ref that is a release tag rather than a
	// branch: an explicit refs/tags/ path, a literal `v1.2.3`, or the
	// `v{{version}}` / `chart-v{{...}}` interpolations the release recipes
	// build their names from. A branch ref like `release/v{{version}}-staging`
	// is not anchored at `v`, so it does not match.
	tagRefspec = regexp.MustCompile(`^["']?(refs/tags/|v\{\{|chart-v|v[0-9])`)
)

// tagCuttingCommand reports, in words fit for a failure message, why one shell
// line from a recipe body creates or publishes a git tag — or "" if it does
// not. It reads the command tokens rather than looking for a version literal,
// so splitting the tag name into a shell variable does not evade it.
func tagCuttingCommand(line string) string {
	if strings.HasPrefix(strings.TrimSpace(line), "#") {
		return "" // a shell comment runs nothing (recipe shebangs land here too).
	}
	fields := strings.Fields(line)
	flag := func(f string) string { return strings.SplitN(f, "=", 2)[0] }
	for i, tok := range fields {
		rest := fields[i+1:]
		switch {
		case tok == "git" && len(rest) > 0 && rest[0] == "tag":
			readOnly := false
			for _, f := range rest[1:] {
				if gitTagReadOnly[flag(f)] {
					readOnly = true
					break
				}
			}
			if !readOnly {
				return "creates a git tag"
			}
		case tok == "git" && len(rest) > 0 && rest[0] == "push":
			for _, f := range rest[1:] {
				if gitPushTagFlags[flag(f)] {
					return "pushes tags"
				}
				if tagRefspec.MatchString(f) {
					return "pushes a release tag"
				}
			}
		case tok == "gh" && len(rest) > 1 && rest[0] == "release" && rest[1] == "create":
			return "creates a GitHub release, which creates its tag"
		}
	}
	return ""
}

// checkOwner is the workflow that posts a given check-run name.
type checkOwner struct {
	file         string
	pushBranches []string
	// mergeGroup records whether the workflow declares the `merge_group:`
	// trigger, i.e. whether it posts its check-runs on the projected trunk a
	// merge queue builds. See merge_queue_test.go.
	mergeGroup bool
}

func (o checkOwner) pushesOn(branch string) bool {
	for _, b := range o.pushBranches {
		if b == branch {
			return true
		}
	}
	return false
}

// checkOwnerIndex resolves a check-run name to the workflow that posts it.
// Job names that interpolate a matrix value (`roundtrip (${{ matrix.ql }})`)
// are expanded into the concrete per-leg names GitHub instantiates, so the
// lookup is exact: a required check named `roundtrip (metricsql)` resolves to
// nothing, because no leg posts that check-run. A prefix match would accept it
// and certify a lane that does not exist.
type checkOwnerIndex struct {
	exact map[string]checkOwner
}

func (i checkOwnerIndex) lookup(name string) (checkOwner, bool) {
	o, ok := i.exact[name]
	return o, ok
}

// matrix keys that carry combinations rather than an axis of values.
const (
	matrixIncludeKey = "include"
	matrixExcludeKey = "exclude"
)

// matrixRef matches one `${{ matrix.<key> }}` reference in a job name.
var matrixRef = regexp.MustCompile(`\$\{\{\s*matrix\.([A-Za-z0-9_-]+)\s*\}\}`)

// matrixScalar renders one matrix value as the string GitHub interpolates.
// Only scalars can appear in a job name; a nested mapping or sequence is a
// shape this expander does not model, and it says so rather than guessing.
func matrixScalar(t *testing.T, path, jobID, key string, n yaml.Node) string {
	t.Helper()
	if n.Kind != yaml.ScalarNode {
		t.Fatalf("%s: job %q matrix key %q holds a non-scalar value; teach matrixCombos "+
			"to expand it instead of leaving the job name unresolved", path, jobID, key)
	}
	return n.Value
}

// matrixComboList decodes `include:` / `exclude:` into key/value combinations.
func matrixComboList(t *testing.T, path, jobID, key string, n yaml.Node) []map[string]string {
	t.Helper()
	if n.Kind != yaml.SequenceNode {
		t.Fatalf("%s: job %q matrix %q is not a sequence", path, jobID, key)
	}
	out := make([]map[string]string, 0, len(n.Content))
	for _, item := range n.Content {
		if item.Kind != yaml.MappingNode {
			t.Fatalf("%s: job %q matrix %q entry is not a mapping", path, jobID, key)
		}
		combo := map[string]string{}
		for i := 0; i+1 < len(item.Content); i += 2 {
			k := item.Content[i].Value
			combo[k] = matrixScalar(t, path, jobID, k, *item.Content[i+1])
		}
		out = append(out, combo)
	}
	return out
}

// generatedMatrices maps a job whose `strategy.matrix` is an expression over a
// preceding job's output to the command that prints the table that expression
// resolves to.
//
// Such a matrix is not readable from the workflow YAML, but it is not unknowable
// either: the generating job runs a module under `.github/scripts`, and that
// module can print the same table on demand. Registering it here keeps the leg
// names in the index. The alternative — treating the job as unexpandable — drops
// every one of its check names silently, and this file exists precisely because
// a release lane certified by absence looks exactly like a healthy one.
//
// The printed table is the FULL set of legs the job can ever instantiate, which
// is what a check-name index wants; a run selecting a subset of them posts a
// subset of these names, never a name outside them.
var generatedMatrices = map[string][]string{
	"mutation.yml#mutate": {"node", ".github/scripts/mutation-matrix.mjs", "dump"},
}

// generatedMatrixCombos runs a registered generator and renders its rows as
// matrix combinations. Every scalar is stringified the way GitHub interpolates
// it, so an integer efficacy of 95 renders as "95" and lands in the job name
// exactly as the workflow writes it.
func generatedMatrixCombos(t *testing.T, path, jobID string, argv []string) []map[string]string {
	t.Helper()

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			stderr = string(exitErr.Stderr)
		}
		t.Fatalf("%s: job %q builds its matrix from `%s`, which failed: %v\n%s",
			path, jobID, strings.Join(argv, " "), err, stderr)
	}

	var rows []map[string]any
	if err := json.Unmarshal(out, &rows); err != nil {
		t.Fatalf("%s: job %q matrix generator `%s` did not print a JSON array of rows: %v",
			path, jobID, strings.Join(argv, " "), err)
	}

	combos := make([]map[string]string, 0, len(rows))
	for _, row := range rows {
		combo := map[string]string{}
		for k, v := range row {
			switch tv := v.(type) {
			case string:
				combo[k] = tv
			case float64:
				combo[k] = strconv.FormatFloat(tv, 'f', -1, 64)
			case bool:
				combo[k] = strconv.FormatBool(tv)
			default:
				t.Fatalf("%s: job %q matrix key %q holds a non-scalar value %v; a job name "+
					"cannot interpolate it, so teach the generator to flatten it", path, jobID, k, v)
			}
		}
		combos = append(combos, combo)
	}
	if len(combos) == 0 {
		t.Fatalf("%s: job %q matrix generator `%s` printed no rows; every check name this job "+
			"posts would drop out of the index unnoticed", path, jobID, strings.Join(argv, " "))
	}
	return combos
}

// matrixCombos expands `strategy.matrix` into the concrete key/value sets
// GitHub instantiates: the cartesian product of the axes, minus `exclude:`,
// extended by `include:` (an include entry that fits an existing combination
// extends it; one that fits none becomes a combination of its own — and with
// no axes at all, every include entry is its own combination). A matrix that is
// generated rather than written comes from its registered generator instead.
func matrixCombos(t *testing.T, path, jobID string, node yaml.Node) []map[string]string {
	t.Helper()
	if argv, ok := generatedMatrices[fmt.Sprintf("%s#%s", filepath.Base(path), jobID)]; ok {
		return generatedMatrixCombos(t, path, jobID, argv)
	}
	if node.IsZero() {
		return []map[string]string{{}}
	}
	if node.Kind != yaml.MappingNode {
		t.Fatalf("%s: job %q has a strategy.matrix this expander cannot enumerate "+
			"(expected a mapping of axes); if it is generated by a preceding job, register its "+
			"generator in generatedMatrices rather than letting the job name go unresolved", path, jobID)
	}

	var raw map[string]yaml.Node
	if err := node.Decode(&raw); err != nil {
		t.Fatalf("%s: job %q strategy.matrix: %v", path, jobID, err)
	}

	axes := make([]string, 0, len(raw))
	for k := range raw {
		if k != matrixIncludeKey && k != matrixExcludeKey {
			axes = append(axes, k)
		}
	}
	sort.Strings(axes)

	combos := []map[string]string{{}}
	for _, axis := range axes {
		values := raw[axis]
		if values.Kind != yaml.SequenceNode {
			t.Fatalf("%s: job %q matrix axis %q is not a sequence", path, jobID, axis)
		}
		next := make([]map[string]string, 0, len(combos)*len(values.Content))
		for _, combo := range combos {
			for _, v := range values.Content {
				grown := maps.Clone(combo)
				grown[axis] = matrixScalar(t, path, jobID, axis, *v)
				next = append(next, grown)
			}
		}
		combos = next
	}

	if ex, ok := raw[matrixExcludeKey]; ok {
		for _, drop := range matrixComboList(t, path, jobID, matrixExcludeKey, ex) {
			kept := make([]map[string]string, 0, len(combos))
			for _, combo := range combos {
				if !comboMatches(combo, drop) {
					kept = append(kept, combo)
				}
			}
			combos = kept
		}
	}

	inc, ok := raw[matrixIncludeKey]
	if !ok {
		return combos
	}
	includes := matrixComboList(t, path, jobID, matrixIncludeKey, inc)
	if len(axes) == 0 {
		combos = make([]map[string]string, 0, len(includes))
		for _, add := range includes {
			combos = append(combos, maps.Clone(add))
		}
		return combos
	}
	for _, add := range includes {
		extended := false
		for _, combo := range combos {
			if !comboCompatible(combo, add, axes) {
				continue
			}
			for k, v := range add {
				combo[k] = v
			}
			extended = true
		}
		if !extended {
			combos = append(combos, maps.Clone(add))
		}
	}
	return combos
}

// comboMatches reports whether combo carries every key/value in filter.
func comboMatches(combo, filter map[string]string) bool {
	for k, v := range filter {
		if combo[k] != v {
			return false
		}
	}
	return true
}

// comboCompatible reports whether an include entry can extend combo without
// overwriting a value that came from an original matrix axis.
func comboCompatible(combo, add map[string]string, axes []string) bool {
	for _, axis := range axes {
		if v, ok := add[axis]; ok && combo[axis] != v {
			return false
		}
	}
	return true
}

// expandJobName resolves every `${{ matrix.* }}` reference in a job name
// against one matrix combination, yielding the exact check-run name.
func expandJobName(t *testing.T, path, jobID, name string, combo map[string]string) string {
	t.Helper()
	resolved := matrixRef.ReplaceAllStringFunc(name, func(ref string) string {
		key := matrixRef.FindStringSubmatch(ref)[1]
		v, ok := combo[key]
		if !ok {
			t.Fatalf("%s: job %q name references matrix.%s, which its strategy.matrix "+
				"does not define", path, jobID, key)
		}
		return v
	})
	if strings.Contains(resolved, "${{") {
		t.Fatalf("%s: job %q name %q still holds an unresolved ${{ … }} expression after "+
			"matrix expansion, so its check-run name cannot be matched exactly. Teach "+
			"workflowCheckOwners to resolve it — do not fall back to a prefix match, which "+
			"would accept required-check names no job ever posts.", path, jobID, name)
	}
	return resolved
}

// workflowCheckOwners indexes every check-run name declared under
// .github/workflows, together with the push triggers of its workflow.
func workflowCheckOwners(t *testing.T) checkOwnerIndex {
	t.Helper()

	entries, err := os.ReadDir(workflowsDir)
	if err != nil {
		t.Fatalf("read %s: %v", workflowsDir, err)
	}

	exact := map[string]checkOwner{}

	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yml" {
			continue
		}
		path := filepath.Join(workflowsDir, e.Name())

		// `on:` is not always a mapping — `on: pull_request_target` and
		// `on: [push, pull_request]` are both legal — so it is decoded
		// separately and only when it has the shape we care about.
		var doc struct {
			On   yaml.Node `yaml:"on"`
			Jobs map[string]struct {
				Name     string `yaml:"name"`
				Strategy struct {
					Matrix yaml.Node `yaml:"matrix"`
				} `yaml:"strategy"`
			} `yaml:"jobs"`
		}
		if err := yaml.Unmarshal([]byte(readFileString(t, path)), &doc); err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}

		var triggers struct {
			Push struct {
				Branches []string `yaml:"branches"`
			} `yaml:"push"`
		}
		if doc.On.Kind == yaml.MappingNode {
			if err := doc.On.Decode(&triggers); err != nil {
				t.Fatalf("parse %s `on:` block: %v", path, err)
			}
		}

		owner := checkOwner{
			file:         path,
			pushBranches: triggers.Push.Branches,
			mergeGroup:   onDeclares(doc.On, mergeGroupTrigger),
		}
		for id, jb := range doc.Jobs {
			name := jb.Name
			if name == "" {
				name = id
			}
			// A matrix is only expanded when the job name reads from it.
			// Jobs whose name is fixed post that one name on every leg, and
			// jobs named by their id post the id — neither needs the axes,
			// and one of them (`compose-smoke-shard`) builds its matrix from
			// a preceding job's output, which is not knowable from source.
			combos := []map[string]string{{}}
			if strings.Contains(name, "${{") {
				combos = matrixCombos(t, path, id, jb.Strategy.Matrix)
			}
			for _, combo := range combos {
				exact[expandJobName(t, path, id, name, combo)] = owner
			}
		}
	}

	if len(exact) == 0 {
		t.Fatalf("parsed no job names out of %s", workflowsDir)
	}
	return checkOwnerIndex{exact: exact}
}
