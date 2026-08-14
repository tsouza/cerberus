// Package dispatchscan derives, from Go source, the arm sets of the
// dispatches PRODUCTION code performs over the shared plan IR.
//
// It is the production-side sibling of internal/sealedscan. sealedscan
// answers "which types are in this sealed set"; dispatchscan answers
// "which of them does this piece of code actually branch on". Both exist
// for the same reason: a hand-maintained restatement of a fact the source
// already determines drifts, and it drifts silently, because the
// restatement's existence implies a check that nobody is running.
//
// Two shapes are derived, one per defect the ratchets built on this
// package police.
//
//   - [Classifiers] finds the bool-returning classifier: a function whose
//     answer about an IR node is computed by a type switch over a sealed
//     interface. Two of those in different packages, under one name, are
//     one question with two answers — the shape #1504 recorded, where
//     internal/api/prom hand-mirrored a 3-kind classifier while
//     internal/chsql covered 5 for the same question and a docstring on
//     each asserted they agreed.
//
//   - [SwitchArms] finds the vocabulary a dispatch accepts: the string
//     cases of an expression switch. A head that gates on the same
//     vocabulary from a hand-written set cannot be diffed against the
//     dispatch any other way, because the dispatch's arms bind to
//     unexported methods and so are not a value any test can read.
//
// It parses declarations only, so it reads a package directory with
// go/parser rather than loading it with golang.org/x/tools/go/packages,
// for the same reason sealedscan does: nothing here needs types, build
// tag resolution or import graphs.
//
// The consequence of parsing rather than type-checking is that an
// interface is identified by its written name — `chplan.Node` at a use
// site, `Node` inside package chplan — rather than by identity. An import
// renamed with an alias therefore reads as a different interface and is
// skipped, which is the conservative direction: the scan under-reports
// rather than binding two unrelated interfaces together because they
// share a short name (`Expr` names a different interface in chplan, in
// lsyntax and in the TraceQL AST).
package dispatchscan

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Classifier is one bool-returning shape classifier: a function that
// answers a yes/no question about an IR node by switching on the node's
// dynamic type.
type Classifier struct {
	// Package is the name of the package declaring the function.
	Package string
	// Func is the function's name.
	Func string
	// Interface is the qualified name of the sealed interface switched
	// over, as written at the switch: "chplan.Node".
	Interface string
	// File is the path of the file declaring the function.
	File string
	// Line is the line of the function declaration.
	Line int
	// Arms holds the sorted type names the switch branches on, as
	// written: ["*chplan.Filter", "*chplan.Project"].
	Arms []string
}

// Classifiers returns every classifier declared in the package rooted at
// dir whose type switch is over one of the interfaces named in sealed —
// a set of qualified names ("chplan.Node"), as [QualifyInterface] builds
// them.
//
// A function qualifies when it returns only bool, takes a parameter typed
// as one of those interfaces, and switches on that parameter's dynamic
// type. Those three together are what make it a restatement of a
// classification rather than an ordinary walk: the answer is a property
// of the node kind alone, so two of them must agree by construction.
//
// A walk that returns a plan, a column set or an error is deliberately
// out of scope even when it switches on the same interface. Those differ
// legitimately between callers — a rewrite rule handles the nodes it
// rewrites — and folding them in would flag most of the optimizer.
func Classifiers(dir string, sealed map[string]bool) ([]Classifier, error) {
	files, pkg, err := parseDir(dir)
	if err != nil {
		return nil, err
	}
	var out []Classifier
	for _, pf := range files {
		for _, decl := range pf.file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil || !returnsOnlyBool(fd.Type) {
				continue
			}
			params := ifaceParams(fd.Type, pkg, sealed)
			if len(params) == 0 {
				continue
			}
			out = append(out, classifiersIn(fd, params, pkg, pf)...)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Line < out[j].Line
	})
	return out, nil
}

// QualifyInterface renders the qualified name Classifiers keys on for an
// interface named iface declared in package pkg.
func QualifyInterface(pkg, iface string) string {
	return pkg + "." + iface
}

