package regression

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// The PreToolUse guards read their JSON payload from stdin. A synchronous
// `readFileSync(0)` on a non-blocking pipe throws EAGAIN when the payload has
// not been written yet, and a guard that reads that as "no payload" exits 0
// with nothing on stderr: the guard is off for that command.
//
// Each case below hands the hook a stdin pipe in non-blocking mode and holds
// the payload back until the hook process is already running, so the read
// always finds an empty pipe first. A hook that reads stdin as a stream waits
// for the payload and then judges it; one that reads synchronously fails open
// on every run.

// lateStdinDelay is how long the payload is withheld after the hook starts:
// long enough that node has loaded the module and reached its stdin read.
const lateStdinDelay = 500 * time.Millisecond

func TestHookPayloadWrittenAfterStart(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("node"); err != nil {
		t.Fatalf("node is required to exercise the PreToolUse hooks: %v", err)
	}

	onMain := newGuardRepo(t, "main", true)
	cases := []struct {
		name, hook, cwd, command string
	}{
		{"guard-git blocks a commit on main", guardGitHookPath, onMain, "git commit -m msg"},
		{"guard-heavy-local blocks a mutation run", "../../.claude/hooks/guard-heavy-local.mjs", repoRoot, "just mutate-chdb"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			hook, err := filepath.Abs(tc.hook)
			if err != nil {
				t.Fatalf("resolve hook path: %v", err)
			}
			payload, err := json.Marshal(map[string]any{
				"tool_name":  "Bash",
				"cwd":        tc.cwd,
				"tool_input": map[string]any{"command": tc.command},
			})
			if err != nil {
				t.Fatalf("marshal payload: %v", err)
			}
			stdin, feed, err := os.Pipe()
			if err != nil {
				t.Fatalf("pipe: %v", err)
			}
			// Fd() puts the file in blocking mode and records that, so the later
			// Fd() call inside exec leaves the flag set here alone.
			if err := syscall.SetNonblock(int(stdin.Fd()), true); err != nil {
				t.Fatalf("set the hook's stdin non-blocking: %v", err)
			}
			cmd := exec.Command("node", hook)
			cmd.Dir = tc.cwd
			cmd.Env = append(os.Environ(), "CERBERUS_ALLOW_HEAVY_LOCAL=")
			cmd.Stdin = stdin
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatalf("start hook: %v", err)
			}
			_ = stdin.Close()
			time.Sleep(lateStdinDelay)
			// A hook that already exited without reading closes the pipe, so this
			// write fails; the exit code below is the verdict either way.
			_, writeErr := feed.Write(payload)
			_ = feed.Close()
			got := 0
			if err := cmd.Wait(); err != nil {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) {
					t.Fatalf("run hook: %v (stderr: %s)", err, stderr.String())
				}
				got = exitErr.ExitCode()
			}
			if got != guardBlock || stderr.Len() == 0 {
				t.Fatalf("exit=%d want=%d with a reason on stderr\ncommand: %s\npayload write: %v\nstderr: %s",
					got, guardBlock, tc.command, writeErr, stderr.String())
			}
		})
	}
}
