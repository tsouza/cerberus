# Spec: extrapolating subquery compatibility coverage

Status: implemented by this change.

## Problem

The PromQL compatibility corpus did not compare `max_over_time`,
`min_over_time`, or `count_over_time` over a subquery whose inner expression is
an extrapolating `rate` or `increase`. Those reducers use the direct aggregate
emitter path, including its fused subquery form, so the real Prometheus
differential did not cover it (#2053).

## Scope

- Add one deterministic, seeded compatibility query for each direct reducer.
- Include an offset-bearing case to exercise the subquery grid carrier.
- Regenerate the Prometheus compatibility roster only from the CI-produced
  `compat-cases.json` artifact.

Production lowering and unrelated range reducers are not changed.

## Design

The three queries are placed with the corpus's existing subquery cases. They use
the already seeded `demo_cpu_usage_seconds_total` counter, so the corpus seed
coverage regression test continues to prove that neither backend compares empty
results. `rate` and `increase` make the inner subqueries extrapolating while the
outer reducers select the direct aggregate path.

## Verification

- Layer 2: the curated PromQL corpus is parsed by the existing seed/corpus
  regression test.
- Compatibility: `compatibility/prometheus` and
  `compatibility/prometheus-forced-route` compare all three cases with reference
  Prometheus and produce the authoritative roster artifact.
- Generated artifact: run `compat-baseline-sync.mjs` on that artifact, never by
  hand, then let the compatibility ratchet validate the resulting baseline.

## Risks

A new case can expose an existing parity defect. The baseline sync refuses to
record a failing case, so any discrepancy is fixed at its lowering or emitter
source before the roster is updated.

## Task List

1. Complete: add the three seeded direct-aggregate compatibility cases and
   document the corpus divergence from upstream.
2. Complete: obtain the Prometheus CI roster artifact and regenerate the parity
   baseline with the sanctioned sync script.
3. Complete: merge only after both Prometheus compatibility lanes and all
   required checks are green.
