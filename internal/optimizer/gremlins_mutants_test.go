// Tests in this file pin behaviour that the gremlins mutation suite had
// reported as LIVED on the phase3-optimizer job — each one constructs an
// input that observably differentiates the original code from the
// mutated branch, so the test fails when the mutant is applied and the
// mutant is reported KILLED.
//
// See `.gremlins.yaml` for the mutation operators in play; the mutant
// IDs in each test's doc comment refer to gremlins's `file:line:col`
// notation as printed in the workflow logs.
package optimizer_test

import (
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/optimizer"
)

// TestFilterAggregateTranspose_SkipsNonColumnRefGroupKeyButKeepsLater
// pins the `continue` at filter_aggregate_transpose.go:99 inside
// passthroughGroupKeys. The check
//
//	if !ok || cr.Qualifier != "" { continue }
//
// skips one ill-shaped group key while still considering the remaining
// keys; flipping the `continue` to `break` (gremlins INVERT_LOOPCTRL)
// would abort the loop on the first non-bare key and drop every later
// bare-column key from the passthrough set, blocking the rewrite.
//
// Input: GROUP BY substr(MetricName, 1), job — a computed key followed
// by a bare column reference. Predicate references the bare key. With
// the original `continue`, "job" lands in passthrough and the rule
// fires. A `break` mutant would return nil passthrough.
func TestFilterAggregateTranspose_SkipsNonColumnRefGroupKeyButKeepsLater(t *testing.T) {
	t.Parallel()

	scan := &chplan.Scan{Table: "otel_metrics_gauge"}
	input := &chplan.Filter{
		Input: &chplan.Aggregate{
			Input: scan,
			GroupBy: []chplan.Expr{
				&chplan.FuncCall{
					Name: "substr",
					Args: []chplan.Expr{&chplan.ColumnRef{Name: "MetricName"}, &chplan.LitInt{V: 1}},
				},
				&chplan.ColumnRef{Name: "job"},
			},
			AggFuncs: []chplan.AggFunc{
				{Name: "count", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}, Alias: "n"},
			},
		},
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: "job"},
			Right: &chplan.LitString{V: "api"},
		},
	}

	out, changed := optimizer.FilterAggregateTranspose().Apply(input)
	if !changed {
		t.Fatalf("expected transpose to fire (later bare-column key 'job' should still pass into passthrough); got changed=false")
	}
	agg, ok := out.(*chplan.Aggregate)
	if !ok {
		t.Fatalf("expected *Aggregate at root after transpose, got %T", out)
	}
	if _, ok := agg.Input.(*chplan.Filter); !ok {
		t.Fatalf("expected Filter under Aggregate, got %T", agg.Input)
	}
}

// TestFilterAggregateTranspose_EmptyAliasIsTreatedAsNoRename pins the
// `alias != ""` half of the alias-mismatch guard at
// filter_aggregate_transpose.go:103. The check
//
//	if alias != "" && alias != cr.Name { continue }
//
// treats the empty string as "no rename" — the key still flows into
// passthrough. Flipping `alias != ""` to `alias == ""` (gremlins
// CONDITIONALS_NEGATION) would swap the meaning: an empty alias would
// satisfy the first leg, the second leg "" != cr.Name is true for any
// non-empty key name, the guard fires, the key is dropped from
// passthrough, and the rule declines.
//
// Input: GROUP BY job with GroupByAliases = [""] — i.e. an aliases
// slice is present but the alias for "job" is the empty string. The
// rule must still fire.
func TestFilterAggregateTranspose_EmptyAliasIsTreatedAsNoRename(t *testing.T) {
	t.Parallel()

	scan := &chplan.Scan{Table: "otel_metrics_gauge"}
	input := &chplan.Filter{
		Input: &chplan.Aggregate{
			Input:          scan,
			GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: "job"}},
			GroupByAliases: []string{""},
			AggFuncs: []chplan.AggFunc{
				{Name: "count", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}, Alias: "n"},
			},
		},
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: "job"},
			Right: &chplan.LitString{V: "api"},
		},
	}

	out, changed := optimizer.FilterAggregateTranspose().Apply(input)
	if !changed {
		t.Fatalf("expected transpose to fire (empty alias is a no-rename); got changed=false")
	}
	agg, ok := out.(*chplan.Aggregate)
	if !ok {
		t.Fatalf("expected *Aggregate at root, got %T", out)
	}
	if _, ok := agg.Input.(*chplan.Filter); !ok {
		t.Fatalf("expected Filter under Aggregate, got %T", agg.Input)
	}
}

