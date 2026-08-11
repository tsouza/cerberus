package derived

import (
	"testing"

	"github.com/tsouza/cerberus/internal/sealedscan"
)

// The same complete enumeration, in a package that derives the marker.
// The derivation diffs the list on every run, so the rule stands aside.

func TestEveryShape(t *testing.T) {
	all := []Shape{&Square{}, &Circle{}, &Triangle{}}
	kinds, err := sealedscan.Implementers(".", "shapeNode")
	if err != nil {
		t.Fatal(err)
	}
	if len(kinds) != len(all) {
		t.Fatal("the list fell behind the sealed set")
	}
}
