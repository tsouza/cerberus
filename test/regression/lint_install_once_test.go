package regression

import (
	"strings"
	"testing"
)

// golangciInstallModeInput is golangci-lint-action's input choosing how the
// step obtains the binary. Absent, it defaults to golangciInstallModeBinary.
const golangciInstallModeInput = "install-mode"

// golangciInstallModeBinary is the action's default install mode: download the
// release archive and extract it with `tar --overwrite` into a path derived
// from $HOME, the version and the platform.
const golangciInstallModeBinary = "binary"

// golangciInstallModeNone makes the action run the `golangci-lint` already on
// PATH, which an earlier installing step of the same job put there.
const golangciInstallModeNone = "none"

// TestGolangciLintInstallsOncePerJob pins #4141: within one job, at most one
// golangci-lint-action step installs the binary, and every later step reuses it
// with `install-mode: none`.
//
// An installing step extracts the archive into
// $HOME/golangci-lint-<version>-<os>-<arch>/, so two installing steps of one
// version in one job write the same file. The second extraction ran 350 ms
// after the first pass finished, while that pass's binary was still mapped for
// execution, and `tar` failed with `Text file busy` before anything was
// analysed — a red required check unrelated to the commit. A step that does not
// install never opens the binary for writing, so it cannot race it.
//
// The rule is deliberately stricter than the hazard: two installs of different
// versions land in different directories and could not race, and a
// `goinstall` step writes GOBIN rather than $HOME, but one installing step per
// job is the shape that keeps every pass on the same binary.
func TestGolangciLintInstallsOncePerJob(t *testing.T) {
	t.Parallel()

	sites := 0
	for where, steps := range workflowJobSteps(t) {
		installer := ""
		for i, step := range steps {
			if !strings.HasPrefix(step.Uses, golangciActionMarker) {
				continue
			}
			sites++
			label := step.Name
			if label == "" {
				label = "step " + toString(i)
			}
			// The action lowercases the input before comparing it.
			mode := strings.ToLower(strings.TrimSpace(toString(step.With[golangciInstallModeInput])))
			if mode == "" {
				mode = golangciInstallModeBinary
			}
			if mode == golangciInstallModeNone {
				if installer == "" {
					t.Errorf("%s: %q runs golangci-lint with `%s: %s`, but no earlier step of the job "+
						"installs it; the step then finds no binary on PATH.",
						where, label, golangciInstallModeInput, golangciInstallModeNone)
				}
				continue
			}
			if installer != "" {
				hazard := ""
				if mode == golangciInstallModeBinary {
					hazard = " Two binary installs of one version extract into the same $HOME path, and " +
						"the second `tar --overwrite` fails with `Text file busy` while the first binary " +
						"is still mapped."
				}
				t.Errorf("%s: %q installs golangci-lint (`%s: %s`) after %q already did; a job installs "+
					"it once.%s Set `%s: %s` on every step after the first.",
					where, label, golangciInstallModeInput, mode, installer, hazard,
					golangciInstallModeInput, golangciInstallModeNone)
			}
			installer = label
		}
	}
	if sites == 0 {
		t.Fatalf("no workflow job uses %s; the lint gate this pin guards has moved", golangciActionMarker)
	}
}
