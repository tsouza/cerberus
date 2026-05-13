package optimizer

// Strategy describes how a Batch's rules are applied to the plan tree.
//
// Three cases ship today (mirroring Spark Catalyst's `Optimizer.scala`
// plus DataFusion's analyzer/optimizer split — see
// docs/optimizer-research.md § 4):
//
//   - Once — run every rule in the batch exactly once, in declared
//     order. Use for genuinely-idempotent passes where re-running adds
//     work without changing the tree, but the rule is **not** a
//     semantic invariant.
//   - Analyzer — run every rule in the batch exactly once and verify
//     idempotence on a second pass; a non-idempotent rule triggers a
//     panic naming the offender. Use for **semantic / must-run** rules
//     (DataFusion `AnalyzerRule` equivalent) — the contract is that
//     the rule produces a canonical form downstream code depends on.
//     Batches with this Strategy must contain only types satisfying
//     AnalyzerRule (see analyzer.go); use AnalyzerBatch() to construct.
//   - FixedPoint(N) — iterate the batch until no rule reports a change
//     (a fixpoint) or N iterations have elapsed, whichever comes
//     first. Use for **heuristic / optional** rules that unlock each
//     other (e.g. fuse-then-transpose-then-fuse-again).
//
// Strategy is a sum type implemented as a small interface with an
// unexported marker; callers construct it via the helpers
// (`Once()` / `Analyzer()` / `FixedPoint(n)`) and never implement it
// themselves.
type Strategy interface {
	// maxIterations returns the per-batch iteration cap.
	// Once → 1; Analyzer → 1; FixedPoint(n) → n.
	maxIterations() int
	// isStrategy is a sealed marker; only types in this package satisfy it.
	isStrategy()
}

type onceStrategy struct{}

func (onceStrategy) maxIterations() int { return 1 }
func (onceStrategy) isStrategy()        {}

// Once returns a Strategy that runs each rule in the batch exactly once.
func Once() Strategy { return onceStrategy{} }

type analyzerStrategy struct{}

func (analyzerStrategy) maxIterations() int { return 1 }
func (analyzerStrategy) isStrategy()        {}

// Analyzer returns a Strategy that runs each AnalyzerRule in the batch
// exactly once and verifies idempotence on a second pass. A
// non-idempotent rule panics with the offending rule's name; the
// must-run contract means analyzer batches always execute before the
// first OptimizerRule sees the plan.
//
// Batches with this Strategy must contain only types satisfying
// AnalyzerRule (see analyzer.go). Construct them via AnalyzerBatch()
// rather than building the Batch struct by hand; AnalyzerBatch enforces
// the rule-type contract at compile time.
func Analyzer() Strategy { return analyzerStrategy{} }

type fixedPointStrategy struct{ n int }

func (s fixedPointStrategy) maxIterations() int { return s.n }
func (fixedPointStrategy) isStrategy()          {}

// FixedPoint returns a Strategy that iterates the batch until a fixpoint
// is reached or n iterations have elapsed. n must be > 0; callers pass
// a generous cap (100 is the project default) — rules that don't
// converge typically signal a bug rather than a tuning concern.
func FixedPoint(n int) Strategy {
	if n < 1 {
		n = 1
	}
	return fixedPointStrategy{n: n}
}

// Batch is a named group of Rules sharing a Strategy. Catalyst-style:
// the optimizer is a sequence of Batches, each batch is a sequence of
// Rules, and the Strategy controls how many times the batch iterates.
type Batch struct {
	Name     string
	Strategy Strategy
	Rules    []Rule
}
