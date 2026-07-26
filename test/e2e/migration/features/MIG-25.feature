@MIG-25 @tier1 @archetype:three-signal
Feature: MIG-25 — decommission is blocked while the incumbent still serves reads
  As an operator I want to audit residual incumbent read traffic and block
  teardown until it reaches zero, in staged order, so that I never retire a
  read path something is still depending on.

  The audit is measured off the incumbent's own request counter, and it is run
  twice from the identical computation: once while a planted reader is still
  querying the incumbent, and once after that reader has stopped and the
  baseline has been re-established. An audit that refuses unconditionally
  fails the second run. The staged teardown order is held to the order the
  runbook documents, because no `cerberus migrate` command emits a
  cutover-stage verdict yet; when the gate grows one, this scenario asserts
  against that instead.

  Scenario: a planted residual reader blocks decommission authorization, and a quiet incumbent unblocks it
    Given the dual-backend stack is live
    And the incumbent's read-traffic baseline captured before any residual reader runs
    Then the incumbent's read traffic since the baseline is exactly zero, before any residual reader has run
    When a residual reader keeps querying the incumbent after cutover would have begun
    And the operator audits incumbent read traffic for decommission authorization
    Then the audit refuses authorization while the incumbent still serves the residual reader's traffic
    And the audit's refusal names the exact residual request count it measured
    And the staged decommission order the runbook documents still gates the read path before the ruler and the ingest leg
    When the residual reader stops and the operator re-establishes the read-traffic baseline
    And the operator audits incumbent read traffic for decommission authorization
    Then the same audit authorizes decommission once the incumbent serves no residual read traffic at all
