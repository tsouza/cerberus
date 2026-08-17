package regression

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Shared fixture-path helpers, split out of the single parity contract test
// in #2194 so each of the resulting ratchets (coverage, enrolment, sections,
// vocabulary) can be read and fail independently.

func repoRootForParity(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// test/regression -> repo root
	return filepath.Dir(filepath.Dir(wd))
}

func parityFixtureDirs(t *testing.T) []string {
	t.Helper()
	root := repoRootForParity(t)
	return []string{
		filepath.Join(root, "test", "spec", "promql"),
		filepath.Join(root, "test", "spec", "logql"),
		filepath.Join(root, "test", "spec", "traceql"),
	}
}

func txtarFilesForParity(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read fixture dir %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".txtar") {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	return out
}
