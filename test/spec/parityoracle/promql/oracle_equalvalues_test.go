package promql

import (
	"math"
	"testing"
)

// minRealDivergenceHeadroom is how far below a REAL divergence
// [summationReorderRelativeTolerance] is required to sit: a factor of a
// million, checked against every pair in
// [TestEqualValuesRejectsRealDivergence].
//
// It is a ratchet on the tolerance, not a property of any pair. The
// derived tolerance clears it by four further orders of magnitude — the
// smallest real divergence the round-trip lane has produced is 3.03e-2
// relative, some 3.3e10 times the tolerance — so a legitimate future
// widening (a larger sample budget, say) has room to move, while any
// widening large enough to bring the bound within a millionth of a genuine
// disagreement turns this red.
const minRealDivergenceHeadroom = 1e6

// relativeDistance is the quantity [EqualValues] actually bounds:
// |a-b| scaled by the larger operand's magnitude.
func relativeDistance(a, b float64) float64 {
	return math.Abs(a-b) / math.Max(math.Abs(a), math.Abs(b))
}

// TestEqualValuesAcceptsMeasuredDivergence enrols every
// cross-implementation divergence this repository has actually MEASURED
// between the real Prometheus engine and cerberus, and asserts the single
// comparator accepts all of them.
//
// Two things are asserted per pair, not one. That the comparator accepts
// it, symmetrically — and that its relative distance is genuinely inside
// [summationReorderRelativeTolerance] rather than merely landing there.
// The second assertion is what makes the first evidence: a pair accepted
// only because the tolerance is loose would show up here as a relative
// distance near the bound, and every pair below is three to four orders of
// magnitude inside it.
func TestEqualValuesAcceptsMeasuredDivergence(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		reference float64
		cerberus  float64
		wantULPs  uint64
	}{
		// Issue #2909: the native increase() grid path and Prometheus fold
		// the same window's samples in different orders. These four pairs
		// are the reason this comparator stopped being exact.
		{"increase_range_step/logql", 21, 21.000000000000004, 1},
		{"increase_range_step/promql", 42, 42.00000000000001, 1},
		{"increase_vector_agg_temporality_union/210", 210, 210.00000000000003, 1},
		{"increase_vector_agg_temporality_union/245", 245, 245.00000000000003, 1},

		// Issue #1985: binop_atan2_scalar_vector — Go's math.Atan2 against
		// ClickHouse's own libm atan2. IEEE-754 requires each to be
		// correctly rounded for its own algorithm, not to agree with the
		// other bit-for-bit.
		{"atan2/api-a", 1.3734007669450157, 1.373400766945016, 1},
		{"atan2/web-b", 1.2341215074081693, 1.2341215074081695, 1},

		// Issue #2598: exp_histogram_set_op_or_mixed_vector_vector_pow —
		// Go's math.Pow against ClickHouse's pow(), same libm argument.
		{"pow", 125061.4728613401, 125061.47286134012, 2},

		// Issue #2024: native exponential-histogram interpolation, Go math
		// against ClickHouse log2/pow.
		{"exp_hist/histogram_fraction", 0.45575480796967555, 0.45575480796967544, 2},
		{"exp_hist/histogram_fraction_negative_bounds", 0.6226721157729304, 0.6226721157729302, 1},
		{"exp_hist/histogram_fraction_negative_range", 0.1043187546327135, 0.10431875463271348, 2},
		{"exp_hist/quantile_latest_sample", 59.71411145835569, 59.71411145835565, 5},
		{"exp_hist/quantile_bare_offset_range", 59.71411145835569, 59.71411145835565, 5},
		{"exp_hist/quantile_multi_series", 6.727171322029717, 6.727171322029716, 1},
		{"exp_hist/quantile_negative_p50", 3.3635856610148593, 3.363585661014858, 3},

		// histogram_quantile_classic_{agg,bare}_rate_min_samples: the
		// quantile's bucket ladder passes through rate() first, and the two
		// engines derive it in different arithmetic.
		{"classic_rate_quantile/api", 0.29999999999999993, 0.30000000000000004, 2},
		{"classic_rate_quantile/web", 0.06666666666666668, 0.06666666666666667, 1},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			if got := ulpDistance(c.reference, c.cerberus); got != c.wantULPs {
				t.Fatalf(
					"ulpDistance(%v, %v) = %d, want %d — the recorded evidence for this pair "+
						"claims a different distance than the values themselves have",
					c.reference, c.cerberus, got, c.wantULPs,
				)
			}
			if !EqualValues(c.reference, c.cerberus) || !EqualValues(c.cerberus, c.reference) {
				t.Fatalf(
					"EqualValues(%v, %v) rejected a measured %d-ULP divergence",
					c.reference, c.cerberus, c.wantULPs,
				)
			}
			if d := relativeDistance(c.reference, c.cerberus); d >= summationReorderRelativeTolerance {
				t.Fatalf(
					"relative distance %g is not inside the tolerance %g — this pair is accepted "+
						"only at the very edge of the bound, which is not what the derivation claims",
					d, summationReorderRelativeTolerance,
				)
			}
		})
	}
}

