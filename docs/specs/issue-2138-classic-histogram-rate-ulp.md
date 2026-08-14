# Spec: #2138 classic histogram rate ULP parity

Status: implemented in the production base; this change enrols and guards it.

## Problem

Five classic `histogram_quantile` rate-window fixtures differed from Prometheus
by one ULP and therefore lacked exact full Prometheus parity contracts.

## Root Cause

Prometheus computes the boundary-extrapolation factor, divides that factor by
the range for `rate`, then multiplies the bucket increase once. Dividing the
completed product is algebraically equal but can round to a different
`float64`. The classic histogram fold now constructs `increase * (factor /
range)` in `internal/promql/histogram_quantile_window.go`.

## Scope

- Enrol the five verified fixtures in exact Prometheus parity.
- Pin the classic lowering's factor association structurally.
- Raise the parity enrolment ratchet by five.

No approximate comparison, tolerance, or production arithmetic alternative is
in scope.

## Task List

1. Complete: add exact full Prometheus parity contracts for the five affected
   classic histogram rate-window fixtures.
2. Complete: add a lowering test that rejects division of the completed bucket
   increase/factor product.
3. Complete: raise the exact parity enrolment floor from 582 to 587 and run
   the focused chDB reference cases plus the required golden regeneration.
