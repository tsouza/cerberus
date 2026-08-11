package restatednearmiss

import "testing"

// The same value, never a claim about the sealed set.

func TestNotCompared(t *testing.T) {
	// Declared and used, but never against a count.
	const retries = 3
	if retries*2 != 6 {
		t.Fatal("arithmetic")
	}
}

func TestComparedAgainstNonCount(t *testing.T) {
	// A scalar returned by the code under test, not a measurement of a
	// set. This is the lexer-offset shape the rule must not touch.
	got := offset()
	const want = 3
	if got != want {
		t.Fatal("offset")
	}
}

func TestCounterInAnotherFunction(t *testing.T) {
	// `seen` is incremented in a DIFFERENT function, so it must not make
	// this comparison a cardinality claim.
	const want = 3
	if offset() != want {
		t.Fatal("offset")
	}
}

func TestHasACounter(t *testing.T) {
	seen := 0
	for range []int{1} {
		seen++
	}
	if seen != 1 {
		t.Fatal("counter")
	}
}

func TestDifferentCardinality(t *testing.T) {
	all := []Shape{&Square{}}
	const wantOther = 7
	if len(all) != wantOther {
		t.Fatal("not the sealed set's size")
	}
}

func offset() int { return 3 }
