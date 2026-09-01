package chplan

import (
	"reflect"
	"testing"

	"github.com/tsouza/cerberus/internal/sealedscan"
)

// The sealing markers on this package's two IR interfaces. A type joins
// chplan.Node by declaring planNode() and chplan.Expr by declaring
// exprNode(), and by nothing else — so those declarations, read out of
// the source, ARE the two sets. Every exhaustiveness ratchet in this
// package derives from them rather than restating them, because a
// hand-maintained count compared against a hand-maintained list is two
// copies of one claim and catches nothing: both get edited in the same
// commit, or in neither.
const (
	nodeMarkerMethod = "planNode"
	exprMarkerMethod = "exprNode"
)

// allExprKinds is one zero-value instance of every concrete Expr type.
//
// Zero values are all the Expr ratchets need: they assert that each
// concrete type is *reached* by a traversal or *handled* by a clone
// switch, neither of which reads the children. Structural equality over
// populated Exprs is a different contract, covered by the Node clone
// tests.
//
// The list is hand-written because the tests need instances, which no
// scan can produce — but it is never trusted. Its completeness is
// asserted against the exprNode() implementer set on every run, so a new
// Expr type fails here by name with nothing to bump.
func allExprKinds() []Expr {
	return []Expr{
		&ColumnRef{},
		&LitString{},
		&InlineString{},
		&LitInt{},
		&LitFloat{},
		&LitBool{},
		&BareIdent{},
		&Binary{},
		&FuncCall{},
		&InList{},
		&FieldAccess{},
		&MapAccess{},
		&Subscript{},
		&LineContent{},
		&LabelJoin{},
		&LabelReplace{},
		&Lambda{},
		&MapWithoutKeys{},
		&MapWithoutEmptyValues{},
		&NestedArrayExists{},
		&ScalarSubquery{},
		&BoundedTraceScope{},
		&InSubquery{},
		&WindowExpr{},
	}
}

// sealedKindNames returns the concrete type name of each value, e.g.
// "ColumnRef".
func sealedKindNames[T any](values []T) map[string]bool {
	names := make(map[string]bool, len(values))
	for _, v := range values {
		names[reflect.TypeOf(v).Elem().Name()] = true
	}
	return names
}

// assertCoversEverySealedKind is the derive-and-diff ratchet shared by
// this package's internal tests: the KEYS of covered must be exactly the
// set of types declaring marker in this package's source. Both
// directions fail loudly and name the offending type — a missing one
// means the caller's table went stale under a new IR type, an extra one
// means a type was deleted or renamed and the table kept its ghost.
func assertCoversEverySealedKind(t *testing.T, marker string, covered map[string]bool, what, remedy string) {
	t.Helper()

	kinds, err := sealedscan.Implementers(".", marker)
	if err != nil {
		t.Fatalf("derive the %s() set: %v", marker, err)
	}
	remaining := make(map[string]bool, len(covered))
	for name := range covered {
		remaining[name] = true
	}
	for _, kind := range kinds {
		if !covered[kind] {
			t.Errorf("%s implements %s() but %s does not cover it — %s",
				kind, marker, what, remedy)
		}
		delete(remaining, kind)
	}
	for kind := range remaining {
		t.Errorf("%s covers %s, which no longer implements %s() — drop it",
			what, kind, marker)
	}
}
