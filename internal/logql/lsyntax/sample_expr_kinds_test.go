package lsyntax

import (
	"reflect"
	"testing"

	"github.com/tsouza/cerberus/internal/sealedscan"
)

// sampleExprMarkerMethod is the sealing marker on SampleExpr. A type
// declaring it IS a sample expression — that declaration is the only
// definition of the set there is, so the ratchet below reads it out of
// the source instead of trusting a list that nothing re-derives.
const sampleExprMarkerMethod = "isSampleExpr"

// sampleExprTypeNames returns the concrete type name of the expr field
// on each table row, e.g. "BinOpExpr".
func sampleExprTypeNames(cases []struct {
	name string
	expr SampleExpr
},
) map[string]bool {
	names := make(map[string]bool, len(cases))
	for _, c := range cases {
		names[reflect.TypeOf(c.expr).Elem().Name()] = true
	}
	return names
}

// assertCoversEverySampleExpr is the derive-and-diff ratchet: covered
// must hold exactly the types declaring isSampleExpr() in this package.
// Both directions name the offender — a missing type means the caller's
// table went stale under a new sample expression, an extra one means a
// type was deleted or renamed and the table kept its ghost.
func assertCoversEverySampleExpr(t *testing.T, covered map[string]bool) {
	t.Helper()

	kinds, err := sealedscan.Implementers(".", sampleExprMarkerMethod)
	if err != nil {
		t.Fatalf("derive the SampleExpr set: %v", err)
	}
	remaining := make(map[string]bool, len(covered))
	for name := range covered {
		remaining[name] = true
	}
	for _, kind := range kinds {
		if !covered[kind] {
			t.Errorf("%s implements %s() but the table does not cover it — add a row",
				kind, sampleExprMarkerMethod)
		}
		delete(remaining, kind)
	}
	for kind := range remaining {
		t.Errorf("the table covers %s, which no longer implements %s() — drop the row",
			kind, sampleExprMarkerMethod)
	}
}
