package lib

import (
	"os"
	"strings"
)

// The migration lane runs the same three tier jobs twice against a release
// commit: once on the push (proving the SOURCE TREE) and once against the image
// goreleaser built (proving the ARTIFACT). Nothing about the job names, the
// step names or the compose file changes between those two runs, so the ONLY
// way a scenario can know which cerberus it is actually driving is to ask the
// binary and the server what they are. These constants are the three answers
// that are possible, and the two helpers compute which one is expected.
//
// Every path asserts. There is no branch in which the version check is
// omitted — only branches in which the expected value differs.
const (
	// SourceBuildVersion is what `go build ./cmd/cerberus` with no ldflags
	// reports: cmd/cerberus/main.go's `var Version = "dev"`.
	// test/regression/migration_tier1_test.go holds the two equal.
	SourceBuildVersion = "dev"

	// LocalImageVersion is what the compose stacks' cerberus reports when its
	// image was built from the repo-root Dockerfile.local, which stamps
	// `-X main.Version=e2e`. The same regression pin holds the two equal.
	LocalImageVersion = "e2e"

	// ExpectVersionEnv carries the ONE version every cerberus in the run must
	// report, and is set only when the CLI and the server are the same bytes —
	// i.e. when .github/scripts/migration-artifact.mjs resolved an image plan.
	// On a from-source run it is unset, because the CLI (`dev`) and the
	// compose server (`e2e`) are genuinely different builds.
	ExpectVersionEnv = "CERBERUS_EXPECT_VERSION"

	// ImageEnv names the image the compose stacks run. Read only to quote back
	// in a provenance failure — the value never changes what is asserted.
	ImageEnv = "CERBERUS_IMAGE"
)

// sharedExpectedVersion returns the run-wide version stamp, or "" when the CLI
// and the server come from separate builds.
func sharedExpectedVersion() string {
	return strings.TrimSpace(os.Getenv(ExpectVersionEnv))
}

// ExpectedCLIVersion is the version the cerberus binary the scenarios exec must
// report. A released artifact pins it run-wide; otherwise the binary was
// compiled from this tree with no ldflags and reports the source stamp.
func ExpectedCLIVersion() string {
	if v := sharedExpectedVersion(); v != "" {
		return v
	}
	return SourceBuildVersion
}

// ExpectedServerVersion is the version the cerberus serving the compose stack
// must report at GET /info. A released artifact pins it run-wide; otherwise the
// stack built its image from Dockerfile.local, which stamps LocalImageVersion.
func ExpectedServerVersion() string {
	if v := sharedExpectedVersion(); v != "" {
		return v
	}
	return LocalImageVersion
}

// ResolvedImage is the image the compose stacks were told to run, or "" when
// nothing supplied one. Diagnostic only.
func ResolvedImage() string {
	return strings.TrimSpace(os.Getenv(ImageEnv))
}
