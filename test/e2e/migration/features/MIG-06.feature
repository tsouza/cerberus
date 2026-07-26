@MIG-06 @tier1 @archetype:kube-prometheus-stack
Feature: MIG-06 — the ingest bridge reconstructs every metric type
  As an operator I want to confirm the ingest bridge reconstructs each
  metric type — counter, gauge, classic histogram, native exponential
  histogram and summary — into the right ClickHouse table with the type it
  was pushed with, so a mis-reconstructed type is caught before I trust any
  query built on top of it, not merely accepted because some row landed
  somewhere.

  Scenario: every OTLP metric shape lands in its declared table with its declared type preserved
    Given the dual-backend stack is live
    When the operator pushes a counter, a gauge, a classic histogram, an exponential histogram and a summary through the collector
    Then each metric lands in its declared ClickHouse table and in no other
    And the declared type of each metric survives on the landed row
    And the metric name and its resource attributes are preserved on the landed rows
    And the offline explain names the same ClickHouse tables the landed rows were found in
    And a query for the counter and the gauge and the classic histogram returns the pushed values through cerberus
