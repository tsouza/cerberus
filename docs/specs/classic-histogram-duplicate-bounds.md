# Spec: classic histogram duplicate bounds

## Problem

`histogram_quantile_classic_duplicate_bounds` stores one OTel classic histogram
with bounds `[1, 1, 5]` and per-bucket counts `[2, 3, 5, 0]`. Cerberus's existing
emitter coalesces the empty repeated-bound interval while retaining the cumulative
count at that boundary, returning `0.6` for phi `0.3`. The fixture was not
enrolled in parity, so that production behaviour was not checked against real
Prometheus.

## Scope

Included: classic histogram quantile duplicate-bound reconciliation, its
executable TXTAR round trip, and full exact Prometheus parity enrolment.

Excluded: float tolerances, fixture exclusions, and native histogram behaviour.

## Design

Keep the existing emitter coalescing implementation. Enrol the fixture in the
full exact Prometheus parity contract so its `0.6` answer is evaluated against
the upstream engine on every chDB run.

## Verification

1. Execute the single enrolled TXTAR fixture with the `chdb` parity lane, which
   evaluates the real in-process Prometheus engine and chDB SQL result.
2. Run the parity-contract ratchet to pin the enrolment increase.
3. Regenerate the PromQL golden shard if the executable fixture output changes.

## Risks

Coalescing must affect only repeated bounds within one histogram row; it must not
merge distinct histogram series or relax comparison precision.

## Task List

1. Complete: verify the existing emitter's duplicate-bound reconciliation against
   the real Prometheus evaluator.
2. Complete: enrol the fixture with the full exact parity contract.
3. Complete: raise the parity enrolment ratchet and run the chDB spec lane.
