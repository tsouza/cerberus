package regression

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// optimizerBatchRow is one batch of the default optimizer pipeline — its
// registered name, its Strategy constructor, and its rule type names in
// declaration order — as read from either `optimizer.Default()`'s source
// or the pipeline table in docs/engine.md.
type optimizerBatchRow struct {
	Name     string
	Strategy string
	Rules    []string
}

func (r optimizerBatchRow) String() string {
	return fmt.Sprintf("%s [%s]: %s", r.Name, r.Strategy, strings.Join(r.Rules, ", "))
}

// engineDocPipelineTableHeader is the first cell of the header row of the
// pipeline table in docs/engine.md. The table is located by this cell, so
// prose around it can move freely.
const engineDocPipelineTableHeader = "| Batch "

// engineDocBacktickRE extracts every backticked identifier in a table cell.
var engineDocBacktickRE = regexp.MustCompile("`([^`]+)`")

// TestEngineDocOptimizerPipelineMatchesDefault pins the batch table in
// docs/engine.md to `optimizer.Default()`: same batches, same order, same
// Strategy per batch, same rule types in the same order. The table is the
// one place a reader learns which rules run and in which batch, so a rule
// added, moved, or renamed in Default() without a matching doc edit fails
// here rather than leaving the documented pipeline describing a driver
// that no longer exists.
//
// Default()'s batch list is read from source with go/ast rather than
// through the Driver, whose batch slice is unexported: the doc describes
// the source shape (batch constructor + rule type names), so the source is
// the honest comparand.
func TestEngineDocOptimizerPipelineMatchesDefault(t *testing.T) {
	root := repoRootForParity(t)
	fromSource := optimizerDefaultBatchesFromSource(t, filepath.Join(root, "internal", "optimizer", "rule.go"))
	fromDoc := optimizerBatchesFromEngineDoc(t, filepath.Join(root, "docs", "engine.md"))

	if len(fromSource) == 0 {
		t.Fatal("optimizer.Default(): no batches parsed from source")
	}
	if len(fromDoc) != len(fromSource) {
		t.Fatalf("docs/engine.md pipeline table lists %d batches; optimizer.Default() registers %d\n doc: %v\n src: %v",
			len(fromDoc), len(fromSource), fromDoc, fromSource)
	}
	for i := range fromSource {
		want, got := fromSource[i], fromDoc[i]
		if want.Name != got.Name || want.Strategy != got.Strategy || strings.Join(want.Rules, ",") != strings.Join(got.Rules, ",") {
			t.Errorf("batch %d differs between docs/engine.md and optimizer.Default():\n doc: %s\n src: %s", i, got, want)
		}
	}
}

