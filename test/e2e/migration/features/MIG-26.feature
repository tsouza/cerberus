@MIG-26 @tier0 @archetype:regulated-airgapped
Feature: MIG-26 — the cutover gate refuses to go without evidence
  As an operator I want a single go/no-go decision that folds every migration
  artifact together and refuses the cutover while any required evidence is
  absent, so that nothing is torn down on the strength of a stage nobody
  actually ran.

  Scenario: the gate refuses a go decision while parity evidence is absent
    Given the classification and the rule graph produced offline for each tagged archetype
    And no query-parity evidence, because proving parity needs a live reference backend
    When the operator asks the cutover gate for a go or no-go decision
    Then the gate returns a no-go decision
    And the gate leaves with its documented no-go status rather than a tool-error status
    And the gate names query parity among the artifacts it is missing
    And the gate still records a verdict for every stage in its checklist
    And the gate states, for the blocking stage, why it cannot prove that stage safe
