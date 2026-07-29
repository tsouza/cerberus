package regression

import (
	"os"
	"strings"
	"testing"
)

// ciWorkflowPath carries the `lint` job, the required check that runs
// golangci-lint across the module.
const ciWorkflowPath = "../../.github/workflows/ci.yml"

// golangciActionMarker opens the golangci-lint step. The version suffix is
// deliberately not part of the marker so an action bump does not silently
// move the step out from under this pin.
const golangciActionMarker = "golangci/golangci-lint-action@"

// skipCacheInput is the input that makes the lane analyse cold.
const skipCacheInput = "skip-cache: true"

// TestLintLaneAnalysesCold pins the `lint` job to a cache-independent run.
//
// golangci-lint-action stores its analysis cache as a GitHub Actions cache
// entry, and those entries are immutable: a key is written once and every
// later run that hits it restores exactly those bytes. A run that populates
// the key with a partially-written cache therefore poisons that key for good
// — until it ages out, every job hitting it reuses the bad cache, and lint's
// verdict becomes a function of cache state rather than of the code.
//
// The observed shape: two pull requests with entirely disjoint diffs both
// failed with byte-identical SA5011 "possible nil pointer dereference"
// findings, in three packages neither of them touched, on code that is
// correct — `if p == nil { t.Fatalf(...) }` guards a later deref, and
// t.Fatalf does not return. staticcheck only knows that from cross-package
// facts; without them it reads the guard as fallthrough and reports a
// deref. Both runs had hit the same primary cache key. Push-to-main passed
// at the same time only because it fell back to a different, older entry.
//
// The false positives are the benign direction. The same missing facts can
// retire a real finding just as quietly, and nothing about a green lane
// would look different. A required check has to be reproducible from the
// source alone, so this lane does not read that cache at all.
//
// The pin matters because the failure it prevents does not look like a
// missing input — it looks like a broken pull request. The findings name
// files the author never opened, which invites either a spurious "fix" to
// untouched code or a nolint directive over a false positive, and both
// leave the unsound gate in place. setup-go's module/build cache is a
// separate mechanism and stays on: Go's build cache is content-addressed,
// so a corrupt entry fails verification rather than being trusted.
func TestLintLaneAnalysesCold(t *testing.T) {
	t.Parallel()

	workflow, err := os.ReadFile(ciWorkflowPath)
	if err != nil {
		t.Fatalf("read %s: %v", ciWorkflowPath, err)
	}

	lines := strings.Split(string(workflow), "\n")

	start := -1
	for i, line := range lines {
		if strings.Contains(line, golangciActionMarker) {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("%s no longer runs %s — the lint gate this pin guards has moved",
			ciWorkflowPath, golangciActionMarker)
	}

	// The step's inputs run from the action line to the next step. Comments
	// are dropped so the prose explaining the input cannot satisfy the pin.
	stepIndent := indentOf(lines[start])
	for i := start + 1; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "- ") && indentOf(line) <= stepIndent {
			break // next step began
		}
		if trimmed == skipCacheInput {
			return
		}
	}

	t.Errorf("the %s step in %s does not set %q.\n"+
		"Without it the lane restores a shared, immutable analysis cache, and a "+
		"single partially-written entry makes lint report findings that depend on "+
		"cache state instead of on the code — inventing them in untouched files, "+
		"and able to suppress real ones just as silently.",
		golangciActionMarker, ciWorkflowPath, skipCacheInput)
}

// indentOf counts the leading spaces on a YAML line, which is how the step
// boundary above is told apart from a nested list item inside an input.
func indentOf(line string) int {
	return len(line) - len(strings.TrimLeft(line, " "))
}
