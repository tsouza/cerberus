@MIG-22 @tier1 @archetype:three-signal
Feature: MIG-22 — cutover moves and rolls back by a URL change alone
  As an operator I want to stand cerberus up as an additional datasource and
  move or roll back my read traffic per datasource by a URL change alone, so
  that every cutover step stays reversible and no big-bang swap is ever
  required.

  The tier-1 stack runs no Grafana, so nothing here provisions a datasource,
  edits one, or rolls a provisioning file back. What this scenario drives is
  the property such a flip rests on: the identical Prometheus-wire request,
  dialled at cerberus's base URL and then back at the incumbent's, answers
  identically at both hosts, while both backends keep serving that same
  request throughout. Driving a real provisioned datasource waits on the
  Grafana leg being added to the tier-1 stack.

  Scenario: the identical read request answers identically at either base URL, and both backends keep serving
    Given the dual-backend stack is live
    And the seeded fixture's own probe metric for each tagged archetype
    When the operator queries the probe metric against the incumbent before anything is retargeted
    And the operator retargets the identical probe request at cerberus, changing nothing but the base URL
    Then cerberus answers the retargeted probe with series of its own, agreeing with the incumbent's own earlier answer
    And the incumbent still answers the same probe itself while cerberus is serving it, because cerberus was stood up alongside it rather than in place of it
    When the operator retargets the identical probe request back at the incumbent, changing nothing but the base URL
    Then the incumbent's answer after the rollback still agrees with its own earlier answer
    And cerberus still answers the probe itself after the rollback
    And the retarget and the rollback dialled one identical request path at the two different hosts, cerberus's and the incumbent's
