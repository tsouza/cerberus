package steps

import (
	"bytes"
	"math"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/test/e2e/migration/tolerances"
)

// TestHeadroomLineReportsObservedOverDeclared pins the one line every run
// prints per band: story, archetype, subject, both numbers and their ratio,
// so the lane's log carries what the shrink-only ratchet needs.
func TestHeadroomLineReportsObservedOverDeclared(t *testing.T) {
	r := headroomReport{
		Story: "MIG-20", Archetype: "three-signal", Subject: "downsample relative delta",
		Observed: 0.05, Band: tolerances.Band{Value: 0.2, Derivation: "test"},
	}
	if got, want := r.Headroom(), 0.25; got != want {
		t.Fatalf("Headroom() = %v, want %v", got, want)
	}
	var buf bytes.Buffer
	if err := writeHeadroom(&buf, r); err != nil {
		t.Fatalf("writeHeadroom: %v", err)
	}
	got := buf.String()
	for _, want := range []string{"MIG-20 three-signal headroom:", "downsample relative delta", "observed 0.05", "declared 0.2", "= 0.250 of the band"} {
		if !strings.Contains(got, want) {
			t.Errorf("headroom line %q lacks %q", got, want)
		}
	}
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("headroom line %q is not newline-terminated", got)
	}
}

// TestHeadroomLineReportsABandExceeded pins that the ratio reads above one
// when the run overshot: the line is printed before the verdict, so it must
// say so rather than clamp.
func TestHeadroomLineReportsABandExceeded(t *testing.T) {
	r := headroomReport{Observed: 3, Band: tolerances.Band{Value: 2, Derivation: "test"}}
	if got := r.Headroom(); got <= 1 {
		t.Fatalf("Headroom() = %v for an observed delta past the band, want > 1", got)
	}
}

// expHistClosedFormRoundingSlack bounds the disagreement between the live
// measurement recorded in tolerances (ClickHouse Float64 exp2 arithmetic,
// one subtraction) and the same quantity from Go's math.Pow. The two agree
// to about 1e-14; a wrong bucket base, lower bound or rank fraction moves
// the result by hundredths, four orders past this slack, so the check
// tolerates rounding and nothing else.
const expHistClosedFormRoundingSlack = 1e-12

// TestExpHistogramMeasuredDiffIsTheEstimatorsClosedFormError derives the
// registry's recorded measurement from the probe's own geometry: the
// estimator interpolates geometrically (value = L · base^f) across the one
// bucket (L, base·L] the probe fills, the probe's observations are linear
// (L · (1 + f)), so at the probed quantile's rank fraction f the error is
// exactly L·(1+f) − L·base^f. A recorded measurement that stopped agreeing
// with this would mean the probe geometry and the registry drifted apart.
func TestExpHistogramMeasuredDiffIsTheEstimatorsClosedFormError(t *testing.T) {
	base := expHistProbeHigh / expHistProbeLow
	trueQuantile, _ := buildExpHistogramProbe()
	linear := expHistProbeLow * (1 + expHistProbeQuantile)
	if math.Abs(trueQuantile-linear) > expHistClosedFormRoundingSlack {
		t.Fatalf("buildExpHistogramProbe's true quantile %v is not the linear L·(1+f) = %v the closed form assumes", trueQuantile, linear)
	}
	geometric := expHistProbeLow * math.Pow(base, expHistProbeQuantile)
	closedForm := linear - geometric
	if diff := math.Abs(closedForm - tolerances.ExpHistogramQuantileMeasuredDiff); diff > expHistClosedFormRoundingSlack {
		t.Fatalf("tolerances.ExpHistogramQuantileMeasuredDiff = %v, but the estimator's closed-form error for the probe geometry (L=%v, base=%v, f=%v) is %v (diff %v)",
			tolerances.ExpHistogramQuantileMeasuredDiff, expHistProbeLow, base, expHistProbeQuantile, closedForm, diff)
	}
}
