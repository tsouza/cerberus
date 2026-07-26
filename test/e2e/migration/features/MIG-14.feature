@MIG-14
Feature: MIG-14 — the retention runway covers the longest query lookback
  As an operator I want to know that the ClickHouse retention I am about to
  provision reaches back at least as far as my longest-range dashboard and
  alert, so that a panel does not silently go blank the day after cutover.

  @tier0 @archetype:prometheus-thanos @archetype:three-signal @archetype:victoriametrics
  Scenario: the configured retention covers the longest lookback the corpus requires
    Given the harvested corpus for each tagged archetype
    And the metrics-ttl-override cerberus schema environment
    And the ClickHouse retention cerberus is configured to provision
    When the operator computes the longest lookback the corpus requires
    Then the computed longest lookback matches its committed golden
    And every query's own lookback is attributed back to the rule or dashboard that needs it
    And every corpus entry outside the PromQL lookback walk is counted, never assumed to reach back no distance
    And the configured retention for each signal covers the longest lookback that signal's queries require

  @tier1 @archetype:three-signal
  Scenario: the live collector-provisioned retention covers the longest lookback the corpus requires
    Given the dual-backend stack is live
    And the harvested corpus for each tagged archetype
    And the ClickHouse retention the live collector actually provisioned
    When the operator computes the longest lookback the corpus requires
    Then the collector-provisioned retention for each signal covers the longest lookback that signal's queries require
