// Package tolerances is the registry docs/migration-testing.md section 6.2
// describes for the two comparison modes that are legitimately allowed a
// numeric epsilon (mode 2, the exp-histogram quantile estimator; mode 3, the
// downsample-only structural comparator): every one lives here, declared once
// with the derivation that produced it, never as an inline number in a
// feature file. The coverage ratchet enforces the "never inline" half
// structurally (no digit may appear in step text); this package is the other
// half — the one place a number is allowed to exist at all, and it always
// carries the reasoning that produced it.
//
// Every band's Value is a constant expression over named derivation inputs
// (a measured quantity or a structural ceiling, and the headroom multiple
// applied to it), never a literal. tolerances_test.go recomputes each Value
// from those inputs and fails on a bare edit of the Value, and it pins a
// ceiling per band so raising one fails until the ceiling in the test is
// edited alongside the derivation — the reviewed override.
//
// The registry is SHRINK-ONLY: a band may be lowered as evidence narrows it,
// but raising one needs an explicit, reviewed derivation change — never a
// quiet widening to make a newly-red scenario pass again.
package tolerances

import (
	"fmt"
	"time"

	"github.com/tsouza/cerberus/test/e2e/migration/seed"
)

// Band is one declared tolerance: the value a comparator is allowed to accept
// as still "the same answer", plus the derivation that produced it. An empty
// Derivation is a bug in this package, not a valid Band — tolerances_test.go
// rejects one.
type Band struct {
	// Value is the accepted delta in the comparator's own unit: an absolute
	// difference for the mode-2 estimator epsilon (ExpHistogramQuantile), a
	// relative fraction for the mode-3 downsample comparator
	// (MIG20Downsample, where 0.12 means 12%).
	Value float64
	// Derivation states, in prose, the math the value comes from — the
	// named inputs and the formula over them, never "measured margin plus
	// headroom" with no formula behind it.
	Derivation string
}

// --- MIG-12: exponential-histogram quantile estimator (mode 2) ------------

// ExpHistogramQuantile's inputs. The band bounds the absolute difference
// between cerberus's histogram_quantile over an OTel exponential (native)
// histogram and the true quantile of the underlying observations, for
// MIG-12's exp-histogram fidelity scenario (docs/migration-testing.md
// section 5, comparison mode 2 — "estimator epsilon").
//
// Derivation (measured, not guessed, against a live cerberus + ClickHouse —
// docs/migration-testing.md section 8 requires the first epsilon come from a
// live measurement):
//
//   - Procedure: seed ONE synthetic exponential-histogram row directly into
//     otel_metrics_exponential_histogram — base 2 (Scale=0), every one of
//     1001 linearly-spaced synthetic observations in [64, 128] folded into
//     the SINGLE positive bucket covering that interval (PositiveOffset=6,
//     PositiveBucketCounts=[1001]) — so the estimator has to interpolate
//     across the whole bucket width rather than land on a boundary the
//     fixture happened to supply. The true 0.95 quantile of that
//     linearly-spaced set is the exact rank-interpolated order statistic,
//     computed independently in Go
//     (test/e2e/migration/steps/then_histogram.go's
//     buildExpHistogramProbe), not read back from cerberus: 124.8.
//   - Measured: run against a live cerberus + ClickHouse pair (same DDL
//     `cerberus migrate schema` renders, applied to a scratch database;
//     schema.DefaultOTelMetrics()'s column layout, not a stub) —
//     `histogram_quantile(0.95, <probe metric>)` returned 123.63985010238025
//     at the seeded instant. Observed |diff| = ExpHistogramQuantileMeasuredDiff.
//     That gap is the estimator's geometric (log-space, base 2) within-bucket
//     interpolation — cerberus's positive-bucket formula is
//     value = base^(PositiveOffset + pos + fraction)
//     (internal/chsql/histogram_quantile_native.go), i.e. GEOMETRIC
//     interpolation across the bucket — versus the LINEAR (arithmetic)
//     distribution of the synthetic observations. For a bucket (L, 2L] and a
//     quantile landing at rank fraction f within it, that error is exactly
//     L·(1+f) − L·2^f; with L = 64 and f = 0.95 the closed form reproduces
//     the measured value to float rounding, and steps_test.go pins that
//     agreement against the probe's own geometry constants. The mismatch is
//     the real, bucket-geometry-bounded estimator error this tolerance
//     exists to bound, not a bug.
//   - Declared: the measured |diff| times ExpHistogramQuantileHeadroom.
//
// A single measurement is a lower bound on the true error distribution, not
// its ceiling. MIG-12's step prints the observed headroom (observed |diff|
// over the declared band) on every run and names both numbers in its
// failure message, so a wider gap on a later run is visible in the lane's
// log; these constants are the one place to widen it — reviewed, per the
// shrink-only rule above, never silently.
const (
	// ExpHistogramQuantileMeasuredDiff is the |diff| the live measurement
	// above observed: |123.63985010238025 − 124.8|.
	ExpHistogramQuantileMeasuredDiff = 1.1601498976197604
	// ExpHistogramQuantileHeadroom is the multiple applied to the measured
	// |diff|: it absorbs float rounding drift between ClickHouse builds in
	// the estimator's exp2/log arithmetic, which is the only thing that can
	// move a deterministic probe's answer between runs.
	ExpHistogramQuantileHeadroom = 1.7
)

