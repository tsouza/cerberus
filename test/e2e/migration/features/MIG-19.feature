@MIG-19 @tier2
Feature: MIG-19 — recording-rule output parity, sample-by-sample
  As an operator I want every landed recorded-series sample compared
  value-for-value against what its own source query says, under the same
  exact-parity epsilon `cerberus migrate verify` uses, so a divergence in the
  write-back path (rule translation, input parity, or write-back timing-lag)
  is a blocker until reconciled rather than a shrug over an aggregate match.

  # Scope, stated honestly.
  #
  # The comparison this scenario runs is: the value the shadow ruler RECORDED,
  # transported through relay-prom -> otel-collector-writeback -> ClickHouse,
  # against the value cerberus computes for the rule's own source expression at
  # the instant the ruler recorded it. That covers rule translation, write-back
  # transport fidelity and evaluation-timestamp alignment — every leg between
  # the ruler's output and the landing zone.
  #
  # It does NOT cover what section 6's PASS cell ultimately wants: a diff
  # against the INCUMBENT's own recorded series. cerberus is on both sides of
  # this comparison, because the Tier-2 substrate stands up ONE ruler; a
  # cerberus evaluation bug that affected the recording rule would affect the
  # re-evaluation identically and cancel out. Closing that gap needs the same
  # missing substrate MIG-18's note names — a second, genuinely independent
  # incumbent ruler recording the same rule set against reference Prometheus —
  # and the value comparator itself is already the one `cerberus migrate
  # verify` uses, so it is substrate that is missing, not comparison logic.
  # docs/migration-testing.md section 6's MIG-19 note records the split.
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
