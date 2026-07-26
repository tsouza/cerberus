@MIG-22 @tier1 @archetype:three-signal
Feature: MIG-22 — cutover moves and reverts by a URL change alone
  As an operator I want to stand cerberus up as an additional datasource and
  move or revert my read traffic per datasource by a URL change alone, so
  that every cutover step stays reversible and no big-bang swap is ever
  required.

  Scenario: read traffic flips to cerberus and reverts to the incumbent, both by a URL change alone
    Given the dual-backend stack is live
    And the seeded fixture's own probe metric for each tagged archetype
    When the operator queries the probe metric against the incumbent before any flip
    And the operator flips the probe from the incumbent to cerberus by a URL change alone
    Then the flipped probe against cerberus agrees with the incumbent's own pre-flip answer
    And the incumbent stayed live and reachable through the flip, because cerberus was stood up alongside it rather than in place of it
    When the operator reverts the probe from cerberus back to the incumbent by a URL change alone
    Then the reverted probe against the incumbent still agrees with its own pre-flip answer
    And cerberus stayed live and reachable through the revert
    And the flip and the revert dialled nothing but a different host for the identical request
