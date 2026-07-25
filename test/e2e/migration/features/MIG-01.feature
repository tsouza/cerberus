@MIG-01 @tier0 @archetype:already-otel @archetype:kube-prometheus-stack @archetype:mimir-cortex @archetype:prometheus-thanos @archetype:regulated-airgapped @archetype:saas-repatriation @archetype:three-signal @archetype:victoriametrics
Feature: MIG-01 — harvest a provenance-tagged query corpus
  As an operator planning a move off Prometheus, I want every query my
  organisation actually runs harvested from my rule files and my exported
  dashboards into one machine-readable corpus, offline and without touching
  the running stack, so that I plan against what is really in production
  rather than against what I remember.

  Scenario: harvest every archetype corpus offline
    Given the committed rule files and exported dashboards for each tagged archetype
    When the operator harvests the query corpus for each archetype
    Then each corpus matches its committed golden byte for byte
    And each corpus names the rule or dashboard each query came from
    And no corpus repeats an identical expression from an identical source
    And each corpus accounts for every input it dropped with a stated reason
    And harvesting the same inputs a second time produces identical bytes
    And harvesting the corpus contacts no network endpoint