// PackageName returns the name of the package rooted at dir, which a
// caller needs to qualify the interfaces declared there. It differs from
// the directory's base name often enough to be worth reading rather than
// assuming.
//
// It returns "" with no error for a directory holding no non-test Go
// source: a caller walking a tree meets those, and they declare nothing
// to qualify.
func PackageName(dir string) (string, error) {
	_, pkg, err := parseDir(dir)
	return pkg, err
}

// classifiersIn collects one Classifier per type switch in fd that
// switches on a parameter of sealed-interface type.
func classifiersIn(fd *ast.FuncDecl, params map[string]string, pkg string, pf parsedFile) []Classifier {
	var out []Classifier
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		sw, ok := n.(*ast.TypeSwitchStmt)
		if !ok {
			return true
		}
		iface, ok := params[typeSwitchSubject(sw)]
		if !ok {
			return true
		}
		arms := typeSwitchArms(sw)
		if len(arms) == 0 {
			return true
		}
		pos := pf.fset.Position(fd.Pos())
		out = append(out, Classifier{
			Package:   pkg,
			Func:      fd.Name.Name,
			Interface: iface,
			File:      pf.path,
			Line:      pos.Line,
			Arms:      arms,
		})
		return true
	})
	return out
}

// SwitchArms returns the sorted, deduplicated string-literal cases of the
// expression switch inside the function named fn in the package rooted at
// dir — the vocabulary that dispatch accepts.
//
// It requires the function to hold exactly one switch whose cases are all
// string literals, and returns an error otherwise. That strictness is the
// vacuity guard: a ratchet that silently derives an empty vocabulary
// passes forever while asserting nothing, so a dispatch reshaped past
// what this recognises must fail loudly and be re-read rather than
// degrade to a no-op.
func SwitchArms(dir, fn string) ([]string, error) {
	files, _, err := parseDir(dir)
	if err != nil {
		return nil, err
	}
	for _, pf := range files {
		for _, decl := range pf.file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil || fd.Name.Name != fn {
				continue
			}
			return stringSwitchArms(fd, dir, fn)
		}
	}
	return nil, fmt.Errorf("dispatchscan: no function %s in %s — the scan lost its grip on "+
		"the source shape, so every ratchet built on it is vacuous", fn, dir)
}

// stringSwitchArms extracts the string cases of fd's single expression
// switch.
func stringSwitchArms(fd *ast.FuncDecl, dir, fn string) ([]string, error) {
	var switches []*ast.SwitchStmt
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if sw, ok := n.(*ast.SwitchStmt); ok {
			switches = append(switches, sw)
		}
		return true
	})
	if len(switches) != 1 {
		return nil, fmt.Errorf("dispatchscan: %s in %s holds %d expression switches, want "+
			"exactly 1 — the dispatch was reshaped, so re-read it rather than trusting a "+
			"vocabulary derived from the wrong one", fn, dir, len(switches))
	}
	seen := map[string]bool{}
	var arms []string
	for _, stmt := range switches[0].Body.List {
		clause, ok := stmt.(*ast.CaseClause)
		if !ok {
			continue
		}
		for _, expr := range clause.List {
			lit, ok := expr.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return nil, fmt.Errorf("dispatchscan: %s in %s switches on a non-string case "+
					"%s — the dispatch is not a vocabulary this scan can derive", fn, dir,
					exprName(expr))
			}
			name, err := strconv.Unquote(lit.Value)
			if err != nil {
				return nil, fmt.Errorf("dispatchscan: %s in %s: unquote case %s: %w",
					fn, dir, lit.Value, err)
			}
			if seen[name] {
				continue
			}
			seen[name] = true
			arms = append(arms, name)
		}
	}
	if len(arms) == 0 {
		return nil, fmt.Errorf("dispatchscan: %s in %s has no string cases — the scan lost "+
			"its grip on the source shape, so every ratchet built on it is vacuous", fn, dir)
	}
	sort.Strings(arms)
	return arms, nil
}

// parsedFile is one parsed non-test source file and where it came from.
type parsedFile struct {
	path string
	file *ast.File
	fset *token.FileSet
}

