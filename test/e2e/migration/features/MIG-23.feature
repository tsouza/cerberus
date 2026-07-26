@MIG-23 @tier1 @archetype:three-signal
Feature: MIG-23 — historical reads split at ClickHouse's own ingest-start boundary
  As an operator I want queries reaching back before ClickHouse's ingest-start
  to keep being served by the incumbent's read path, so that every historical
  query stays answerable while ClickHouse holds only what it has actually
  ingested, and I want that boundary measured off the live substrate rather
  than guessed.

  The router that performs the split is operator-owned infrastructure — an
  ingress rule, a proxy route, or a per-datasource pattern in Grafana — and
  lives outside this repository; cerberus ships no boundary knob and no
  backend-routing layer. What this scenario proves is the substrate fact such
  a router is built on, and which no cerberus feature can supply: ClickHouse
  answers only at or after its own ingest-start, the incumbent answers before
  it, and ClickHouse's retention has not yet aged past the boundary — so the
  incumbent cannot be retired yet.

  Scenario: ClickHouse's ingest-start is measured, cerberus answers only at or after it, the incumbent answers before it, and retention has not yet retired the split
    Given the dual-backend stack is live
    And the seeded fixture's own ingest-start instant for each tagged archetype
    When the operator measures ClickHouse's own earliest held instant and counts its rows either side of the boundary
    Then the boundary ClickHouse itself reports is the boundary the fixture declared, so the split point is observed rather than assumed
    And ClickHouse holds no rows before its own ingest-start
    And ClickHouse holds every row the seeder declared at or after ingest-start
    When the operator queries the probe metric at a post-boundary instant against cerberus
    Then cerberus answers the post-boundary instant with every series the seeder declared
    When the operator queries the probe metric at the pre-boundary instant against cerberus
    Then cerberus returns no series at all for the pre-boundary instant, tracing back to ClickHouse holding nothing there
    When the operator queries the same pre-boundary instant against the incumbent
    Then the incumbent answers the pre-boundary instant with every series the seeder left it from before ingest-start
    When the operator reads the live ClickHouse retention and compares it against the measured ingest-start
    Then the retention comparison refuses to call the split removable, because ClickHouse's retention has not yet aged past its own ingest-start
    And ClickHouse's retention still covers the pre-boundary instant, so cerberus answering nothing there is an ingest-start gap rather than an expired one
