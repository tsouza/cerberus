// Package optimizer rewrites a chplan tree to an equivalent, cheaper one by
// running registered rules to a fixpoint.
//
// Each Rule implements `Apply(n) (n', changed bool)`. The Driver visits
// every node in the tree bottom-up, gives each rule a chance to rewrite,
// and re-runs the whole pass until no rule reports a change (or the
// iteration cap is reached).
//
// Rules ship in their own files (filter_fusion.go, constant_fold.go,
// projection_pushdown.go); the default rule set is wired in Default().
//
// Rules are grouped into Batches (Catalyst-style). Each Batch carries a
// Strategy (`Once`, `Analyzer`, or `FixedPoint(n)`) that controls how
// its rules iterate. Batches run sequentially in the order Default()
// returns them; within a batch, rules run in declared order.
//
// Rules themselves split into two contracts (DataFusion-style):
//
//   - AnalyzerRule — semantic / must-run / idempotent. Lives in an
//     Analyzer-strategy batch and runs before any OptimizerRule sees
//     the plan. See analyzer.go.
//   - OptimizerRule (the plain Rule interface) — heuristic / optional.
//     Lives in an Once or FixedPoint(n) batch.
//
// See batch.go.
package optimizer

import (
	"context"

	"go.opentelemetry.io/otel"

	"github.com/tsouza/cerberus/internal/cerbtrace"
	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/telemetry"
)

// tracer emits the `optimize` pipeline-stage span.
var tracer = otel.Tracer("github.com/tsouza/cerberus/internal/optimizer")

// Rule is one rewrite pass over the plan IR.
type Rule interface {
	// Name returns the rule's identifier (used in debug + test fixtures).
	Name() string
	// Apply rewrites n if a pattern matches and returns the new node + a
	// changed-flag. When no pattern matches it returns n unchanged.
	Apply(n chplan.Node) (chplan.Node, bool)
}

// defaultMaxIterations is the fixpoint cap used by Default()'s
// FixedPoint batches and by the New() back-compat wrapper. Generous;
// rules that don't converge typically signal a bug rather than a
// tuning concern.
const defaultMaxIterations = 100

// Driver runs a sequence of Batches over a chplan tree.
type Driver struct {
	batches []Batch
}

// New builds a Driver with the supplied rule set wrapped in a single
// `FixedPoint(100)` batch named "default". Preserved for back-compat
// with callers that pre-date Batch grouping; new code should call
// NewWithBatches.
func New(rules ...Rule) *Driver {
	return NewWithBatches(Batch{
		Name:     "default",
		Strategy: FixedPoint(defaultMaxIterations),
		Rules:    rules,
	})
}

// NewWithBatches builds a Driver that runs the given batches in order.
// Each batch iterates per its Strategy; later batches see the output of
// earlier ones.
func NewWithBatches(batches ...Batch) *Driver {
	return &Driver{batches: batches}
}

// Default returns a Driver configured with all the seed rules grouped
// into Catalyst-style batches. The split borrows the DataFusion
// `AnalyzerRule` vs `OptimizerRule` distinction:
//
//   - "analyzer.constant-fold-semantic" (Analyzer, must-run) —
//     ConstantFoldSemantic reduces literal-only arithmetic / comparison
//     binaries (`1+2 → 3`, `1=0 → false`). Downstream rules rely on
//     pure-literal subtrees having collapsed to a single Lit, so the
//     pass is a semantic invariant rather than a heuristic improvement.
//     Idempotent by construction; the Analyzer strategy enforces that
//     contract via a verification pass and panics on violation.
//   - "optimizer.constant-fold-heuristic" (Once) —
//     ConstantFoldHeuristic applies boolean algebraic identities
//     (`true AND X → X`, `false OR X → X`, `false AND X → false`,
//     `true OR X → true`). Single bottom-up sweep reaches its fixpoint;
//     the identities are ergonomic — they shrink emitted SQL but the
//     result is correct either way. Fires on the real corpus (e.g. the
//     TraceQL `rate() by(kind)` drilldown whose lowering emits a
//     `(... AND true) AND true` predicate above a MetricsAggregate);
//     reachable since #812 made the optimizer walk total across all 26
//     chplan node types.
//   - "optimizer.predicate-pushdown" (FixedPoint) — FilterFusion + the
//     transpose rules can unlock each other: fuse adjacent filters,
//     transpose the fused filter through Aggregate / RangeWindow, then
//     possibly fuse again as new neighbours appear. Iteration is
//     load-bearing here. See doc.go for which transpose rules fire on
//     the current corpus versus which are speculative correctness
//     insurance.
//   - "optimizer.projection" (FixedPoint) — ProjectionPushdown may
//     iterate as pushdown unused-column elimination cascades through
//     nested Projects. Only one pass changes anything in the current
//     rule set, but the FixedPoint strategy lets additional rules join
//     the batch without changing wiring.
//   - "optimizer.set-op-linearize" (FixedPoint) — FlattenVectorSetOp
//     collapses a left-assoc chain of the SAME associative vector
//     set-op (`a or b or c …` / `a and b and c …`) into one N-ary
//     NaryVectorSetOp so the emitter renders ONE windowed single-pass
//     instead of one per nesting level. Runs last so the per-arm
//     subtrees have already been rewritten (pushdown / projection)
//     before they're frozen into N-ary arms. Iterates bottom-up: each
//     level absorbs the already-flattened left child until the whole
//     chain is one node. Parity-preserving — changes execution shape,
//     not results; `unless` is skipped (not associative).
//
// Order matters across batches: the analyzer batch runs first
// (must-run); the heuristic constant fold then canonicalises bool
// literals so that predicate pushdown sees a tree where filters whose
// predicates contained `true AND ...` have already collapsed.
func Default() *Driver {
	return NewWithBatches(
		AnalyzerBatch("analyzer.constant-fold-semantic", ConstantFoldSemantic{}),
		// analyzer.scan-resource-bound (Analyzer, must-run): RequireScanResourceBound
		// is an EARLY signal for the spans-scan resource-bound invariant (the
		// chsql.Emit chokepoint is the sufficient enforcement). It is verify-only:
		// it lifts the facts already on the node — a NestedSetAnnotate with
		// TraceLimit > 0 must carry its lock-step BoundedTraceScope leaf — and
		// panics if that pairing was broken in lowering. No ctx, no schema, no
		// synthesis; mutates nothing. Runs before the heuristic batches; the
		// verification rule never mutates, so the batch is idempotent.
		AnalyzerBatch(
			"analyzer.scan-resource-bound",
			RequireScanResourceBound{},
		),
		Batch{
			Name:     "optimizer.constant-fold-heuristic",
			Strategy: Once(),
			Rules:    []Rule{ConstantFoldHeuristic{}},
		},
		Batch{
			Name:     "optimizer.predicate-pushdown",
			Strategy: FixedPoint(defaultMaxIterations),
			Rules: []Rule{
				FilterFusion{},
				FilterAggregateTranspose(),
				FilterRangeWindowTranspose(),
			},
		},
		Batch{
			Name:     "optimizer.projection",
			Strategy: FixedPoint(defaultMaxIterations),
			Rules:    []Rule{ProjectionPushdown{}},
		},
		Batch{
			Name:     "optimizer.set-op-linearize",
			Strategy: FixedPoint(defaultMaxIterations),
			Rules:    []Rule{FlattenVectorSetOp{}},
		},
	)
}

