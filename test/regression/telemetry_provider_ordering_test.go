package regression

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// Layer 8 — startup ordering of the OTel MeterProvider install.
//
// OTel's package-global MeterProvider is a DELEGATING shim. Instruments minted
// off it before the real provider is installed forward to the SDK once
// delegation happens — but a SYNCHRONOUS counter Add recorded before that
// moment is dropped on the floor, with no buffering and no error. Every
// zero-seeded counter cerberus builds at construction time exists precisely to
// make a healthy replica export a flat 0 rather than Grafana's "No data", so
// losing the seed silently reverts the panel to the state the seed was added to
// fix.
//
// The failure is close to invisible in every other layer: observable gauges
// re-register through the delegate and DO survive, so the ClickHouse pool
// census keeps appearing next to the counters that quietly did not. Nothing in
// the binary errors, nothing in a unit test notices, and the only symptom is a
// panel that reads "No data" on an idle replica.
//
// So the ordering is pinned structurally: in run(), otel.SetMeterProvider must
// execute before ANY construction that mints instruments.

// meterProviderInstall is the call that ends delegation.
const meterProviderInstall = "otel.SetMeterProvider"

// instrumentMinters are the run() calls that build instruments during
// construction — each seeds counters that a delegating provider would discard.
var instrumentMinters = []string{
	"chclient.New",
	"newAdmitLimiters",
}

// TestMeterProviderInstalledBeforeInstrumentsAreMinted pins the ordering.
func TestMeterProviderInstalledBeforeInstrumentsAreMinted(t *testing.T) {
	path := filepath.Join(repoRoot, "cmd", "cerberus", "main.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	run := findFuncDecl(file, "run")
	if run == nil {
		t.Fatalf("%s declares no run() — this pin describes cerberus's startup sequence", path)
	}

	installLine, ok := callLine(fset, run, meterProviderInstall)
	if !ok {
		t.Fatalf("run() never calls %s; the global MeterProvider would stay a delegating shim "+
			"and every zero-seeded counter would export nothing", meterProviderInstall)
	}

	for _, minter := range instrumentMinters {
		line, found := callLine(fset, run, minter)
		if !found {
			t.Fatalf("run() no longer calls %s; this pin's inventory of instrument-minting "+
				"constructions is stale and must be updated rather than dropped", minter)
		}
		if line < installLine {
			t.Errorf("run() calls %s at line %d, before %s at line %d. Instruments minted off the "+
				"un-delegated global MeterProvider lose every synchronous Add recorded before "+
				"delegation — including the zero seeds that keep a healthy replica's panels at 0 "+
				"instead of \"No data\"", minter, line, meterProviderInstall, installLine)
		}
	}
}

// findFuncDecl returns the named top-level function, or nil.
func findFuncDecl(file *ast.File, name string) *ast.FuncDecl {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv == nil && fn.Name.Name == name {
			return fn
		}
	}
	return nil
}

// callLine reports the line of the FIRST call to the dotted name (pkg.Fn) or
// bare identifier (Fn) inside fn.
func callLine(fset *token.FileSet, fn *ast.FuncDecl, name string) (int, bool) {
	line := 0
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if callName(call.Fun) != name {
			return true
		}
		if pos := fset.Position(call.Pos()).Line; line == 0 || pos < line {
			line = pos
		}
		return true
	})
	return line, line != 0
}

// callName renders a call target as "pkg.Fn" or "Fn"; anything else is "".
func callName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		if pkg, ok := f.X.(*ast.Ident); ok {
			return pkg.Name + "." + f.Sel.Name
		}
	}
	return ""
}
