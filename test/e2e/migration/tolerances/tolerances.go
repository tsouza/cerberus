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
// The registry is SHRINK-ONLY: a band may be lowered as evidence narrows it,
// but raising one needs an explicit, reviewed derivation change — never a
// quiet widening to make a newly-red scenario pass again.
package tolerances

import "time"

// ExpHistogramQuantileEpsilon bounds the absolute difference between
// cerberus's histogram_quantile over an OTel exponential (native) histogram
// and the true quantile of the underlying observations, for MIG-12's
// exp-histogram fidelity scenario (docs/migration-testing.md section 5,
// comparison mode 2 — "estimator epsilon").
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
//     at the seeded instant. Observed |diff| = 1.1601498976197604. That gap
//     is the estimator's geometric (log-space, base 2) within-bucket
//     interpolation — cerberus's positive-bucket formula is
//     value = base^(PositiveOffset + pos + fraction)
//     (internal/chsql/histogram_quantile_native.go), i.e. GEOMETRIC
//     interpolation across the bucket — versus the LINEAR (arithmetic)
//     distribution of the synthetic observations. That mismatch is exactly
//     the real, bucket-geometry-bounded estimator error this tolerance
//     exists to bound, not a bug: it is bounded above by the bucket's own
//     width in log space (one full doubling here, base^1 = 2x), so no
//     reasonable declared epsilon needs to exceed that bound.
//   - Declared: 2.0, roughly 1.7x headroom over the single observed run
//     above.
//
// A single measurement is a lower bound on the true error distribution, not
// its ceiling; MIG-17's per-query max/median divergence reporting (already
// wired for the exact-parity corpus) is the mechanism that would surface a
// wider gap on a later run, at which point this constant is the one place to
// widen it — reviewed, per the shrink-only rule above, never silently.
const ExpHistogramQuantileEpsilon = 2.0

// Band is one declared tolerance: the value a comparator is allowed to accept
// as still "the same answer", plus the derivation that produced it. An empty
// Derivation is a bug in this package, not a valid Band — every constructor
// below fills it.
type Band struct {
	// Value is the accepted relative delta, e.g. 0.12 for 12%.
	Value float64
	// Derivation states, in prose, the aggregation math the value comes
	// from — never "measured margin plus headroom" with no formula behind
	// it.
	Derivation string
}

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
// fixture, whose verify window is 20 minutes (seed.Window: VerifyEnd minus
// VerifyStart). MIG20DownsampleBucket of 2 minutes evenly divides that
// window into 10 buckets, giving a structural ceiling of 2/20 = 0.10. The
// declared band adds headroom over that ceiling to absorb the fixture
// counter's per-step jitter (it does not grow by a perfectly constant amount
// every sample), rounding 0.10 up to 0.15 rather than shaving it to the exact
// theoretical minimum.
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

// MIG20Downsample is MIG-20's declared downsample band. See the package
// derivation comment above MIG20DownsampleBucket's use in it.
var MIG20Downsample = Band{
	Value: 0.15,
	Derivation: "counter-aware downsample: worst-case leading-edge loss is one bucket's share of the " +
		"window's total growth (B/W). Three-signal's live verify window is 20m; MIG20DownsampleBucket's 2m " +
		"bucket evenly divides it into 10 buckets, giving a structural ceiling of 2/20 = 0.10. The declared " +
		"0.15 adds headroom over that ceiling for the fixture counter's per-step jitter.",
}
