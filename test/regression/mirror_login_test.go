package regression

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The GHCR mirror (.github/scripts/lib/mirror.mjs) holds cerberus's own copy of
// every upstream image its CI pulls, and `pullImageWithRetry` reaches for that
// copy before Docker Hub. The packages are PRIVATE — publishing them would put
// other projects' images under this org's name for anyone to pull — so a job
// only reaches the mirror if it has logged into ghcr.io first.
//
// A missing login is not a hard failure: `acquireFromMirror` gets a denial, says
// so at notice level, and the job falls back to Docker Hub. That is precisely
// what makes it worth a gate. A job silently back on the anonymous Docker Hub
// path looks green until the day the quota is spent, which is the failure this
// whole mechanism exists to remove.

// mirrorLoginMarker is the line `docker/login-action` carries when it is pointed
// at GHCR. Matching the registry rather than the step name keeps the gate on
// what the step DOES.
const mirrorLoginMarker = "registry: ghcr.io"

// imageConsumingMarkers are the module and action names that acquire an image
// through the shared policy, i.e. through the mirror.
var imageConsumingMarkers = []string{
	"compose-pull-images.mjs",
	"pull-images.mjs",
	"pull-buildkit-image.mjs",
	"migration-artifact.mjs",
	"promql-surface-gate.mjs",
	// The composite action's first step is pull-buildkit-image.mjs.
	"setup-buildx",
}

// justInvocation matches a workflow step calling a just recipe, which is how
// most lanes reach an image acquisition: the pull is in the Justfile, not in the
// workflow.
var justInvocation = regexp.MustCompile(`\bjust\s+(_?[a-z][a-z0-9_-]*)`)

// shellInvocation matches a step or recipe running one of the repository's own
// shell scripts, which is how the compatibility lanes reach their pre-pull: the
// acquisition is in the harness script, not in the workflow.
var shellInvocation = regexp.MustCompile(`([A-Za-z0-9_./$${}-]*[a-z0-9-]+\.sh)`)

// pullingRecipes computes the transitive closure of Justfile recipes that end up
// acquiring an image — directly, or by invoking another recipe that does.
func pullingRecipes(t *testing.T) map[string]bool {
	t.Helper()

	buf, err := os.ReadFile("../../Justfile")
	if err != nil {
		t.Fatalf("read Justfile: %v", err)
	}
	recipes := justRecipes(t, string(buf))

	pulls := map[string]bool{}
	for name, body := range recipes {
		for _, marker := range imageConsumingMarkers {
			if strings.Contains(body, marker) {
				pulls[name] = true
			}
		}
	}

	// Fixpoint: a recipe that calls a pulling recipe pulls. The recipe graph is
	// a few hundred nodes, so iterating to stability costs nothing.
	for changed := true; changed; {
		changed = false
		for name, body := range recipes {
			if pulls[name] {
				continue
			}
			for _, m := range justInvocation.FindAllStringSubmatch(body, -1) {
				if pulls[m[1]] {
					pulls[name] = true
					changed = true
					break
				}
			}
		}
	}

	if len(pulls) == 0 {
		t.Fatal("no Justfile recipe acquires an image — the guard is scanning for a shape that no longer exists")
	}
	return pulls
}

// pullingScripts is the set of shell-script BASENAMES in the tree that acquire
// an image. Basename rather than path because a workflow step spells the script
// however its working directory demands, and the name is unique in this tree.
func pullingScripts(t *testing.T) map[string]bool {
	t.Helper()

	scripts := map[string]bool{}
	for _, file := range buildScanFiles(t) {
		if filepath.Ext(file) != ".sh" {
			continue
		}
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for _, marker := range imageConsumingMarkers {
			if strings.Contains(string(src), marker) {
				scripts[filepath.Base(file)] = true
			}
		}
	}

	if len(scripts) == 0 {
		t.Fatal("no shell script in the tree acquires an image — the guard is scanning for a shape that no " +
			"longer exists")
	}
	return scripts
}

// TestEveryImageConsumingJobLogsIntoTheMirror pins that a workflow job which
// acquires an upstream image first authenticates to the registry the mirrored
// copy lives in. Without the login the job still works — it just works the old
// way, drawing on the shared anonymous Docker Hub quota that took the
// compatibility and compose-smoke lanes down.
func TestEveryImageConsumingJobLogsIntoTheMirror(t *testing.T) {
	t.Parallel()

	pulls := pullingRecipes(t)
	scripts := pullingScripts(t)

	consuming := 0
	for _, file := range buildScanFiles(t) {
		if !strings.HasPrefix(filepath.ToSlash(file), "../../.github/workflows/") {
			continue
		}
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}

		for job, body := range workflowJobs(string(src)) {
			why := jobImageAcquisition(body, pulls, scripts)
			if why == "" {
				continue
			}
			consuming++
			if !strings.Contains(body, mirrorLoginMarker) {
				t.Errorf("%s job %q acquires images (%s) but never logs into ghcr.io, so every pull it makes "+
					"misses the mirror and falls back to Docker Hub's shared anonymous quota. Add a "+
					"`docker/login-action` step with `%s` and give the job `packages: read`.",
					file, job, why, mirrorLoginMarker)
			}
		}
	}

	if consuming == 0 {
		t.Fatal("no workflow job acquires an image — the guard is scanning for a shape that no longer exists")
	}
}

// jobImageAcquisition names why a job acquires images, or "" when it does not.
func jobImageAcquisition(body string, pulls, scripts map[string]bool) string {
	for _, line := range logicalLines(body) {
		if isProse(line) {
			continue
		}
		for _, marker := range imageConsumingMarkers {
			if strings.Contains(line, marker) {
				return marker
			}
		}
		for _, m := range justInvocation.FindAllStringSubmatch(line, -1) {
			if pulls[m[1]] {
				return "just " + m[1]
			}
		}
		for _, m := range shellInvocation.FindAllStringSubmatch(line, -1) {
			if base := filepath.Base(m[1]); scripts[base] {
				return base
			}
		}
	}
	return ""
}