// TestEqualValuesRejectsRealDivergence is the negative control that keeps
// [summationReorderRelativeTolerance] from being a rubber stamp.
//
// Every pair below is a REAL disagreement about the answer that the same
// round-trip lane produced — the duplicate-timestamp fixtures of issue
// #2905, whose values differ because the two engines disagree about which
// of two samples at one timestamp exists at all, not about how to round a
// sum. If a future change widens the tolerance far enough to swallow these,
// the comparator has stopped checking anything and this test says so.
//
// It asserts the SEPARATION, not just the verdict, and that distinction is
// what stops the test being a formality. A plain "is it rejected?" check
// passes for any tolerance below ~3e-2 — a bound thirty billion times wider
// than the one derived here would still satisfy it. Requiring
// [minRealDivergenceHeadroom] instead caps how far the tolerance can be
// widened before this test goes red, so the epsilon is bounded in practice
// and not merely on paper.
func TestEqualValuesRejectsRealDivergence(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		reference float64
		cerberus  float64
	}{
		// sorted_slab_avg_over_time_range_step: the reference averaged one
		// sample per timestamp, cerberus averaged both.
		{"avg_over_time", 2.6666666666666665, 2.75},

		// sorted_slab_sum_over_time_range_step: a constant +3 absolute,
		// across the fixture's range of values.
		{"sum_over_time/low", 8, 11},
		{"sum_over_time/high", 22, 25},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			d := relativeDistance(c.reference, c.cerberus)
			if d/summationReorderRelativeTolerance < minRealDivergenceHeadroom {
				t.Fatalf(
					"relative distance %g is only %.0fx the tolerance %g, below the %.0fx headroom "+
						"this comparator is required to keep — the tolerance has been widened toward "+
						"a divergence that is a real disagreement about the answer",
					d, d/summationReorderRelativeTolerance, summationReorderRelativeTolerance,
					float64(minRealDivergenceHeadroom),
				)
			}
			if EqualValues(c.reference, c.cerberus) || EqualValues(c.cerberus, c.reference) {
				t.Fatalf(
					"EqualValues(%v, %v) = true, want false — %g relative apart, %.0fx the tolerance",
					c.reference, c.cerberus, d, d/summationReorderRelativeTolerance,
				)
			}
		})
	}
}

// TestEqualValuesToleranceBoundary walks the comparator across its own
// bound from both sides, so the bound cannot silently widen or vanish.
//
// The probes are built from the tolerance itself rather than from literal
// values: at a magnitude of 1.0 one ULP is ~2.2e-16 while the tolerance is
// ~9.1e-13 — some four thousand ULPs — so a 5% step either side of it is
// representable many times over and neither verdict is a rounding artefact.
func TestEqualValuesToleranceBoundary(t *testing.T) {
	t.Parallel()

	// probeMargin is how far inside and outside the bound the two probes
	// sit, as a fraction of the bound.
	const probeMargin = 0.05

	for _, base := range []float64{1.0, -1.0, 1e-9, 1e9} {
		inside := base * (1 + math.Copysign(summationReorderRelativeTolerance*(1-probeMargin), base))
		outside := base * (1 + math.Copysign(summationReorderRelativeTolerance*(1+probeMargin), base))

		if !EqualValues(base, inside) {
			t.Errorf(
				"EqualValues(%v, %v) = false, want true — %g relative apart, inside the %g tolerance",
				base, inside, relativeDistance(base, inside), summationReorderRelativeTolerance,
			)
		}
		if EqualValues(base, outside) {
			t.Errorf(
				"EqualValues(%v, %v) = true, want false — %g relative apart, outside the %g tolerance",
				base, outside, relativeDistance(base, outside), summationReorderRelativeTolerance,
			)
		}
	}
}

