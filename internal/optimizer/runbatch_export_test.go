package optimizer

import "github.com/tsouza/cerberus/internal/chplan"

// RunBatchForTest exposes the unexported runBatch driver to the external
// optimizer_test package so tests can observe the `rulesApplied` counter
// directly. Driver.Run only surfaces the counter via telemetry/span
// attributes, which the gremlins INCREMENT_DECREMENT mutants on the two
// `rulesApplied++` sites (rule.go) escape — returning the counter here
// makes the increment observable so a test can pin it.
func RunBatchForTest(plan chplan.Node, batch Batch, rulesApplied int) (chplan.Node, int) {
	return runBatch(plan, batch, rulesApplied)
}
