package regression

import (
	"os"
	"path/filepath"
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
// total OVER. Adding a required check to branch protection without adding it
// here is the one drift this file cannot see; adding it here without wiring it
// into release.yml fails immediately.
var branchProtectionContexts = []string{
	"chart-validate",
	"check",
	"compatibility/loki",
	"compatibility/prometheus",
	"compatibility/prometheus-forced-route",
	"compatibility/tempo",
	"compose-smoke",
	"coverage",
	"dashboard",
	"forbid-skip",
	"lint",
	"mutation",
	"probe",
	"profile",
	"property (PromQL + LogQL + TraceQL, rapid N=500)",
	"roundtrip (logql)",
	"roundtrip (promql)",
	"roundtrip (traceql)",
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
				"silence. Either require it, or de-gate it explicitly with a written reason.",
				ctx, releaseWorkflowPath, preflightJob)
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
	for name, body := range recipes {
		for _, line := range strings.Split(body, "\n") {
			if !strings.Contains(line, "git tag") && !strings.Contains(line, "git push") {
				continue
			}
			if strings.Contains(line, "v{{version}}") || strings.Contains(line, "chart-v") {
				t.Errorf("Justfile recipe %q creates or pushes a release tag:\n\t%s\n"+
					"Tags are created BY release.yml after it publishes. A hand-made tag makes "+
					"the version gate see the version as already released, so the release is "+
					"skipped in silence — every job green, nothing shipped.",
					name, strings.TrimSpace(line))
			}
		}
	}
}

// checkOwner is the workflow that posts a given check-run name.
type checkOwner struct {
	file         string
	pushBranches []string
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
// are indexed by their literal prefix rather than expanded — enumerating the
// matrix would be more machinery than the assertion needs, and the prefix is
// unambiguous in practice.
type checkOwnerIndex struct {
	exact    map[string]checkOwner
	prefixes []struct {
		prefix string
		owner  checkOwner
	}
}

func (i checkOwnerIndex) lookup(name string) (checkOwner, bool) {
	if o, ok := i.exact[name]; ok {
		return o, true
	}
	for _, p := range i.prefixes {
		if p.prefix != "" && strings.HasPrefix(name, p.prefix) {
			return p.owner, true
		}
	}
	return checkOwner{}, false
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
	var prefixes []struct {
		prefix string
		owner  checkOwner
	}

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
				Name string `yaml:"name"`
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

		owner := checkOwner{file: path, pushBranches: triggers.Push.Branches}
		for id, jb := range doc.Jobs {
			name := jb.Name
			if name == "" {
				name = id
			}
			if i := strings.Index(name, "${{"); i >= 0 {
				prefixes = append(prefixes, struct {
					prefix string
					owner  checkOwner
				}{strings.TrimRight(name[:i], " "), owner})
				continue
			}
			exact[name] = owner
		}
	}

	if len(exact) == 0 {
		t.Fatalf("parsed no job names out of %s", workflowsDir)
	}
	return checkOwnerIndex{exact: exact, prefixes: prefixes}
}
