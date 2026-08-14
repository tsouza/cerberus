# Spec: parity DELTA input adapter

Tracks epic #1986. Status: in progress.

## Problem

The Prometheus parity reader currently passes OTel DELTA rows to upstream Prometheus unchanged.
Prometheus only models cumulative counters and histograms, so `rate`, `increase`, and `irate` compare
against a different input meaning. `histogram_quantile_classic_delta_temporality.txtar` is enrolled
today but is hollow for this reason.

## Scope

Included:

- A test-only, independently implemented DELTA-to-cumulative input adapter for scalar, classic, and
  native-histogram series before the upstream Prometheus oracle runs.
- Focused adapter coverage and Prometheus enrolment for the nine direct DELTA fixtures plus their
  existing cumulative controls.
- The parity enrolment ratchet and generated parity accounting.

Excluded:

- Production PromQL, schema, SQL, API, or compatibility-harness changes.
- Any adapter for Loki or Tempo, or any exception/tolerance scope.

## Design

The chDB reader retains each seeded row's `AggregationTemporality`. For DELTA series only, it converts
the oracle input into a running total after the OTel row has been independently translated into its
Prometheus-shaped scalar, classic-histogram, or native-histogram series. Scalar and classic points are
prefix summed per full label set. Native histogram components are downscaled independently to the
coarsest scale in each addition, then prefix summed by bucket index. CUMULATIVE rows remain untouched.

The adapter is called only by `evaluatePrometheusParity`, after reading the seed and before invoking
the upstream engine. It imports no production lowering, schema, or emitter code; the existing oracle
dependency boundary therefore remains intact.

## Verification

- Add focused chdb-tagged unit tests for scalar, classic, native, and CUMULATIVE control inputs.
- Run narrowed `test/spec` adapter tests and all enrolled DELTA/control fixture cases.
- Run the parity oracle dependency and enrolment contract tests.
- Regenerate required accounting with `just update-golden parity` and review only its generated diff.

## Risks

- A shared production temporality implementation would make the oracle agree by construction; the
  adapter stays test-only and independently stated.
- Incorrect native bucket alignment or downscaling would alter the reference input; focused
  differing-offset and differing-scale coverage must pin bucket-index accumulation.

## Task List For Owner Review

1. Capture seeded temporality and adapt DELTA scalar, classic, and native oracle inputs to cumulative
   form only on the Prometheus evaluation path. Verify focused adapter tests.
2. Enrol the nine direct DELTA fixtures and the needed cumulative controls with full Prometheus parity;
   raise the enrolment floor by the exact increase. Verify each narrowed fixture.
3. Regenerate the parity-owned artifacts with `just update-golden parity`, inspect the diff, and run
   the parity dependency and contract tests.
4. Reconcile valid native DELTA scale changes to the coarsest scale before addition and enrol the
   native CUMULATIVE control. Verify scale-change coverage and the updated enrolment ratchet.
