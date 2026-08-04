package regression

import (
	"os/exec"
	"strings"
	"testing"
)

// #1568 established empirically that GitHub's server-side merge does NOT
// honour the `-merge` .gitattributes driver #1567 relies on: two throwaway
// branches inserting non-overlapping records into the same `-merge`-marked
// file were reported "mergeable" by the GitHub API even though a local `git
// merge` of the identical pair refused with a conflict. Most `-merge` files
// already have a required, content-exact regenerate-and-diff ratchet
// elsewhere; generated-baseline-structural-guard.mjs is the fast structural
// backstop layered on top (see its own doc comment for the full audit).
//
// A backstop that exists but is not wired into a required job is invisible —
// it keeps compiling, keeps passing its own self-test, and stops being run
// the moment its CI step is dropped or its job renamed. This pins the one
// link a PR diff can silently break: the script's invocation inside the
// `forbid-skip` job of ci.yml (a required status check), and that its
// self-test — including the "every configured TARGET is currently clean"
// assertion — passes against the tree as checked in.
// ciWorkflowPath is shared with lint_cache_test.go.
const (
	forbidSkipJobName     = "forbid-skip"
	structuralGuardScript = "generated-baseline-structural-guard.mjs"
)

func TestGeneratedBaselineStructuralGuardWired(t *testing.T) {
	t.Parallel()

	job := workflowJobBody(t, readFileString(t, ciWorkflowPath), forbidSkipJobName)
	if !strings.Contains(job, structuralGuardScript) {
		t.Fatalf("%s job %q does not invoke %s — the #1568 structural guard over generated `-merge` "+
			"baselines is not wired into a required PR check. Body:\n%s",
			ciWorkflowPath, forbidSkipJobName, structuralGuardScript, job)
	}
	if !strings.Contains(job, structuralGuardScript+" --self-test") {
		t.Fatalf("%s job %q invokes %s without --self-test first — a regression in the guard's own "+
			"comparison logic (e.g. the sorted-order or duplicate-key check silently inverted) would ship "+
			"undetected. Body:\n%s",
			ciWorkflowPath, forbidSkipJobName, structuralGuardScript, job)
	}

	// The script reads its TARGETS by repo-root-relative path (matching how
	// ci.yml invokes it — `node .github/scripts/...` from the checkout root),
	// so it must run with that as its working directory.
	scriptPath := ".github/scripts/" + structuralGuardScript

	cmd := exec.Command("node", scriptPath, "--self-test")
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node %s --self-test failed: %v\n%s", structuralGuardScript, err, out)
	}

	cmd = exec.Command("node", scriptPath)
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node %s reported a structural violation against the committed tree:\n%s", structuralGuardScript, out)
	}
}
