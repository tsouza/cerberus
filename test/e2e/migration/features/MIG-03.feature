@MIG-03 @tier0 @archetype:saas-repatriation @archetype:victoriametrics
Feature: MIG-03 — classify every harvested query against cerberus support
  As an operator I want every harvested query bucketed as supported or
  unsupported, with the offending dialect construct named where cerberus has
  no equivalent, so that I know exactly what must be rewritten before cutover
  and nothing is silently dropped on the way.

  Scenario: classify a dialect-tainted corpus offline
    Given the harvested corpus for each tagged archetype
    When the operator classifies the corpus offline
    Then each classification matches its committed golden byte for byte
    And every harvested query lands in exactly one bucket
    And every unsupported query names the construct that rejected it
    And every entry the harvester dropped is carried into the classification
    And the bucket counts add up to the number of queries classified
