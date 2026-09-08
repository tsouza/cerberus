package optimizer_test

// Rule × rule interaction matrix (Layer 4 § A).
//
// The optimizer ships 8 TREE-REWRITING rules across 5 batches — the
// complete Default() registration minus the two verify-only rules
// excluded below:
//
//   - ConstantFoldSemantic, NormalizeScanTimeBound (analyzer batches)
//   - ConstantFoldHeuristic (Once batch)
//   - FilterFusion, FilterAggregateTranspose,
//     FilterRangeWindowTranspose (predicate-pushdown FixedPoint batch)
//   - ProjectionPushdown (projection FixedPoint batch)
//   - FlattenVectorSetOp (set-op-linearize FixedPoint batch)
//
// Excluded, deliberately and not as a gap: RequireScanTimeBound and
// RequireScanResourceBound. Both are verify-only — neither ever returns
// changed=true; they read the node, panic on a contract violation, and
// hand the tree back untouched. A rule that cannot change the tree cannot
// change the CONVERGED tree in either registration order, so a commutation
// assertion over one would hold for any implementation including a no-op:
// it would be a test that cannot fail. What those two rules actually have
// to say is pinned where it can fail — scan_time_bound_test.go and
// scan_resource_bound_test.go assert the panic fires on a violating plan
// and that a conforming plan comes back unchanged. The exclusion is
// enforced rather than asserted in prose:
// TestRuleInteractionMatrixCoversDefaultRules reads the live Default()
// registration and fails if a rule is neither in the matrix nor in the
// named verify-only set.
//
// C(8,2) = 28 unordered pairs, and every one of them is exercised.
// TestRuleInteractionMatrix enumerates the pairs from the rule list itself
// and fails on any pair with no registered plan, so "every pair" is a
// derived fact rather than a claim in a comment. For each pair the goal is
// the same: construct a plan where BOTH rules are applicable, run a Driver
// that registers exactly those two rules (in either order), and assert the
// final tree is identical regardless of registration order. This catches
// commutation bugs: a pair where the rule firing order changes the
// converged tree shape would be a hazard for the Catalyst-style Batch
// driver (each FixedPoint batch picks up rules in declared order and
// iterates).
//
// FilterProjectTranspose and MVSubstitution were retired (2026-06); the
// pairs that involved them are gone.
//
// Strategy. Rather than reach for `RunWithRuleOrder` (which the driver
// doesn't expose), each pair wires two NewWithBatches drivers — one with
// `[r1, r2]` and one with `[r2, r1]`. Both run inside a single
// FixedPoint(100) batch so the iteration order converges. The pair is
// "interaction-stable" iff
// Driver(r1, r2).Run(plan).Equal(Driver(r2, r1).Run(plan)).
//
// Note. Some pairs are conceptually trivial (e.g. ConstantFoldSemantic +
// ProjectionPushdown — they touch disjoint plan slots). We still pin every
// pair so future rule additions can't quietly break a commutation property
// the codebase implicitly relies on. That motivation is exactly why
// FlattenVectorSetOp belongs here despite running alone in its own batch
// today: it REPLACES a VectorSetOp with a NaryVectorSetOp, a node the other
// rules' patterns no longer match, so it is the one rule whose firing order
// can decide whether another rule ever sees a subtree at all.

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/optimizer"
)

// The eight tree-rewriting rules, instantiated once so matrixRules and the
// pairPlans keys name the same objects — a pairPlans key is built by
// pairKey from these very values, which is what makes a key impossible to
// mistype into a pair that does not exist.
var (
	ruleFoldSemantic   optimizer.Rule = optimizer.ConstantFoldSemantic{}
	ruleFoldHeuristic  optimizer.Rule = optimizer.ConstantFoldHeuristic{}
	ruleFilterFusion   optimizer.Rule = optimizer.FilterFusion{}
	ruleAggTranspose   optimizer.Rule = optimizer.FilterAggregateTranspose()
	ruleRWTranspose    optimizer.Rule = optimizer.FilterRangeWindowTranspose()
	ruleProjPushdown   optimizer.Rule = optimizer.ProjectionPushdown{}
	ruleNormalizeBound optimizer.Rule = optimizer.NormalizeScanTimeBound{}
	ruleFlattenSetOp   optimizer.Rule = optimizer.FlattenVectorSetOp{}
)

