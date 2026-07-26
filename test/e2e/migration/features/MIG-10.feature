@MIG-10
Feature: MIG-10 — render the ClickHouse schema cerberus reads
  As an operator I want to review the exact CREATE statements cerberus expects
  before anything is provisioned, rendered from the same environment the server
  reads, so that applying them stays a deliberate decision I make by hand.

  @tier0
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

  @tier1
  Scenario: diff the rendered schema against the live collector-created tables
    Given the dual-backend stack is live
    And the cerberus schema environment bound to the live tenant database
    When the operator renders the ClickHouse schema offline
    And the operator diffs the rendered schema against the live database
    Then the diff covers every table cerberus reads
    And every rendered table exists in the live database
    And every rendered column exists on its live table
    And every column cerberus reads exists on its live table
