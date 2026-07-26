@MIG-15 @tier1 @archetype:three-signal
Feature: MIG-15 — multi-tenant isolation holds at the ClickHouse boundary
  As an operator, I want to confirm multi-tenant isolation (no cross-tenant
  reads), so that a per-tenant ClickHouse database migrated from mimir-cortex
  keeps one tenant's dashboards from ever reading another tenant's data.

  Scenario: cerberus never surfaces data planted outside its configured tenant database
    Given the dual-backend stack is live
    And a foreign tenant's series planted directly in ClickHouse, outside cerberus's configured database
    When the operator queries cerberus for every metric name it can see
    Then the foreign tenant's metric name never appears in cerberus's answer
    And cerberus's own configured-tenant metrics still appear in cerberus's answer
