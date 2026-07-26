@MIG-23 @tier1 @archetype:three-signal
Feature: MIG-23 — historical reads split at ClickHouse's own ingest-start boundary
  As an operator I want queries reaching back before ClickHouse's ingest-start
  to route transparently to the incumbent's read path, so that every
  historical query stays answerable while ClickHouse holds only what it has
  actually ingested, and I want that boundary to stay configurable,
  observable, and gated rather than a one-time guess.

  Scenario: ClickHouse's ingest-start boundary is real and measured, the incumbent still answers there, and the router stays required until retention ages past it
    Given the dual-backend stack is live
    And the seeded fixture's own ingest-start instant for each tagged archetype
    When the operator counts ClickHouse's own rows strictly before and at-or-after ingest-start
    Then ClickHouse holds exactly zero rows before its own ingest-start
    And ClickHouse holds at least one row at or after ingest-start
    When the operator queries the probe metric at the pre-boundary instant against cerberus
    Then cerberus returns no series at all for the pre-boundary instant, tracing back to ClickHouse holding nothing there
    When the operator queries the same pre-boundary instant against the incumbent
    Then the incumbent still answers the pre-boundary instant, the precondition any router relies on to serve it from there instead
    When the operator reads the live ClickHouse retention and compares it against the time elapsed since ingest-start
    Then the retention comparison refuses to call the split-router removable, because the live retention has not yet aged past ingest-start