// matrixRules is the tree-rewriting rule set the matrix enumerates pairs
// over. TestRuleInteractionMatrixCoversDefaultRules pins it against the
// live Default() registration, so a new rule cannot join production
// without joining the matrix.
func matrixRules() []optimizer.Rule {
	return []optimizer.Rule{
		ruleFoldSemantic,
		ruleFoldHeuristic,
		ruleFilterFusion,
		ruleAggTranspose,
		ruleRWTranspose,
		ruleProjPushdown,
		ruleNormalizeBound,
		ruleFlattenSetOp,
	}
}

// matrixVerifyOnlyRules names the Default() rules the matrix deliberately
// does not pair, with the reason recorded in the file header: a rule that
// never returns changed=true cannot make a converged tree order-dependent,
// so a commutation assertion over it could not fail.
func matrixVerifyOnlyRules() []string {
	return []string{
		optimizer.RequireScanTimeBound{}.Name(),
		optimizer.RequireScanResourceBound{}.Name(),
	}
}

// pairKey is the order-independent identity of an unordered rule pair.
func pairKey(a, b optimizer.Rule) string {
	names := []string{a.Name(), b.Name()}
	sort.Strings(names)
	return names[0] + " x " + names[1]
}

// firingRule wraps a Rule and counts the applications that actually
// rewrote a node. It is what keeps the matrix from going hollow: a
// commutation assertion over a plan neither rule can match holds
// trivially, so every pair must first PROVE both of its rules fired on
// the shared plan.
type firingRule struct {
	inner optimizer.Rule
	fired int
}

func (r *firingRule) Name() string { return r.inner.Name() }

func (r *firingRule) Apply(n chplan.Node) (chplan.Node, bool) {
	out, changed := r.inner.Apply(n)
	if changed {
		r.fired++
	}
	return out, changed
}

// twoRuleConverge runs the plan through two drivers — one ordered
// (a, b), one ordered (b, a) — and asserts the final trees are
// `Equal`. Both drivers iterate to fixpoint (100 iterations), so any
// commutation property of the pair surfaces as a tree-shape diff.
//
// Before comparing, it asserts that BOTH rules rewrote something in BOTH
// orders. That is the pair's applicability precondition made checkable:
// without it a plan that has drifted out of one rule's pattern (or a rule
// whose pattern narrowed) would keep passing while silently testing
// nothing. Requiring it in both orders rather than once also covers the
// pairs where one rule only becomes applicable AFTER the other fires —
// the fixpoint gets there from either starting order or the pair is not
// interaction-stable to begin with.
func twoRuleConverge(t *testing.T, label string, plan chplan.Node, a, b optimizer.Rule) {
	t.Helper()
	outAB := runOrdered(t, label, "(a, b)", plan, a, b)
	outBA := runOrdered(t, label, "(b, a)", plan, b, a)
	if !outAB.Equal(outBA) {
		t.Fatalf("%s: order-dependent fixpoint\n--- (a, b) ---\n%#v\n--- (b, a) ---\n%#v", label, outAB, outBA)
	}
}

// runOrdered runs plan through a single FixedPoint batch holding first
// then second, in that declared order, and fails if either rule never
// rewrote a node.
func runOrdered(t *testing.T, label, order string, plan chplan.Node, first, second optimizer.Rule) chplan.Node {
	t.Helper()
	f := &firingRule{inner: first}
	s := &firingRule{inner: second}
	d := optimizer.NewWithBatches(optimizer.Batch{
		Name:     "pair",
		Strategy: optimizer.FixedPoint(100),
		Rules:    []optimizer.Rule{f, s},
	})
	out := d.Run(context.Background(), plan)
	for _, r := range []*firingRule{f, s} {
		if r.fired == 0 {
			t.Fatalf("%s %s: rule %q never rewrote a node — the pair's plan does not make both "+
				"rules applicable, so the commutation assertion below would be vacuous",
				label, order, r.Name())
		}
	}
	return out
}

