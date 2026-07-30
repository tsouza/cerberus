package lib

import (
	"fmt"
	"os/exec"
)

// DockerBin is the client every compose invocation in this harness goes
// through. Named once so Run can recognise it and refuse it.
const DockerBin = "docker"

// Compose runs `docker compose -f file <args...>` with dir as the working
// directory, returning combined output on failure so a scenario failure names
// the real docker error rather than just a non-zero exit.
//
// The child INHERITS this process's environment, which is the reason compose
// invocations live here rather than at their call sites. Compose resolves which
// project — and so which checkout's containers, networks and volumes — an
// invocation addresses from COMPOSE_PROJECT_SUFFIX in that environment (see
// scripts/compose-project-suffix.sh). Handing the child a constructed
// environment instead resolves the unsuffixed project, so a pause or a teardown
// lands in whichever checkout owns that one.
func Compose(dir, file string, args ...string) error {
	full := append([]string{"compose", "-f", file}, args...)
	cmd := exec.Command(DockerBin, full...) //nolint:gosec // harness-authored fixed compose file + fixed subcommand
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("migration harness: docker %v: %w: %s", full, err, string(out))
	}
	return nil
}
