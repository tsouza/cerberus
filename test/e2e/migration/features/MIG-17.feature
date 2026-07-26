@MIG-17 @tier1 @archetype:three-signal
Feature: MIG-17 — semantic hotspots verified specifically
  As an operator, I want to verify the semantic hotspots (counter-reset,
  staleness/absence, histogram quantiles) specifically, not just as a spot
  check, so that the query shapes most likely to diverge are the ones I have
  the strongest evidence about before I cut over.

  Scenario: the semantic-hotspot sub-corpus agrees between the reference backend and cerberus
    Given the dual-backend stack is live
    And the committed semantic-hotspot corpus for each tagged archetype
    When the operator verifies the corpus against the incumbent
    Then the verify report replayed more than zero queries
    And every configured head returned at least one comparison unit
    And the diverge count is exactly zero
    And no family compared zero evidence
    And every hotspot query is individually evidenced, not only the aggregate
    And the verify command's exit status agrees with the report's own verdict
