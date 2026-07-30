package regression

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// repoRoot locates the module root from this package's directory.
const repoRoot = "../.."

// testdataDir holds fixture inputs, not mutable production source.
const testdataDir = "testdata"

// mutationMatrixScript owns the phase table mutation.yml expands into its
// matrix. It is JavaScript rather than YAML because the selector that reads it
// has to reason about each leg's scope and exclude regex, and a workflow's own
// `include:` block is data the workflow cannot read back.
const mutationMatrixScript = ".github/scripts/mutation-matrix.mjs"

// mutationMatrixDumpMode makes that script write the validated table to stdout
// as JSON and nothing else, which is how this test reads it.
const mutationMatrixDumpMode = "dump"

// mutationLeg is one matrix entry: a package scope plus the files it skips.
type mutationLeg struct {
	Phase        string `json:"phase"`
	Scope        string `json:"scope"`
	ExcludeFiles string `json:"exclude_files"`
}

// mutationLegs reads the phase table by running the script that owns it, so
// this test and the CI selector can never disagree about what the legs are.
//
// A missing `node` is a hard failure, not a reason to pass: the repo already
// requires node for commitlint, markdownlint, and every .github/scripts module,
// and a partition check that quietly opts out on a thin machine is exactly the
// silent gap the assertions below exist to close.
func mutationLegs(t *testing.T) []mutationLeg {
	t.Helper()

	cmd := exec.Command("node", mutationMatrixScript, mutationMatrixDumpMode)
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			stderr = string(exitErr.Stderr)
		}
		t.Fatalf("run `node %s %s`: %v\n%s", mutationMatrixScript, mutationMatrixDumpMode, err, stderr)
	}

	var legs []mutationLeg
	if err := json.Unmarshal(out, &legs); err != nil {
		t.Fatalf("parse the %s table: %v", mutationMatrixScript, err)
	}

	return legs
}

// TestMutationLegsPartitionEveryScopedFile pins the invariant that every
// mutable source file under a mutated package is claimed by exactly one leg.
//
// gremlins has no notion of "the set of files I am supposed to cover". Each leg
// is an independent `--exclude-files` regex over its scope, so the partition is
// only ever implied by the intersection of N hand-written regexes. Both ways of
// getting that wrong are silent:
//
//   - A file excluded by every leg is mutated by nobody. Its efficacy is not
//     reported as 0% — it is not reported at all, and the lane stays green while
//     the file's tests go unmeasured. internal/logql/dotted_labels.go and
//     internal/logql/lsyntax/dotted_labels.go sat in exactly this state, having
//     been excluded round by round to stop an OOM that was really the runaway
//     mutant class (see mutation_timeout_max_test.go).
//   - A file excluded by no leg is mutated by all of them. Eight files under
//     internal/logql were mutated four times over, quadrupling their share of
//     the lane's runtime and folding their mutants into three efficacy bars that
//     were never meant to own them.
//
// Neither shape can be seen by reading any single leg — which is why this is a
// test and not a comment. A file added to a mutated package with no matching
// leg is a real gap, so the fix is to claim it in a leg, never to widen an
// exclusion until this passes.
func TestMutationLegsPartitionEveryScopedFile(t *testing.T) {
	t.Parallel()

	legs := mutationLegs(t)
	if len(legs) == 0 {
		t.Fatalf("%s declares no matrix legs; the partition check below would vacuously pass",
			mutationMatrixScript)
	}

	// claims[file] = the legs that mutate it.
	claims := map[string][]string{}
	scoped := map[string]bool{}

	for _, leg := range legs {
		if leg.Scope == "" {
			t.Errorf("matrix leg %q has no scope", leg.Phase)

			continue
		}
		dir := filepath.Join(repoRoot, filepath.FromSlash(strings.TrimPrefix(leg.Scope, "./")))

		var exclude *regexp.Regexp
		if leg.ExcludeFiles != "" {
			compiled, err := regexp.Compile(leg.ExcludeFiles)
			if err != nil {
				t.Errorf("matrix leg %q has an uncompilable exclude_files %q: %v",
					leg.Phase, leg.ExcludeFiles, err)

				continue
			}
			exclude = compiled
		}

		for _, file := range mutableGoFiles(t, dir) {
			rel, err := filepath.Rel(dir, file)
			if err != nil {
				t.Fatalf("relativise %s against %s: %v", file, dir, err)
			}
			scoped[file] = true
			// gremlins matches exclude_files against the path relative to the
			// scope argument, so match on that and not on the repo-relative path.
			if exclude != nil && exclude.MatchString(filepath.ToSlash(rel)) {
				continue
			}
			claims[file] = append(claims[file], leg.Phase)
		}
	}

	if len(scoped) == 0 {
		t.Fatalf("no mutable Go files found under any matrix scope in %s; the scopes are probably "+
			"wrong, and every assertion below would vacuously pass", mutationMatrixScript)
	}

	for file := range scoped {
		shown := filepath.ToSlash(strings.TrimPrefix(file, repoRoot+string(filepath.Separator)))
		switch owners := claims[file]; len(owners) {
		case 1:
		case 0:
			t.Errorf("%s is mutated by no leg: every leg's exclude_files skips it, so its mutation "+
				"efficacy is never measured and the lane stays green regardless of its tests. Claim "+
				"it in exactly one leg.", shown)
		default:
			t.Errorf("%s is mutated by %d legs (%s): its mutants run once per leg, multiplying the "+
				"lane's runtime and counting toward efficacy bars that were not meant to own it. "+
				"Exclude it from all but one.", shown, len(owners), strings.Join(owners, ", "))
		}
	}
}

// mutableGoFiles returns the files gremlins can mutate under dir: non-test Go
// sources, recursively. Test sources are excluded because gremlins mutates
// production code only, and testdata because it holds fixtures rather than
// compiled source.
func mutableGoFiles(t *testing.T, dir string) []string {
	t.Helper()

	var out []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == testdataDir {
				return filepath.SkipDir
			}

			return nil
		}
		name := d.Name()
		if strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			out = append(out, path)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}

	return out
}
