package lib

import (
	"fmt"
	"os"
	"path/filepath"
)

// EnvSuiteReport names the file a tier's godog suite writes its cucumber-JSON
// run report to. It is set by `.github/scripts/migration-e2e.mjs` MODE=run,
// which owns the path — NOT by the workflow — so a tier job cannot ship
// without a report simply because whoever wrote its YAML stanza forgot a step.
// `MODE=attest` then reads those reports back and holds every scenario the
// coverage ratchet counts to "actually executed, and passed".
//
// A suite invoked WITHOUT this variable (a bare `go test -tags=migration ...`
// during local development) writes no report and behaves exactly as before.
// That is deliberate: the attestation gate lives in CI, where MODE=run always
// sets the variable, so a developer iterating on one scenario is not forced to
// produce an artifact nobody reads.
const EnvSuiteReport = "MIGRATION_REPORT"

// prettyFormat keeps the human-readable per-step output in the job log — the
// only thing a reader has when a scenario fails on a runner. The cucumber
// formatter is ADDED to it rather than replacing it: godog's Options.Format is
// a comma-separated list of `name[:outfile]` pairs, and a pair with no outfile
// writes to Options.Output (stdout here).
const prettyFormat = "pretty"

// cucumberFormat is godog's built-in machine-readable formatter. Its output is
// the one the attestation gate parses, so the report shape is godog's own
// rather than something this repo invents and has to keep in step.
const cucumberFormat = "cucumber"

// reportDirPerm is the mode the report's parent directory is created with when
// it does not already exist — godog opens the file with os.Create and fails
// the whole suite on a missing directory.
const reportDirPerm = 0o755

// SuiteFormat returns the godog Options.Format string a tier suite should run
// with: the pretty formatter alone when no report was requested, or the pretty
// formatter plus a cucumber report at EnvSuiteReport's path when one was.
//
// Every tier suite calls this instead of hardcoding "pretty", so a new tier
// (tier2-ruler and whatever follows it) is attestable by construction rather
// than by remembering to wire a formatter.
func SuiteFormat() (string, error) {
	path := os.Getenv(EnvSuiteReport)
	if path == "" {
		return prettyFormat, nil
	}
	if !filepath.IsAbs(path) {
		// `go test` runs a package's binary with the package directory as its
		// working directory, so a relative path here would land next to the
		// tier's test file rather than where the caller meant. Refusing is
		// better than writing the report somewhere the reader will not look.
		return "", fmt.Errorf("%s must be an absolute path, got %q", EnvSuiteReport, path)
	}
	if err := os.MkdirAll(filepath.Dir(path), reportDirPerm); err != nil {
		return "", fmt.Errorf("create the directory for %s=%s: %w", EnvSuiteReport, path, err)
	}
	return prettyFormat + "," + cucumberFormat + ":" + path, nil
}