// TestFilterAggregateTranspose_RenamedAliasSkipsKeyButKeepsLater pins
// the `continue` at filter_aggregate_transpose.go:104 inside the alias
// branch:
//
//	if alias != "" && alias != cr.Name { continue }
//
// The loop must skip the renamed key but keep iterating; a `break`
// mutant (gremlins INVERT_LOOPCTRL) would abort on the first renamed
// key and drop every later valid bare-column entry from passthrough.
//
// Input: GROUP BY job, env with GroupByAliases = ["renamed_job", "env"]
// — the first entry is renamed away from its column, the second is a
// no-rename. Predicate references "env". The original `continue` lands
// "env" in passthrough and the rule fires; a `break` would skip "env"
// and the rule would decline.
func TestFilterAggregateTranspose_RenamedAliasSkipsKeyButKeepsLater(t *testing.T) {
	t.Parallel()

	scan := &chplan.Scan{Table: "otel_metrics_gauge"}
	input := &chplan.Filter{
		Input: &chplan.Aggregate{
			Input: scan,
			GroupBy: []chplan.Expr{
				&chplan.ColumnRef{Name: "job"},
				&chplan.ColumnRef{Name: "env"},
			},
			GroupByAliases: []string{"renamed_job", "env"},
			AggFuncs: []chplan.AggFunc{
				{Name: "count", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}, Alias: "n"},
			},
		},
		Predicate: &chplan.Binary{
			Op:    chplan.OpEq,
			Left:  &chplan.ColumnRef{Name: "env"},
			Right: &chplan.LitString{V: "prod"},
		},
	}

	out, changed := optimizer.FilterAggregateTranspose().Apply(input)
	if !changed {
		t.Fatalf("expected transpose to fire (predicate on 'env' which has a no-rename alias); got changed=false")
	}
	agg, ok := out.(*chplan.Aggregate)
	if !ok {
		t.Fatalf("expected *Aggregate at root, got %T", out)
	}
	if _, ok := agg.Input.(*chplan.Filter); !ok {
		t.Fatalf("expected Filter under Aggregate, got %T", agg.Input)
	}
}