// ExpHistogramQuantile is MIG-12's declared estimator epsilon.
var ExpHistogramQuantile = Band{
	Value: ExpHistogramQuantileMeasuredDiff * ExpHistogramQuantileHeadroom,
	Derivation: fmt.Sprintf(
		"exponential-histogram quantile estimator: the live probe (1001 linearly-spaced observations in one "+
			"base-2 bucket, q=0.95) measured |histogram_quantile − true quantile| = %v, the geometric-vs-linear "+
			"within-bucket interpolation error L·(1+f) − L·2^f at L=64, f=0.95; the declared band is that "+
			"measured diff times a headroom of %v, giving %v.",
		ExpHistogramQuantileMeasuredDiff, ExpHistogramQuantileHeadroom,
		ExpHistogramQuantileMeasuredDiff*ExpHistogramQuantileHeadroom,
	),
}

// --- MIG-20: downsample-only structural comparator (mode 3) ---------------

// MIG-20's downsample band and the bucket it is derived for.
//
// Derivation: a counter-aware downsample reconstructs one value per bucket as
// the MAXIMUM raw sample observed in that bucket (see
// test/e2e/migration/tolerant.DownsampleCounterAware). Comparing the total
// increase that reconstruction shows against the raw series' own total
// increase, the only structural loss is at the LEADING edge: growth that
// happened between the bucket's start and its first committed value is
// attributed to that bucket rather than "before the window", so in the worst
// case one whole bucket's share of the window's total growth goes
// unaccounted. For a roughly-linear counter over a window of duration W
// downsampled into buckets of duration B, that worst case is B / W.
//
// MIG-20's Tier-1 scenario runs against the three-signal archetype's live
// fixture, whose verify window is MIG20VerifyWindow (seed.Window: VerifyEnd
// minus VerifyStart — the seed window less its range-lookback margin).
// MIG20DownsampleBucket evenly divides that window, giving the structural
// ceiling B/W for a counter that grows at a constant rate.
//
// The fixture counter does not grow at a constant rate: each step draws its
// increment uniformly from [0, seed.CounterMaxIncrement) (seed.buildCounter).
// The leading edge the reconstruction loses is one bucket's growth, so the
// band is derived for the worst placement of that jitter — a leading bucket
// growing at the draw's PEAK rate over a window growing at its MEAN rate —
// which scales the structural ceiling by the draw's peak-to-mean ratio. For
// a uniform draw over [0, N) that ratio is (N-1) / ((N-1)/2) = 2 for every
// N > 1, so the band is 2 x B/W. No headroom is added on top: the model
// already places every unit of jitter where it hurts most.
//
// A production deployment comparing against REAL Thanos 5m/1h blocks over a
// multi-week window would derive a far tighter band from the same formula
// (B/W shrinks fast as W grows) — this value is declared for the bucket/
// window pair this Tier-1 scenario actually exercises, not asserted as a
// general-purpose constant.

// MIG20DownsampleBucket is the downsample bucket width MIG-20's live
// comparator uses — the number MIG20Downsample's band below is derived from.
// It is named here, beside the derivation that depends on it, so the two can
// never drift apart silently.
const MIG20DownsampleBucket = 2 * time.Minute

// MIG20VerifyWindow is the live fixture's verify window — the W in the B/W
// derivation — read from the seed geometry rather than restated.
const MIG20VerifyWindow = seed.SeedWindow - seed.RangeLookbackMargin

// MIG20StructuralCeiling is B/W: the share of the window's growth one
// bucket's leading edge can hold for a constant-rate counter.
const MIG20StructuralCeiling = float64(MIG20DownsampleBucket) / float64(MIG20VerifyWindow)

// MIG20CounterPeakToMeanRatio is the fixture counter's per-step increment
// peak over its mean: the draw is uniform over [0, CounterMaxIncrement), so
// the peak is CounterMaxIncrement-1 and the mean half of that.
const MIG20CounterPeakToMeanRatio = float64(seed.CounterMaxIncrement-1) /
	(float64(seed.CounterMaxIncrement-1) / 2)

// MIG20Downsample is MIG-20's declared downsample band. See the package
// derivation comment above MIG20DownsampleBucket.
var MIG20Downsample = Band{
	Value: MIG20StructuralCeiling * MIG20CounterPeakToMeanRatio,
	Derivation: fmt.Sprintf(
		"counter-aware downsample: worst-case leading-edge loss is one bucket's share of the window's "+
			"total growth (B/W). Three-signal's live verify window is %s; MIG20DownsampleBucket's %s bucket "+
			"gives a structural ceiling of %.2f for a constant-rate counter. The fixture counter's increments "+
			"are drawn uniformly from [0, %d), whose peak-to-mean ratio is %.0f; a leading bucket at the peak "+
			"rate over a window at the mean rate scales the ceiling by that ratio, giving %.2f.",
		MIG20VerifyWindow, MIG20DownsampleBucket, MIG20StructuralCeiling, seed.CounterMaxIncrement,
		MIG20CounterPeakToMeanRatio, MIG20StructuralCeiling*MIG20CounterPeakToMeanRatio,
	),
}
