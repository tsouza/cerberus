# Spec: Classic Histogram Parity Closure

Tracks #1986.

## Problem

Eleven executable classic-histogram PromQL fixtures still lack a `parity:`
contract. Their generated expected rows only check Cerberus against chDB, not
against the independent Prometheus oracle.

## Scope

- Run each comparable fixture through the real Prometheus parity path.
- Enrol every fixture that matches the reference result and ratchet the floor.
- Treat every reference mismatch as a Cerberus defect; fix only a shared,
  demonstrated production cause and add executable coverage.
- Do not change parity tolerances, add exceptions, or address non-classic
  histogram work tracked by #1986.

## Design

The fixtures own their parity declaration. The chDB parity runner reads the
same seed into its independent Prometheus evaluator, while production lowering
continues to supply Cerberus's result. A common failure is fixed at the
production lowering layer, then each affected fixture is re-run before enrolment.

## Verification

1. Run each candidate fixture subtest in the `chdb` PromQL parity lane.
2. Add `parity:` only to passing fixtures and update the enrolment floor.
3. Run `just update-golden parity` for the generated parity ledger, then rerun
   the focused parity subtests and the enrolment contract test.

## Risks

Exact float comparison can reveal real final-bit arithmetic disagreement. This
work does not relax comparison: a mismatch remains a production defect unless
the fixture cannot exercise a comparable sample result.

## Task List

1. Complete: audit twelve candidates, including the eleven requested fixtures
   and the equivalent `topk by(le)` fixture found during the corpus check.
2. Complete: fix `by(...)` aggregations that discard `le`, with a focused
   lowering test and the `histogram_quantile_classic_agg_plateau` parity case.
3. Complete: correct six seeds that omitted the mandatory zero-count `+Inf`
   bucket, enrol the three newly passing fixtures, and ratchet the floor to 588.
4. Keep the remaining real classic-histogram differences under #1986: duplicate
   bound reconciliation and exact rate-window rounding need independent
   production fixes. The repaired rate fixtures remain exact comparisons; no
   tolerance or exception was introduced.