// TestConstantFoldSemantic_MapAccessFoldsWhenOnlyMapChanges pins the
// `if !mc && !kc` early-return guard at constant_fold.go:144 inside
// foldExprSemantic's MapAccess case. The condition is "neither child
// changed — return the original Node unchanged"; flipping `&&` to `||`
// (gremlins INVERT_LOGICAL) would early-return whenever EITHER child
// is unchanged, which means a MapAccess whose Map sub-expression
// folded but whose Key did NOT (the realistic shape, since keys are
// usually plain column references or string literals) would silently
// keep the unfolded Map sub-expression.
//
// Input: MapAccess{ Map: Binary{1 + 2}, Key: LitString{"foo"} } — the
// Map is a pure-literal arithmetic subtree (foldable), the Key is a
// literal (nothing to fold). The folded result must be a MapAccess
// whose Map is `LitInt(3)`.
func TestConstantFoldSemantic_MapAccessFoldsWhenOnlyMapChanges(t *testing.T) {
	t.Parallel()

	mapExpr := &chplan.MapAccess{
		Map: &chplan.Binary{
			Op:    chplan.OpAdd,
			Left:  &chplan.LitInt{V: 1},
			Right: &chplan.LitInt{V: 2},
		},
		Key: &chplan.LitString{V: "foo"},
	}
	plan := &chplan.Project{
		Input: &chplan.Scan{Table: "t"},
		Projections: []chplan.Projection{
			{Expr: mapExpr, Alias: "val"},
		},
	}

	out, changed := optimizer.ConstantFoldSemantic{}.Apply(plan)
	if !changed {
		t.Fatalf("ConstantFoldSemantic should have folded the Map sub-expression of MapAccess (1+2 → 3)")
	}

	proj, ok := out.(*chplan.Project)
	if !ok {
		t.Fatalf("expected *Project, got %T", out)
	}
	ma, ok := proj.Projections[0].Expr.(*chplan.MapAccess)
	if !ok {
		t.Fatalf("expected projection expr *MapAccess, got %T", proj.Projections[0].Expr)
	}
	li, ok := ma.Map.(*chplan.LitInt)
	if !ok {
		t.Fatalf("expected folded MapAccess.Map to be *LitInt, got %T", ma.Map)
	}
	if li.V != 3 {
		t.Fatalf("expected folded MapAccess.Map = LitInt(3), got LitInt(%d)", li.V)
	}
}

// TestConstantFoldHeuristic_MapAccessFoldsWhenOnlyMapChanges pins the
// `if !mc && !kc` early-return guard at constant_fold.go:190 inside
// foldExprHeuristic's MapAccess case. Same shape as the semantic test
// above, but exercises the boolean-identity heuristic instead of the
// pure-literal arithmetic semantic pass.
//
// Input: MapAccess{ Map: Binary{ true AND X }, Key: ColumnRef{"k"} }.
// The Map sub-expression is `true AND X`, which the heuristic should
// collapse to `X`. The Key is a plain ColumnRef — nothing to fold. If
// the early-return guard is flipped from `&&` to `||`, the rule would
// see kc=false and bail before propagating the Map-side fold.
func TestConstantFoldHeuristic_MapAccessFoldsWhenOnlyMapChanges(t *testing.T) {
	t.Parallel()

	innerCol := &chplan.ColumnRef{Name: "X"}
	mapExpr := &chplan.MapAccess{
		Map: &chplan.Binary{
			Op:    chplan.OpAnd,
			Left:  &chplan.LitBool{V: true},
			Right: innerCol,
		},
		Key: &chplan.ColumnRef{Name: "k"},
	}
	plan := &chplan.Project{
		Input: &chplan.Scan{Table: "t"},
		Projections: []chplan.Projection{
			{Expr: mapExpr, Alias: "val"},
		},
	}

	out, changed := optimizer.ConstantFoldHeuristic{}.Apply(plan)
	if !changed {
		t.Fatalf("ConstantFoldHeuristic should have collapsed `true AND X` inside MapAccess.Map")
	}

	proj, ok := out.(*chplan.Project)
	if !ok {
		t.Fatalf("expected *Project, got %T", out)
	}
	ma, ok := proj.Projections[0].Expr.(*chplan.MapAccess)
	if !ok {
		t.Fatalf("expected projection expr *MapAccess, got %T", proj.Projections[0].Expr)
	}
	if ma.Map != chplan.Expr(innerCol) {
		t.Fatalf("expected MapAccess.Map to collapse to inner ColumnRef X, got %#v", ma.Map)
	}
}

