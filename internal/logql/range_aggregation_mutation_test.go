package logql

import (
	"context"
	"testing"
	"time"

	syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// This file pins the range-aggregation mutants gremlins reports LIVED in
// the phase4-logql-aggregation lane. Each test reaches the mutated line
// and asserts the original behaviour so tightly the mutation breaks it.
//
// EQUIVALENCE verdicts (no killing test possible — left documented here so
// a future agent doesn't burn time re-attempting them):
//
//   - range_aggregation.go:`len(e.Left.Unwrap.PostFilters) > 0`
//     CONDITIONALS_BOUNDARY `> 0` → `>= 0`. The mutant differs from the
//     original ONLY at len==0. At that point `inner` is always a
//     *chplan.Filter (the andFilter call just above unconditionally wraps
//     it), so applyUnwrapPostFilters with an empty filter slice
//     destructures that Filter (`pred = f.Predicate; inner = f.Input`) and
//     reconstructs an identical
//     `&chplan.Filter{Input: f.Input, Predicate: f.Predicate}`, while
//     wrapLabelsWithMarks(labelsExpr, nil) returns labelsExpr verbatim
//     (duration.go:`len(marks) == 0`) and the hasMarks return stays false.
//     The emitted plan is structurally identical, so no assertion can
//     observe the flip. Genuinely equivalent.
//
// A second entry stood here, on rangeAggregationGroupBy's grouping-map
// capacity hint, and it was wrong (cerberus issue #2984). It reasoned that
// the loop appends exactly two entries per group regardless, so the slice's
// contents, length and order are unchanged, and that capacity is not
// observable in the emitted SQL or plan. Every clause of that is true and
// the conclusion does not follow: cap is readable on any slice a test can
// reach, and this one IS the map FuncCall the function returns. Its verdict
// now lives with the kill, in
// [TestRangeAggregationGroupBy_CapHintMutantKilled], which is where the
// citation went too — a mutant carries one verdict, and leaving the
// citation in this ledger would leave it carrying two.

// TestAbsentOverTimeExtendsMatcherWindowByIntervalPlusOffset pins the
// ARITHMETIC_BASE mutant on
// range_aggregation.go:lowerAbsentOverTime:`lc.withMatcherWindowExtension(e.Left.Interval + e.Left.Offset)`.
//
// This is the absent_over_time twin of the per-series window-extension
// arithmetic already pinned for the ordinary path by
// [TestLowerRangeAggregationExtendsMatcherWindowByIntervalPlusOffset].
// The `+` must extend the inner selector's pre-scan timestamp clamp back
// by Interval+Offset so the leftmost matrix anchor sees its full lookback.
// An ARITHMETIC_BASE flip to `-` yields Interval-Offset (a smaller, wrong
// extension). With asymmetric Interval=10m / Offset=3m the two produce
// distinct, unambiguous clamp timestamps (13m vs 7m back from Start).
func TestAbsentOverTimeExtendsMatcherWindowByIntervalPlusOffset(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()

	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	end := start.Add(1 * time.Hour)
	step := time.Minute

	query := `absent_over_time({app="api"}[10m] offset 3m)`
	expr, err := syntax.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	ra, ok := expr.(*syntax.RangeAggregationExpr)
	if !ok {
		t.Fatalf("ParseExpr(%q) -> %T, want *syntax.RangeAggregationExpr", query, expr)
	}
	if ra.Operation != syntax.OpRangeTypeAbsent {
		t.Fatalf("fixture invalid: Operation = %q, want absent_over_time", ra.Operation)
	}
	if ra.Left.Interval != 10*time.Minute || ra.Left.Offset != 3*time.Minute {
		t.Fatalf("fixture invalid: Interval=%v Offset=%v, want 10m / 3m", ra.Left.Interval, ra.Left.Offset)
	}

	plan, err := lowerRangeAggregation(ra, s, lowerCtx{Start: start, End: end, Step: step})
	if err != nil {
		t.Fatalf("lowerRangeAggregation: %v", err)
	}
	if _, ok := plan.(*chplan.AbsentOverTime); !ok {
		t.Fatalf("lowered top node = %T, want *chplan.AbsentOverTime", plan)
	}

	_, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("chsql.Emit: %v", err)
	}

	wantExtended := start.Add(-(ra.Left.Interval + ra.Left.Offset)).Format("2006-01-02 15:04:05.000000000")
	if !argsContain(args, wantExtended) {
		t.Errorf("emitted SQL args do not carry extended Start %q (expected `+` arithmetic, Interval+Offset=13m)\nargs=%v", wantExtended, args)
	}

	// The `-` mutant would clamp at Start - (Interval - Offset) = Start - 7m.
	wrongShorter := start.Add(-(ra.Left.Interval - ra.Left.Offset)).Format("2006-01-02 15:04:05.000000000")
	if argsContain(args, wrongShorter) {
		t.Errorf("emitted SQL args carry the `-` mutant's Start %q — ARITHMETIC_BASE may have flipped `+` to `-`\nargs=%v", wrongShorter, args)
	}
}

