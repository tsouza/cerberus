@MIG-09 @tier2
Feature: MIG-09 — the shadow ruler evaluates against cerberus and its recording-rule output round-trips through it
  As an operator I want to stand up a query-only external ruler against
  cerberus, confirm it evaluates rules against cerberus for real rather than
  against a mock, confirm its recording-rule output lands back in ClickHouse
  and becomes selectable through cerberus again, and confirm it fires only
  into a null receiver, before ever pointing a real paging alert at it.

  Scenario: the ruler evaluates the rule fixture and its recording-rule output round-trips through cerberus
    Given the shadow-ruler stack is live
    And the recording rule's underlying series is seeded into ClickHouse
    When the operator waits for the rule fixture to evaluate against cerberus
    Then the recording rule evaluates against cerberus without a query error
    And the alert rule evaluates against cerberus without a query error
    When the operator polls until the recorded series lands in ClickHouse
    Then the recorded series is selectable through cerberus
    When the operator records the dead-end receiver's baseline and sends a test notification through its one contact point
    Then the dead-end receiver is the only place the notification reaches
