package promql

import (
	"math"
	"strconv"
	"strings"
	"testing"
)

// TestGoShortestGSciBoundsMatchStrconv pins
// [goShortestGSciLowerBound] / [goShortestGSciUpperBound] against the
// Go standard library itself rather than against a remembered rule.
//
// Both runtime `%g` renderers in this package — [openMetricsFloatExpr]
// and [nativeHistogramShortestGString] — decide "scientific or fixed"
// by comparing the magnitude against this pair, so the pair IS the
// layout rule. `strconv/ftoa.go` pins the decision precision `eprec` to
// 6 whenever the requested precision is "shortest", making the fixed
// window `[1e-4, 1e6)`; the two renderers used to disagree about the
// upper end (one carried 1e6, the other 1e21 — the point where
// CLICKHOUSE switches, which is the one number that is certainly not
// Go's), and `1e6` rendered `1000000` where Prometheus writes `1e+06`.
//
// The sweep is what makes this non-tautological: it does not restate
// the constants, it asks strconv where the boundary actually is and
// fails if the constants are anywhere else.
func TestGoShortestGSciBoundsMatchStrconv(t *testing.T) {
	t.Parallel()

	// usesScientific reports how Go's shortest %g actually lays v out.
	usesScientific := func(v float64) bool {
		return strings.ContainsAny(strconv.FormatFloat(v, 'g', -1, 64), "eE")
	}

	// Walk every decimal exponent either side of both boundaries. A
	// single-digit mantissa keeps the shortest-digit count at 1, which is
	// the case the `eprec = 6` clamp governs.
	for exp := -12; exp <= 24; exp++ {
		v := math.Pow(10, float64(exp))
		want := usesScientific(v)
		got := v < goShortestGSciLowerBound || v >= goShortestGSciUpperBound
		if got != want {
			t.Errorf("1e%d: constants say scientific=%v, strconv renders %q",
				exp, got, strconv.FormatFloat(v, 'g', -1, 64))
		}
	}

	// The two exact boundary magnitudes, stated as the constants
	// themselves so a pair that drifted TOGETHER still fails.
	if got := strconv.FormatFloat(goShortestGSciUpperBound, 'g', -1, 64); !strings.Contains(got, "e") {
		t.Errorf("FormatFloat(goShortestGSciUpperBound, 'g', -1, 64) = %q; the upper bound must be the first magnitude Go renders scientifically", got)
	}
	if got := strconv.FormatFloat(math.Nextafter(goShortestGSciUpperBound, 0), 'g', -1, 64); strings.Contains(got, "e") {
		t.Errorf("FormatFloat(just below goShortestGSciUpperBound, 'g', -1, 64) = %q; want fixed notation", got)
	}
	if got := strconv.FormatFloat(goShortestGSciLowerBound, 'g', -1, 64); strings.Contains(got, "e") {
		t.Errorf("FormatFloat(goShortestGSciLowerBound, 'g', -1, 64) = %q; the lower bound itself must still be fixed", got)
	}
	if got := strconv.FormatFloat(math.Nextafter(goShortestGSciLowerBound, 0), 'g', -1, 64); !strings.Contains(got, "e") {
		t.Errorf("FormatFloat(just below goShortestGSciLowerBound, 'g', -1, 64) = %q; want scientific notation", got)
	}

	// The exponent pad width: Go writes at least two exponent digits.
	if got := strconv.FormatFloat(1e-5, 'g', -1, 64); got != "1e-05" {
		t.Errorf("FormatFloat(1e-5, 'g', -1, 64) = %q; want %q (goSciExpPadBelow = %d)", got, "1e-05", goSciExpPadBelow)
	}
	if got := strconv.FormatFloat(1e-15, 'g', -1, 64); got != "1e-15" {
		t.Errorf("FormatFloat(1e-15, 'g', -1, 64) = %q; want %q", got, "1e-15")
	}
}
