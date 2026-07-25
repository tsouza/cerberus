@MIG-04 @tier0 @archetype:kube-prometheus-stack
Feature: MIG-04 — build the recording-rule dependency graph
  As an operator I want to know which derived series my dashboards and alerts
  actually consume, and which recorded series nothing reads, so that I keep
  materialising only what is still needed after the cutover.

  Scenario: build the recording-rule dependency graph offline
    Given the committed rule files and the harvested corpus for each tagged archetype
    When the operator builds the recording-rule dependency graph offline
    Then each rule graph matches its committed golden byte for byte
    And every recorded series is marked either consumed or orphan
    And every consumed series lists the consumers that keep it materialised
    And every rule input the builder could not parse is counted with a stated reason
    And building the graph from the same inputs a second time produces identical bytes
