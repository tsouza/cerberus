package rejectionparity

import (
	"go/ast"
	"go/printer"
	"go/token"
	"sort"
	"strings"
)

// Evidence is the machine-DERIVED reachability evidence for one
// catalogued site: the facts about the current lowering source that a
// class=internal Rationale is a prose claim ABOUT. Nothing in it is
// hand-written — Generate recomputes every field from the go/ast scan
// on every regeneration, which is precisely what makes it evidence
// rather than a second declaration to drift alongside the first.
//
// A rationale says "this site cannot be reached from the wire". Every
// such claim rests on two things and only two things: the conditions
// that must hold for the error to be constructed at all (Guard), and
// how control arrives at the function that constructs it (Callers,
// CallerDispatch). When either moves, the ground under the prose has
// moved with it, and Generate demotes the entry to unclassified so the
// claim has to be re-made by a human rather than silently inherited —
// see reclassifyOnEvidenceDrift.
type Evidence struct {
	// Guard is the chain of conditions, outermost first, that must hold
	// for the error construction to be reached inside its own function:
	// each enclosing `if` (or the `else` arm of one), and each `switch` /
	// type-switch case arm, rendered from the AST with whitespace
	// collapsed so reformatting alone never moves it. An empty chain
	// means the site is constructed unconditionally in its function's
	// body, in which case the whole unreachability claim rests on
	// Callers.
	Guard []string `json:"guard"`

	// Callers is the sorted set of functions declared in the SAME
	// lowering package that call the site's enclosing function. It is
	// the local reaching set: a new entry here is a new way to arrive at
	// the site, which is exactly the change #1738 recorded (a lowering
	// path reached a guard whose rationale claimed an earlier gate) and
	// exactly what a rationale about dispatch order stops being true
	// under.
	Callers []string `json:"callers"`

	// CallerDispatch is the sorted union of the package-declared
	// functions those callers themselves call — the dispatch fabric one
	// hop above the site. Rationales overwhelmingly cite a SIBLING
	// dispatch ("count_values is intercepted by lowerCountValues before
	// buildAggFunc is consulted"): the interception lives in the caller,
	// not in the site's own function, so neither Guard nor Callers moves
	// when it is removed. This field does move, which is what turns
	// "X routes to Y first" from an unchecked assertion into a fact the
	// regeneration re-derives.
	CallerDispatch []string `json:"caller_dispatch"`
}

// Equal reports whether two evidence values were derived from
// equivalent source. A nil receiver equals only another nil.
func (e *Evidence) Equal(o *Evidence) bool {
	if e == nil || o == nil {
		return e == nil && o == nil
	}
	return equalStrings(e.Guard, o.Guard) &&
		equalStrings(e.Callers, o.Callers) &&
		equalStrings(e.CallerDispatch, o.CallerDispatch)
}

