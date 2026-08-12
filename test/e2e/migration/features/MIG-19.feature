@MIG-19 @tier2
Feature: MIG-19 — recording-rule output parity against the incumbent, sample-by-sample
  As an operator I want every landed recorded-series sample compared
  value-for-value against what the incumbent ruler's own engine computes at
  that same instant, under the same exact-parity epsilon `cerberus migrate
  verify` uses, so a divergence in the write-back path (rule translation, input
  parity, or write-back timing-lag) or in cerberus's own evaluation is a
  blocker until reconciled rather than a shrug over an aggregate match.

  # Scope, stated honestly.
  #
  # The comparison this scenario runs is: the value the shadow ruler RECORDED,
  # transported through relay-prom -> otel-collector-writeback -> ClickHouse,
  # against the value cerberus computes for the rule's own source expression at
  # the instant the ruler recorded it. That covers rule translation, write-back
  # transport fidelity and evaluation-timestamp alignment — every leg between
  # the ruler's output and the landing zone.
  #
  # That comparison alone would have cerberus on BOTH sides, so a cerberus
  # evaluation bug would move the recorded value and the re-evaluation
  # identically and cancel out. The last two steps close that: the oracle is
  # the INCUMBENT ruler — reference Prometheus, evaluating the same expression
  # over its own copy of the same samples, remote-written from the one fixture
  # that produced the ClickHouse rows — so cerberus stands on exactly one side.
  #
  # The incumbent's OWN recorded series is checked against that same engine on
  # the incumbent's own grid, which is what stops the incumbent leg degrading
  # into a query endpoint: delete its `record:` rule and that step fails. The
  # two recorded series are not compared point-for-point to each other because
  # the two rulers never record at the same instants and cannot be made to
  # (Prometheus offsets a group by hash(group, file) % interval, Grafana ticks
  # epoch-aligned), and the source series is a ramp — so a cross-grid
  # comparison would diff two correct answers to two different questions, and
  # only a tolerance wide enough to swallow the ramp would let it pass.
  #
  # Two assertions guard against a verdict that holds by construction. The
  # cadence check is a count oracle: a value-only comparison cannot see an
  # evaluation the write-back leg dropped entirely, because a sample that never
  # landed is never compared. The variation check is an anti-degeneracy oracle:
  # the source series is seeded with a RAMPED counter rate specifically so the
  # recorded value differs at every evaluation, because a flat series makes
  # every comparison zero against zero and agreement then proves nothing.

  Scenario: landed samples match a live re-evaluation of their source query
    Given the shadow-ruler stack is live
    And the recording rule's source series is seeded with live samples
    When the operator waits for the ruler's write-back to land
    Then the landed sample count matches the ruler's evaluation cadence across the window they span
    And the landed samples do not all carry the same value
    And every landed sample matches a live re-evaluation of the recording rule's source query within the exact-parity epsilon
    And the incumbent ruler recorded the same rule over its own copy of the source data
    And every landed sample matches what the incumbent ruler's engine computes at the same instant
