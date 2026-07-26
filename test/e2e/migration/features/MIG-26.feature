@MIG-26
Feature: MIG-26 — the cutover gate refuses to go without evidence
  As an operator I want a single go/no-go decision that folds every migration
  artifact together and refuses the cutover while any required evidence is
  absent, so that nothing is torn down on the strength of a stage nobody
  actually ran.

  @tier0 @archetype:regulated-airgapped
  Scenario: the gate refuses a go decision while parity evidence is absent
    Given the classification and the rule graph produced offline for each tagged archetype
    And no query-parity evidence, because proving parity needs a live reference backend
    When the operator asks the cutover gate for a go or no-go decision
    Then the gate returns a no-go decision
    And the gate leaves with its documented no-go status rather than a tool-error status
    And the gate names query parity among the artifacts it is missing
    And the gate still records a verdict for every stage in its checklist
    And the gate states, for the blocking stage, why it cannot prove that stage safe

  @tier1 @archetype:regulated-airgapped
  Scenario: the live ClickHouse retention does not yet prove the compliance-retention mandate is met
    Given the dual-backend stack is live
    And the declared compliance-retention mandate for each tagged archetype
    When the operator reads the retention ClickHouse actually provisions, live
    Then the retention the gate decides on was actually read off the live ClickHouse
    And the live retention does not yet prove the compliance-retention mandate is met
