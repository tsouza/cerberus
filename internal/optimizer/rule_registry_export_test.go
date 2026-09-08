package optimizer

// DefaultRuleNamesForTest returns the Name() of every rule Default()
// registers, in batch-then-declaration order, to the external
// optimizer_test package. Driver.batches is unexported and stays that way
// — the rule-interaction matrix needs to prove it enumerates the SAME rule
// set the production driver runs, and reading the registration through a
// test-only accessor is how it does that without widening the Driver's
// public surface.
func DefaultRuleNamesForTest() []string {
	var out []string
	for _, b := range Default().batches {
		for _, r := range b.Rules {
			out = append(out, r.Name())
		}
	}
	return out
}
