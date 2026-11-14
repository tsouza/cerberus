package regression

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// agplLicenceTitle is the title line of the GNU AGPL licence text, which
// every AGPL-3.0 LICENSE file and every full licence notice carries.
const agplLicenceTitle = "GNU AFFERO GENERAL PUBLIC LICENSE"

// agplTitleReaders are the tracked files that name the title to detect it:
// the agpl-clean gate, its test, and this test.
var agplTitleReaders = []string{
	".github/scripts/agpl-clean.mjs",
	".github/scripts/agpl-clean.test.mjs",
	"test/regression/no_agpl_licence_text_test.go",
}

// TestNoTrackedFileCarriesTheAGPLLicenceText — AGPL-3.0 material is used to
// build and test cerberus, never distributed with it, so the repository
// carries no AGPL licence text: a vendored AGPL subtree arrives with its
// LICENSE, and an AGPL source file with its notice. The upstream corpora the
// compatibility harnesses read live in the Go module cache instead.
func TestNoTrackedFileCarriesTheAGPLLicenceText(t *testing.T) {
	t.Parallel()

	cmd := exec.Command("git", "grep", "-l", "-I", "-F", agplLicenceTitle)
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	// git grep exits 1 when nothing matches.
	if err != nil && cmd.ProcessState.ExitCode() != 1 {
		t.Fatalf("git grep: %v", err)
	}
	var carriers []string
	for _, file := range strings.Fields(string(out)) {
		if !slices.Contains(agplTitleReaders, file) {
			carriers = append(carriers, file)
		}
	}
	if len(carriers) > 0 {
		t.Fatalf("tracked files carry the AGPL licence text:\n  %s\n"+
			"AGPL-3.0 material may be read from the Go module cache at test time, "+
			"never committed (compatibility/loki/README.md § Licensing).",
			strings.Join(carriers, "\n  "))
	}
	if !strings.Contains(string(out), agplTitleReaders[0]) {
		t.Fatalf("git grep did not find %s, which names the licence title; the search is not reading the tree", agplTitleReaders[0])
	}
}
