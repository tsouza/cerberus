package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/config"
	"github.com/tsouza/cerberus/internal/promql"
)

// retiredClassicMergeValue is the value every retired-setting case sets: one
// bucket entry, which the removed bucket-merge ceiling would have refused
// nearly every classic-histogram merge under, so a value that still took
// effect would show in the resolved bounds.
const retiredClassicMergeValue = "1"

// retiredClassicMergeWarning is the part of each retired setting's startup
// warning that says why it is ignored.
const retiredClassicMergeWarning = "the classic-histogram bucket-merge ceiling was removed in v1.25.0"

// TestBoot_RetiredClassicMergeSettingsAreIgnoredWithOneWarning pins the boot
// path for the two settings of the removed classic-histogram bucket-merge
// ceiling, set in the environment or in cerberus.yaml, alone or together:
// the configuration loads, the bounds resolve exactly as they do with neither
// set, and each setting set logs exactly one warning naming it.
func TestBoot_RetiredClassicMergeSettingsAreIgnoredWithOneWarning(t *testing.T) {
	inCerberusYAML(t, "")
	unsetRetiredClassicMergeSettings(t)
	baseCfg, err := config.FromEnv()
	if err != nil {
		t.Fatalf("config.FromEnv with neither setting: %v", err)
	}
	var baseLog bytes.Buffer
	baseEngine, baseProm, err := bootBounds(slog.New(slog.NewTextHandler(&baseLog, nil)), baseCfg)
	if err != nil {
		t.Fatalf("bootBounds with neither setting: %v", err)
	}
	if baseLog.Len() != 0 {
		t.Fatalf("neither setting set logged %q; want nothing", baseLog.String())
	}

	sets := [][]string{
		{promql.EnvClassicBucketMergeMaxEntriesRetired},
		{promql.EnvClassicBucketMergeMaxCostUnitsRetired},
		{promql.EnvClassicBucketMergeMaxEntriesRetired, promql.EnvClassicBucketMergeMaxCostUnitsRetired},
	}
	for _, source := range []string{"env", "yaml"} {
		for _, names := range sets {
			t.Run(source+"/"+strings.Join(names, "+"), func(t *testing.T) {
				unsetRetiredClassicMergeSettings(t)
				var yaml strings.Builder
				for _, name := range names {
					if source == "env" {
						t.Setenv(name, retiredClassicMergeValue)
					} else {
						fmt.Fprintf(&yaml, "%s: %q\n", name, retiredClassicMergeValue)
					}
				}
				inCerberusYAML(t, yaml.String())

				cfg, err := config.FromEnv()
				if err != nil {
					t.Fatalf("config.FromEnv: %v; want the retired settings accepted", err)
				}
				var buf bytes.Buffer
				engineBounds, promBounds, err := bootBounds(slog.New(slog.NewTextHandler(&buf, nil)), cfg)
				if err != nil {
					t.Fatalf("bootBounds: %v", err)
				}
				if !reflect.DeepEqual(engineBounds, baseEngine) || promBounds != baseProm {
					t.Fatalf("bounds %+v / %+v; want those resolved with neither setting, %+v / %+v",
						engineBounds, promBounds, baseEngine, baseProm)
				}
				lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
				if len(lines) != len(names) {
					t.Fatalf("logged %d lines; want one warning per setting set (%d):\n%s", len(lines), len(names), buf.String())
				}
				for _, name := range names {
					var hits int
					for _, line := range lines {
						if strings.Contains(line, "level=WARN") && strings.Contains(line, name+" is ignored") &&
							strings.Contains(line, retiredClassicMergeWarning) {
							hits++
						}
					}
					if hits != 1 {
						t.Fatalf("%d warnings for %s; want exactly one:\n%s", hits, name, buf.String())
					}
				}
			})
		}
	}
}

// unsetRetiredClassicMergeSettings unsets both retired settings for the test
// and restores their previous values afterwards.
func unsetRetiredClassicMergeSettings(t *testing.T) {
	t.Helper()
	for _, name := range []string{promql.EnvClassicBucketMergeMaxEntriesRetired, promql.EnvClassicBucketMergeMaxCostUnitsRetired} {
		unsetEnv(t, name)
	}
}

// TestRun_ResolvesBoundsThroughBootBounds pins run()'s wiring to the function
// TestBoot_RetiredClassicMergeSettingsAreIgnoredWithOneWarning drives. run()
// cannot be called from a test (lifecycle_test.go has the reason), so the
// retired-setting warning reaches the server's startup only while run()
// resolves its bounds through bootBounds: a run() that calls
// resolveBoundOverrides directly still starts, on the same bounds, and
// silently drops the warning.
func TestRun_ResolvesBoundsThroughBootBounds(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, pkgSourceDir+"/main.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	var run *ast.FuncDecl
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "run" {
			run = fn
		}
	}
	if run == nil {
		t.Fatal("main.go declares no run()")
	}
	calls := map[string]int{}
	ast.Inspect(run, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if id, ok := call.Fun.(*ast.Ident); ok {
				calls[id.Name]++
			}
		}
		return true
	})
	if calls["bootBounds"] != 1 {
		t.Errorf("run() calls bootBounds %d times, want once: the retired-setting warning is logged only on that path", calls["bootBounds"])
	}
	if calls["resolveBoundOverrides"] != 0 {
		t.Errorf("run() calls resolveBoundOverrides directly; it must go through bootBounds, or a retired setting is ignored without a warning")
	}
}

// unsetEnv unsets key for the test and restores its previous value
// afterwards.
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unsetenv %s: %v", key, err)
	}
}