// TestEqualValuesHasNoAbsoluteFloor pins the deliberate choice documented
// on [summationReorderRelativeTolerance]: the comparison is relative only,
// so at zero it degenerates to exact equality rather than to a blanket
// absolute epsilon. Zero against the smallest representable positive value
// is a rejection, not a rounding allowance.
func TestEqualValuesHasNoAbsoluteFloor(t *testing.T) {
	t.Parallel()

	smallestSubnormal := math.Nextafter(0, math.Inf(1))

	if EqualValues(0, smallestSubnormal) {
		t.Errorf("EqualValues(0, %v) = true, want false — a relative bound has no floor at zero", smallestSubnormal)
	}
	if !EqualValues(0, 0) {
		t.Error("EqualValues(0, 0) = false, want true")
	}
	if !EqualValues(0, math.Copysign(0, -1)) {
		t.Error("EqualValues(+0, -0) = false, want true — IEEE-754 treats zero as a single point")
	}
}

// TestEqualValuesNaNAndInfinity pins the two shapes the relative
// comparison cannot express. NaN==NaN is TRUE because PromQL produces NaN
// as a legitimate answer; every other NaN or infinity pairing is a
// disagreement about the KIND of answer, which no tolerance can bridge.
func TestEqualValuesNaNAndInfinity(t *testing.T) {
	t.Parallel()

	nan, posInf, negInf := math.NaN(), math.Inf(1), math.Inf(-1)

	if !EqualValues(nan, nan) {
		t.Error("EqualValues(NaN, NaN) = false, want true")
	}
	if EqualValues(nan, 1) || EqualValues(1, nan) {
		t.Error("a NaN and a real value must not compare equal")
	}
	if !EqualValues(posInf, posInf) || !EqualValues(negInf, negInf) {
		t.Error("an infinity must compare equal to itself")
	}
	if EqualValues(posInf, negInf) || EqualValues(negInf, posInf) {
		t.Error("+Inf and -Inf must not compare equal")
	}
	if EqualValues(posInf, math.MaxFloat64) || EqualValues(math.MaxFloat64, posInf) {
		t.Error("an infinity and a finite value must not compare equal at any tolerance")
	}
	// The largest finite magnitudes of opposite sign overflow a-b to +Inf.
	// That must read as "far apart", not as an accidental acceptance.
	if EqualValues(math.MaxFloat64, -math.MaxFloat64) {
		t.Error("EqualValues(MaxFloat64, -MaxFloat64) = true, want false")
	}
}

// TestSummationReorderToleranceMatchesItsDerivation checks the constant is
// still the expression its doc comment derives — 2(n-1)u at the stated
// sample budget — so a hand-edit to the number alone, uncoupled from the
// arithmetic that justifies it, fails here rather than passing quietly.
func TestSummationReorderToleranceMatchesItsDerivation(t *testing.T) {
	t.Parallel()

	u := 1.0 / math.Pow(2, 53)
	if got := float64(float64UnitRoundoff); got != u {
		t.Fatalf("float64UnitRoundoff = %g, want 2^-53 = %g", got, u)
	}

	want := 2 * (float64(maxReorderedSamplesPerOutputValue) - 1) * u
	if got := float64(summationReorderRelativeTolerance); got != want {
		t.Fatalf(
			"summationReorderRelativeTolerance = %g, want 2(n-1)u = %g for n = %d",
			got, want, maxReorderedSamplesPerOutputValue,
		)
	}
}
