package regression

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// `.claude/hooks/guard-git.mjs` is a PreToolUse hook that refuses a `git
// commit` / `git push` aimed at `main`, and refuses either while lefthook's
// git hooks are missing. Its whole value is that it fires on the commands an
// agent actually types, and its whole cost is that a false positive blocks
// every commit in the session with no way around it.
//
// The command an agent actually types is the part that is easy to get wrong.
// Agents work in linked worktrees and reach them by changing directory first —
// `cd <worktree> && git commit …`. The hook payload's `cwd` is the session's
// project directory, a DIFFERENT checkout of the same repository on a
// different branch. Resolving the branch from the payload's `cwd` therefore
// reads the wrong branch, and while the project checkout sits on `main` the
// guard refuses every commit made from every worktree — work that was never
// aimed at `main` at all. That regression is invisible to any test that only
// exercises the plain `git commit` form, because that form happens to resolve
// to the right directory by accident.
//
// So the cases below drive the hook end to end, as the harness does: a JSON
// payload on stdin, an exit code out. Exit 0 allows, exit 2 blocks.
const (
	guardAllow = 0
	guardBlock = 2

	guardGitHookPath = "../../.claude/hooks/guard-git.mjs"
)

// lefthookHookNames are the hook files guard-git.mjs looks for. Their contents
// must mention lefthook — the guard rejects an unrelated script sitting at the
// same path, since that would satisfy a bare existence check while running
// none of the repo's gates.
var lefthookHookNames = []string{"pre-commit", "commit-msg", "pre-push"}

func TestGuardGitHook(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("node"); err != nil {
		t.Fatalf("node is required to exercise the PreToolUse hook: %v", err)
	}
	hook, err := filepath.Abs(guardGitHookPath)
	if err != nil {
		t.Fatalf("resolve hook path: %v", err)
	}

	onMain := newGuardRepo(t, "main", true)
	onFeature := newGuardRepo(t, "fix/some-work", true)
	noHooks := newGuardRepo(t, "fix/other-work", false)

	cases := []struct {
		name    string
		cwd     string
		command string
		want    int
	}{
		{
			name:    "commit on main is blocked",
			cwd:     onMain,
			command: "git commit -m msg",
			want:    guardBlock,
		},
		{
			// The regression this file exists for.
			name:    "commit in a worktree reached by cd is allowed from a project dir on main",
			cwd:     onMain,
			command: "cd " + onFeature + " && git commit -m msg",
			want:    guardAllow,
		},
		{
			name:    "commit via git -C is allowed from a project dir on main",
			cwd:     onMain,
			command: "git -C " + onFeature + " commit -m msg",
			want:    guardAllow,
		},
		{
			name:    "cd into a checkout on main is still blocked",
			cwd:     onFeature,
			command: "cd " + onMain + " && git commit -m msg",
			want:    guardBlock,
		},
		{
			name:    "push with an explicit main refspec is blocked from a feature branch",
			cwd:     onFeature,
			command: "git push origin HEAD:main",
			want:    guardBlock,
		},
		{
			name:    "ordinary push from a feature branch is allowed",
			cwd:     onFeature,
			command: "git push",
			want:    guardAllow,
		},
		{
			name:    "commit without lefthook installed is blocked",
			cwd:     noHooks,
			command: "git commit -m msg",
			want:    guardBlock,
		},
		{
			name:    "a non-git command is allowed",
			cwd:     onMain,
			command: "ls -la",
			want:    guardAllow,
		},
		{
			name:    "a read-only git command is allowed on main",
			cwd:     onMain,
			command: "git status --short",
			want:    guardAllow,
		},
		{
			name:    "the rtk wrapper does not hide a commit on main",
			cwd:     onMain,
			command: "rtk git commit -m msg",
			want:    guardBlock,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, stderr := runGuard(t, hook, tc.cwd, tc.command)
			if got != tc.want {
				t.Fatalf("exit=%d want=%d\ncommand: %s\nstderr: %s", got, tc.want, tc.command, stderr)
			}
			if tc.want == guardBlock && stderr == "" {
				t.Errorf("guard blocked without explaining why; an agent has nothing to act on")
			}
		})
	}
}

// runGuard feeds the hook a PreToolUse payload and returns its exit code plus
// whatever it wrote to stderr, which is the text Claude Code shows the model.
func runGuard(t *testing.T, hook, cwd, command string) (int, string) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"tool_name":  "Bash",
		"cwd":        cwd,
		"tool_input": map[string]any{"command": command},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	cmd := exec.Command("node", hook)
	cmd.Stdin = bytes.NewReader(payload)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Dir = cwd
	runErr := cmd.Run()
	if runErr == nil {
		return 0, stderr.String()
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return exitErr.ExitCode(), stderr.String()
	}
	t.Fatalf("run hook: %v (stderr: %s)", runErr, stderr.String())
	return 0, ""
}

// newGuardRepo builds a throwaway git repository on the named branch, with or
// without lefthook's hook files, and returns its path.
func newGuardRepo(t *testing.T, branch string, withLefthook bool) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet", "--initial-branch", branch)
	runGit(t, dir, "config", "user.email", "regression@cerberus.invalid")
	runGit(t, dir, "config", "user.name", "regression")
	runGit(t, dir, "commit", "--quiet", "--allow-empty", "-m", "root")

	if !withLefthook {
		return dir
	}
	hooks := filepath.Join(dir, ".git", "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatalf("mkdir hooks: %v", err)
	}
	for _, name := range lefthookHookNames {
		script := "#!/bin/sh\nlefthook run " + name + "\n"
		if err := os.WriteFile(filepath.Join(hooks, name), []byte(script), 0o755); err != nil {
			t.Fatalf("write %s hook: %v", name, err)
		}
	}
	return dir
}
