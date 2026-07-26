@MIG-15 @tier1 @archetype:three-signal
Feature: MIG-15 — multi-tenant isolation holds at the ClickHouse boundary
  As an operator, I want to confirm multi-tenant isolation (no cross-tenant
  reads, per-tenant read budget enforced), so that a per-tenant ClickHouse
  database migrated from mimir-cortex keeps one tenant's dashboards from ever
  reading another tenant's data, and keeps one tenant's query from spending
  a budget the operator never granted it.

  Scenario: cerberus reads only its own tenant database and refuses an over-budget read
    Given the dual-backend stack is live
    And a foreign tenant's series planted in its own ClickHouse database, in the table layout cerberus reads
    When the operator queries cerberus for every metric name and label it can see
    And the operator asks cerberus to read within, and then beyond, its configured sample budget
    Then the planted series is readable in the foreign tenant's own database
    And the foreign tenant's metric name never appears in cerberus's answer
    And every metric name the archetype declares appears in cerberus's answer
    And the foreign tenant's label never appears in cerberus's label surface
    And every mapped label the archetype declares appears in cerberus's label surface
    And the read within the sample budget returns every series the archetype declares
    And the read beyond the sample budget is refused with cerberus's sample-budget error
