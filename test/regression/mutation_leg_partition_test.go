package regression

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// repoRoot locates the module root from this package's directory.
const repoRoot = "../.."

// testdataDir holds fixture inputs, not mutable production source.
const testdataDir = "testdata"

// mutationMatrixJob is the job whose matrix partitions packages across legs.
const mutationMatrixJob = "mutate"

// mutationWorkflow mirrors just enough of mutation.yml to read the leg matrix.
// Everything else in the file is deliberately left untyped: this test pins the
// file/leg partition, not the workflow's shape.
type mutationWorkflow struct {
	Jobs map[string]struct {
		Strategy struct {
			Matrix struct {
				Include []mutationLeg `yaml:"include"`
			} `yaml:"matrix"`
		} `yaml:"strategy"`
	} `yaml:"jobs"`
}

// mutationLeg is one matrix entry: a package scope plus the files it skips.
type mutationLeg struct {
	Phase        string `yaml:"phase"`
	Scope        string `yaml:"scope"`
	ExcludeFiles string `yaml:"exclude_files"`
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

	raw, err := os.ReadFile(mutationWorkflowPath)
	if err != nil {
		t.Fatalf("read %s: %v", mutationWorkflowPath, err)
	}

	var wf mutationWorkflow
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse %s: %v", mutationWorkflowPath, err)
	}

	job, ok := wf.Jobs[mutationMatrixJob]
	if !ok {
		t.Fatalf("%s has no %q job; if the mutation matrix moved, re-point this test at it rather "+
			"than deleting it — the partition it guards is still unenforced anywhere else.",
			mutationWorkflowPath, mutationMatrixJob)
	}

	legs := job.Strategy.Matrix.Include
	if len(legs) == 0 {
		t.Fatalf("%s job %q declares no matrix legs; the partition check below would vacuously pass",
			mutationWorkflowPath, mutationMatrixJob)
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
			exclude, err = regexp.Compile(leg.ExcludeFiles)
			if err != nil {
				t.Errorf("matrix leg %q has an uncompilable exclude_files %q: %v",
					leg.Phase, leg.ExcludeFiles, err)

				continue
			}
		}

		for _, file := range mutableGoFiles(t, dir) {
			rel, relErr := filepath.Rel(dir, file)
			if relErr != nil {
				t.Fatalf("relativise %s against %s: %v", file, dir, relErr)
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
			"wrong, and every assertion below would vacuously pass", mutationWorkflowPath)
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
