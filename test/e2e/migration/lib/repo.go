// Package lib holds the artifact-collection and assertion helpers the Layer-14
// migration scenarios share: repository-root discovery, building the cerberus
// binary the scenarios drive, running that binary offline with its exit code
// captured as data, and comparing an artifact against a committed golden.
//
// The package carries no build tag on purpose. It is the machinery every
// assertion rests on, so it is compiled by `go build ./...` and linted by
// golangci-lint (which sets no build tags) like any other package.
package lib

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// modulePath is the cerberus module line RepoRoot looks for. Matching the
// module path — rather than the mere presence of a go.mod — keeps the walk
// from stopping inside a nested module (the AGPL-quarantined test/oracle tree
// has its own go.mod).
const modulePath = "module github.com/tsouza/cerberus"

// harnessDir is the harness tree's path relative to the repository root. Every
// fixture, feature and golden path is resolved against it.
const harnessDir = "test/e2e/migration"

// RepoRoot walks up from the working directory and returns the first ancestor
// holding the cerberus go.mod. Scenarios run from the tier package's directory,
// so every fixture path is resolved against this root rather than against a
// relative path that changes with the caller.
func RepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("migration harness: working directory: %w", err)
	}
	return repoRootFrom(dir)
}

// repoRootFrom is RepoRoot's pure core, walking up from an explicit directory.
func repoRootFrom(dir string) (string, error) {
	start := dir
	for {
		mod := filepath.Join(dir, "go.mod")
		buf, err := os.ReadFile(mod) //nolint:gosec // walking known ancestors of the working directory
		if err == nil && strings.Contains(string(buf), modulePath) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("migration harness: no %q go.mod above %s", modulePath, start)
		}
		dir = parent
	}
}

// HarnessPath joins path elements onto the harness tree under the repository
// root, so a caller names `archetypes/<a>/rules` rather than repeating the
// tree's location.
func HarnessPath(root string, elem ...string) string {
	return filepath.Join(append([]string{root, harnessDir}, elem...)...)
}