// TestCapturePattern_PreservesInnerBindings pins the `inner == nil`
// branch at pattern.go:140 inside capturePattern.Match:
//
//	if inner == nil { inner = Bindings{} }
//
// Capture wraps an inner pattern and adds its own (name, node)
// binding. When the inner pattern returns a non-nil Bindings map
// (e.g., it itself is a Capture), the outer Capture must add to that
// existing map rather than reinitialise it. Flipping `==` to `!=`
// (gremlins CONDITIONALS_NEGATION) would reinitialise on every match
// where the inner already produced bindings — dropping the inner's
// captures entirely.
//
// Input: a nested Capture pair `Capture("outer", Capture("inner",
// Kind(KindScan)))` matched against a Scan. The match must yield both
// "outer" and "inner" bindings pointing at the same Scan.
func TestCapturePattern_PreservesInnerBindings(t *testing.T) {
	t.Parallel()

	scan := &chplan.Scan{Table: "otel_logs"}
	pat := optimizer.Capture(
		"outer",
		optimizer.Capture("inner", optimizer.Kind(optimizer.KindScan)),
	)

	b, ok := pat.Match(scan)
	if !ok {
		t.Fatalf("expected nested Capture to match a Scan")
	}
	gotOuter, hasOuter := b.Get("outer")
	if !hasOuter {
		t.Fatalf("expected outer binding to be present")
	}
	if gotOuter != scan {
		t.Errorf("outer binding mismatch: got %p, want %p", gotOuter, scan)
	}
	gotInner, hasInner := b.Get("inner")
	if !hasInner {
		t.Fatalf("expected inner binding to be preserved by outer Capture (CONDITIONALS_NEGATION mutant `==` → `!=` would reinit the bindings map and drop 'inner')")
	}
	if gotInner != scan {
		t.Errorf("inner binding mismatch: got %p, want %p", gotInner, scan)
	}
}

// TestConstantFold_FloatLtIsStrictAtEquality pins the `l < r` fold at
// constant_fold.go:327 inside foldFloatFloat's OpLt case. A gremlins
// CONDITIONALS_BOUNDARY mutant flips `<` to `<=`, which only changes the
// result when the two operands are EQUAL: `5.0 < 5.0` is false but
// `5.0 <= 5.0` is true. Every non-equal operand pair folds identically
// under both, so the equality case is the sole observable witness.
//
// Input: Binary{OpLt, LitFloat(5), LitFloat(5)} inside a projection.
// The semantic fold must produce LitBool(false); the `<=` mutant would
// produce LitBool(true).
func TestConstantFold_FloatLtIsStrictAtEquality(t *testing.T) {
	t.Parallel()

	cmp := &chplan.Binary{
		Op:    chplan.OpLt,
		Left:  &chplan.LitFloat{V: 5},
		Right: &chplan.LitFloat{V: 5},
	}
	plan := &chplan.Project{
		Input: &chplan.Scan{Table: "t"},
		Projections: []chplan.Projection{
			{Expr: cmp, Alias: "lt"},
		},
	}

	out, changed := optimizer.ConstantFoldSemantic{}.Apply(plan)
	if !changed {
		t.Fatalf("ConstantFoldSemantic should fold `5.0 < 5.0` to a literal bool")
	}
	proj, ok := out.(*chplan.Project)
	if !ok {
		t.Fatalf("expected *Project, got %T", out)
	}
	lb, ok := proj.Projections[0].Expr.(*chplan.LitBool)
	if !ok {
		t.Fatalf("expected folded expr *LitBool, got %T", proj.Projections[0].Expr)
	}
	if lb.V {
		t.Fatalf("expected `5.0 < 5.0` to fold to false (CONDITIONALS_BOUNDARY `<`→`<=` would yield true), got true")
	}
}

// TestRunBatch_AnalyzerBranchCountsEachChange pins the `rulesApplied++`
// at rule.go:187 inside runBatch's analyzer branch. A gremlins
// INCREMENT_DECREMENT mutant flips `++` to `--`, so a changing analyzer
// rule would DECREMENT the counter (driving it negative) rather than
// incrementing it. Driver.Run only surfaces the counter via telemetry,
// so we drive runBatch directly via the RunBatchForTest seam and assert
// the exact post-count.
//
// Input: an AnalyzerBatch with one IdempotentTestAnalyzerRule that
// rewrites Scan{raw}→Scan{canon} exactly once. Starting from
// rulesApplied=0 the single change must leave the counter at 1; the `--`
// mutant would leave it at -1.
func TestRunBatch_AnalyzerBranchCountsEachChange(t *testing.T) {
	t.Parallel()

	calls := 0
	batch := optimizer.AnalyzerBatch(
		"count-analyzer",
		optimizer.IdempotentTestAnalyzerRule{Calls: &calls},
	)

	_, got := optimizer.RunBatchForTest(&chplan.Scan{Table: "raw"}, batch, 0)
	if got != 1 {
		t.Fatalf("analyzer batch with one change must increment rulesApplied to 1 (INCREMENT_DECREMENT `++`→`--` would give -1), got %d", got)
	}
}

