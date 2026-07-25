package lib

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// updateGoldensEnv, when set to a non-empty value, rewrites the golden from the
// observed artifact instead of comparing against it.
const updateGoldensEnv = "MIGRATION_UPDATE_GOLDENS"

// ciEnv is the variable every CI provider sets. Regeneration is refused when it
// is present: a golden is a reviewed artifact, and a lane that rewrites its own
// expectations asserts only that the tool still does whatever it does.
const ciEnv = "CI"

// goldenFileMode / goldenDirMode are the permissions a regenerated golden and
// its parent directory get.
const (
	goldenFileMode = 0o644
	goldenDirMode  = 0o755
)

// maxGoldenDiffLines caps how many differing lines a mismatch reports, so a
// wholesale regeneration does not bury the first real difference under a
// thousand-line dump.
const maxGoldenDiffLines = 20

// AssertGolden compares actual against the committed golden at path, returning
// an error naming the first differing lines when they diverge. Under
// MIGRATION_UPDATE_GOLDENS the golden is rewritten instead — unless CI is set,
// where regeneration is refused.
func AssertGolden(path string, actual []byte) error {
	if os.Getenv(updateGoldensEnv) != "" {
		if os.Getenv(ciEnv) != "" {
			return fmt.Errorf(
				"migration harness: refusing to regenerate golden %s under %s: a golden is reviewed in a pull request, never rewritten by the lane that checks it",
				path, ciEnv,
			)
		}
		return writeGolden(path, actual)
	}

	want, err := os.ReadFile(path) //nolint:gosec // harness-authored golden path
	if err != nil {
		return fmt.Errorf("migration harness: read golden %s: %w", path, err)
	}
	if bytes.Equal(want, actual) {
		return nil
	}
	return fmt.Errorf("migration harness: golden %s does not match the artifact:\n%s", path, diff(want, actual))
}

// writeGolden replaces the golden at path with data, creating its directory.
func writeGolden(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), goldenDirMode); err != nil {
		return fmt.Errorf("migration harness: create golden directory for %s: %w", path, err)
	}
	if err := os.WriteFile(path, data, goldenFileMode); err != nil {
		return fmt.Errorf("migration harness: write golden %s: %w", path, err)
	}
	return nil
}

// diff renders the differing lines between want and got, capped at
// maxGoldenDiffLines, plus a trailing note when either side has extra lines.
func diff(want, got []byte) string {
	wantLines := strings.Split(string(want), "\n")
	gotLines := strings.Split(string(got), "\n")

	var b strings.Builder
	shown := 0
	for i := 0; i < len(wantLines) || i < len(gotLines); i++ {
		w, g := lineAt(wantLines, i), lineAt(gotLines, i)
		if w == g {
			continue
		}
		if shown == maxGoldenDiffLines {
			fmt.Fprintf(&b, "  … further differences suppressed after %d lines\n", maxGoldenDiffLines)
			break
		}
		fmt.Fprintf(&b, "  line %d:\n    golden: %q\n    actual: %q\n", i+1, w, g)
		shown++
	}
	fmt.Fprintf(&b, "  golden has %d lines, artifact has %d\n", len(wantLines), len(gotLines))
	return b.String()
}

// lineAt returns the i-th line, or the empty string past the end, so the two
// sides can be walked with one index.
func lineAt(lines []string, i int) string {
	if i < len(lines) {
		return lines[i]
	}
	return ""
}