// parseDir parses the package's non-test sources, returning them with the
// package name. Test files are excluded: a dispatch is a production
// declaration, and a test-only classifier would be reported as a mirror
// of the production one it exists to verify.
func parseDir(dir string) ([]parsedFile, string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, "", fmt.Errorf("dispatchscan: read package directory: %w", err)
	}
	fset := token.NewFileSet()
	var out []parsedFile
	var pkg string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil, "", fmt.Errorf("dispatchscan: parse %s: %w", name, perr)
		}
		pkg = file.Name.Name
		out = append(out, parsedFile{path: path, file: file, fset: fset})
	}
	return out, pkg, nil
}

// returnsOnlyBool reports whether every result of ft is a bare bool. A
// classifier answers yes or no and nothing else: a function also
// returning the node it found is an unwrapper, whose arms legitimately
// differ per caller.
func returnsOnlyBool(ft *ast.FuncType) bool {
	if ft.Results == nil || len(ft.Results.List) == 0 {
		return false
	}
	for _, r := range ft.Results.List {
		id, ok := r.Type.(*ast.Ident)
		if !ok || id.Name != "bool" {
			return false
		}
	}
	return true
}

// ifaceParams maps each parameter name whose declared type is one of the
// sealed interfaces to that interface's qualified name.
func ifaceParams(ft *ast.FuncType, pkg string, sealed map[string]bool) map[string]string {
	if ft.Params == nil {
		return nil
	}
	out := map[string]string{}
	for _, p := range ft.Params.List {
		qualified, ok := qualifiedTypeName(p.Type, pkg)
		if !ok || !sealed[qualified] {
			continue
		}
		for _, name := range p.Names {
			out[name.Name] = qualified
		}
	}
	return out
}

// qualifiedTypeName renders a parameter's type as a qualified name,
// treating a bare identifier as local to pkg.
func qualifiedTypeName(expr ast.Expr, pkg string) (string, bool) {
	switch v := expr.(type) {
	case *ast.Ident:
		return QualifyInterface(pkg, v.Name), true
	case *ast.SelectorExpr:
		id, ok := v.X.(*ast.Ident)
		if !ok {
			return "", false
		}
		return QualifyInterface(id.Name, v.Sel.Name), true
	}
	return "", false
}

// typeSwitchSubject returns the name of the identifier a type switch
// switches on, or "" for a subject this scan does not recognise.
func typeSwitchSubject(sw *ast.TypeSwitchStmt) string {
	var subject ast.Expr
	switch assign := sw.Assign.(type) {
	case *ast.AssignStmt:
		if len(assign.Rhs) == 1 {
			if ta, ok := assign.Rhs[0].(*ast.TypeAssertExpr); ok {
				subject = ta.X
			}
		}
	case *ast.ExprStmt:
		if ta, ok := assign.X.(*ast.TypeAssertExpr); ok {
			subject = ta.X
		}
	}
	if id, ok := subject.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// typeSwitchArms returns the sorted type names a type switch branches on.
// The `nil` case and the default clause carry no type name and are
// skipped: neither says anything about which node kinds the classifier
// covers.
func typeSwitchArms(sw *ast.TypeSwitchStmt) []string {
	seen := map[string]bool{}
	var arms []string
	for _, stmt := range sw.Body.List {
		clause, ok := stmt.(*ast.CaseClause)
		if !ok {
			continue
		}
		for _, expr := range clause.List {
			name := exprName(expr)
			if name == "" || name == "nil" || seen[name] {
				continue
			}
			seen[name] = true
			arms = append(arms, name)
		}
	}
	sort.Strings(arms)
	return arms
}

// exprName renders a type expression as its written name, unwrapping a
// pointer. It returns "" for a shape this scan does not recognise, so the
// caller skips it rather than recording a bogus arm.
func exprName(expr ast.Expr) string {
	switch v := expr.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.StarExpr:
		inner := exprName(v.X)
		if inner == "" {
			return ""
		}
		return "*" + inner
	case *ast.SelectorExpr:
		outer := exprName(v.X)
		if outer == "" {
			return ""
		}
		return outer + "." + v.Sel.Name
	}
	return ""
}
