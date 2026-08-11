package enumerated

import "testing"

// A complete enumeration with no derivation behind it: the whole set,
// hand-listed, with nothing re-deriving it.

func TestEveryShape(t *testing.T) {
	all := []Shape{&Square{}, &Circle{}, &Triangle{}} // WANT
	for _, s := range all {
		_ = s
	}
}

// A deliberate subset, which must stay unflagged: choosing two of the
// three is a policy, not a mirror of the set.
func TestSomeShapes(t *testing.T) {
	some := []Shape{&Square{}, &Circle{}}
	for _, s := range some {
		_ = s
	}
}
