@MIG-21 @tier1 @archetype:three-signal
Feature: MIG-21 — cross-signal correlation with trace_id as an indexed column
  As an operator, I want to verify cross-signal correlation (exemplar to
  trace, trace to logs, span-metrics, service-graph) with trace_id as an
  indexed column, so that jumping from a log line to its trace works the same
  way it did on my incumbent stack.

  Scenario: a log line's trace_id resolves through cerberus to its own trace
    Given the dual-backend stack is live
    And a log record from the live fixture carrying a trace_id
    When the operator resolves that log line's trace_id back through cerberus to its trace
    Then trace_id is a first-class indexed column in both the logs table and the traces table
    And the log line cerberus returned carries the trace_id the fixture wrote for it
    And the trace cerberus resolves for that trace_id carries every span the fixture wrote for it
