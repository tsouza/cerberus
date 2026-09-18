package regression

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// `.claude/hooks/guard-heavy-local.mjs` is the PreToolUse hook that refuses a
// mutation run or a golden regeneration on the developer host. Its node:test
// suite lives beside it — the hook is not under .github/scripts, so the
// gate-suites-wired check does not see it — and this test is what runs that
// suite on the required `check` lane, the same way guard_git_hook_test.go
// drives the sibling hook.
const guardHeavyLocalSuitePath = "../../.claude/hooks/guard-heavy-local.test.mjs"

func TestGuardHeavyLocalHookSuite(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("node"); err != nil {
		t.Fatalf("node is required to exercise the PreToolUse hook: %v", err)
	}
	suite, err := filepath.Abs(guardHeavyLocalSuitePath)
	if err != nil {
		t.Fatalf("resolve suite path: %v", err)
	}
	cmd := exec.Command("node", "--test", suite)
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node --test %s failed: %v\n%s", guardHeavyLocalSuitePath, err, out)
	}
}
