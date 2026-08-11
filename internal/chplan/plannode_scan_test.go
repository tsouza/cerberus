package chplan_test

import (
	"testing"

	"github.com/tsouza/cerberus/internal/sealedscan"
)

// planNodeMarkerMethod is the sealing marker on chplan.Node. A type
// declaring it IS a plan node — that method declaration is the only
// definition of the Node set there is, which is why the ratchets read it
// out of the source rather than trusting any list.
const planNodeMarkerMethod = "planNode"

// planNodeImplementers returns the sorted names of every type declaring
// the `planNode()` sealing marker — i.e. the live, complete set of
// chplan.Node kinds.
//
// Every ratchet in this package over "the Node set" derives from here. A
// hand-maintained count or list compared against another hand-maintained
// count or list proves nothing: both are edited together by the same
// person in the same commit, so neither can catch the type that was
// added without touching either. Scanning the marker method means adding
// a Node kind trips the ratchets with no number and no list to bump.
//
// The scan itself lives in internal/sealedscan, which does the same job
// for every sealed interface in the tree, so the Node ratchets and the
// Expr ratchets cannot disagree about what "the implementer set" means.
func planNodeImplementers(t *testing.T) []string {
	t.Helper()
	names, err := sealedscan.Implementers(".", planNodeMarkerMethod)
	if err != nil {
		t.Fatalf("derive the Node set: %v", err)
	}
	return names
}