// TestRuleInteractionMatrix is the matrix itself: it enumerates every
// unordered pair of matrixRules, looks the pair's shared plan shape up in
// pairPlans, and asserts order-independent convergence. A pair with no
// registered plan is a hard failure naming the pair — that is what makes
// "every pair is covered" a checked property instead of a comment. The
// reverse direction is checked too: a pairPlans entry that names no live
// pair (a stale key left behind by a renamed or retired rule, or a pair
// registered twice so that one entry overwrote another) fails.
func TestRuleInteractionMatrix(t *testing.T) {
	t.Parallel()

	rules := matrixRules()
	covered := map[string]bool{}
	for i := 0; i < len(rules); i++ {
		for j := i + 1; j < len(rules); j++ {
			a, b := rules[i], rules[j]
			key := pairKey(a, b)
			if covered[key] {
				t.Fatalf("duplicate rule in matrixRules: pair %q enumerated twice", key)
			}
			covered[key] = true

			build, ok := pairPlans[key]
			if !ok {
				t.Errorf("no plan registered for rule pair %q — add a pairPlans entry whose "+
					"plan makes BOTH rules applicable; do not drop the pair", key)
				continue
			}
			t.Run(key, func(t *testing.T) {
				t.Parallel()
				twoRuleConverge(t, key, build(), a, b)
			})
		}
	}

	for key := range pairPlans {
		if !covered[key] {
			t.Errorf("pairPlans has entry %q that is not a pair of matrixRules — "+
				"a rule was renamed or retired; fix the key or delete the entry", key)
		}
	}
	if len(pairPlans) != len(covered) {
		t.Errorf("pairPlans holds %d entries for %d pairs — two entries share a key and one "+
			"overwrote the other", len(pairPlans), len(covered))
	}
}

// TestRuleInteractionMatrixCoversDefaultRules pins the matrix's rule list
// against the live Default() registration: every rule the production
// driver runs is either paired by the matrix or named in the verify-only
// exclusion set, and nothing in either list is absent from Default(). This
// is the ratchet — registering a new rewriting rule in Default() without
// enrolling it here fails the build rather than silently shrinking the
// matrix's coverage.
func TestRuleInteractionMatrixCoversDefaultRules(t *testing.T) {
	t.Parallel()

	registered := map[string]bool{}
	for _, name := range optimizer.DefaultRuleNamesForTest() {
		registered[name] = true
	}

	enrolled := map[string]bool{}
	for _, r := range matrixRules() {
		enrolled[r.Name()] = true
		if !registered[r.Name()] {
			t.Errorf("matrixRules enumerates %q, which Default() does not register — "+
				"remove it from the matrix or re-register it", r.Name())
		}
	}
	for _, name := range matrixVerifyOnlyRules() {
		if enrolled[name] {
			t.Errorf("%q is both paired and listed as verify-only — pick one", name)
		}
		enrolled[name] = true
		if !registered[name] {
			t.Errorf("verify-only exclusion names %q, which Default() does not register — "+
				"drop the stale exclusion", name)
		}
	}

	for name := range registered {
		if !enrolled[name] {
			t.Errorf("Default() registers %q, which the interaction matrix neither pairs nor "+
				"excludes — add it to matrixRules (and a pairPlans entry per new pair), or, if it "+
				"is verify-only, to matrixVerifyOnlyRules with the reason in the file header", name)
		}
	}
}

// labelFilter builds a `<col> = <v>` predicate. Used pervasively.
func labelFilter(col, v string) chplan.Expr {
	return &chplan.Binary{
		Op:    chplan.OpEq,
		Left:  &chplan.ColumnRef{Name: col},
		Right: &chplan.LitString{V: v},
	}
}

// trueEq is a pure-literal tautology (`n = n`) — the shape
// ConstantFoldSemantic folds to `true`.
func trueEq(n int64) chplan.Expr {
	return &chplan.Binary{Op: chplan.OpEq, Left: &chplan.LitInt{V: n}, Right: &chplan.LitInt{V: n}}
}

// sumAgg is the one-key / one-aggregate Aggregate shape the matrix's
// aggregate-shaped plans share.
func sumAgg(input chplan.Node, key string) *chplan.Aggregate {
	return &chplan.Aggregate{
		Input:   input,
		GroupBy: []chplan.Expr{&chplan.ColumnRef{Name: key}},
		AggFuncs: []chplan.AggFunc{
			{Fn: chplan.FnSum, Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}, Alias: "sum_value"},
		},
	}
}

