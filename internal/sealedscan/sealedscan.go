// Package sealedscan derives, from Go source, the implementer set of a
// sealed marker interface.
//
// A sealed interface in this repository is closed by an unexported,
// niladic marker method with an empty body — `planNode()` on
// chplan.Node, `exprNode()` on chplan.Expr, `isSampleExpr()` on
// lsyntax.SampleExpr. The method declarations ARE the definition of the
// set: there is no registry to consult and no list that could be
// authoritative, because a type joins the set by declaring the method
// and by nothing else.
//
// This package exists so that every ratchet over such a set reads the
// same derived answer. A hand-maintained count or list compared against
// another hand-maintained count or list proves nothing — both are edited
// together, by the same author, in the same commit, so neither can catch
// the type that was added without touching either. Scanning for the
// marker method means adding an implementer trips every ratchet built on
// it with no number to bump and no list to extend.
//
// It parses declarations only, so it reads a package directory with
// go/parser rather than loading it with golang.org/x/tools/go/packages:
// nothing here needs types, build-tag resolution or import graphs, which
// is all the heavier loader would buy.
//
// Two consequences of parsing rather than type-checking, neither of
// which any package in this tree runs into today. Markers are keyed by
// method name alone, so two interfaces in one package sealed by the same
// unexported niladic method would be reported as one. And the scan is
// per-directory, so an interface whose implementers live in a different
// package than its declaration reports no marker at all.
package sealedscan

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// minSealedSetSize is the smallest implementer count Markers reports.
// One implementer is not a set: nothing can enumerate it incorrectly,
// and any literal mentioning that single type would trivially "cover"
// it.
const minSealedSetSize = 2

// Marker is one sealed interface found in a package: the unexported
// niladic method that seals it, the interface type that declares the
// method, and the sorted names of the types implementing it.
type Marker struct {
	// Method is the sealing method name, e.g. "planNode".
	Method string
	// Interface is the interface type declaring Method, e.g. "Node".
	Interface string
	// Implementers holds the sorted bare type names declaring Method,
	// pointer receivers unwrapped, e.g. ["Aggregate", "Filter", …].
	Implementers []string
}

// Markers returns every sealed marker interface declared in the package
// rooted at dir, sorted by method name. Test files are ignored: a marker
// is a production declaration, and a test-only implementer would inflate
// the set a ratchet is meant to pin.
//
// Only interfaces with at least minSealedSetSize implementers are
// reported. Below that there is no set to keep in step: an interface
// with a single implementer cannot be enumerated wrongly, and an
// unexported niladic method with an empty body on a lone type is far
// more likely to be an ordinary no-op — a decoder whose close() has
// nothing to release — than a sealing marker.
func Markers(dir string) ([]Marker, error) {
	sealed, impls, err := scan(dir)
	if err != nil {
		return nil, err
	}
	out := make([]Marker, 0, len(sealed))
	for method, iface := range sealed {
		names := impls[method]
		if len(names) < minSealedSetSize {
			continue
		}
		sort.Strings(names)
		out = append(out, Marker{Method: method, Interface: iface, Implementers: names})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Method < out[j].Method })
	return out, nil
}

// Implementers returns the sorted type names declaring the sealing
// method in the package rooted at dir.
//
// It returns an error when the scan finds none. That is the vacuity
// guard every caller depends on: a ratchet that silently derives an
// empty set passes forever while asserting nothing, so losing the grip
// on the source shape must fail loudly rather than degrade to a
// no-op.
func Implementers(dir, method string) ([]string, error) {
	_, impls, err := scan(dir)
	if err != nil {
		return nil, err
	}
	names := impls[method]
	if len(names) == 0 {
		return nil, fmt.Errorf(
			"sealedscan: scanned %s and found no %s() declarations — the scan lost its "+
				"grip on the source shape, so every ratchet built on it is vacuous", dir, method,
		)
	}
	sort.Strings(names)
	return names, nil
}

// scan parses the package's non-test sources once, returning the sealing
// methods it declares (method name -> interface name) and the types
// declaring each (method name -> receiver type names).
func scan(dir string) (sealed map[string]string, impls map[string][]string, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("sealedscan: read package directory: %w", err)
	}
	sealed = map[string]string{}
	impls = map[string][]string{}
	embedders := map[string][]string{}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if perr != nil {
			return nil, nil, fmt.Errorf("sealedscan: parse %s: %w", name, perr)
		}
		collectFile(file, sealed, impls, embedders)
	}
	for method, names := range impls {
		impls[method] = resolveEmbeddedBases(names, embedders)
	}
	return sealed, impls, nil
}