// optimizerDefaultBatchesFromSource walks `func Default()` in rule.go and
// returns each argument of its NewWithBatches(...) call as a batch row.
// Two constructor shapes are recognised — `AnalyzerBatch("name", R1{}, …)`
// and `Batch{Name: "name", Strategy: S(...), Rules: []Rule{R1{}, R2(), …}}`
// — which are the two shapes Default() is written in; any other argument
// shape fails the test so a new constructor is added here deliberately.
func optimizerDefaultBatchesFromSource(t *testing.T, path string) []optimizerBatchRow {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var call *ast.CallExpr
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "Default" || fn.Recv != nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if call != nil {
				return false
			}
			c, ok := n.(*ast.CallExpr)
			if ok {
				if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "NewWithBatches" {
					call = c
					return false
				}
			}
			return true
		})
	}
	if call == nil {
		t.Fatalf("%s: no NewWithBatches(...) call inside func Default()", path)
	}
	var rows []optimizerBatchRow
	for _, arg := range call.Args {
		switch a := arg.(type) {
		case *ast.CallExpr:
			id, ok := a.Fun.(*ast.Ident)
			if !ok || id.Name != "AnalyzerBatch" || len(a.Args) < 1 {
				t.Fatalf("%s: unrecognised batch constructor at %s", path, fset.Position(a.Pos()))
			}
			row := optimizerBatchRow{Name: stringLit(t, fset, a.Args[0]), Strategy: "Analyzer"}
			for _, r := range a.Args[1:] {
				row.Rules = append(row.Rules, ruleTypeName(t, fset, r))
			}
			rows = append(rows, row)
		case *ast.CompositeLit:
			row := optimizerBatchRow{}
			for _, elt := range a.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					t.Fatalf("%s: positional Batch literal at %s", path, fset.Position(elt.Pos()))
				}
				switch kv.Key.(*ast.Ident).Name {
				case "Name":
					row.Name = stringLit(t, fset, kv.Value)
				case "Strategy":
					sc, ok := kv.Value.(*ast.CallExpr)
					if !ok {
						t.Fatalf("%s: Strategy is not a constructor call at %s", path, fset.Position(kv.Value.Pos()))
					}
					row.Strategy = sc.Fun.(*ast.Ident).Name
				case "Rules":
					lit, ok := kv.Value.(*ast.CompositeLit)
					if !ok {
						t.Fatalf("%s: Rules is not a slice literal at %s", path, fset.Position(kv.Value.Pos()))
					}
					for _, r := range lit.Elts {
						row.Rules = append(row.Rules, ruleTypeName(t, fset, r))
					}
				}
			}
			rows = append(rows, row)
		default:
			t.Fatalf("%s: unrecognised NewWithBatches argument at %s", path, fset.Position(arg.Pos()))
		}
	}
	return rows
}

func stringLit(t *testing.T, fset *token.FileSet, e ast.Expr) string {
	t.Helper()
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		t.Fatalf("expected a string literal at %s", fset.Position(e.Pos()))
	}
	return strings.Trim(lit.Value, "\"`")
}

// ruleTypeName returns the rule's type name for either registration
// spelling Default() uses: a zero-value composite literal (`FilterFusion{}`)
// or a constructor call (`FilterAggregateTranspose()`).
func ruleTypeName(t *testing.T, fset *token.FileSet, e ast.Expr) string {
	t.Helper()
	switch r := e.(type) {
	case *ast.CompositeLit:
		if id, ok := r.Type.(*ast.Ident); ok {
			return id.Name
		}
	case *ast.CallExpr:
		if id, ok := r.Fun.(*ast.Ident); ok {
			return id.Name
		}
	}
	t.Fatalf("unrecognised rule registration at %s", fset.Position(e.Pos()))
	return ""
}

// optimizerBatchesFromEngineDoc reads the pipeline table in docs/engine.md:
// the table whose header row starts with engineDocPipelineTableHeader, each
// body row being `| <batch> | <Strategy> | <rules> | <what it buys> |` with
// the batch name and every rule name backticked.
func optimizerBatchesFromEngineDoc(t *testing.T, path string) []optimizerBatchRow {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(string(raw), "\n")
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(l, engineDocPipelineTableHeader) {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("%s: no table with a header row starting %q", path, engineDocPipelineTableHeader)
	}
	var rows []optimizerBatchRow
	// start+1 is the separator row; body rows follow until the first
	// non-table line.
	for _, l := range lines[start+2:] {
		if !strings.HasPrefix(l, "|") {
			break
		}
		cells := strings.Split(strings.Trim(l, "|"), "|")
		if len(cells) < 3 {
			t.Fatalf("%s: pipeline table row has %d cells: %q", path, len(cells), l)
		}
		name := engineDocBacktickRE.FindStringSubmatch(cells[0])
		if name == nil {
			t.Fatalf("%s: pipeline table row has no backticked batch name: %q", path, l)
		}
		row := optimizerBatchRow{Name: name[1], Strategy: strings.TrimSpace(cells[1])}
		for _, m := range engineDocBacktickRE.FindAllStringSubmatch(cells[2], -1) {
			row.Rules = append(row.Rules, m[1])
		}
		rows = append(rows, row)
	}
	return rows
}
