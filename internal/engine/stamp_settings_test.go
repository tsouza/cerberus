package engine

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chopt"
)

// The packages whose constants a stamped setting's name or value may resolve
// through, by import path, and their directories relative to this package.
const (
	modulePath     = "github.com/tsouza/cerberus/"
	enginePkgPath  = modulePath + "internal/engine"
	chclientPkg    = modulePath + "internal/chclient"
	choptPkgPath   = modulePath + "internal/chopt"
	stampFuncName  = "WithQuerySetting"
	stampNameArg   = 1
	stampValueArg  = 2
	stampArgsCount = 3
)

var stampPkgDirs = map[string]string{
	enginePkgPath: ".",
	chclientPkg:   "../chclient",
	choptPkgPath:  "../chopt",
}

// srcFile is one parsed non-test Go file and the package each of its import
// names refers to.
type srcFile struct {
	fset    *token.FileSet
	ast     *ast.File
	imports map[string]string
}

// srcPkg is a package's non-test files, its package-level constants and its
// package-level functions.
type srcPkg struct {
	path   string
	files  []*srcFile
	consts map[string]constDecl
	funcs  map[string]funcDecl
}

type constDecl struct {
	expr ast.Expr
	file *srcFile
}

type funcDecl struct {
	decl *ast.FuncDecl
	file *srcFile
}

// srcTree is the parsed packages, keyed by import path.
type srcTree map[string]*srcPkg

func parseStampPackages(t *testing.T) srcTree {
	t.Helper()
	tree := srcTree{}
	for path, dir := range stampPkgDirs {
		matches, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatalf("glob %s: %v", dir, err)
		}
		pkg := &srcPkg{path: path, consts: map[string]constDecl{}, funcs: map[string]funcDecl{}}
		fset := token.NewFileSet()
		for _, name := range matches {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse %s: %v", name, err)
			}
			sf := &srcFile{fset: fset, ast: f, imports: map[string]string{}}
			for _, imp := range f.Imports {
				ipath, _ := strconv.Unquote(imp.Path.Value)
				local := ipath[strings.LastIndex(ipath, "/")+1:]
				if imp.Name != nil {
					local = imp.Name.Name
				}
				sf.imports[local] = ipath
			}
			pkg.files = append(pkg.files, sf)
			for _, decl := range f.Decls {
				switch d := decl.(type) {
				case *ast.GenDecl:
					if d.Tok != token.CONST {
						continue
					}
					for _, spec := range d.Specs {
						vs := spec.(*ast.ValueSpec)
						for i, n := range vs.Names {
							if i < len(vs.Values) {
								pkg.consts[n.Name] = constDecl{expr: vs.Values[i], file: sf}
							}
						}
					}
				case *ast.FuncDecl:
					if d.Recv == nil && d.Body != nil {
						pkg.funcs[d.Name.Name] = funcDecl{decl: d, file: sf}
					}
				}
			}
		}
		tree[path] = pkg
	}
	return tree
}

// resolve returns the constant value of e as written in file of pkg: a string
// unquoted, a number as its literal text. ok is false for anything that is
// not a literal or a chain of package-level constants over one.
func (tr srcTree) resolve(pkg *srcPkg, file *srcFile, e ast.Expr) (string, bool) {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind == token.STRING {
			s, err := strconv.Unquote(x.Value)
			return s, err == nil
		}
		return x.Value, x.Kind == token.INT
	case *ast.ParenExpr:
		return tr.resolve(pkg, file, x.X)
	case *ast.Ident:
		c, ok := pkg.consts[x.Name]
		if !ok {
			return "", false
		}
		return tr.resolve(pkg, c.file, c.expr)
	case *ast.SelectorExpr:
		id, ok := x.X.(*ast.Ident)
		if !ok {
			return "", false
		}
		other, ok := tr[file.imports[id.Name]]
		if !ok {
			return "", false
		}
		c, ok := other.consts[x.Sel.Name]
		if !ok {
			return "", false
		}
		return tr.resolve(other, c.file, c.expr)
	}
	return "", false
}

// stamp is one setting a stamp site sends: its name, and its value when the
// value is a constant.
type stamp struct {
	site     string
	name     string
	value    string
	valueSet bool
}

