@MIG-12 @tier1 @archetype:three-signal
Feature: MIG-12 — metric-type and histogram fidelity
  As an operator I want to confirm metric-type and histogram fidelity
  (temporality, classic bucket layout, exponential-histogram quantiles) so
  that rate, increase and quantile panels keep reading the same shape of
  data after I move my read traffic to cerberus.

  Scenario: metric temporality, bucket layout and exponential-histogram quantiles hold up live
    Given the dual-backend stack is live
    And the committed histogram-fidelity corpus for each tagged archetype
    And a known exponential histogram is seeded into the live database
    When the operator verifies the corpus against the incumbent
    Then the verify report replayed more than zero queries
    And every configured head returned at least one comparison unit
    And the diverge count is exactly zero
    And no family compared zero evidence
    And the verify command's exit status agrees with the report's own verdict
    And the counter lands in the sum table under cumulative, monotonic temporality
    And the histogram lands in the histogram table under cumulative temporality
    And the reconstructed exponential-histogram quantile stays within the declared estimator tolerance of the true quantile
