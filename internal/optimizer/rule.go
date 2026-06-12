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
	"github.com/tsouza/cerberus/internal/schema"
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
//     result is correct either way.
//   - "optimizer.predicate-pushdown" (FixedPoint) — FilterFusion + the
//     transpose rules can unlock each other: fuse adjacent filters,
//     transpose the fused filter through Project / Aggregate, then
//     possibly fuse again as new neighbours appear. Iteration is
//     load-bearing here.
//   - "optimizer.projection" (FixedPoint) — ProjectionPushdown may
//     iterate as pushdown unused-column elimination cascades through
//     nested Projects. Only one pass changes anything in the current
//     rule set, but the FixedPoint strategy lets additional rules join
//     the batch without changing wiring.
//   - "optimizer.mv-substitution" (FixedPoint) — MVSubstitution
//     rewrites `RangeWindow(Scan(base))` to `RangeWindow(Scan(rollup))`
//     when the operator has declared a pre-aggregated rollup whose
//     window + aggregation operator commute with the query's step +
//     range + outer function. Runs after predicate-pushdown so a
//     filter transposed under a RangeWindow can still see the
//     post-substitution scan (the rollup table exposes the same
//     series-identity columns as the base table). Default schema
//     ships the canonical `otel_metrics_sum_5m` / `otel_metrics_sum_1h`
//     entries — operators using a custom schema can call
//     `DefaultWithSchema(...)` to plug their own rollups in.
//
// Order matters across batches: the analyzer batch runs first
// (must-run); the heuristic constant fold then canonicalises bool
// literals so that predicate pushdown sees a tree where filters whose
// predicates contained `true AND ...` have already collapsed.
func Default() *Driver {
	return DefaultWithSchema(schema.DefaultOTelMetrics())
}

