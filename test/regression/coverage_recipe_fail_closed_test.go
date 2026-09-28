package regression

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestCoverageRecipeFailsClosedOnGoTestFailure pins the coverage profile to a
// successful test run. Before #2089, both go test pipelines ended in `|| true`,
// so an entire package could fail while its partial profile still reached the
// floor comparison as apparently valid evidence.
//
// tsouza/cerberus#2634 split the old single `coverage` recipe's two `go test`
// pipelines into their own `coverage-default` / `coverage-chdb` recipes (so
// CI can shard them across parallel jobs); `coverage-merge` folds the two
// profiles and `coverage` chains all three for local use.
//
// tsouza/cerberus#3113 then extracted the chdb-tagged `go test` invocation
// itself out of the coverage-chdb recipe body into coverage-chdb.mjs's own
// source (mirroring coverage-merge's own #3095 extraction) — so the
// evidence for that HALF of this test moved from `just --dump`'s recipe-body
// text to the script's source: it still must declare an explicit -timeout
// and must not tolerate a failed run. The default-tag lane's `go test`
// invocation is unchanged and is still read via justDump() (#3093); the
// pipefail check stays a direct root-Justfile read: `set shell := [...]` is
// a per-invocation `just` SETTING, which can only be declared once, in the
// root file itself.
func TestCoverageRecipeFailsClosedOnGoTestFailure(t *testing.T) {
	t.Parallel()

	d := justDump(t)
	var recipe strings.Builder
	for _, name := range []string{"coverage-default", "coverage-merge"} {
		recipe.WriteString(d.recipe(t, name).bodyText(t))
		recipe.WriteString("\n")
	}
	body := recipe.String()

	if got := strings.Count(body, "go test -timeout"); got != 1 {
		t.Fatalf("coverage-default/coverage-merge recipes carry %d literal go test run(s), want exactly the default-tag lane's", got)
	}
	if strings.Contains(body, "|| true") {
		t.Fatalf("coverage recipes tolerate a failed command with `|| true`; a partial profile is not valid evidence")
	}

	scriptSource, err := os.ReadFile("../../.github/scripts/coverage-chdb.mjs")
	if err != nil {
		t.Fatalf("read coverage-chdb.mjs: %v", err)
	}
	script := string(scriptSource)
	if !strings.Contains(script, "'-timeout', `${MAIN_SWEEP_TIMEOUT_MINUTES}m`,") {
		t.Fatal("coverage-chdb.mjs's main sweep no longer declares an explicit go test -timeout")
	}
	if strings.Contains(script, "|| true") || strings.Contains(script, ".catch(") {
		t.Fatal("coverage-chdb.mjs tolerates a failed command; a partial profile is not valid evidence")
	}
	if !strings.Contains(script, "if (code !== 0) return code;") {
		t.Fatal("coverage-chdb.mjs no longer propagates its go test sweep's exit code")
	}

	rootJustfile, err := os.ReadFile("../../Justfile")
	if err != nil {
		t.Fatalf("read Justfile: %v", err)
	}
	if !strings.Contains(string(rootJustfile), `set shell := ["bash", "-eu", "-o", "pipefail", "-c"]`) {
		t.Fatalf("Justfile shell does not enable pipefail; a failed go test before the output-filter pipe would be hidden")
	}
}

// Cross-package coverage includes zero-count metadata for unlinked packages.
// Cached results can retain those blocks at old source coordinates after a
// comment-only edit. Both producers must measure the current source instead.
func TestCoverageProducersMeasureCurrentSource(t *testing.T) {
	t.Parallel()
	defaultBody := justDump(t).recipe(t, "coverage-default").bodyText(t)
	const commandPrefix = "go test "
	_, command, ok := strings.Cut(defaultBody, commandPrefix)
	if !ok {
		t.Fatal("coverage-default has no go test invocation")
	}
	defaultFlags, _, ok := strings.Cut(command, "-coverpkg=")
	if !ok {
		t.Fatal("coverage-default has no cross-package instrumentation")
	}
	defaultArgv := append([]string{"test"}, strings.Fields(defaultFlags)...)
	defaultArgv = append(defaultArgv, "-coverpkg=./...", "-coverprofile=cover.out", "./...")

	node := exec.Command("node", "--input-type=module", "-e", "import { mainSweepArgv } from './.github/scripts/coverage-chdb.mjs'; console.log(JSON.stringify(mainSweepArgv('./...')));")
	node.Dir = "../.."
	output, err := node.CombinedOutput()
	if err != nil {
		t.Fatalf("read chDB producer argv: %v\n%s", err, output)
	}
	var chdbArgv []string
	if err := json.Unmarshal(output, &chdbArgv); err != nil {
		t.Fatal(err)
	}
	for name, argv := range map[string][]string{"default": defaultArgv, "chdb": chdbArgv} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			write := func(name, contents string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			write("go.mod", "module example.test/coveragecache\n\ngo 1.26.0\n")
			const source = "package fixture\n\nfunc Value() int {\n return 1\n}\n"
			for _, pkg := range []string{"a", "b"} {
				if err := os.Mkdir(filepath.Join(dir, pkg), 0o700); err != nil {
					t.Fatal(err)
				}
				write(pkg+"/value.go", source)
				write(pkg+"/value_test.go", "package fixture\nimport \"testing\"\nfunc TestValue(t *testing.T) { if Value() != 1 { t.Fatal(\"value\") } }\n")
			}
			const measurementTimeout = 2 * time.Minute
			run := func(args []string) {
				t.Helper()
				ctx, cancel := context.WithTimeout(t.Context(), measurementTimeout)
				defer cancel()
				cmd := exec.CommandContext(ctx, "go", args...)
				cmd.Dir = dir
				cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=")
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("go %v: %v\n%s", args, err, output)
				}
			}
			// Populate the cache with exactly this producer's flags, including tags
			// and timeout, before the source coordinate changes.
			warmArgv := slices.DeleteFunc(slices.Clone(argv), func(arg string) bool { return arg == "-count=1" })
			run(warmArgv)
			write("a/value.go", "// Shift source coordinates without changing behavior.\n"+source)
			run(argv)
			profile := "cover.out"
			if name == "chdb" {
				profile = "cover-chdb.out"
			}
			data, err := os.ReadFile(filepath.Join(dir, profile))
			if err != nil {
				t.Fatal(err)
			}
			const currentBlock = "example.test/coveragecache/a/value.go:4.18,6.2 1 "
			found := false
			for line := range strings.SplitSeq(string(data), "\n") {
				if !strings.Contains(line, "/a/value.go:") {
					continue
				}
				if !strings.HasPrefix(line, currentBlock) {
					t.Fatalf("obsolete coverage block after source shift: %s", line)
				}
				found = true
			}
			if !found {
				t.Fatal("current source coverage block is missing")
			}
		})
	}
}
