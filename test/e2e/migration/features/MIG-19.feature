@MIG-19 @tier2
Feature: MIG-19 — recording-rule output parity, sample-by-sample
  As an operator I want every landed recorded-series sample compared
  value-for-value against what its own source query says, under the same
  exact-parity epsilon `cerberus migrate verify` uses, so a divergence in the
  write-back path (rule translation, input parity, or write-back timing-lag)
  is a blocker until reconciled rather than a shrug over an aggregate match.

  Scenario: landed samples match a live re-evaluation of their source query
    Given the shadow-ruler stack is live
    And the recording rule's source series is seeded with live samples
    When the operator waits for the ruler's write-back to land
    Then every landed sample matches a live re-evaluation of the recording rule's source query within the exact-parity epsilon
