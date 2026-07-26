@MIG-11 @tier1 @archetype:three-signal
Feature: MIG-11 — resource-attribute and metric-name mapping
  As an operator I want to confirm label and metric-name mapping (resource
  attributes to Prometheus label names, dots rewritten to underscores,
  suffixes) so that my queries and alert routing keys are unchanged after I
  move my read traffic to cerberus.

  Scenario: resource-attribute and grouping label mapping agree between the reference backend and cerberus
    Given the dual-backend stack is live
    And the committed label-mapping corpus for each tagged archetype
    When the operator verifies the corpus against the incumbent
    Then the verify report replayed more than zero queries
    And every configured head returned at least one comparison unit
    And the diverge count is exactly zero
    And no family compared zero evidence
    And the verify command's exit status agrees with the report's own verdict
    And the reference and cerberus agree on the mapped label value sets
