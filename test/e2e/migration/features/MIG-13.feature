@MIG-13 @tier2
Feature: MIG-13 — recording-rule output lands in the ClickHouse landing zone
  As an operator I want every recording rule's output to reproducibly land in
  the ClickHouse landing zone the ruler writes through, so a dashboard built
  on a low-cardinality recorded series never has to regress to scanning raw
  high-cardinality data, and no derived series silently disappears after
  cutover.

  Background:
    Given the tier-2 ruler stack is live

  Scenario: the recording rule's output is reproducible in the CH landing zone
    Given the recording rule's source series is seeded with live samples
    When the operator waits for the ruler's write-back to land
    Then the recorded series is selectable through cerberus
    And the recorded series carries at least one sample
    And no recorded metric name is silently missing from the landing zone
