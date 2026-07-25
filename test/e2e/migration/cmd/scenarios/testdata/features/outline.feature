@MIG-04 @tier0 @archetype:kube-prometheus-stack
Feature: MIG-04 — a feature-tagged outline
  A narrative the enumerator carries no opinion about.

  Scenario Outline: an outline collapses to one record
    Given the committed fixtures for the <case>
    When the operator runs the offline command
    Then the artifact matches its committed golden
    And every entry names the source it came from

    @examples-only-tag
    Examples:
      | case      |
      | first     |
      | second    |