// TestRunBatch_FixedPointBranchCountsEachChange pins the `rulesApplied++`
// at rule.go:200 inside runBatch's FixedPoint branch. Same INCREMENT_
// DECREMENT mutant class as the analyzer-branch test above, but on the
// iterative branch.
//
// Input: a FixedPoint batch with renamingRule a→b. The fixpoint loop
// changes once (iter 1: a→b) then converges (iter 2: no change), so the
// counter must end at exactly 1; the `--` mutant would give -1.
func TestRunBatch_FixedPointBranchCountsEachChange(t *testing.T) {
	t.Parallel()

	var calls int
	batch := optimizer.Batch{
		Name:     "count-fixedpoint",
		Strategy: optimizer.FixedPoint(10),
		Rules:    []optimizer.Rule{renamingRule("rename", "a", "b", &calls)},
	}

	_, got := optimizer.RunBatchForTest(&chplan.Scan{Table: "a"}, batch, 0)
	if got != 1 {
		t.Fatalf("FixedPoint batch with one change must increment rulesApplied to 1 (INCREMENT_DECREMENT `++`→`--` would give -1), got %d", got)
	}
}

// matchAllPattern is a Pattern that matches EVERY node, including nil,
// recording nothing. It exists to witness the rule_pattern.go:53 guard:
// stock kindPattern.Match(nil) returns (nil,false), so it can't reveal
// whether Apply's nil-guard short-circuited before calling Match. A
// pattern that matches nil makes the difference observable.
type matchAllPattern struct{}

func (matchAllPattern) Match(_ chplan.Node) (optimizer.Bindings, bool) {
	return optimizer.Bindings{}, true
}

// TestPatternRule_ApplyNilShortCircuitsBeforeMatch pins the leading
// `n == nil ||` term of the nil-guard at rule_pattern.go:53:
//
//	if n == nil || r == nil || r.Match == nil || r.Transform == nil {
//		return n, false
//	}
//
// A gremlins INVERT_LOGICAL mutant flips the first `||` to `&&`, yielding
// `(n == nil && r == nil) || r.Match == nil || r.Transform == nil`. With
// a fully-populated rule (Match + Transform both non-nil) the whole guard
// then evaluates false even when n is nil, so Apply falls through to
// `r.Match.Match(nil)` instead of short-circuiting.
//
// Input: a PatternRule whose Match matches nil and whose Transform emits
// a sentinel, called via Apply(nil). Original: n==nil short-circuits →
// (nil,false), Transform never runs. Mutant: guard false → Match(nil)
// succeeds → Transform fires → (sentinel,true).
func TestPatternRule_ApplyNilShortCircuitsBeforeMatch(t *testing.T) {
	t.Parallel()

	sentinel := &chplan.Scan{Table: "TRANSFORMED"}
	rule := &optimizer.PatternRule{
		RuleName:  "match-all",
		Match:     matchAllPattern{},
		Transform: func(_ optimizer.Bindings) chplan.Node { return sentinel },
	}

	out, changed := rule.Apply(nil)
	if changed {
		t.Fatalf("Apply(nil) must short-circuit on the `n == nil` guard (INVERT_LOGICAL `||`→`&&` would call Match(nil) and fire Transform), got changed=true")
	}
	if out != nil {
		t.Fatalf("Apply(nil) must return nil node, got %#v", out)
	}
}
