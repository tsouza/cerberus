@MIG-24 @tier2 @archetype:regulated-airgapped
Feature: MIG-24 — the staged cutover order is enforced via the cutover gate
  As an operator I want the cutover order (informational, then dashboards,
  then recording rules, then paging last) enforced mechanically, so that no
  stage — least of all the paging repoint — ever moves ahead of the parity
  evidence that authorizes it. `cerberus migrate gate` is the single go/no-go
  every stage is gated on: this scenario pins both sides of that gate, the
  refusal and the authorization, so "blocked until parity evidence exists"
  is provably reversible once that evidence actually exists, not merely a
  one-way door.

  Scenario: the gate blocks the cutover while parity evidence is absent, and authorizes it once that evidence exists
    Given the classification and the rule graph produced offline for each tagged archetype
    And no query-parity evidence, because proving parity needs a live reference backend
    When the operator asks the cutover gate for a go or no-go decision
    Then the gate returns a no-go decision
    And the gate leaves with its documented no-go status rather than a tool-error status
    And the gate names query parity among the artifacts it is missing
    And the gate still records a verdict for every stage in its checklist
    And the gate states, for the blocking stage, why it cannot prove that stage safe
    Given clean query-parity evidence, because the reference and cerberus already agree
    When the operator asks the cutover gate for a go or no-go decision using that evidence
    Then the gate returns a go decision
    And the gate leaves with a clean exit status rather than its no-go or tool-error status
    And the gate reports no blocking stage
