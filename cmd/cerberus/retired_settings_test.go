package main

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/config"
	"github.com/tsouza/cerberus/internal/promql"
)

// TestBoot_RetiredClassicMergeNameWarnsAndStarts pins the boot path for the
// retired classic bucket-merge ceiling name: the configuration still loads,
// the bounds still resolve (the old value ignored, the new one or the default
// applied), and exactly one warning naming the replacement is logged while the
// old name is set. The new name alone logs nothing.
func TestBoot_RetiredClassicMergeNameWarnsAndStarts(t *testing.T) {
	const pinned int64 = 1234
	for _, tc := range []struct {
		name        string
		retired     string
		current     string
		wantCeiling int64
		wantWarning bool
	}{
		{"retired alone", "50000000", "", promql.DefaultResourceBounds().ClassicBucketMergeMaxEntries, true},
		{"both set", "50000000", "1234", pinned, true},
		{"current alone", "", "1234", pinned, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setOrUnset(t, promql.EnvClassicBucketMergeMaxCostUnitsRetired, tc.retired)
			setOrUnset(t, promql.EnvClassicBucketMergeMaxEntries, tc.current)

			cfg, err := config.FromEnv()
			if err != nil {
				t.Fatalf("config.FromEnv: %v", err)
			}
			var buf bytes.Buffer
			_, promBounds, err := bootBounds(slog.New(slog.NewTextHandler(&buf, nil)), cfg)
			if err != nil {
				t.Fatalf("bootBounds: %v", err)
			}
			if promBounds.ClassicBucketMergeMaxEntries != tc.wantCeiling {
				t.Fatalf("ClassicBucketMergeMaxEntries = %d, want %d", promBounds.ClassicBucketMergeMaxEntries, tc.wantCeiling)
			}
			lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
			if !tc.wantWarning {
				if buf.Len() != 0 {
					t.Fatalf("logged %q, want nothing", buf.String())
				}
				return
			}
			if len(lines) != 1 {
				t.Fatalf("logged %d lines, want one warning:\n%s", len(lines), buf.String())
			}
			for _, want := range []string{"level=WARN", promql.EnvClassicBucketMergeMaxCostUnitsRetired, promql.EnvClassicBucketMergeMaxEntries, "ignored"} {
				if !strings.Contains(lines[0], want) {
					t.Fatalf("warning %q does not mention %q", lines[0], want)
				}
			}
		})
	}
}

// setOrUnset sets key to v for the test, or unsets it when v is empty, and
// restores the previous value afterwards either way.
func setOrUnset(t *testing.T, key, v string) {
	t.Helper()
	t.Setenv(key, v)
	if v == "" {
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("unsetenv %s: %v", key, err)
		}
	}
}

// TestRun_ResolvesBoundsThroughBootBounds pins run()'s wiring to the function
// TestBoot_RetiredClassicMergeNameWarnsAndStarts drives. run() cannot be
// called from a test (lifecycle_test.go has the reason), so the retired-name
// warning reaches the server's startup only while run() resolves its bounds
// through bootBounds: a run() that calls resolveBoundOverrides directly still
// starts, on the right ceiling, and silently drops the warning.
func TestRun_ResolvesBoundsThroughBootBounds(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, parser.SkipObjectResolution)
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
