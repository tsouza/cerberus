@MIG-02 @tier1 @archetype:kube-prometheus-stack
Feature: MIG-02 — inventory the incumbent's realized cardinality
  As an operator I want my metrics and labels ranked by their realized
  cardinality before I migrate, so I know which series and which high-churn
  dimensions will dominate cerberus's ingest and query cost — and so a source
  the inventory cannot read fails loudly instead of reporting an empty rank.

  Scenario: the inventory ranks realized cardinality and refuses an unreadable source
    Given the dual-backend stack is live
    And the seeded archetype's fixture declaration
    When the operator inventories the reference Prometheus
    Then the inventory ranks metrics by their realized head-series cardinality
    And the inventory flags the high-churn label as high-churn
    And the inventory's per-metric label cardinality matches the seeded fixture
    Given a Prometheus source that returns not-found for its TSDB status endpoint
    When the operator inventories that source
    Then the inventory fails with its documented source-unreachable error
