@MIG-10 @tier0
Feature: MIG-10 — render the ClickHouse schema cerberus reads
  As an operator I want to review the exact CREATE statements cerberus expects
  before anything is provisioned, rendered from the same environment the server
  reads, so that applying them stays a deliberate decision I make by hand.

  Scenario Outline: render the schema under a schema environment
    Given the <case> cerberus schema environment
    When the operator renders the ClickHouse schema offline
    Then the rendered schema matches the committed golden for that environment
    And the rendered schema declares a table for every signal cerberus reads
    And the renderer reports success without opening a database connection
    And the renderer applies nothing to any database

    Examples:
      | case                 |
      | default              |
      | metrics-ttl-override |
