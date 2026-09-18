package regression

import (
	"go/ast"
	goparser "go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// telemetryExpoHistogramMaxSizeConst is the constant internal/telemetry
// collects cerberus's own duration histogram with. internal/telemetry is a
// leaf that imports no internal layer, so it cannot read
// chplan.OTelExpoHistogramDefaultMaxSize itself; this test is the link.
const telemetryExpoHistogramMaxSizeConst = "queryDurationExpoHistogramMaxSize"

// TestOTelExpoHistogramDefaultMaxSizeIsSharedWithTelemetry pins the SDK
// view's bucket budget to the query side's merge width cap. The cap's
// whole justification is "a merge is never coarser than what a single
// series already tolerates by default", which is only true while the two
// numbers are the same number.
func TestOTelExpoHistogramDefaultMaxSizeIsSharedWithTelemetry(t *testing.T) {
	t.Parallel()

	path := filepath.Join(repoRootForParity(t), "internal", "telemetry", "telemetry.go")
	got, ok := packageIntConst(t, path, telemetryExpoHistogramMaxSizeConst)
	if !ok {
		t.Fatalf("%s: const %s not found", path, telemetryExpoHistogramMaxSizeConst)
	}
	if got != chplan.OTelExpoHistogramDefaultMaxSize {
		t.Fatalf("%s: %s = %d, but chplan.OTelExpoHistogramDefaultMaxSize = %d — the SDK view's bucket budget and the merge width cap must be the same OTel default",
			path, telemetryExpoHistogramMaxSizeConst, got, chplan.OTelExpoHistogramDefaultMaxSize)
	}
}

// packageIntConst looks up `<name> = <int literal>` in a package-level
// const block of the given file.
func packageIntConst(t *testing.T, path, name string) (int64, bool) {
	t.Helper()

	file, err := goparser.ParseFile(token.NewFileSet(), path, nil, goparser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, ident := range vs.Names {
				if ident.Name != name || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.INT {
					t.Fatalf("%s: const %s is not an integer literal", path, name)
				}
				v, err := strconv.ParseInt(lit.Value, 0, 64)
				if err != nil {
					t.Fatalf("%s: parse const %s: %v", path, name, err)
				}
				return v, true
			}
		}
	}
	return 0, false
}
