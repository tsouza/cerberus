@MIG-18 @tier2
Feature: MIG-18 — alert-firing parity between the incumbent and shadow rulers
  As an operator I want fire/resolve timestamps, active labels and rendered
  annotations diffed between my incumbent ruler and cerberus's shadow ruler,
  quantized to their shared evaluation interval rather than compared as raw
  floats, so sub-interval scheduling skew between two independently-ticking
  rulers is never mistaken for a real divergence — and I want a
  multi-window multi-burn-rate SLO rule's burn-rate delta to hold zero across
  the FULL bake window, not just a spot check, before I repoint a single
  paging alert.
  #
  # Both scenarios below are honestly blocked in this substrate today: MIG-18
  # needs a genuinely independent INCUMBENT ruler + Alertmanager leg
  # (docs/migration-testing.md's own fixture requirement: "Tier-2 dual
  # rulers + dead-end Alertmanagers"), and this Tier-2 stack
  # (tiers/tier2-ruler) stands up only the shadow leg — Grafana-managed
  # alerting plus one dead-end receiver. The comparator both scenarios
  # exercise (eval-interval quantization, the bake-window predicate) is real
  # and unit-tested (see steps/tier2_alerting_test.go); only the second
  # ruler/Alertmanager leg and the MWMBR rule fixture are missing. See this
  # branch's task report for the full accounting.

  Background:
    Given the tier-2 ruler stack is live

  Scenario: fire/resolve parity is eval-interval-quantized, not a raw-timestamp diff
    Given the incumbent ruler evaluates the same rule set over the same overlap
    When the shadow ruler evaluates the same rule set over the same overlap
    And fire and resolve timestamps are diffed, eval-interval-quantized
    Then false-positive, false-negative and timing-skew counts are all zero

  Scenario: the MWMBR SLO burn-rate delta holds zero across the full bake window
    Given a multi-window multi-burn-rate SLO rule is armed near threshold on both rulers
    Then the multi-window multi-burn-rate SLO delta holds zero across the full bake window
