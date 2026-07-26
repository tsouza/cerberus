@MIG-16 @tier1 @archetype:three-signal
Feature: MIG-16 — differential query parity over the full corpus
  As an operator, I want to run the differential query-parity harness over my
  full corpus until the diverge count reaches zero, so that I know cerberus
  answers my real queries the same way my incumbent Prometheus does before I
  cut any read traffic over.

  Scenario: every corpus query agrees between the reference backend and cerberus
    Given the dual-backend stack is live
    And the committed differential-parity corpus for each tagged archetype
    When the operator verifies the corpus against the incumbent
    Then the verify report replayed more than zero queries
    And every configured head returned at least one comparison unit
    And the diverge count is exactly zero
    And no family compared zero evidence
    And the verify command's exit status agrees with the report's own verdict
