@MIG-25 @tier1 @archetype:three-signal
Feature: MIG-25 — decommission is blocked while the incumbent still serves reads
  As an operator I want to audit residual incumbent read traffic and block
  teardown until it reaches zero, in staged order, so that I never retire a
  read path something is still depending on.

  Scenario: a planted residual reader blocks decommission authorization, and teardown stays staged
    Given the dual-backend stack is live
    And the incumbent's read-traffic baseline captured before any residual reader runs
    Then the incumbent's read traffic since the baseline is exactly zero, before any residual reader has run
    When a residual reader keeps querying the incumbent after cutover would have begun
    And the operator audits incumbent read traffic for decommission authorization
    Then the audit refuses authorization while the incumbent still serves the residual reader's traffic
    And the audit's authorization artifact names the exact residual request count it refused on
    And the staged decommission order still gates the read path before the ruler and the ingest leg