// TestAbsentSynthLabelsLogicalConjunction pins BOTH INVERT_LOGICAL mutants
// on range_aggregation.go:`m.Type == labels.MatchEqual && !seen &&
// !dropped[m.Name]` inside [absentSynthLabels]:
//
//	if _, seen := values[m.Name]; m.Type == labels.MatchEqual && !seen && !dropped[m.Name] {
//	                                                          ^col61      ^col70
//
// The line records a stream-selector label on its FIRST equality-matcher
// sighting; any later occurrence (a non-equality matcher, or a second
// matcher on the same name) must instead DROP the label from the synthesised
// absent_over_time output series. Both `&&` operators guard that "first
// equality, never seen, never dropped" condition.
//
// Fixture `{app="api", env=~"prod"}`:
//   - app="api" is an equality matcher seen first → kept as {app:"api"}.
//   - env=~"prod" is a regex (non-equality) matcher → NOT recorded; the
//     label never appears in the output.
//
// Original output: exactly [{app:"api"}].
//
//   - col61 `&&`→`||` makes the test `MatchEqual || (!seen && !dropped)`,
//     so the regex matcher (`!seen && !dropped` both true) is recorded →
//     env spuriously appears.
//   - col70 `&&`→`||` makes it `(MatchEqual && !seen) || !dropped`, so the
//     regex matcher (`!dropped[env]` true) is recorded → env spuriously
//     appears.
//
// Either flip injects an `env` key the original never emits, so asserting
// the exact [{app:"api"}] output kills both.
func TestAbsentSynthLabelsLogicalConjunction(t *testing.T) {
	t.Parallel()

	query := `absent_over_time({app="api", env=~"prod"}[5m])`
	expr, err := syntax.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	ra, ok := expr.(*syntax.RangeAggregationExpr)
	if !ok {
		t.Fatalf("ParseExpr(%q) -> %T, want *syntax.RangeAggregationExpr", query, expr)
	}

	got := absentSynthLabels(ra.Left.Left)

	if len(got) != 1 {
		t.Fatalf("absentSynthLabels = %+v (len %d), want exactly one label {app:api}\n"+
			"a longer slice means an && flipped to || and recorded the env regex matcher", got, len(got))
	}
	if got[0].Key != "app" || got[0].Value != "api" {
		t.Errorf("absentSynthLabels[0] = {%q:%q}, want {app:api}", got[0].Key, got[0].Value)
	}
}

// TestAbsentSynthLabelsDuplicateEqualityDropped is a second, independent
// guard on the same range_aggregation.go:`m.Type == labels.MatchEqual &&
// !seen && !dropped[m.Name]` conjunction, exercising the
// `!seen` operand directly (the col61/col70 fixture above only flexes the
// non-equality path). A duplicate equality matcher on the same name must
// drop the label entirely.
//
// Fixture `{app="api", app="web"}`:
//   - app="api" recorded on first sight.
//   - app="web" is seen (values[app] set) → original drops `app` entirely.
//
// Original output: empty.
//
// Under col70 `&&`→`||` (`(MatchEqual && !seen) || !dropped[app]`) the
// second matcher passes via `!dropped[app]` and `app` survives (twice),
// so a non-empty result kills the mutant. (col61 likewise survives via
// MatchEqual.)
func TestAbsentSynthLabelsDuplicateEqualityDropped(t *testing.T) {
	t.Parallel()

	query := `absent_over_time({app="api", app="web"}[5m])`
	expr, err := syntax.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	ra, ok := expr.(*syntax.RangeAggregationExpr)
	if !ok {
		t.Fatalf("ParseExpr(%q) -> %T, want *syntax.RangeAggregationExpr", query, expr)
	}

	got := absentSynthLabels(ra.Left.Left)

	if len(got) != 0 {
		t.Fatalf("absentSynthLabels = %+v (len %d), want empty — a duplicate equality "+
			"matcher must drop the label; a non-empty result means an && flipped to ||", got, len(got))
	}
}

// isErrorBypassIdentity reports whether `e` is the exact
// errorBypassIdentityExpr shape:
//
//	if(mapContains(<fullLabels>, '__error__'), withDetectedLevel(...), identity)
//
// This shape is produced ONLY when applyUnwrapPostFilters returns
// hasMarks=true (applyUnwrapPostFilters -> applyUnwrapRowSemantics ->
// lowerRangeAggregation -> errorBypassIdentityExpr). The
// ordinary identity projection is a `mapConcat(...)` from
// withDetectedLevelAndColumns, which never matches this predicate, so the
// helper cleanly distinguishes "error-bypass applied" from "not applied".
func isErrorBypassIdentity(e chplan.Expr) bool {
	fc, ok := e.(*chplan.FuncCall)
	if !ok || fc.Fn != chplan.FnIf || len(fc.Args) != 3 {
		return false
	}
	cond, ok := fc.Args[0].(*chplan.FuncCall)
	if !ok || cond.Fn != chplan.FnMapContainsKey || len(cond.Args) != 2 {
		return false
	}
	lit, ok := cond.Args[1].(*chplan.LitString)
	return ok && lit.V == "__error__"
}

