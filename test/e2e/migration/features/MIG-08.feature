@MIG-08 @tier1 @archetype:kube-prometheus-stack
Feature: MIG-08 — soak the heaviest queries and confirm graceful degradation
  As an operator I want to soak-replay my heaviest queries and confirm
  cerberus degrades gracefully under fault injection, with a working
  rollback, so I know a ClickHouse outage fails safe and reversibly instead
  of hanging the gateway or blocking my path back to the incumbent.

  What this scenario does NOT yet reach, out of its story's PASS assertion in
  docs/migration-testing.md section 6, stated here so a green run is never
  read as more than it is: no memory is captured alongside the latency
  percentiles; no query.maxSamples or Go-side result-buffering bound is
  proven to stop a heavy range query exhausting the gateway; the fault is a
  container pause only, never a container kill, so a hard process death is
  unexercised; the heaviest query is synthesised from the archetype's own
  fixture declaration rather than drawn from the harvested corpus; and the
  prometheus-thanos archetype named in that cell has no seeded fixture here,
  so only the kube-prometheus-stack half of the corpus is soaked.

  Scenario: the heaviest query is soaked, degrades gracefully under a ClickHouse fault, and cerberus recovers
    Given the dual-backend stack is live
    And the seeded archetype's fixture declaration
    When the operator replays the heaviest query against cerberus repeatedly
    Then every replay returns the fixture's declared series set and cerberus reports latency percentiles
    When the operator pauses the ClickHouse container
    Then cerberus degrades the same heavy query cleanly instead of hanging
    And the reference Prometheus still answers the rollback query with the same series set
    Then the operator resumes the ClickHouse container and cerberus recovers the same series set
