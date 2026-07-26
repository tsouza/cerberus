@MIG-18 @tier2
Feature: MIG-18 — the shadow ruler's own fire and resolve edges, observed end to end
  As an operator I want the shadow ruler I am about to trust with paging to
  prove, against real data, that it holds an alert down for its provisioned
  pending window before it fires, that the notification it emits carries the
  labels my routes and silences are written against and an annotation rendered
  from that alert's own label set rather than shipped as a raw template, and
  that clearing the condition produces a matching resolve edge at the one
  dead-end receiver — so that "it computes but never pages" is a demonstrated
  property of the substrate rather than a claim about its configuration.

  # Scope, stated honestly. This proves the SHADOW ruler's own lifecycle. The
  # incumbent-versus-shadow DIFF the story ultimately wants — the
  # eval-interval-quantized comparison of two independently scheduled rulers'
  # notification streams, and the multi-window multi-burn-rate burn-rate delta
  # held at zero across a full bake window — needs a second, genuinely
  # independent ruler with its own dead-end Alertmanager, plus an MWMBR rule
  # fixture provisioned on both sides. The Tier-2 substrate stands up ONE
  # ruler and ONE receiver, so there is no second stream to diff, and one
  # ruler diffed against itself is definitionally clean. The comparator that
  # diff will use is real and unit-tested today
  # (QuantizeToEvalInterval / DiffAlertStreams / BakeWindowHoldsZero in
  # steps/tier2_alerting.go, pinned by steps/tier2_alerting_test.go); what is
  # missing is the substrate. docs/migration-testing.md section 6's MIG-18 row
  # records that split.

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
    And the resolving edge does not precede the firing edge