// stampsOf walks every call in body, written in file of pkg, and collects the
// settings it stamps: a chclient.WithQuerySetting call's, and those of every
// chclient function it calls, expanded through that function's own body.
func (tr srcTree) stampsOf(t *testing.T, pkg *srcPkg, file *srcFile, body ast.Node, expanded map[string]bool, out *[]stamp) {
	t.Helper()
	chc := tr[chclientPkg]
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		var callee string
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			id, ok := fn.X.(*ast.Ident)
			if !ok || file.imports[id.Name] != chclientPkg {
				return true
			}
			callee = fn.Sel.Name
		case *ast.Ident:
			if pkg != chc {
				return true
			}
			callee = fn.Name
		default:
			return true
		}
		site := fmt.Sprintf("%s: %s(...)", file.fset.Position(call.Pos()), callee)
		if callee == stampFuncName {
			if len(call.Args) != stampArgsCount {
				t.Errorf("%s: %d arguments; want %d", site, len(call.Args), stampArgsCount)
				return true
			}
			name, ok := tr.resolve(pkg, file, call.Args[stampNameArg])
			if !ok {
				t.Errorf("%s: the setting name is not a constant, so the stamp cannot be classified", site)
				return true
			}
			value, valueSet := tr.resolve(pkg, file, call.Args[stampValueArg])
			*out = append(*out, stamp{site: site, name: name, value: value, valueSet: valueSet})
			return true
		}
		if fd, ok := chc.funcs[callee]; ok && !expanded[callee] {
			expanded[callee] = true
			tr.stampsOf(t, chc, fd.file, fd.decl.Body, expanded, out)
		}
		return true
	})
}

// engineStamps returns every setting the engine's source stamps.
func engineStamps(t *testing.T) []stamp {
	t.Helper()
	tr := parseStampPackages(t)
	eng := tr[enginePkgPath]
	var out []stamp
	for _, f := range eng.files {
		tr.stampsOf(t, eng, f, f.ast, map[string]bool{}, &out)
	}
	return out
}

// stampClasses names the classes s falls in.
func stampClasses(t *testing.T, s stamp) []string {
	t.Helper()
	var classes []string
	droppable := false
	if d, ok := chopt.LookupDroppableSetting(s.name); ok {
		switch {
		case d.OnlyAt == nil:
			droppable = true
		case !s.valueSet:
			t.Errorf("%s: %s is droppable only at %v, but the value stamped is not a constant", s.site, s.name, d.OnlyAt)
		default:
			droppable = d.DroppableAt(s.value)
		}
	}
	if droppable {
		classes = append(classes, "chopt.DroppableSettings")
	}
	if _, ok := chopt.GateSettingByName(s.name); ok && !droppable {
		classes = append(classes, "chopt.GateSettings")
	}
	if slices.Contains(FunctionRequiredStampSettings(), s.name) {
		classes = append(classes, "FunctionRequiredStampSettings")
	}
	if slices.Contains(FloorKnownStampSettings(), s.name) {
		classes = append(classes, "FloorKnownStampSettings")
	}
	return classes
}

// TestStampedSettings_AreEachClassifiedOnce reads the engine's source and
// collects every setting it stamps: the name of every
// chclient.WithQuerySetting call, and of every WithQuerySetting call inside a
// chclient function the engine calls (WithTSGridSetting,
// WithResultCacheSetting, WithWorkloadSetting and any other), each name
// resolved through the package-level constants it is spelled with. Each one
// must be in exactly one class (stamp_settings.go): a droppable setting at a
// value it may be dropped at, a gate setting at any other value, a setting a
// function of the statement requires, or a setting the 24.8 floor knows. A
// new stamp of an unclassified setting, or one whose name is not a constant,
// fails. Every entry of each class must be stamped somewhere, so no class
// lists a setting the engine never sends.
func TestStampedSettings_AreEachClassifiedOnce(t *testing.T) {
	t.Parallel()
	stamps := engineStamps(t)
	if len(stamps) == 0 {
		t.Fatal("no stamped setting found in the engine's source")
	}
	stamped := map[string]bool{}
	droppableStamped := map[string]bool{}
	for _, s := range stamps {
		stamped[s.name] = true
		classes := stampClasses(t, s)
		if len(classes) != 1 {
			t.Errorf("%s stamps %s=%s, which is in %d classes %v; want exactly one", s.site, s.name, s.value, len(classes), classes)
		}
		if slices.Contains(classes, "chopt.DroppableSettings") {
			droppableStamped[s.name] = true
		}
	}
	for _, d := range chopt.DroppableSettings() {
		if !droppableStamped[d.Name] {
			t.Errorf("chopt.DroppableSettings lists %s, which the engine never stamps at a droppable value", d.Name)
		}
	}
	listed := FloorKnownStampSettings()
	listed = append(listed, FunctionRequiredStampSettings()...)
	for _, g := range chopt.GateSettings() {
		listed = append(listed, g.Name)
	}
	for _, name := range listed {
		if !stamped[name] {
			t.Errorf("%s is classified, but the engine never stamps it", name)
		}
	}
}
