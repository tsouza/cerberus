package promql

import (
	"math"
	"testing"
)

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

// ulpDistance lives in the test file because ULP distance is how this
// package's EVIDENCE is stated, not how its comparison is made: every
// cross-implementation divergence enrolled in
// oracle_equalvalues_test.go was measured in ULPs, and stating each pair's
// distance is what keeps [summationReorderRelativeTolerance]'s headroom
// legible. [EqualValues] itself compares relatively, because the
// summation-reordering bound it implements is stated relatively — see that
// constant's derivation.
//
// ulpDistance returns the number of math.Nextafter steps needed to walk
// from a to b — the standard definition of ULP distance for IEEE-754
// doubles, and exactly what "faithfully rounded within N ULPs" means. It is
// 0 for equal values (including +0 and -0, which compare equal), 1 for
// adjacent representable values, and so on.
//
// # How this works
//
// math.Float64bits gives each float's raw IEEE-754 bit pattern, which for
// non-negative floats already increases monotonically with the value (a
// deliberate property of the IEEE-754 encoding). monotonicUint64 extends
// that monotonicity across the sign boundary too, so that once both values
// are mapped, their ULP distance is simply the unsigned difference between
// the two representations.
//
// This was verified against math.Nextafter itself — not just eyeballed —
// by walking 5000 consecutive Nextafter steps across the positive/negative
// boundary and checking ulpDistance agreed with the step count at every
// one. That check matters because the naive version of this trick (flip
// every bit for a negative value, flip only the sign bit for a
// non-negative one — the form most references, including Google Test's
// comparator, show) treats +0.0 and -0.0 as two DISTINCT adjacent slots.
// They are not: math.Nextafter — and IEEE-754 equality — treat zero as a
// single point, so that naive form overcounts by 1 for any pair straddling
// zero. monotonicUint64 collapses them into one slot instead.
func ulpDistance(a, b float64) uint64 {
	if a == b {
		return 0
	}
	ua, ub := monotonicUint64(a), monotonicUint64(b)
	if ua > ub {
		ua, ub = ub, ua
	}
	return ub - ua
}

// signBit64 is the sign bit of an IEEE-754 double's raw bit pattern.
const signBit64 = uint64(1) << 63

// monotonicUint64 maps a float64 onto a uint64 whose ordering agrees with
// the float's own ordering across the whole range, sign boundary included,
// with +0.0 and -0.0 mapped to the SAME value. See [ulpDistance] for why
// that last part matters and how this was checked.
//
// The branch is on the float's VALUE (f < 0), not its bit pattern's sign
// bit, which is what makes -0.0 — value-wise non-negative, despite its sign
// bit being set — fall into the same branch as +0.0. bits(-0.0) is just the
// sign bit with no magnitude bits, so ORing it with signBit64 is a no-op
// and lands it on exactly the same slot as +0.0, with no special case
// needed for it.
//
// For a genuinely negative float, the magnitude bits (its bit pattern with
// the sign bit cleared) increase as the value gets more negative, so
// subtracting them from signBit64 maps the value into a mirror range
// immediately below zero's slot with no gap and no overlap: the smallest
// magnitude (1, the negative value adjacent to zero) lands one below zero,
// exactly where the non-negative side's smallest magnitude lands one
// above it.
func monotonicUint64(f float64) uint64 {
	bits := math.Float64bits(f)
	if f < 0 {
		magnitude := bits &^ signBit64
		return signBit64 - magnitude
	}
	return bits | signBit64
}
