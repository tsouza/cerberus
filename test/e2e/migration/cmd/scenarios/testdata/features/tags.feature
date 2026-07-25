@MIG-26 @tier1
Feature: MIG-26 — scenario tags merge with feature tags
  A narrative the enumerator carries no opinion about.

  @archetype:regulated-airgapped @archetype:already-otel @archetype:Bad-Name @nonvocabulary
  Scenario: a scenario carrying its own tags
    Given the artifacts produced offline
    When the operator asks for a decision
    Then the decision is recorded

  @tier0
  Scenario: a scenario with no outcome step
    Given the artifacts produced offline
    When the operator asks for a decision
