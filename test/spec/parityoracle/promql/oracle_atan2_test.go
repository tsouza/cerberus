package promql

import (
	"math"
	"testing"
)

// TestEqualAtan2Values_IssueValues pins the exact pair from issue #1985: the
// real Prometheus engine (Go math.Atan2) answered 1.3734007669450157 for
// `2 atan2 up{job="api",instance="a"}` where cerberus (ClickHouse's own
// atan2) answered 1.373400766945016. These are adjacent float64 values —
// exactly the 1-ULP libm divergence [EqualAtan2Values] exists to accept —
// and must compare equal under it.
func TestEqualAtan2Values_IssueValues(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		reference float64
		cerberus  float64
	}{
		{"api/a series", 1.3734007669450157, 1.373400766945016},
		{"web/b series", 1.2341215074081693, 1.2341215074081695},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			if dist := ulpDistance(c.reference, c.cerberus); dist != 1 {
				t.Fatalf(
					"ulpDistance(%v, %v) = %d, want 1 — the issue's own evidence claims these are "+
						"adjacent doubles; if this fails, the issue's premise or this math is wrong",
					c.reference, c.cerberus, dist,
				)
			}
			if !EqualAtan2Values(c.reference, c.cerberus) {
				t.Fatalf("EqualAtan2Values(%v, %v) = false, want true (1 ULP apart)", c.reference, c.cerberus)
			}
			// Symmetry: the comparator must not depend on argument order.
			if !EqualAtan2Values(c.cerberus, c.reference) {
				t.Fatalf("EqualAtan2Values is not symmetric for %v, %v", c.cerberus, c.reference)
			}
		})
	}
}

// TestEqualAtan2Values_RejectsTwoULPs constructs a value exactly 2 ULPs away
// from a baseline via two chained math.Nextafter steps, and asserts
// EqualAtan2Values rejects it. Without this, atan2ULPTolerance could silently
// regress into "always true" and no test would notice.
func TestEqualAtan2Values_RejectsTwoULPs(t *testing.T) {
	t.Parallel()

	base := 1.3734007669450157
	oneULP := math.Nextafter(base, math.Inf(1))
	twoULP := math.Nextafter(oneULP, math.Inf(1))

	if dist := ulpDistance(base, oneULP); dist != 1 {
		t.Fatalf("ulpDistance(base, oneULP) = %d, want 1", dist)
	}
	if dist := ulpDistance(base, twoULP); dist != 2 {
		t.Fatalf("ulpDistance(base, twoULP) = %d, want 2", dist)
	}

	if !EqualAtan2Values(base, oneULP) {
		t.Fatalf("EqualAtan2Values(base, oneULP) = false, want true")
	}
	if EqualAtan2Values(base, twoULP) {
		t.Fatalf("EqualAtan2Values(base, twoULP) = true, want false — 2 ULPs exceeds the tolerance")
	}
}

// TestEqualAtan2Values_NaN pins the same NaN==NaN carve-out EqualValues
// documents, and asserts a NaN never spuriously agrees with a real number
// just because ulpDistance is not called on that path.
func TestEqualAtan2Values_NaN(t *testing.T) {
	t.Parallel()

	nan := math.NaN()
	if !EqualAtan2Values(nan, nan) {
		t.Fatalf("EqualAtan2Values(NaN, NaN) = false, want true")
	}
	if EqualAtan2Values(nan, 1.0) || EqualAtan2Values(1.0, nan) {
		t.Fatalf("EqualAtan2Values must not treat NaN as equal to a real number")
	}
}

// TestEqualValues_UnaffectedByAtan2Tolerance is the negative control: an
// ordinary, non-atan2 comparison one ULP apart must still be EXACT under
// EqualValues. This is what makes atan2ULPTolerance a narrow exception
// rather than a change to the package's default equality rule.
func TestEqualValues_UnaffectedByAtan2Tolerance(t *testing.T) {
	t.Parallel()

	base := 1.3734007669450157
	oneULP := math.Nextafter(base, math.Inf(1))

	if EqualValues(base, oneULP) {
		t.Fatalf(
			"EqualValues(base, oneULP) = true, want false — EqualValues must stay exact for every " +
				"fixture that is not atan2; only EqualAtan2Values relaxes it",
		)
	}
}

// TestUlpDistance_Zero asserts equal values (including the a==b fast path
// and the +0/-0 case, which compare equal under Go's ==) have distance 0.
func TestUlpDistance_Zero(t *testing.T) {
	t.Parallel()

	if d := ulpDistance(1.5, 1.5); d != 0 {
		t.Fatalf("ulpDistance(1.5, 1.5) = %d, want 0", d)
	}
	if d := ulpDistance(0.0, math.Copysign(0, -1)); d != 0 {
		t.Fatalf("ulpDistance(+0, -0) = %d, want 0", d)
	}
}

// TestUlpDistance_NegativeRange checks the sign-boundary fold monotonicUint64
// performs: two adjacent negative doubles must report distance 1, the same
// as two adjacent positive ones, and a pair straddling zero must report the
// correct count too.
func TestUlpDistance_NegativeRange(t *testing.T) {
	t.Parallel()

	negBase := -1.0
	negNext := math.Nextafter(negBase, math.Inf(-1)) // more negative, 1 ULP away
	if d := ulpDistance(negBase, negNext); d != 1 {
		t.Fatalf("ulpDistance(-1.0, nextMoreNegative) = %d, want 1", d)
	}

	posBase := 1.0
	posNext := math.Nextafter(posBase, math.Inf(1))
	if d := ulpDistance(posBase, posNext); d != 1 {
		t.Fatalf("ulpDistance(1.0, nextMorePositive) = %d, want 1", d)
	}

	// Straddling zero: the smallest positive subnormal and the smallest
	// (in magnitude) negative subnormal are 2 ULPs apart (one step each
	// side of zero) — verified directly against math.Nextafter's own
	// walk in TestUlpDistance_MatchesNextafterWalk, since +0.0 and -0.0
	// are easy to double-count as two slots instead of one.
	smallestPos := math.Nextafter(0, math.Inf(1))
	smallestNeg := math.Nextafter(math.Copysign(0, -1), math.Inf(-1))
	if d := ulpDistance(smallestPos, smallestNeg); d != 2 {
		t.Fatalf("ulpDistance(smallestPos, smallestNeg) = %d, want 2", d)
	}
}

// TestUlpDistance_MatchesNextafterWalk is the ground-truth check for
// ulpDistance: it walks math.Nextafter one step at a time across the
// positive/negative boundary and asserts ulpDistance agrees with the
// running step count at every single step, not just at a couple of
// hand-picked points. This is what caught the original implementation's
// off-by-one at the +0.0/-0.0 seam during development — a bit-pattern
// formula can look right on a spot check and still be wrong at the one
// place floats behave non-uniformly.
func TestUlpDistance_MatchesNextafterWalk(t *testing.T) {
	t.Parallel()

	const steps = 5000
	const start = -1e-300
	const target = 1e-300 + 1 // any value above the walked range

	x := start
	for i := 1; i <= steps; i++ {
		x = math.Nextafter(x, target)
		if got := ulpDistance(start, x); got != uint64(i) {
			t.Fatalf("after %d Nextafter step(s) from %v, ulpDistance = %d, want %d", i, start, got, i)
		}
	}
}
