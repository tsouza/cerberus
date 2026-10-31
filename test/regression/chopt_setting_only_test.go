package regression

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chopt"
)

// choptRegistryPath declares every chopt.Feature* ID constant.
const choptRegistryPath = "../../internal/chopt/registry.go"

// choptwireSourcePath holds choptwire.SettingsRules, the one place a
// setting-only feature's constant may be read.
const choptwireSourcePath = "../../internal/choptwire/choptwire.go"

// settingsRulesFunc is the choptwire function that maps an EnabledSet onto
// engine.SettingsRules.
const settingsRulesFunc = "SettingsRules"

// TestSettingOnlyFeaturesReachOnlySettingsRules holds chopt.Feature.SettingOnly
// to what it claims. A feature marked setting-only must reach the binary
// through choptwire.SettingsRules alone, which feeds the per-query settings
// the engine stamps at dispatch: its Feature* constant is read inside that
// function and in no other non-test file under cmd/ or internal/ outside
// internal/chopt (comments do not count). A read anywhere else (a lowerer table, a schema strategy, a
// handler flag) could change emitted SQL, and the strict-scan floor ratchet
// would then be skipping a feature its lane can exercise.
func TestSettingOnlyFeaturesReachOnlySettingsRules(t *testing.T) {
	t.Parallel()

	constFor := choptFeatureConstants(t)
	var marked []string
	for _, f := range chopt.Registry() {
		if !f.SettingOnly {
			continue
		}
		name, ok := constFor[f.ID]
		if !ok {
			t.Fatalf("setting-only feature %q has no Feature* constant in %s", f.ID, choptRegistryPath)
		}
		marked = append(marked, name)
	}
	if len(marked) == 0 {
		t.Fatalf("no chopt feature is marked SettingOnly — the check reads the wrong field or the mark was dropped")
	}

	start, end := funcSpan(t, choptwireSourcePath, settingsRulesFunc)
	inside := map[string]bool{}
	for _, root := range []string{"../../cmd", "../../internal"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if filepath.Base(path) == "chopt" && filepath.Dir(path) == filepath.Clean("../../internal") {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			ast.Inspect(file, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != "chopt" || !slices.Contains(marked, sel.Sel.Name) {
					return true
				}
				pos := fset.Position(sel.Pos())
				if filepath.Clean(path) == filepath.Clean(choptwireSourcePath) && pos.Offset >= start && pos.Offset < end {
					inside[sel.Sel.Name] = true
					return true
				}
				t.Errorf("%s reads chopt.%s, which is marked SettingOnly; a setting-only feature may be read "+
					"only inside choptwire.%s — drop the mark or move the read", pos, sel.Sel.Name, settingsRulesFunc)
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	for _, name := range marked {
		if !inside[name] {
			t.Errorf("chopt.%s is marked SettingOnly but choptwire.%s never reads it — the feature reaches no "+
				"per-query setting at all", name, settingsRulesFunc)
		}
	}
}

// choptFeatureConstants maps each feature ID to the name of the constant that
// declares it in the chopt registry source.
func choptFeatureConstants(t *testing.T) map[string]string {
	t.Helper()
	src, err := os.ReadFile(choptRegistryPath)
	if err != nil {
		t.Fatalf("read %s: %v", choptRegistryPath, err)
	}
	re := regexp.MustCompile(`(?m)^\s*(Feature\w+)\s*=\s*"([^"]+)"`)
	out := map[string]string{}
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		out[m[2]] = m[1]
	}
	if len(out) == 0 {
		t.Fatalf("%s declares no Feature* constants", choptRegistryPath)
	}
	return out
}

// funcSpan is the byte range of the top-level function name's body in path.
func funcSpan(t *testing.T, path, name string) (int, int) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Name.Name != name || fn.Body == nil {
			continue
		}
		return fset.Position(fn.Body.Pos()).Offset, fset.Position(fn.Body.End()).Offset
	}
	t.Fatalf("%s declares no function %s", path, name)
	return 0, 0
}