// Run rewrites plan by applying each batch in order, returning the
// optimized tree. Run never mutates plan; rules construct fresh nodes
// when they rewrite.
//
// The ctx parameter carries the parent OpenTelemetry span (typically
// the otelhttp request span). Run wraps the whole pass in an
// `optimize` pipeline-stage span so a query's flame graph shows how
// long rule iteration took. The total number of rule-application
// changes is surfaced as `cerberus.rules_applied` on the span and as
// the `cerberus.optimizer.rules_applied` histogram so dashboards can
// aggregate how much rewriting the optimizer is doing across the fleet.
func (d *Driver) Run(ctx context.Context, plan chplan.Node) chplan.Node {
	_, span := tracer.Start(ctx, cerbtrace.SpanOptimize)
	defer span.End()
	rulesApplied := 0
	for _, batch := range d.batches {
		plan, rulesApplied = runBatch(plan, batch, rulesApplied)
	}
	span.SetAttributes(cerbtrace.AttrRulesApplied.Int(rulesApplied))
	telemetry.RecordRulesApplied(ctx, rulesApplied)
	return plan
}

// runBatch applies batch.Rules to plan per batch.Strategy. Returns the
// rewritten plan plus an updated `rulesApplied` counter — incremented
// every time a rule reports a tree change. The counter is surfaced as
// `cerberus.rules_applied` on the `optimize` span emitted by Driver.Run.
//
// Analyzer batches receive special handling: every rule must satisfy
// AnalyzerRule and is invoked via applyAnalyzerRule, which runs the
// rule once over the tree and then a verification pass. A
// non-idempotent rule (verification pass produces further change) is a
// must-run contract violation and panics with the offending rule's
// name. This makes the contract surface at test time rather than
// silently misbehaving in production.
func runBatch(plan chplan.Node, batch Batch, rulesApplied int) (chplan.Node, int) {
	if _, isAnalyzer := batch.Strategy.(analyzerStrategy); isAnalyzer {
		for _, rule := range batch.Rules {
			if _, ok := rule.(AnalyzerRule); !ok {
				panic("optimizer: analyzer batch " + batch.Name + " contains non-AnalyzerRule " + rule.Name() + "; use AnalyzerBatch() to construct analyzer batches")
			}
			var changed bool
			plan, changed = applyAnalyzerRule(plan, rule)
			if changed {
				rulesApplied++
			}
		}
		return plan, rulesApplied
	}
	maxIter := batch.Strategy.maxIterations()
	for i := 0; i < maxIter; i++ {
		var iterationChanged bool
		for _, rule := range batch.Rules {
			rewritten, changed := applyToTree(plan, rule)
			plan = rewritten
			iterationChanged = iterationChanged || changed
			if changed {
				rulesApplied++
			}
		}
		if !iterationChanged {
			return plan, rulesApplied
		}
	}
	return plan, rulesApplied
}

// applyToTree walks n bottom-up, applying rule at each node. Returns the
// rewritten tree plus whether any node changed.
func applyToTree(n chplan.Node, rule Rule) (chplan.Node, bool) {
	if n == nil {
		return nil, false
	}
	rewritten, childrenChanged := chplan.RewriteChildren(n, func(c chplan.Node) (chplan.Node, bool) {
		return applyToTree(c, rule)
	})
	result, here := rule.Apply(rewritten)
	return result, childrenChanged || here
}
