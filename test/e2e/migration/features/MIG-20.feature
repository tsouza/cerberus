@MIG-20 @tier1 @archetype:three-signal
Feature: MIG-20 — long-range panels agree with a downsampled aggregate within a declared band
  As an operator, I want to verify long-range panels against the incumbent's
  downsampled aggregates within a documented, counter-aware delta, so that a
  dashboard reading cerberus's cheap raw/rollup path shows the same growth my
  incumbent's downsampled storage tier would have shown, without demanding
  the bit-for-bit equality a downsample can never give.

  Scenario: cerberus's raw-path counter growth agrees with a downsampled reconstruction of the reference
    Given the dual-backend stack is live
    And the declared downsample tolerance for the long-range comparison
    When the operator compares cerberus's counter growth against a downsampled reconstruction of the reference's raw samples
    Then the observed delta stays within the declared downsample band
    And the comparison measured real counter growth on both the raw and the downsampled side
