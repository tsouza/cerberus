// Package tolerances is the migration lane's registry of declared numeric
// epsilons (docs/migration-testing.md section 6.2, "The tolerances
// registry"). A predicate of kind 3 — "bounded by a declared constant",
// |a - b| <= epsilon — may reference only a name from this package, never an
// inline number in a .feature file: the coverage ratchet's V12 rejects a
// digit in step text specifically so the first epsilon cannot land inline.
//
// Every entry here carries its derivation as documentation, not as a bare
// number: where it was measured, against what, and on what date, so a
// reviewer can tell a real measurement from a guess.
//
// The registry is shrink-only: an epsilon may be lowered freely as evidence
// accumulates, but raising one is a reviewed decision, never a quiet fix for
// a scenario that started failing. There is no CI mechanism enforcing that
// today (the registry has exactly one entry, added in this change) — the
// discipline is the same one docs/migration-testing.md section 6.3 states
// for the rest of the lane: shrink-only by convention, tightened by review.
package tolerances

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