// rateWindow is the RangeWindow shape the matrix's window-shaped plans
// share. It is an instant windowed-array leaf (OuterRange == 0, non-metrics
// Input), so NormalizeScanTimeBound is applicable to it — which is what
// lets one RangeWindow shape serve both the transpose pairs and the
// scan-time-bound pairs.
func rateWindow(input chplan.Node) *chplan.RangeWindow {
	return &chplan.RangeWindow{
		Input:           input,
		Func:            "rate",
		Range:           5 * time.Minute,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
}

// setOp builds a canonical-column VectorSetOp. Every link of a chain uses
// the same column names so FlattenVectorSetOp's same-shape guard is
// satisfied and the chain actually linearises.
func setOp(op chplan.VectorSetOpKind, l, r chplan.Node) *chplan.VectorSetOp {
	return &chplan.VectorSetOp{
		Left: l, Right: r, Op: op,
		MetricNameColumn: "MetricName", AttributesColumn: "Attributes",
		TimestampColumn: "TimeUnix", ValueColumn: "Value",
	}
}

// orChain wraps arm in a two-link `or` chain — `((arm or b) or c)` — the
// left-leaning shape FlattenVectorSetOp collapses into one N-ary node. The
// pair's other rule fires inside `arm`, so both rules see the same tree and
// the flatten's arm-freezing order is what is under test.
func orChain(arm chplan.Node) chplan.Node {
	return setOp(chplan.VectorSetOr,
		setOp(chplan.VectorSetOr, arm, &chplan.Scan{Table: "b"}),
		&chplan.Scan{Table: "c"})
}

// pairPlans maps a pairKey to a builder for the plan shape that makes BOTH
// rules of the pair applicable. Keys are built by pairKey from the rule
// values themselves, so a key always names a real pair.
// TestRuleInteractionMatrix fails on any pair missing from here.
var pairPlans = map[string]func() chplan.Node{
	// ConstantFoldSemantic × ConstantFoldHeuristic.
	// `(1+2=3) AND X`: semantic folds the arithmetic to `true`, heuristic
	// then collapses `true AND X → X`.
	pairKey(ruleFoldSemantic, ruleFoldHeuristic): func() chplan.Node {
		return &chplan.Filter{
			Input: &chplan.Scan{Table: "otel_metrics_gauge"},
			Predicate: &chplan.Binary{
				Op: chplan.OpAnd,
				Left: &chplan.Binary{
					Op:    chplan.OpEq,
					Left:  &chplan.Binary{Op: chplan.OpAdd, Left: &chplan.LitInt{V: 1}, Right: &chplan.LitInt{V: 2}},
					Right: &chplan.LitInt{V: 3},
				},
				Right: labelFilter("MetricName", "up"),
			},
		}
	},

	// ConstantFoldSemantic × FilterFusion.
	// Filter(Filter(scan, p1), 1=1) — semantic collapses `1=1 → true`;
	// fusion combines into a single Filter.
	pairKey(ruleFoldSemantic, ruleFilterFusion): func() chplan.Node {
		return &chplan.Filter{
			Input: &chplan.Filter{
				Input:     &chplan.Scan{Table: "otel_metrics_gauge"},
				Predicate: labelFilter("MetricName", "up"),
			},
			Predicate: trueEq(1),
		}
	},

	// ConstantFoldSemantic × FilterAggregateTranspose.
	pairKey(ruleFoldSemantic, ruleAggTranspose): func() chplan.Node {
		return &chplan.Filter{
			Input: sumAgg(&chplan.Scan{Table: "otel_metrics_gauge"}, "job"),
			Predicate: &chplan.Binary{
				Op:    chplan.OpAnd,
				Left:  trueEq(1),
				Right: labelFilter("job", "api"),
			},
		}
	},

	// ConstantFoldSemantic × FilterRangeWindowTranspose.
	pairKey(ruleFoldSemantic, ruleRWTranspose): func() chplan.Node {
		return &chplan.Filter{
			Input: rateWindow(&chplan.Scan{Table: "otel_metrics_sum"}),
			Predicate: &chplan.Binary{
				Op:    chplan.OpAnd,
				Left:  trueEq(2),
				Right: labelFilter("Attributes", "irrelevant"), // bare column ref over passthrough
			},
		}
	},

	// ConstantFoldSemantic × ProjectionPushdown.
	// Disjoint targets (expression slots vs Scan.Columns) but pin
	// commutation. `Value * (2 + 3)`: semantic folds the pure-literal
	// `2 + 3` sub-expression to 5 while ProjectionPushdown narrows the
	// Scan to [Value] — the scaling literal is deliberately a nested
	// binary rather than a bare one, because semantic only folds a Binary
	// whose BOTH operands are literals and a projection like `Value + 0`
	// gives it nothing to do.
	pairKey(ruleFoldSemantic, ruleProjPushdown): func() chplan.Node {
		return &chplan.Project{
			Input: &chplan.Scan{Table: "otel_metrics_gauge"},
			Projections: []chplan.Projection{
				{
					Expr: &chplan.Binary{
						Op:   chplan.OpMul,
						Left: &chplan.ColumnRef{Name: "Value"},
						Right: &chplan.Binary{
							Op:    chplan.OpAdd,
							Left:  &chplan.LitInt{V: 2},
							Right: &chplan.LitInt{V: 3},
						},
					},
					Alias: "scaled",
				},
			},
		}
	},

	// ConstantFoldSemantic × NormalizeScanTimeBound.
	// The fold target sits in the RangeWindow's own input Filter, so both
	// rules rewrite the same branch: normalize clones the RangeWindow to
	// stamp InstantScanBounded, the fold rewrites the Filter underneath it.
	pairKey(ruleFoldSemantic, ruleNormalizeBound): func() chplan.Node {
		return rateWindow(&chplan.Filter{
			Input: &chplan.Scan{Table: "otel_metrics_sum"},
			Predicate: &chplan.Binary{
				Op:    chplan.OpAnd,
				Left:  trueEq(3),
				Right: labelFilter("MetricName", "up"),
			},
		})
	},

	// ConstantFoldSemantic × FlattenVectorSetOp.
	pairKey(ruleFoldSemantic, ruleFlattenSetOp): func() chplan.Node {
		return orChain(&chplan.Filter{
			Input: &chplan.Scan{Table: "a"},
			Predicate: &chplan.Binary{
				Op:    chplan.OpAnd,
				Left:  trueEq(1),
				Right: labelFilter("MetricName", "up"),
			},
		})
	},

	// ConstantFoldHeuristic × FilterFusion.
	// Filter(Filter(scan, p1), true AND p2) — heuristic collapses
	// `true AND p2 → p2`, fusion merges.
	pairKey(ruleFoldHeuristic, ruleFilterFusion): func() chplan.Node {
		return &chplan.Filter{
			Input: &chplan.Filter{
				Input:     &chplan.Scan{Table: "otel_metrics_gauge"},
				Predicate: labelFilter("MetricName", "up"),
			},
			Predicate: &chplan.Binary{
				Op:    chplan.OpAnd,
				Left:  &chplan.LitBool{V: true},
				Right: labelFilter("job", "api"),
			},
		}
	},

	// ConstantFoldHeuristic × FilterAggregateTranspose.
	pairKey(ruleFoldHeuristic, ruleAggTranspose): func() chplan.Node {
		return &chplan.Filter{
			Input: sumAgg(&chplan.Scan{Table: "otel_metrics_gauge"}, "job"),
			Predicate: &chplan.Binary{
				Op:    chplan.OpAnd,
				Left:  &chplan.LitBool{V: true},
				Right: labelFilter("job", "api"),
			},
		}
	},

	// ConstantFoldHeuristic × FilterRangeWindowTranspose.
	pairKey(ruleFoldHeuristic, ruleRWTranspose): func() chplan.Node {
		return &chplan.Filter{
			Input: rateWindow(&chplan.Scan{Table: "otel_metrics_sum"}),
			Predicate: &chplan.Binary{
				Op:    chplan.OpAnd,
				Left:  &chplan.LitBool{V: true},
				Right: labelFilter("Attributes", "irrelevant"),
			},
		}
	},

	// ConstantFoldHeuristic × ProjectionPushdown.
	// The `true AND <p>` projection is what makes the heuristic
	// applicable here; a projection list of bare ColumnRefs alone gives it
	// nothing to collapse. Narrowing is unaffected either way — the
	// surviving predicate reads the same column the unfolded one did.
	pairKey(ruleFoldHeuristic, ruleProjPushdown): func() chplan.Node {
		return &chplan.Project{
			Input: &chplan.Scan{Table: "otel_metrics_gauge"},
			Projections: []chplan.Projection{
				{Expr: &chplan.ColumnRef{Name: "Value"}, Alias: "v"},
				{
					Expr: &chplan.Binary{
						Op:    chplan.OpAnd,
						Left:  &chplan.LitBool{V: true},
						Right: labelFilter("MetricName", "up"),
					},
					Alias: "is_up",
				},
			},
		}
	},

	// ConstantFoldHeuristic × NormalizeScanTimeBound.
	pairKey(ruleFoldHeuristic, ruleNormalizeBound): func() chplan.Node {
		return rateWindow(&chplan.Filter{
			Input: &chplan.Scan{Table: "otel_metrics_sum"},
			Predicate: &chplan.Binary{
				Op:    chplan.OpAnd,
				Left:  &chplan.LitBool{V: true},
				Right: labelFilter("MetricName", "up"),
			},
		})
	},

	// ConstantFoldHeuristic × FlattenVectorSetOp.
	pairKey(ruleFoldHeuristic, ruleFlattenSetOp): func() chplan.Node {
		return orChain(&chplan.Filter{
			Input: &chplan.Scan{Table: "a"},
			Predicate: &chplan.Binary{
				Op:    chplan.OpAnd,
				Left:  &chplan.LitBool{V: true},
				Right: labelFilter("MetricName", "up"),
			},
		})
	},

	// FilterFusion × FilterAggregateTranspose.
	pairKey(ruleFilterFusion, ruleAggTranspose): func() chplan.Node {
		return &chplan.Filter{
			Input: &chplan.Filter{
				Input:     sumAgg(&chplan.Scan{Table: "otel_metrics_gauge"}, "job"),
				Predicate: labelFilter("job", "api"),
			},
			Predicate: labelFilter("job", "web"),
		}
	},

	// FilterFusion × FilterRangeWindowTranspose.
	pairKey(ruleFilterFusion, ruleRWTranspose): func() chplan.Node {
		return &chplan.Filter{
			Input: &chplan.Filter{
				Input:     rateWindow(&chplan.Scan{Table: "otel_metrics_sum"}),
				Predicate: labelFilter("Attributes", "v1"),
			},
			Predicate: labelFilter("Attributes", "v2"),
		}
	},

	// FilterFusion × ProjectionPushdown.
	// Disjoint shapes today (fusion fires on Filter(Filter), pushdown on
	// Project(Scan)). Pin commutation as a forward guarantee.
	pairKey(ruleFilterFusion, ruleProjPushdown): func() chplan.Node {
		return &chplan.Filter{
			Input: &chplan.Filter{
				Input: &chplan.Project{
					Input: &chplan.Scan{Table: "otel_metrics_gauge"},
					Projections: []chplan.Projection{
						{Expr: &chplan.ColumnRef{Name: "MetricName"}},
						{Expr: &chplan.ColumnRef{Name: "Value"}},
					},
				},
				Predicate: labelFilter("MetricName", "up"),
			},
			Predicate: labelFilter("MetricName", "down"),
		}
	},

	// FilterFusion × NormalizeScanTimeBound.
	// The two adjacent Filters sit UNDER the RangeWindow, so fusion
	// rewrites the subtree normalize is stamping the parent of.
	pairKey(ruleFilterFusion, ruleNormalizeBound): func() chplan.Node {
		return rateWindow(&chplan.Filter{
			Input: &chplan.Filter{
				Input:     &chplan.Scan{Table: "otel_metrics_sum"},
				Predicate: labelFilter("MetricName", "up"),
			},
			Predicate: labelFilter("job", "api"),
		})
	},

	// FilterFusion × FlattenVectorSetOp.
	pairKey(ruleFilterFusion, ruleFlattenSetOp): func() chplan.Node {
		return orChain(&chplan.Filter{
			Input: &chplan.Filter{
				Input:     &chplan.Scan{Table: "a"},
				Predicate: labelFilter("MetricName", "up"),
			},
			Predicate: labelFilter("job", "api"),
		})
	},

	// FilterAggregateTranspose × FilterRangeWindowTranspose.
	pairKey(ruleAggTranspose, ruleRWTranspose): func() chplan.Node {
		return &chplan.Filter{
			Input:     rateWindow(sumAgg(&chplan.Scan{Table: "otel_metrics_sum"}, "Attributes")),
			Predicate: labelFilter("Attributes", "v1"),
		}
	},

	// FilterAggregateTranspose × ProjectionPushdown.
	pairKey(ruleAggTranspose, ruleProjPushdown): func() chplan.Node {
		return &chplan.Filter{
			Input: sumAgg(&chplan.Project{
				Input: &chplan.Scan{Table: "otel_metrics_gauge"},
				Projections: []chplan.Projection{
					{Expr: &chplan.ColumnRef{Name: "job"}},
					{Expr: &chplan.ColumnRef{Name: "Value"}},
				},
			}, "job"),
			Predicate: labelFilter("job", "api"),
		}
	},

	// FilterAggregateTranspose × NormalizeScanTimeBound.
	// Filter(Aggregate(Scan)) sits under the instant windowed leaf, so the
	// transpose rewrites inside the very node normalize stamps.
	pairKey(ruleAggTranspose, ruleNormalizeBound): func() chplan.Node {
		return rateWindow(&chplan.Filter{
			Input:     sumAgg(&chplan.Scan{Table: "otel_metrics_sum"}, "Attributes"),
			Predicate: labelFilter("Attributes", "v1"),
		})
	},

	// FilterAggregateTranspose × FlattenVectorSetOp.
	pairKey(ruleAggTranspose, ruleFlattenSetOp): func() chplan.Node {
		return orChain(&chplan.Filter{
			Input:     sumAgg(&chplan.Scan{Table: "a"}, "job"),
			Predicate: labelFilter("job", "api"),
		})
	},

	// FilterRangeWindowTranspose × ProjectionPushdown.
	pairKey(ruleRWTranspose, ruleProjPushdown): func() chplan.Node {
		return &chplan.Filter{
			Input: rateWindow(&chplan.Project{
				Input: &chplan.Scan{Table: "otel_metrics_sum"},
				Projections: []chplan.Projection{
					{Expr: &chplan.ColumnRef{Name: "Attributes"}},
					{Expr: &chplan.ColumnRef{Name: "Value"}},
					{Expr: &chplan.ColumnRef{Name: "TimeUnix"}},
				},
			}),
			Predicate: labelFilter("Attributes", "v1"),
		}
	},

	// FilterRangeWindowTranspose × NormalizeScanTimeBound.
	// The head-on pair: both rules target the SAME RangeWindow node — the
	// transpose rebuilds it with the pushed-down Filter as its Input, the
	// normalize stamps it with InstantScanBounded. Either order must
	// converge on a bound RangeWindow whose Filter has been pushed under
	// it; a rebuild that dropped the stamp would show up here.
	pairKey(ruleRWTranspose, ruleNormalizeBound): func() chplan.Node {
		return &chplan.Filter{
			Input:     rateWindow(&chplan.Scan{Table: "otel_metrics_sum"}),
			Predicate: labelFilter("Attributes", "v1"),
		}
	},

	// FilterRangeWindowTranspose × FlattenVectorSetOp.
	pairKey(ruleRWTranspose, ruleFlattenSetOp): func() chplan.Node {
		return orChain(&chplan.Filter{
			Input:     rateWindow(&chplan.Scan{Table: "a"}),
			Predicate: labelFilter("Attributes", "v1"),
		})
	},

	// ProjectionPushdown × NormalizeScanTimeBound.
	// RangeWindow(Scan) is simultaneously ProjectionPushdown's stage-node
	// shape (4) — it narrows the Scan to the ts/value/group columns — and
	// the instant windowed leaf normalize stamps.
	pairKey(ruleProjPushdown, ruleNormalizeBound): func() chplan.Node {
		return rateWindow(&chplan.Scan{Table: "otel_metrics_sum"})
	},

	// ProjectionPushdown × FlattenVectorSetOp.
	pairKey(ruleProjPushdown, ruleFlattenSetOp): func() chplan.Node {
		return orChain(&chplan.Project{
			Input: &chplan.Scan{Table: "a"},
			Projections: []chplan.Projection{
				{Expr: &chplan.ColumnRef{Name: "MetricName"}},
				{Expr: &chplan.ColumnRef{Name: "Value"}},
			},
		})
	},

	// NormalizeScanTimeBound × FlattenVectorSetOp.
	// A set-op arm that is itself an instant windowed leaf: the flatten
	// freezes the arm into an N-ary node, normalize stamps the arm. Either
	// order must produce a flattened chain whose RangeWindow arm is bound —
	// an arm frozen BEFORE it was stamped, and then never revisited, would
	// diverge here.
	pairKey(ruleNormalizeBound, ruleFlattenSetOp): func() chplan.Node {
		return orChain(rateWindow(&chplan.Scan{Table: "a"}))
	},
}
