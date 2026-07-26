@MIG-13
Feature: MIG-13 — recording-rule output lands in the ClickHouse landing zone
  As an operator I want every recording rule's output to land in the ClickHouse
  landing zone and read back through cerberus's normal read path under its
  declared name, so that dashboards built on a low-cardinality recorded series
  never have to regress to scanning raw high-cardinality data, and no derived
  series silently disappears after cutover.

  @tier1 @archetype:three-signal
  Scenario: a recorded series written into the landing zone reads back through cerberus
    Given the dual-backend stack is live
    And a recorded series is written into the ClickHouse landing zone as if a ruler had produced it
    When the operator reads the recorded series back through cerberus
    Then cerberus returns the recorded series under its declared name with the exact value the landing zone holds

  @tier2
  Scenario: the recording rule's output is reproducible in the CH landing zone
    Given the tier-2 ruler stack is live
    And the recording rule's source series is seeded with live samples
    When the operator waits for the ruler's write-back to land
    Then the recorded series is selectable through cerberus
    And the recorded series carries at least one sample
    And no recorded metric name is silently missing from the landing zone
