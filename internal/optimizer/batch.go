package optimizer

// Strategy describes how a Batch's rules are applied to the plan tree.
//
// Two cases ship today (mirroring Spark Catalyst's
// `Optimizer.scala`):
//
//   - Once — run every rule in the batch exactly once, in declared order.
//     Use for idempotent or analyzer-shaped passes (e.g. ConstantFold)
//     where re-running adds work without changing the tree.
//   - FixedPoint(N) — iterate the batch until no rule reports a change
//     (a fixpoint) or N iterations have elapsed, whichever comes first.
//     Use for rules that unlock each other (e.g. fuse-then-transpose-
//     then-fuse-again).
//
// Strategy is a sum type implemented as a small interface with an
// unexported marker; callers construct it via the helpers
// (`Once()` / `FixedPoint(n)`) and never implement it themselves.
type Strategy interface {
	// maxIterations returns the per-batch iteration cap.
	// Once → 1; FixedPoint(n) → n.
	maxIterations() int
	// isStrategy is a sealed marker; only types in this package satisfy it.
	isStrategy()
}

type onceStrategy struct{}

func (onceStrategy) maxIterations() int { return 1 }
func (onceStrategy) isStrategy()        {}

// Once returns a Strategy that runs each rule in the batch exactly once.
func Once() Strategy { return onceStrategy{} }

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
