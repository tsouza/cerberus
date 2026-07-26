@MIG-07 @tier1 @archetype:kube-prometheus-stack
Feature: MIG-07 — the collector scrape path is equivalent to Prometheus scraping
  As an operator I want to confirm the collector's scrape path reconstructs
  what my reference Prometheus scrapes directly from the same target — series
  presence, scrape meta-metrics, classic histogram bucket survival — so I
  trust the collector-fed read path before I rely on it for anything scraped,
  not only for what I remote-write myself.

  Scenario: the collector's scrape of a target matches the reference Prometheus's own scrape of it
    Given the dual-backend stack is live
    Then scrape meta-metrics are produced under both the reference path and the collector path
    And every meta-metric present under the reference path is also present under the collector path
    And the labels the collector path drops are exactly the ones the named OTel translation replaces
    And the classic histogram from the scrape target yields through cerberus the quantile the reference itself yields