// DefaultWithSchema is like Default but binds the MV-substitution rule
// to the supplied Metrics schema's rollup registry. Use this from API
// handler wiring when the deployment overrides the default OTel
// schema; the rule needs to see the operator's configured rollup
// tables to find substitution candidates.
func DefaultWithSchema(metrics schema.Metrics) *Driver {
	return NewWithBatches(
		AnalyzerBatch("analyzer.constant-fold-semantic", ConstantFoldSemantic{}),
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
				FilterProjectTranspose(),
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
			Name:     "optimizer.mv-substitution",
			Strategy: FixedPoint(defaultMaxIterations),
			Rules:    []Rule{MVSubstitution(metrics.Rollups(), metrics.ValueColumn)},
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
	rewritten, childrenChanged := rewriteChildren(n, func(c chplan.Node) (chplan.Node, bool) {
		return applyToTree(c, rule)
	})
	result, here := rule.Apply(rewritten)
	return result, childrenChanged || here
}

// rewriteSingleInput is the shared clone-on-change shape for nodes
// with exactly one Input child: apply fn to input; when unchanged,
// hand back the original node, otherwise clone via `clone(newInput)`.
func rewriteSingleInput(
	orig chplan.Node,
	input chplan.Node,
	fn func(chplan.Node) (chplan.Node, bool),
	clone func(chplan.Node) chplan.Node,
) (chplan.Node, bool) {
	newInput, ch := fn(input)
	if !ch {
		return orig, false
	}
	return clone(newInput), true
}

// rewriteLeftRight is the shared clone-on-change shape for binary nodes
// with exactly two children (Left / Right): apply fn to each; when
// neither changed, hand back the original node, otherwise clone via
// `clone(newLeft, newRight)`.
func rewriteLeftRight(
	orig chplan.Node,
	left, right chplan.Node,
	fn func(chplan.Node) (chplan.Node, bool),
	clone func(newLeft, newRight chplan.Node) chplan.Node,
) (chplan.Node, bool) {
	newLeft, lch := fn(left)
	newRight, rch := fn(right)
	if !lch && !rch {
		return orig, false
	}
	return clone(newLeft, newRight), true
}

// rewriteChildren clones n with each child replaced by `fn(child)`. Returns
// the new (or same) node and whether any child changed.
//
// EXHAUSTIVENESS CONTRACT: every concrete chplan.Node implementation MUST
// be handled by exactly one of the category dispatchers below. A node that
// recurses into its `Children()` lets every optimizer rule (predicate
// pushdown, PREWHERE, projection-pushdown, MV, constant-fold) fire beneath
// it; a node that falls through to the final `return n, false` becomes an
// OPAQUE LEAF that silently DISABLES all rules on its subtree. The only
// legitimate no-recursion arms are genuine leaves whose `Children()` is
// always nil (Scan, OneRow, StepGrid), handled by rewriteLeafNode. The
// compile-time test TestRewriteChildren_Exhaustive enumerates every Node
// type and fails if a node with non-nil children is returned unchanged, so
// a future new node type can't silently reintroduce the gap.
//
// The dispatch is split into per-shape helpers (unary-Input, binary
// Left/Right, leaf, and the irregular shapes) purely to keep each function
// under the cyclomatic-complexity gate; the behaviour is a single flat
// "match the concrete type, recurse into its children" rule.
func rewriteChildren(n chplan.Node, fn func(chplan.Node) (chplan.Node, bool)) (chplan.Node, bool) {
	if out, changed, handled := rewriteLeafNode(n); handled {
		return out, changed
	}
	if out, changed, handled := rewriteUnaryNode(n, fn); handled {
		return out, changed
	}
	if out, changed, handled := rewriteBinaryNode(n, fn); handled {
		return out, changed
	}
	if out, changed, handled := rewriteIrregularNode(n, fn); handled {
		return out, changed
	}
	return n, false
}

// rewriteLeafNode handles the genuine leaves — nodes whose `Children()` is
// always nil. They are returned unchanged (recursing would be a no-op).
func rewriteLeafNode(n chplan.Node) (out chplan.Node, changed, handled bool) {
	switch v := n.(type) {
	case *chplan.Scan:
		return v, false, true
	case *chplan.OneRow:
		return v, false, true
	case *chplan.StepGrid:
		return v, false, true
	}
	return n, false, false
}

// rewriteUnaryNode handles every node with exactly one Node-typed child.
// Most carry it as `Input`; the Metrics* aggregation nodes carry it as
// `Inner`. Each rebuilds via the shared clone-on-change rewriteSingleInput
// shape.
func rewriteUnaryNode(n chplan.Node, fn func(chplan.Node) (chplan.Node, bool)) (out chplan.Node, changed, handled bool) {
	switch v := n.(type) {
	case *chplan.Filter:
		out, changed = rewriteSingleInput(v, v.Input, fn, func(in chplan.Node) chplan.Node {
			cp := *v
			cp.Input = in
			return &cp
		})
	case *chplan.Project:
		out, changed = rewriteSingleInput(v, v.Input, fn, func(in chplan.Node) chplan.Node {
			cp := *v
			cp.Input = in
			return &cp
		})
	case *chplan.NestedSetAnnotate:
		out, changed = rewriteSingleInput(v, v.Input, fn, func(in chplan.Node) chplan.Node {
			cp := *v
			cp.Input = in
			return &cp
		})
	case *chplan.Aggregate:
		out, changed = rewriteSingleInput(v, v.Input, fn, func(in chplan.Node) chplan.Node {
			cp := *v
			cp.Input = in
			return &cp
		})
	case *chplan.RangeWindow:
		out, changed = rewriteSingleInput(v, v.Input, fn, func(in chplan.Node) chplan.Node {
			cp := *v
			cp.Input = in
			return &cp
		})
	case *chplan.AbsentOverTime:
		out, changed = rewriteSingleInput(v, v.Input, fn, func(in chplan.Node) chplan.Node {
			cp := *v
			cp.Input = in
			return &cp
		})
	case *chplan.Limit:
		out, changed = rewriteSingleInput(v, v.Input, fn, func(in chplan.Node) chplan.Node {
			cp := *v
			cp.Input = in
			return &cp
		})
	case *chplan.OrderBy:
		out, changed = rewriteSingleInput(v, v.Input, fn, func(in chplan.Node) chplan.Node {
			cp := *v
			cp.Input = in
			return &cp
		})
	case *chplan.RangeLWR:
		out, changed = rewriteSingleInput(v, v.Input, fn, func(in chplan.Node) chplan.Node {
			cp := *v
			cp.Input = in
			return &cp
		})
	case *chplan.RangeBucketFanout:
		out, changed = rewriteSingleInput(v, v.Input, fn, func(in chplan.Node) chplan.Node {
			cp := *v
			cp.Input = in
			return &cp
		})
	case *chplan.HistogramQuantile:
		out, changed = rewriteSingleInput(v, v.Input, fn, func(in chplan.Node) chplan.Node {
			cp := *v
			cp.Input = in
			return &cp
		})
	case *chplan.HistogramQuantileNative:
		out, changed = rewriteSingleInput(v, v.Input, fn, func(in chplan.Node) chplan.Node {
			cp := *v
			cp.Input = in
			return &cp
		})
	case *chplan.MetricsAggregate:
		out, changed = rewriteSingleInput(v, v.Inner, fn, func(in chplan.Node) chplan.Node {
			cp := *v
			cp.Inner = in
			return &cp
		})
	case *chplan.MetricsHistogramOverTime:
		out, changed = rewriteSingleInput(v, v.Inner, fn, func(in chplan.Node) chplan.Node {
			cp := *v
			cp.Inner = in
			return &cp
		})
	case *chplan.MetricsSecondStage:
		out, changed = rewriteSingleInput(v, v.Input, fn, func(in chplan.Node) chplan.Node {
			cp := *v
			cp.Input = in
			return &cp
		})
	default:
		return n, false, false
	}
	return out, changed, true
}

// rewriteBinaryNode handles every node with exactly two Node-typed
// children carried as `Left` / `Right`. Each rebuilds via the shared
// rewriteLeftRight shape.
func rewriteBinaryNode(n chplan.Node, fn func(chplan.Node) (chplan.Node, bool)) (out chplan.Node, changed, handled bool) {
	switch v := n.(type) {
	case *chplan.CrossJoin:
		out, changed = rewriteLeftRight(v, v.Left, v.Right, fn, func(l, r chplan.Node) chplan.Node {
			cp := *v
			cp.Left = l
			cp.Right = r
			return &cp
		})
	case *chplan.StructuralJoin:
		out, changed = rewriteLeftRight(v, v.Left, v.Right, fn, func(l, r chplan.Node) chplan.Node {
			cp := *v
			cp.Left = l
			cp.Right = r
			return &cp
		})
	case *chplan.VectorJoin:
		out, changed = rewriteLeftRight(v, v.Left, v.Right, fn, func(l, r chplan.Node) chplan.Node {
			cp := *v
			cp.Left = l
			cp.Right = r
			return &cp
		})
	case *chplan.VectorSetOp:
		out, changed = rewriteLeftRight(v, v.Left, v.Right, fn, func(l, r chplan.Node) chplan.Node {
			cp := *v
			cp.Left = l
			cp.Right = r
			return &cp
		})
	case *chplan.SetOperation:
		out, changed = rewriteLeftRight(v, v.Left, v.Right, fn, func(l, r chplan.Node) chplan.Node {
			cp := *v
			cp.Left = l
			cp.Right = r
			return &cp
		})
	default:
		return n, false, false
	}
	return out, changed, true
}

// rewriteIrregularNode handles the nodes whose child shape doesn't fit the
// unary / binary helpers: TopK (Input + optional KExpr), UnionAll
// (N inputs), and MetricsCompare (Inner + optional RootLookup).
func rewriteIrregularNode(n chplan.Node, fn func(chplan.Node) (chplan.Node, bool)) (out chplan.Node, changed, handled bool) {
	switch v := n.(type) {
	case *chplan.TopK:
		newInput, ch := fn(v.Input)
		var newKExpr chplan.Node
		kCh := false
		if v.KExpr != nil {
			newKExpr, kCh = fn(v.KExpr)
		}
		if !ch && !kCh {
			return v, false, true
		}
		cp := *v
		if ch {
			cp.Input = newInput
		}
		if kCh {
			cp.KExpr = newKExpr
		}
		return &cp, true, true
	case *chplan.UnionAll:
		// Recurse into each arm so existing optimizer rules
		// (constant-fold, PREWHERE promotion, etc.) can rewrite the
		// per-arm Project(Filter(Scan)) subtrees the PromQL
		// classic-histogram companion-suffix lowering emits.
		newInputs := make([]chplan.Node, len(v.Inputs))
		ch := false
		for i, in := range v.Inputs {
			newIn, c := fn(in)
			if c {
				ch = true
			}
			newInputs[i] = newIn
		}
		if !ch {
			return v, false, true
		}
		cp := *v
		cp.Inputs = newInputs
		return &cp, true, true
	case *chplan.MetricsCompare:
		// Inner is the always-present child; RootLookup is an optional
		// second child (the service-graph root-name join side). Recurse
		// into both, rebuilding only when one changed.
		newInner, innerCh := fn(v.Inner)
		var newRootLookup chplan.Node
		rootCh := false
		if v.RootLookup != nil {
			newRootLookup, rootCh = fn(v.RootLookup)
		}
		if !innerCh && !rootCh {
			return v, false, true
		}
		cp := *v
		if innerCh {
			cp.Inner = newInner
		}
		if rootCh {
			cp.RootLookup = newRootLookup
		}
		return &cp, true, true
	}
	return n, false, false
}