// rangeWindowIdentityExpr returns the series-identity projection expr from
// a lowered range-aggregation plan (the first projection of the
// RangeWindow's input Project, aliased to the ResourceAttributes column),
// exactly as lowered — including the [canonicalIdentityExpr] key-order
// wrap. Callers that want to inspect the underlying label shape run it
// through [requireCanonicalIdentity], which asserts the wrap is there
// before peeling it.
func rangeWindowIdentityExpr(t *testing.T, n chplan.Node) chplan.Expr {
	t.Helper()
	rw, ok := n.(*chplan.RangeWindow)
	if !ok {
		t.Fatalf("lowered top node = %T, want *chplan.RangeWindow", n)
	}
	p, ok := rw.Input.(*chplan.Project)
	if !ok {
		t.Fatalf("RangeWindow.Input = %T, want *chplan.Project", rw.Input)
	}
	if len(p.Projections) == 0 {
		t.Fatalf("RangeWindow.Input Project has no projections")
	}
	return p.Projections[0].Expr
}

// The two tests below pin both mutant halves of the
// [applyUnwrapPostFilters] hasMarks return
// range_aggregation.go:`&chplan.Filter{Input: inner, Predicate: pred}, labelsExpr, len(marks) > 0, nil`.
//
// They call applyUnwrapPostFilters DIRECTLY rather than observing the
// lowered plan's series identity, because the plan-level observation
// stopped discriminating: every unwrap conversion now contributes its
// own SampleExtractionErr mark (bare `| unwrap x` models
// convertFloat's `strconv.ParseFloat` failure, `bytes` models
// convertBytes's — cerberus issue #3183), so `unwrapHasErrorMarks` is
// true for an unwrap query whatever the POST-filters do, and an
// assertion on the identity wrapper would pass under either mutant.
// The return expression itself is unchanged and still the mutation
// target; only where the tests observe it moved.

// TestUnwrapPostFilterMarksGateErrorBypass_Negation pins the
// CONDITIONALS_NEGATION half. `| status > 100` is a NumericLabelFilter,
// which stamps a LabelFilterErr mark → marks==1 → hasMarks MUST be
// true. The mutant `<= 0` reports false.
func TestUnwrapPostFilterMarksGateErrorBypass_Negation(t *testing.T) {
	t.Parallel()

	if got := unwrapPostFilterHasMarks(t, `sum_over_time({app="api"} | logfmt | unwrap latency | status > 100 [5m])`); !got {
		t.Errorf("applyUnwrapPostFilters reported hasMarks=false, but the numeric post-filter " +
			"`status > 100` stamps a LabelFilterErr mark.\n" +
			"A CONDITIONALS_NEGATION flip of `len(marks) > 0` to `<= 0` lived.")
	}
}

// TestUnwrapPostFilterNoMarksSkipErrorBypass_Boundary pins the
// CONDITIONALS_BOUNDARY half. `| foo = "bar"` is a StringLabelFilter,
// for which labelFiltererLower returns NO marks — a string comparison
// cannot fail to parse — so hasMarks MUST be false. The mutant `>= 0`
// reports true for the empty slice.
func TestUnwrapPostFilterNoMarksSkipErrorBypass_Boundary(t *testing.T) {
	t.Parallel()

	if got := unwrapPostFilterHasMarks(t, `sum_over_time({app="api"} | logfmt | unwrap latency | foo = "bar" [5m])`); got {
		t.Errorf("applyUnwrapPostFilters reported hasMarks=true, but the string post-filter " +
			"`foo = \"bar\"` stamps no mark.\n" +
			"A CONDITIONALS_BOUNDARY flip of `len(marks) > 0` to `>= 0` lived.")
	}
}

// unwrapPostFilterHasMarks lowers query's selector the way
// [lowerRangeAggregation] does and returns applyUnwrapPostFilters's
// hasMarks result for its unwrap post-filters.
func unwrapPostFilterHasMarks(t *testing.T, query string) bool {
	t.Helper()

	s := schema.DefaultOTelLogs()
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	lc := lowerCtx{Start: start, End: start.Add(time.Hour), Step: time.Minute}

	expr, err := syntax.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	ra, ok := expr.(*syntax.RangeAggregationExpr)
	if !ok {
		t.Fatalf("ParseExpr(%q) -> %T, want *syntax.RangeAggregationExpr", query, expr)
	}
	if ra.Left.Unwrap == nil || len(ra.Left.Unwrap.PostFilters) == 0 {
		t.Fatalf("fixture invalid: expected a non-empty unwrap PostFilters slice")
	}

	innerLc := lc.withMatcherWindowExtension(ra.Left.Interval + ra.Left.Offset)
	inner, labelsExpr, err := lowerLogRange(ra.Left, s, innerLc)
	if err != nil {
		t.Fatalf("lowerLogRange: %v", err)
	}
	_, _, hasMarks, err := applyUnwrapPostFilters(inner, ra.Left.Unwrap.PostFilters, s, labelsExpr)
	if err != nil {
		t.Fatalf("applyUnwrapPostFilters: %v", err)
	}
	return hasMarks
}
