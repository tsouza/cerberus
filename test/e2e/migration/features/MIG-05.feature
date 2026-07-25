@MIG-05 @tier0 @archetype:already-otel @archetype:kube-prometheus-stack @archetype:mimir-cortex @archetype:prometheus-thanos @archetype:regulated-airgapped @archetype:saas-repatriation @archetype:three-signal @archetype:victoriametrics
Feature: MIG-05 — explain every corpus query without a backend
  As an operator I want to see the ClickHouse SQL cerberus would run for each
  of my queries, and the tables it would read, with no database in the loop,
  so that I can review the plan before anything is pointed at production.

  Scenario: explain every corpus query offline
    Given the harvested corpus for each tagged archetype
    When the operator explains the corpus offline
    Then each explain report matches its committed golden byte for byte
    And every explained query yields either ClickHouse SQL or a named unsupported symbol
    And every query that yields SQL names the ClickHouse tables it would read
    And every unsupported entry names the corpus source it came from
    And explaining the same corpus a second time produces identical bytes
    And explaining the corpus contacts no network endpoint