func collectFile(file *ast.File, sealed map[string]string, impls, embedders map[string][]string) {
	ast.Inspect(file, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.TypeSpec:
			collectInterface(v, sealed)
			collectEmbedders(v, embedders)
		case *ast.FuncDecl:
			if method, recv, ok := markerDecl(v); ok {
				impls[method] = append(impls[method], recv)
			}
		}
		return true
	})
}

// collectEmbedders records, for each struct type, the named types it
// embeds — `base -> [types embedding base]`.
func collectEmbedders(ts *ast.TypeSpec, embedders map[string][]string) {
	st, ok := ts.Type.(*ast.StructType)
	if !ok || st.Fields == nil {
		return
	}
	for _, f := range st.Fields.List {
		if len(f.Names) != 0 {
			continue // a named field is not an embedding
		}
		if base := receiverOrEmbeddedName(f.Type); base != "" {
			embedders[base] = append(embedders[base], ts.Name.Name)
		}
	}
}

// resolveEmbeddedBases rewrites a marker's receiver list so that a type
// which exists only to SUPPLY the marker to others is replaced by the
// types that embed it.
//
// Go promotes an embedded type's methods, so a package can seal an
// interface by declaring the marker once on a shared base struct and
// embedding that base — internal/logql/lsyntax does exactly this with
// stageBase. Counting the declaration alone would derive a set of one,
// which is not merely incomplete but confidently wrong: the vacuity
// guard cannot fire on a non-empty set, so a ratchet would diff a real
// table against a fictional set and report the whole table as extra.
//
// The base itself drops out. It carries the method but is not a member:
// nothing constructs a bare stageBase as a StageExpr, and admitting it
// would make every ratchet over the set demand a row for it.
func resolveEmbeddedBases(names []string, embedders map[string][]string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		if into := embedders[name]; len(into) > 0 {
			out = append(out, into...)
			continue
		}
		out = append(out, name)
	}
	return dedupe(out)
}

func dedupe(names []string) []string {
	seen := make(map[string]bool, len(names))
	out := make([]string, 0, len(names))
	for _, n := range names {
		if seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// collectInterface records ts as a sealed interface when it declares an
// unexported niladic method — the sealing shape.
func collectInterface(ts *ast.TypeSpec, sealed map[string]string) {
	it, ok := ts.Type.(*ast.InterfaceType)
	if !ok || it.Methods == nil {
		return
	}
	for _, m := range it.Methods.List {
		ft, ok := m.Type.(*ast.FuncType)
		if !ok || len(m.Names) != 1 {
			continue
		}
		name := m.Names[0].Name
		if ast.IsExported(name) || !niladic(ft) {
			continue
		}
		sealed[name] = ts.Name.Name
	}
}

// markerDecl reports whether fd is a sealing marker implementation —
// `func (*T) marker() {}` — and returns the method and bare receiver
// type names. The empty body is part of the shape: a marker method
// carries no behaviour, so an unexported niladic method that actually
// does something (a lifecycle `close()`, say) is not one.
func markerDecl(fd *ast.FuncDecl) (method, recv string, ok bool) {
	if fd.Recv == nil || fd.Body == nil || len(fd.Body.List) != 0 {
		return "", "", false
	}
	if ast.IsExported(fd.Name.Name) || !niladic(fd.Type) {
		return "", "", false
	}
	recv = receiverName(fd.Recv)
	if recv == "" {
		return "", "", false
	}
	return fd.Name.Name, recv, true
}

func niladic(ft *ast.FuncType) bool {
	return ft.Params.NumFields() == 0 && ft.Results.NumFields() == 0
}

// receiverName returns the bare type name of a method receiver,
// unwrapping the pointer a marker declaration uses (`func (*Scan)
// planNode() {}`). It returns "" for a shape the scan does not
// recognise, so the caller skips it rather than recording a bogus name.
func receiverName(recv *ast.FieldList) string {
	if len(recv.List) != 1 {
		return ""
	}
	return receiverOrEmbeddedName(recv.List[0].Type)
}

// receiverOrEmbeddedName reduces a receiver or embedded-field type
// expression to its bare name, unwrapping a pointer. Generic types are
// unwrapped to their base name too, so `Box[T]` reads as `Box`.
func receiverOrEmbeddedName(expr ast.Expr) string {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if idx, ok := expr.(*ast.IndexExpr); ok {
		expr = idx.X
	}
	ident, ok := expr.(*ast.Ident)
	if !ok {
		return ""
	}
	return ident.Name
}
