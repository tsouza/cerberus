@MIG-13 @tier1 @archetype:three-signal
Feature: MIG-13 — recording-rule output lands in the ClickHouse read path
  As an operator I want to confirm every recording-rule output lands in the
  ClickHouse landing zone and reads back through cerberus's normal read path
  under its declared name, so that dashboards and alerts built on recorded
  series keep working once the write-back leg is wired in.

  Scenario: a recorded series written into the landing zone reads back through cerberus
    Given the dual-backend stack is live
    And a recorded series is written into the ClickHouse landing zone as if a ruler had produced it
    When the operator reads the recorded series back through cerberus
    Then cerberus returns the recorded series under its declared name with the exact value the landing zone holds
