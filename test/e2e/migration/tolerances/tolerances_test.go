package tolerances_test

import (
	"strings"
	"testing"

	"github.com/tsouza/cerberus/test/e2e/migration/seed"
	"github.com/tsouza/cerberus/test/e2e/migration/tolerances"
)

// ratchetCase is one declared band under the two checks docs/migration-testing.md
// section 6.2 makes mechanical.
type ratchetCase struct {
	name string
	band tolerances.Band
	// derived recomputes the band's Value from its named derivation inputs.
	// It is spelled out here, independently of the package, so a Value
	// edited to a bare literal no longer equals it.
	derived float64
	// ceiling is the shrink-only pin: the Value this test last reviewed.
	// Lowering a band passes without touching this file; raising one fails
	// until the ceiling is raised here alongside the derivation change,
	// which is the explicit reviewed override the registry demands.
	ceiling float64
}

// ratchetCases lists every band the package declares. A band missing from
// this table has no ratchet, so adding one to the package means adding its
// row here.
var ratchetCases = []ratchetCase{
	{
		name:    "ExpHistogramQuantile",
		band:    tolerances.ExpHistogramQuantile,
		derived: tolerances.ExpHistogramQuantileMeasuredDiff * tolerances.ExpHistogramQuantileHeadroom,
		ceiling: 1.1601498976197604 * 1.7,
	},
	{
		name: "MIG20Downsample",
		band: tolerances.MIG20Downsample,
		derived: (float64(tolerances.MIG20DownsampleBucket) / float64(tolerances.MIG20VerifyWindow)) *
			(float64(seed.CounterMaxIncrement-1) / (float64(seed.CounterMaxIncrement-1) / 2)),
		ceiling: 0.2,
	},
}

// TestEveryBandRecomputesFromItsDerivation is the bare-edit ratchet: a
// Value that no longer equals the formula over its own named inputs was
// edited as a number rather than as a derivation.
func TestEveryBandRecomputesFromItsDerivation(t *testing.T) {
	for _, c := range ratchetCases {
		if c.band.Value != c.derived {
			t.Errorf("%s: Value %v does not recompute from its derivation inputs (%v) — edit the named inputs, never the value",
				c.name, c.band.Value, c.derived)
		}
	}
}

// TestNoBandExceedsItsReviewedCeiling is the shrink-only ratchet: a band may
// come down freely, but going up needs the ceiling in ratchetCases raised in
// the same change.
func TestNoBandExceedsItsReviewedCeiling(t *testing.T) {
	for _, c := range ratchetCases {
		if c.band.Value > c.ceiling {
			t.Errorf("%s: Value %v exceeds the reviewed ceiling %v — the registry is shrink-only; raising a band means raising its ceiling in this test alongside the derivation",
				c.name, c.band.Value, c.ceiling)
		}
	}
}

// TestEveryBandIsPositiveAndDerived rejects the two degenerate bands: a
// non-positive Value (which no comparator can satisfy or which accepts
// everything, depending on sign) and an empty Derivation.
func TestEveryBandIsPositiveAndDerived(t *testing.T) {
	for _, c := range ratchetCases {
		if c.band.Value <= 0 {
			t.Errorf("%s: Value %v is not positive", c.name, c.band.Value)
		}
		if strings.TrimSpace(c.band.Derivation) == "" {
			t.Errorf("%s: Derivation is empty — every band carries the math it comes from", c.name)
		}
	}
}
