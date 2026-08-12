@MIG-18 @tier2
Feature: MIG-18 — alert-firing parity between the incumbent and shadow rulers
  As an operator I want the shadow ruler I am about to trust with paging to
  prove, against real data, that it holds an alert down for its provisioned
  pending window before it fires, that the notification it emits carries the
  labels my routes and silences are written against and an annotation rendered
  from that alert's own label set rather than shipped as a raw template, that
  clearing the condition produces a matching resolve edge — and, above all,
  that a real multi-window multi-burn-rate SLO rule reaches the SAME verdict on
  the shadow ruler as it does on the incumbent I am migrating away from, so
  "it pages exactly when the old stack pages" is a demonstrated property rather
  than a claim about its configuration.

  # Scope. The first scenario proves the shadow ruler's own lifecycle; the
  # second is the incumbent-versus-shadow DIFF the story is ultimately about.
  #
  # The diff runs against two genuinely independent legs that share no code:
  # the shadow (Grafana-managed alerting, querying cerberus over ClickHouse,
  # notifying its own dead-end receiver) and the incumbent (reference
  # Prometheus, evaluating over its own TSDB, dispatching through its own
  # Alertmanager into its own dead-end receiver). Both are handed ONE fixture
  # rendered into each backend's wire shape, so the two rulers cannot disagree
  # because they read different data. Both evaluate the same multi-window
  # multi-burn-rate rule set, over one SLO whose error budget is burning and
  # one whose budget is intact — so a shadow ruler that missed the burn fails
  # as a false negative and one that paged the healthy SLO fails as a false
  # positive.
  #
  # What is deliberately NOT asserted as zero is quantized timing skew. Two
  # independently scheduled rulers pick evaluation instants up to one interval
  # apart and neither is late: Prometheus offsets a group by
  # hash(group, file) % interval and Grafana ticks epoch-aligned, and the
  # substrate cannot phase-lock them. Section 5 of docs/migration-testing.md
  # names that skew as not a cerberus artifact. It is bounded and measured
  # instead — by one evaluation interval plus the harness's own measured write
  # span — which is strictly more than quantization alone can say, because
  # quantization discards the magnitude a genuinely late ruler would show up in.

  Scenario: the shadow ruler holds the probe alert down, fires it with its provisioned labels and rendered annotation, then resolves it
    Given the shadow-ruler stack is live
    And the shadow ruler's fire-and-resolve probe is armed above its firing threshold
    When the operator waits for the shadow ruler to report the probe alert firing
    Then the shadow ruler reported the probe alert pending before it reported it firing
    And the probe alert did not fire before its provisioned pending window had elapsed
    When the operator waits for the firing notification to reach the dead-end receiver
    Then the dead-end receiver captured a firing edge for the probe alert
    And that firing edge carries the probe alert's provisioned labels
    And that firing edge carries an annotation rendered from the probe alert's own label set
    When the operator clears the probe below its firing threshold and waits for the resolve notification
    Then the dead-end receiver captured a resolving edge for the probe alert
    And the resolving edge closes the alert instance the firing edge opened
    And the resolving edge did not land before the operator cleared the probe
    Given the same error budget burn is seeded to both rulers' backends
    When the operator waits for both rulers to report the burn alerts firing
    Then both rulers paged for the same alerts, with neither raising one the other did not
    And neither ruler paged for the service whose error budget was intact
    And the two rulers' firing edges landed within the skew two independent schedulers can differ by
    And the burn rate the two rulers evaluate holds equal across the whole bake window
