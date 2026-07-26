@MIG-08 @tier1 @archetype:kube-prometheus-stack
Feature: MIG-08 — soak the heaviest queries and confirm graceful degradation
  As an operator I want to soak-replay my heaviest queries and confirm
  cerberus degrades gracefully under fault injection, with a working
  rollback, so I know a ClickHouse outage fails safe and reversibly instead
  of hanging the gateway or blocking my path back to the incumbent.

  Scenario: the heaviest query is soaked, degrades gracefully under a ClickHouse fault, and cerberus recovers
    Given the dual-backend stack is live
    And the seeded archetype's fixture declaration
    When the operator replays the heaviest query against cerberus repeatedly
    Then every replay succeeds and cerberus reports latency percentiles
    When the operator pauses the ClickHouse container
    Then cerberus degrades the same heavy query cleanly instead of hanging
    And the reference Prometheus still answers a rollback query directly
    Then the operator resumes the ClickHouse container and cerberus recovers
