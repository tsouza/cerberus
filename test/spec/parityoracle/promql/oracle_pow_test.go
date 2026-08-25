package promql

import (
	"math"
	"testing"
)

// TestEqualPowValues_IssueValues pins the exact pair from issue #2598: the
// real Prometheus engine (Go math.Pow) answered 125061.4728613401 for the
// `^` (pow) operator over two histogram_quantile-derived floats, where
// cerberus (ClickHouse's own pow()) answered 125061.47286134012. These are
// two ULPs apart — verified directly via ulpDistance, not just eyeballed
// from the issue's own "1 ULP" shorthand, which undercounts this pair — and
// must compare equal under [EqualPowValues].
func TestEqualPowValues_IssueValues(t *testing.T) {
	t.Parallel()

	const reference = 125061.4728613401
	const cerberus = 125061.47286134012

	if dist := ulpDistance(reference, cerberus); dist != powULPTolerance {
		t.Fatalf(
			"ulpDistance(%v, %v) = %d, want %d — the issue's own evidence claims these are "+
				"within tolerance; if this fails, the issue's premise or this math is wrong",
			reference, cerberus, dist, powULPTolerance,
		)
	}
	if !EqualPowValues(reference, cerberus) {
		t.Fatalf("EqualPowValues(%v, %v) = false, want true (%d ULPs apart)", reference, cerberus, powULPTolerance)
	}
	// Symmetry: the comparator must not depend on argument order.
	if !EqualPowValues(cerberus, reference) {
		t.Fatalf("EqualPowValues is not symmetric for %v, %v", cerberus, reference)
	}
}

// TestEqualPowValues_RejectsBeyondTolerance constructs a value one ULP
// beyond powULPTolerance away from a baseline and asserts EqualPowValues
// rejects it. Without this, powULPTolerance could silently regress into
// "always true" and no test would notice.
func TestEqualPowValues_RejectsBeyondTolerance(t *testing.T) {
	t.Parallel()

	base := 125061.4728613401
	v := base
	for range powULPTolerance {
		v = math.Nextafter(v, math.Inf(1))
	}
	if dist := ulpDistance(base, v); dist != powULPTolerance {
		t.Fatalf("ulpDistance(base, v) = %d, want %d", dist, powULPTolerance)
	}
	if !EqualPowValues(base, v) {
		t.Fatalf("EqualPowValues(base, %d ULPs) = false, want true", powULPTolerance)
	}

	v = math.Nextafter(v, math.Inf(1))
	if EqualPowValues(base, v) {
		t.Fatalf(
			"EqualPowValues(base, %d ULPs) = true, want false — exceeds the tolerance",
			powULPTolerance+1,
		)
	}
}

// TestEqualPowValues_NaN pins the same NaN==NaN carve-out EqualValues and
// the other named tolerances make.
func TestEqualPowValues_NaN(t *testing.T) {
	t.Parallel()

	nan := math.NaN()
	if !EqualPowValues(nan, nan) {
		t.Fatalf("EqualPowValues(NaN, NaN) = false, want true")
	}
	if EqualPowValues(nan, 1.0) || EqualPowValues(1.0, nan) {
		t.Fatalf("EqualPowValues must not treat NaN as equal to a real number")
	}
}

// TestEqualValues_UnaffectedByPowTolerance is the negative control: an
// ordinary, non-pow comparison one ULP apart must still be EXACT under
// EqualValues. This is what makes powULPTolerance a narrow exception rather
// than a change to the package's default equality rule.
func TestEqualValues_UnaffectedByPowTolerance(t *testing.T) {
	t.Parallel()

	base := 125061.4728613401
	oneULP := math.Nextafter(base, math.Inf(1))

	if EqualValues(base, oneULP) {
		t.Fatalf(
			"EqualValues(base, oneULP) = true, want false — EqualValues must stay exact for every " +
				"fixture that is not pow; only EqualPowValues relaxes it",
		)
	}
}