// Diff renders the human-readable difference between the stored
// evidence and freshly derived evidence, one line per field that
// moved. It is what the drift gate prints, so the reader learns WHICH
// fact the rationale rested on stopped holding without opening the
// artifact.
func (e *Evidence) Diff(o *Evidence) []string {
	var out []string
	if e == nil || o == nil {
		return []string{"evidence present on one side only — regenerate the catalogue"}
	}
	for _, f := range []struct {
		name       string
		have, want []string
	}{
		{"guard", e.Guard, o.Guard},
		{"callers", e.Callers, o.Callers},
		{"caller_dispatch", e.CallerDispatch, o.CallerDispatch},
	} {
		if equalStrings(f.have, f.want) {
			continue
		}
		out = append(out, f.name+": catalogued ["+strings.Join(f.have, " | ")+
			"], derived from the current source ["+strings.Join(f.want, " | ")+"]")
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// pkgScan is one lowering package parsed once: the error sites it
// contains, and the intra-package call graph the evidence is derived
// from. Both halves come out of the same parse, so the guard chain and
// the reaching set can never describe different revisions of the file.
type pkgScan struct {
	sites []Site
	// evidence is keyed by site key.
	evidence map[string]*Evidence
}

// astScope carries the per-package state guard derivation needs: the
// FileSet the nodes were parsed with, the set of function names the
// package declares, and the name-keyed call graph over them.
type astScope struct {
	fset     *token.FileSet
	declared map[string]bool
	// calls maps a declaring function name to the sorted set of
	// package-declared function names its body calls.
	calls map[string][]string
	// callers is the reverse of calls.
	callers map[string][]string
}

// guardChain renders the conditions that must hold for the node at the
// bottom of stack to execute, outermost first. stack is the ancestor
// chain from the enclosing function body down to (and including) the
// error-construction call.
func guardChain(fset *token.FileSet, stack []ast.Node) []string {
	var out []string
	for i := 0; i+1 < len(stack); i++ {
		// A switch's arms are not its direct children: the ancestor
		// chain runs SwitchStmt -> BlockStmt -> CaseClause, so the arm
		// that actually holds the site is two hops down.
		switch p := stack[i].(type) {
		case *ast.SwitchStmt:
			if cc := caseAt(stack, i+2); cc != nil {
				out = append(out, caseGuard(fset, renderNode(fset, p.Tag), p.Body, cc))
			}
		case *ast.TypeSwitchStmt:
			if cc := caseAt(stack, i+2); cc != nil {
				out = append(out, caseGuard(fset, renderNode(fset, p.Assign), p.Body, cc))
			}
		default:
			out = append(out, guardsFor(fset, stack[i], stack[i+1])...)
		}
	}
	return out
}

// caseAt returns the switch arm at position i of the ancestor chain, or
// nil when the site sits somewhere other than an arm body (a switch tag
// or init statement).
func caseAt(stack []ast.Node, i int) *ast.CaseClause {
	if i >= len(stack) {
		return nil
	}
	cc, _ := stack[i].(*ast.CaseClause)
	return cc
}

// guardsFor renders the conditions parent imposes on child. Loops are
// deliberately not guards: a `for` decides how OFTEN a site runs, never
// whether a query shape can reach it.
func guardsFor(fset *token.FileSet, parent, child ast.Node) []string {
	switch p := parent.(type) {
	case *ast.IfStmt:
		cond := ifText(fset, p)
		if p.Else != nil && sameNode(p.Else, child) {
			return []string{"else of if " + cond}
		}
		return []string{"if " + cond}
	case *ast.FuncLit:
		return []string{"inside func literal"}
	case *ast.BlockStmt:
		return fallThroughGuards(fset, p.List, child)
	case *ast.CaseClause:
		return fallThroughGuards(fset, p.Body, child)
	}
	return nil
}

// caseGuard renders one switch arm: its case expressions (or `default`)
// plus the switch tag they are matched against.
//
// For a `default` arm the arm inventory of the WHOLE switch is
// rendered too. A default arm is reached exactly when no other arm
// matched, so its reachability is a fact about the sibling arms, not
// about itself — and "every op the grammar produces is mapped, so the
// default is unreachable" is the single most common shape of
// class=internal rationale in the catalogue. Without the inventory,
// deleting a `case` would make the default reachable while leaving the
// evidence, and therefore the rationale, untouched.
func caseGuard(fset *token.FileSet, tag string, body *ast.BlockStmt, cc *ast.CaseClause) string {
	arm := armText(fset, cc)
	if tag != "" {
		arm += " of switch " + tag
	} else {
		arm += " of switch"
	}
	if len(cc.List) == 0 {
		arm += " over [" + strings.Join(armInventory(fset, body), "; ") + "]"
	}
	return arm
}

// fallThroughGuards renders the statements that lexically PRECEDE child
// in a statement list and that RETURN — a switch whose every arm
// returns, or an else-less `if` whose body returns. Getting past one of
// those is itself a condition ("none of these matched"), and it is how
// the mapper and unwrap-chain functions in all three lowerings are
// written: a switch of returning cases, or a run of
// `if v, ok := x.(T); ok { return … }`, then a trailing
// `return fmt.Errorf(…)`. The error site carries no lexical guard of
// its own there, so without this the arm inventory and the assertion
// chain those rationales rest on would go unrecorded — deleting a case
// or an assertion would make the site reachable while leaving the
// evidence, and therefore the rationale, untouched.
func fallThroughGuards(fset *token.FileSet, list []ast.Stmt, child ast.Node) []string {
	var out []string
	for _, st := range list {
		if sameNode(st, child) {
			break
		}
		var (
			tag  string
			body *ast.BlockStmt
		)
		switch s := st.(type) {
		case *ast.SwitchStmt:
			tag, body = renderNode(fset, s.Tag), s.Body
		case *ast.TypeSwitchStmt:
			tag, body = renderNode(fset, s.Assign), s.Body
		case *ast.IfStmt:
			if s.Else == nil && endsInReturn(s.Body) {
				out = append(out, "past returning if "+ifText(fset, s))
			}
			continue
		default:
			continue
		}
		if !allArmsReturn(body) {
			continue
		}
		out = append(out, "past exhaustive switch "+tag+" over ["+
			strings.Join(armInventory(fset, body), "; ")+"]")
	}
	return out
}

// ifText renders an `if` head, init statement included. The init is
// load-bearing: the whole type-assertion chain shape
// `if v, ok := x.(T); ok { return … }` puts the discriminating
// expression in the init and leaves `ok` as the condition, so a
// condition-only rendering would make four different assertions look
// like four identical guards.
func ifText(fset *token.FileSet, s *ast.IfStmt) string {
	cond := renderNode(fset, s.Cond)
	if s.Init == nil {
		return cond
	}
	return renderNode(fset, s.Init) + "; " + cond
}

// endsInReturn reports whether a block's last statement is a return.
func endsInReturn(body *ast.BlockStmt) bool {
	if body == nil || len(body.List) == 0 {
		return false
	}
	_, ok := body.List[len(body.List)-1].(*ast.ReturnStmt)
	return ok
}

// allArmsReturn reports whether every arm of a switch body ends in a
// return, which is what makes the statement after the switch reachable
// only when no arm matched.
func allArmsReturn(body *ast.BlockStmt) bool {
	if body == nil || len(body.List) == 0 {
		return false
	}
	for _, st := range body.List {
		cc, ok := st.(*ast.CaseClause)
		if !ok || len(cc.Body) == 0 {
			return false
		}
		if _, isReturn := cc.Body[len(cc.Body)-1].(*ast.ReturnStmt); !isReturn {
			return false
		}
	}
	return true
}

// armInventory renders every arm of a switch body in source order.
func armInventory(fset *token.FileSet, body *ast.BlockStmt) []string {
	if body == nil {
		return nil
	}
	out := make([]string, 0, len(body.List))
	for _, st := range body.List {
		if cc, ok := st.(*ast.CaseClause); ok {
			out = append(out, armText(fset, cc))
		}
	}
	return out
}

// armText renders one arm's selector — its case expressions, or
// `default`.
func armText(fset *token.FileSet, cc *ast.CaseClause) string {
	if len(cc.List) == 0 {
		return "default"
	}
	parts := make([]string, 0, len(cc.List))
	for _, e := range cc.List {
		parts = append(parts, renderNode(fset, e))
	}
	return "case " + strings.Join(parts, ", ")
}

// sameNode reports node identity. ast.Node values are pointers, so
// interface comparison is identity — but an untyped nil child would
// panic-compare, hence the guard.
func sameNode(a, b ast.Node) bool {
	if a == nil || b == nil {
		return false
	}
	return a == b
}

// renderNode prints an AST node back to source with all runs of
// whitespace collapsed, so a condition that gets re-wrapped across
// lines (or re-indented) renders identically and never trips the drift
// gate on formatting alone.
func renderNode(fset *token.FileSet, n ast.Node) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	if err := printer.Fprint(&b, fset, n); err != nil {
		return ""
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// callGraph builds the name-keyed intra-package call graph over the
// files' declared functions. Callee names are matched by identifier —
// a bare `foo(...)` or the selector tail of `x.foo(...)` — and kept
// only when the package declares that name, so the graph never leaves
// the lowering package. Matching by name over-approximates (a method
// call on an imported type whose name collides with a local function
// contributes an edge), which is the safe direction: an extra edge can
// only ever demand a re-verification that was not strictly required,
// never hide one that was.
func callGraph(files []*ast.File) (declared map[string]bool, calls, callers map[string][]string) {
	declared = map[string]bool{}
	for _, f := range files {
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
				declared[fn.Name.Name] = true
			}
		}
	}
	callSets := map[string]map[string]bool{}
	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			set := callSets[fn.Name.Name]
			if set == nil {
				set = map[string]bool{}
				callSets[fn.Name.Name] = set
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if name, ok := calleeName(call.Fun); ok && declared[name] {
					set[name] = true
				}
				return true
			})
		}
	}
	calls = map[string][]string{}
	callers = map[string][]string{}
	for caller, set := range callSets {
		calls[caller] = sortedSet(set)
		for callee := range set {
			callers[callee] = append(callers[callee], caller)
		}
	}
	for callee := range callers {
		sort.Strings(callers[callee])
	}
	return declared, calls, callers
}

// calleeName extracts the identifier a call expression names.
func calleeName(fun ast.Expr) (string, bool) {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name, true
	case *ast.SelectorExpr:
		return f.Sel.Name, true
	case *ast.ParenExpr:
		return calleeName(f.X)
	}
	return "", false
}

// dispatchOf returns the sorted union of everything the given callers
// call — the dispatch fabric one hop above a site.
func dispatchOf(sc *astScope, callers []string) []string {
	union := map[string]bool{}
	for _, c := range callers {
		for _, callee := range sc.calls[c] {
			union[callee] = true
		}
	}
	return sortedSet(union)
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
