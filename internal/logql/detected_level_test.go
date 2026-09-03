package logql

import (
	"testing"

	syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestDetectedLevel_RoutesAllMatcherOps exercises the four LogQL label
// matcher kinds (`=`, `!=`, `=~`, `!~`) against the synthesized
// `detected_level` label. Every kind must lower to a `chplan.Binary`
// whose left-hand side is the multiIf normalisation of SeverityText
// (the SQL-level CASE expression Loki's reference engine emits via
// `pkg/distributor/field_detection.go::normalizeLogLevel`), not the
// plain `ResourceAttributes["detected_level"]` lookup that every other
// label name lowers to. The op carries through unchanged.
func TestDetectedLevel_RoutesAllMatcherOps(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()

	cases := []struct {
		name   string
		query  string
		wantOp chplan.BinaryOp
	}{
		{"eq", `{job="api"} | detected_level="error"`, chplan.OpEq},
		{"neq", `{job="api"} | detected_level!="info"`, chplan.OpNe},
		{"regex", `{job="api"} | detected_level=~"warn|error"`, chplan.OpMatch},
		{"notregex", `{job="api"} | detected_level!~"fatal"`, chplan.OpNotMatch},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := syntax.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}

			filterBin := mustFindDetectedLevelBinary(t, expr, s)
			if filterBin.Op != tc.wantOp {
				t.Errorf("matcher %q produced op %q; want %q", tc.name, filterBin.Op, tc.wantOp)
			}
			// The LHS must be a `chplan.FuncCall` with name `multiIf` —
			// the marker that the synthesized normalisation kicked in.
			// A regression that forgot to route `detected_level` would
			// emit a `chplan.MapAccess` on ResourceAttributes here.
			fn, ok := filterBin.Left.(*chplan.FuncCall)
			if !ok {
				t.Fatalf("matcher %q: LHS = %T; want *chplan.FuncCall (multiIf)", tc.name, filterBin.Left)
			}
			if fn.Fn != chplan.FnMultiIf {
				t.Errorf("matcher %q: LHS func = %q; want %q", tc.name, fn.Fn, chplan.FnMultiIf)
			}
			// The matcher value must ride on the RHS as a LitString.
			lit, ok := filterBin.Right.(*chplan.LitString)
			if !ok {
				t.Fatalf("matcher %q: RHS = %T; want *chplan.LitString", tc.name, filterBin.Right)
			}
			_ = lit // matcher's value isn't structurally asserted; the parser already exercised it.
		})
	}
}

// TestDetectedLevel_StreamSelector covers the rarer case where
// `detected_level` is named in the stream selector itself
// (`{detected_level="error"}`) rather than as a pipe label filter.
// The synthesized expression must still take over the LHS.
func TestDetectedLevel_StreamSelector(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	expr, err := syntax.ParseExpr(`{detected_level="error"}`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}

	filterBin := mustFindDetectedLevelBinary(t, expr, s)
	if filterBin.Op != chplan.OpEq {
		t.Errorf("stream-selector op = %q; want %q", filterBin.Op, chplan.OpEq)
	}
	if _, ok := filterBin.Left.(*chplan.FuncCall); !ok {
		t.Errorf("stream-selector LHS = %T; want *chplan.FuncCall (multiIf)", filterBin.Left)
	}
}

// TestDetectedLevel_GroupingLevelAliasesToDetectedLevel pins the
// `sum by (level) (...)` fix. With this fix, a vector aggregation
// `by (level)` resolves the synthesized severity dimension through the
// augmented `ResourceAttributes[detected_level]` map lookup — the
// outer SELECT can't see `SeverityText` (the inner RangeWindow only
// exposes the (ResourceAttributes, Value) tuple), so the synthesized
// key has to ride in the map. Without the alias, the outer would read
// `ResourceAttributes[level]`, which the OTel-CH seeder writes to
// nothing on the loki-compat fixture, and all 4 severity series would
// collapse to a single empty-value group (the 15 `matrix length:
// expected=4 actual=1` failures this PR clears).
func TestDetectedLevel_GroupingLevelAliasesToDetectedLevel(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	expr, err := syntax.ParseExpr(`sum by (level) (count_over_time({app="api"}[5m]))`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := lower(expr, s, lowerCtx{})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}

	// Walk to the outer Aggregate's GroupBy. The fix routes both `level`
	// and `detected_level` aliases to a MapAccess on
	// ResourceAttributes[detected_level].
	outerProject, ok := plan.(*chplan.Project)
	if !ok {
		t.Fatalf("lower top is %T; want *chplan.Project", plan)
	}
	outerAgg, ok := outerProject.Input.(*chplan.Aggregate)
	if !ok {
		t.Fatalf("Project.Input is %T; want *chplan.Aggregate", outerProject.Input)
	}
	if got, want := len(outerAgg.GroupBy), 1; got != want {
		t.Fatalf("GroupBy length = %d; want %d", got, want)
	}
	ma, ok := outerAgg.GroupBy[0].(*chplan.MapAccess)
	if !ok {
		t.Fatalf("GroupBy[0] = %T; want *chplan.MapAccess (synthesized lookup)", outerAgg.GroupBy[0])
	}
	col, ok := ma.Map.(*chplan.ColumnRef)
	if !ok || col.Name != s.ResourceAttributesColumn {
		t.Fatalf("GroupBy MapAccess.Map = %v; want ColumnRef(%q)", ma.Map, s.ResourceAttributesColumn)
	}
	keyLit, ok := ma.Key.(*chplan.LitString)
	if !ok || keyLit.V != detectedLevelLabel {
		t.Fatalf("GroupBy MapAccess.Key = %v; want %q (level alias canonicalised to detected_level)", ma.Key, detectedLevelLabel)
	}
}

// TestDetectedLevel_RangeAggregationLevelByUsesSeverityText pins the
// inner range-aggregation `by (level)` form. At the inner Project
// layer, `SeverityText` is still in scope, so the group-key value
// embeds the full multiIf normalisation directly — the outer
// MapAccess-via-`detected_level` approach can't apply because the
// inner Project IS what populates the augmented map (and at this
// layer the map hasn't been augmented yet).
func TestDetectedLevel_RangeAggregationLevelByUsesSeverityText(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	expr, err := syntax.ParseExpr(`avg_over_time({app="api"} | logfmt | unwrap latency [5m]) by (level)`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := lower(expr, s, lowerCtx{})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}

	// Walk past the RangeWindow → Project → look at the identity
	// projection's expression. Under the key-order canonicalisation it
	// should be a `map(...)` whose value for `level` is the multiIf
	// normalisation (a FuncCall named multiIf), not a MapAccess on
	// ResourceAttributes.
	rw, ok := plan.(*chplan.RangeWindow)
	if !ok {
		t.Fatalf("lower top is %T; want *chplan.RangeWindow", plan)
	}
	proj, ok := rw.Input.(*chplan.Project)
	if !ok {
		t.Fatalf("RangeWindow.Input is %T; want *chplan.Project", rw.Input)
	}
	if len(proj.Projections) == 0 {
		t.Fatalf("Project has no projections")
	}
	identity := requireCanonicalIdentity(t, proj.Projections[0].Expr)
	mapCall, ok := identity.(*chplan.FuncCall)
	if !ok || mapCall.Fn != chplan.FnMap {
		t.Fatalf("identity projection = %v; want FuncCall(map, ...)", identity)
	}
	// args = ["level", <levelExpr>] for `by (level)`.
	if got, want := len(mapCall.Args), 2; got != want {
		t.Fatalf("map call args = %d; want %d", got, want)
	}
	keyLit, ok := mapCall.Args[0].(*chplan.LitString)
	if !ok || keyLit.V != "level" {
		t.Fatalf("map call args[0] = %v; want LitString(\"level\")", mapCall.Args[0])
	}
	multiIf, ok := mapCall.Args[1].(*chplan.FuncCall)
	if !ok || multiIf.Fn != chplan.FnMultiIf {
		t.Fatalf("map call args[1] = %v; want FuncCall(multiIf, ...) (SeverityText-derived expression)", mapCall.Args[1])
	}
}

// TestDetectedLevel_GroupingDetectedLevelCanonical pins the canonical
// form: `by (detected_level)` and `by (level)` produce structurally
// identical plans. The MapAccess key is always `detected_level` because
// the inner range aggregation's augmentation populates that canonical
// key (not the `level` alias).
func TestDetectedLevel_GroupingDetectedLevelCanonical(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	for _, alias := range []string{"level", "detected_level"} {
		t.Run(alias, func(t *testing.T) {
			t.Parallel()
			expr, err := syntax.ParseExpr(`sum by (` + alias + `) (count_over_time({app="api"}[5m]))`)
			if err != nil {
				t.Fatalf("ParseExpr: %v", err)
			}
			plan, err := lower(expr, s, lowerCtx{})
			if err != nil {
				t.Fatalf("lower: %v", err)
			}
			outerProject := plan.(*chplan.Project)
			outerAgg := outerProject.Input.(*chplan.Aggregate)
			ma := outerAgg.GroupBy[0].(*chplan.MapAccess)
			keyLit := ma.Key.(*chplan.LitString)
			if keyLit.V != detectedLevelLabel {
				t.Errorf("by (%s): MapAccess.Key = %q; want canonical %q", alias, keyLit.V, detectedLevelLabel)
			}
		})
	}
}

// TestDetectedLevel_LabelFilterLevelDoesNotAlias pins that the `level`
// short alias does NOT apply in label-filter context — pipelines like
// `{job="api"} | logfmt | level="error"` resolve `level` through the
// labels map (so parser-extracted keys still win) rather than routing
// to the SeverityText-derived expression. This is the boundary case
// the [isDetectedLevelGroupingLabel] / [isDetectedLevelLabel] split
// guards: matchers take the strict path so parser stages keep
// working, aggregation grouping takes the broader path so
// `by (level)` matches upstream Loki.
func TestDetectedLevel_LabelFilterLevelDoesNotAlias(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	expr, err := syntax.ParseExpr(`{job="api"} | logfmt | level="error"`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := lower(expr, s, lowerCtx{})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	// The `level="error"` filter must lower to a MapAccess on the
	// parser-augmented labels map (which is itself a mapConcat over
	// ResourceAttributes + extracted keys), not to a multiIf on
	// SeverityText. Walk the predicate tree and assert the level
	// matcher's LHS is a MapAccess, not a FuncCall named multiIf.
	filt, ok := plan.(*chplan.Filter)
	if !ok {
		t.Fatalf("lower top is %T; want *chplan.Filter", plan)
	}
	// The `level="error"` comparison's LHS must resolve `level` through
	// the labels maps — NOT through the SeverityText-derived
	// `multiIf(...)` the detected_level alias would emit. After task #59
	// the LHS is the structured-metadata-over-stream coalescing wrapper
	// `if(mapContains(LogAttributes, "level"), LogAttributes["level"],
	// <parser-merged map>["level"])`, whose parser-merged side is itself
	// a MapAccess on the `| logfmt` mapConcat. Assert (1) the comparison
	// LHS is that `if(...)` coalescing FuncCall (never a `multiIf`), and
	// (2) a `MapAccess(_, "level")` value lookup survives inside it.
	var sawLevelCompare, sawMapAccessLevel bool
	walkExprTree(filt.Predicate, func(e chplan.Expr) {
		bin, ok := e.(*chplan.Binary)
		if !ok || bin.Op != chplan.OpEq {
			return
		}
		rhs, ok := bin.Right.(*chplan.LitString)
		if !ok || rhs.V != "error" {
			return
		}
		lhs, ok := bin.Left.(*chplan.FuncCall)
		if !ok || lhs.Fn != chplan.FnIf {
			return
		}
		sawLevelCompare = true
	})
	walkExprTree(filt.Predicate, func(e chplan.Expr) {
		ma, ok := e.(*chplan.MapAccess)
		if !ok {
			return
		}
		keyLit, ok := ma.Key.(*chplan.LitString)
		if !ok || keyLit.V != "level" {
			return
		}
		sawMapAccessLevel = true
	})
	if !sawLevelCompare {
		t.Errorf("expected `level=\"error\"` LHS to be the structured-over-stream `if(...)` coalescing wrapper (not a SeverityText multiIf); got plan %v", filt.Predicate)
	}
	if !sawMapAccessLevel {
		t.Errorf("expected a MapAccess(<labels>, \"level\") value lookup inside the coalescing wrapper; got plan %v", filt.Predicate)
	}
}

// TestDetectedLevel_NoColumnRefToDetectedLevelLabel verifies that no
// stray `ResourceAttributes["detected_level"]` MapAccess survives in
// the lowered tree — the synthesized normalisation should fully
// shadow the plain STREAM-LABEL lookup. A failure here would mean the
// dispatch missed a code path and fell through to the resource map.
//
// A `LogAttributes["detected_level"]` MapAccess IS expected: the
// detected_level source resolution reads the structured-metadata key
// first (reference Loki's extractLogLevel step 1), so the assertion is
// scoped to the ResourceAttributes (stream-label) column only.
func TestDetectedLevel_NoColumnRefToDetectedLevelLabel(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	expr, err := syntax.ParseExpr(`{job="api"} | detected_level="error"`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := lower(expr, s, lowerCtx{})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}

	var found bool
	walkChplanExpr(plan, func(e chplan.Expr) {
		ma, ok := e.(*chplan.MapAccess)
		if !ok {
			return
		}
		col, ok := ma.Map.(*chplan.ColumnRef)
		if !ok || col.Name != s.ResourceAttributesColumn {
			return
		}
		lit, ok := ma.Key.(*chplan.LitString)
		if !ok {
			return
		}
		if lit.V == detectedLevelLabel {
			found = true
		}
	})
	if found {
		t.Errorf("plan still contains ResourceAttributes[\"detected_level\"] stream-label lookup; want fully synthesized expression")
	}
}

// mustFindDetectedLevelBinary locates the `chplan.Binary` whose RHS is
// the matcher value LitString for a `detected_level` filter, by
// walking the filter predicate's AND tree. The helper is the lowest-
// noise way to assert against a synthesized LHS without re-emitting
// SQL.
func mustFindDetectedLevelBinary(t *testing.T, expr syntax.Expr, s schema.Logs) *chplan.Binary {
	t.Helper()
	plan, err := lower(expr, s, lowerCtx{})
	if err != nil {
		t.Fatalf("lower: %v", err)
	}

	var bins []*chplan.Binary
	walkChplanExpr(plan, func(e chplan.Expr) {
		if b, ok := e.(*chplan.Binary); ok {
			if isMatchOp(b.Op) {
				bins = append(bins, b)
			}
		}
	})

	for _, b := range bins {
		fn, ok := b.Left.(*chplan.FuncCall)
		if !ok || fn.Fn != chplan.FnMultiIf {
			continue
		}
		if _, ok := b.Right.(*chplan.LitString); ok {
			return b
		}
	}
	t.Fatalf("no detected_level Binary found in plan; bins=%d", len(bins))
	return nil
}

// walkChplanExpr is a minimal sibling of the local expr walker in
// internal/chsql's collectColumnRefs. Visits every Expr node
// inside a chplan.Node (recurses through Binary / FuncCall / MapAccess
// arms; other Expr kinds carry leaves).
func walkChplanExpr(n chplan.Node, fn func(chplan.Expr)) {
	switch v := n.(type) {
	case *chplan.Filter:
		walkExprTree(v.Predicate, fn)
		walkChplanExpr(v.Input, fn)
	case *chplan.Project:
		for _, p := range v.Projections {
			walkExprTree(p.Expr, fn)
		}
		walkChplanExpr(v.Input, fn)
	case *chplan.Scan:
		// nothing
	}
}

func walkExprTree(e chplan.Expr, fn func(chplan.Expr)) {
	if e == nil {
		return
	}
	fn(e)
	switch v := e.(type) {
	case *chplan.Binary:
		walkExprTree(v.Left, fn)
		walkExprTree(v.Right, fn)
	case *chplan.FuncCall:
		for _, a := range v.Args {
			walkExprTree(a, fn)
		}
	case *chplan.MapAccess:
		walkExprTree(v.Map, fn)
		walkExprTree(v.Key, fn)
	}
}

func isMatchOp(op chplan.BinaryOp) bool {
	switch op {
	case chplan.OpEq, chplan.OpNe, chplan.OpMatch, chplan.OpNotMatch:
		return true
	}
	return false
}

// canonicalLevelGroups is an INDEPENDENT restatement of the
// variant-to-canonical table [levelNormalizationGroups] holds. It is written
// out again on purpose rather than read from the source: assertions that
// derived their expectations from the very table under test would mirror a
// wrong edit to it instead of failing on one. Order matches upstream Loki's
// `normalizeLogLevel` switch — trace / debug / info / warn / error /
// critical / fatal.
var canonicalLevelGroups = []struct {
	variants  []string
	canonical string
}{
	{[]string{"trace", "trc"}, "trace"},
	{[]string{"debug", "dbg"}, "debug"},
	{[]string{"info", "inf", "information"}, "info"},
	{[]string{"warn", "wrn", "warning"}, "warn"},
	{[]string{"error", "err"}, "error"},
	{[]string{"critical"}, "critical"},
	{[]string{"fatal"}, "fatal"},
}

// TestNormaliseLevelExpr_MultiIfArgsCapacityAndShape kills the
// ARITHMETIC_BASE mutants gremlins reports on the slice-capacity hint
// detected_level.go:`(len(levelNormalizationGroups)+1)*2+1`. Substituting an
// operator there changes the PRE-ALLOCATED capacity, but `append` grows past
// a too-small hint and simply under-fills a too-large one, so a semantic-only
// test cannot observe the difference: the SQL the emitter prints from the
// resulting FuncCall is byte-identical however the slice was allocated.
//
// Both facts this test pins are COMPUTED from `canonicalLevelGroups` into
// `wantLen`, never written down as an integer, so neither can be restated
// wrongly here:
//
//  1. `len(fn.Args)` — the load-bearing count: one leading (empty, unknown)
//     pair, one (cond, literal) pair per canonical group, and the trailing
//     default branch. A structural mutation that drops or duplicates a slot
//     shows up immediately.
//  2. `cap(fn.Args)` — the direct kill. `make([]T, 0, N)` hands back a slice
//     whose cap is exactly N, and the appends that follow fill it exactly, so
//     the finished slice still reports the hint. Any ARITHMETIC_BASE mutation
//     shifts the hint off that count, and `append`'s growth schedule never
//     lands back on it.
//
// That last sentence is the claim an equivalence note has no way to prove on
// its own, so it is not left as prose: `TestNormaliseLevelExpr_CapHintMutantsAreKilled`
// replays every operator substitution and asserts the resulting capacity
// differs from the unmutated one.
//
// The cap assertion is the only direct kill available for a slice-capacity
// arithmetic mutant — append's growth strategy hides the change from
// length-only tests.
func TestNormaliseLevelExpr_MultiIfArgsCapacityAndShape(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	expr := detectedLevelExpr(s)

	fn, ok := expr.(*chplan.FuncCall)
	if !ok {
		t.Fatalf("detectedLevelExpr returned %T; want *chplan.FuncCall (multiIf)", expr)
	}
	if fn.Fn != chplan.FnMultiIf {
		t.Fatalf("FuncCall.Fn = %q; want %q", fn.Fn, chplan.FnMultiIf)
	}

	// The leading (empty → "unknown") pair — reference Loki stamps
	// `detected_level="unknown"` when no level is detectable
	// (pkg/distributor/field_detection.go, constants.LogLevelUnknown)
	// — precedes the canonical groups, then the lowercased default.
	wantLen := multiIfArgCount(len(canonicalLevelGroups))
	if got := len(fn.Args); got != wantLen {
		t.Fatalf("len(multiIf.Args) = %d; want %d (one (empty, unknown) pair, "+
			"one (cond, literal) pair per canonical group, one default)", got, wantLen)
	}

	// Kill: any arithmetic mutation on the capacity hint either
	// over-allocates (the appends fit, and the final cap is the mutated
	// hint) or under-allocates (append re-grows via the runtime's
	// schedule) — both produce a final cap different from the exact arg
	// count. Asserting cap == wantLen pins the arithmetic;
	// `TestNormaliseLevelExpr_CapHintMutantsAreKilled` proves it
	// discriminates every operator substitution.
	if got, want := cap(fn.Args), wantLen; got != want {
		t.Fatalf("cap(multiIf.Args) = %d; want %d (mutant `*` → `/`/`%%` or `+` → `-`/`*` in detected_level.go:`(len(levelNormalizationGroups)+1)*2+1` would shift the capacity hint and re-allocate via append's growth schedule)", got, want)
	}
}

// capHintOps are the binary arithmetic operators gremlins' ARITHMETIC_BASE
// operator substitutes into an expression. Applying each of them to each
// operator position of the capacity hint enumerates that hint's whole mutant
// set.
var capHintOps = []string{"+", "-", "*", "/", "%"}

// applyCapHintOp evaluates `a op b` for the operators ARITHMETIC_BASE can
// produce. Every b it is called with below is non-zero, so `/` and `%` are
// total here.
func applyCapHintOp(t *testing.T, a int, op string, b int) int {
	t.Helper()

	switch op {
	case "+":
		return a + b
	case "-":
		return a - b
	case "*":
		return a * b
	case "/":
		return a / b
	case "%":
		return a % b
	}
	t.Fatalf("unknown arithmetic operator %q", op)
	return 0
}

// capHint evaluates the SHAPE of detected_level.go's capacity hint
// `(len(levelNormalizationGroups)+1)*2+1` with each of its three operator
// positions supplied explicitly: `(n innerOp 1) mulOp 2 tailOp 1`. Passing the
// operators in is what lets the caller enumerate the mutants of the hint
// without writing any of their values down.
func capHint(t *testing.T, n int, innerOp, mulOp, tailOp string) int {
	t.Helper()

	return applyCapHintOp(t,
		applyCapHintOp(t,
			applyCapHintOp(t, n, innerOp, 1),
			mulOp, 2),
		tailOp, 1)
}

// multiIfArgCount derives the number of arguments normaliseLevelExpr's multiIf
// carries for `groups` canonical level groups: one leading (empty, unknown)
// pair, one (cond, literal) pair per group, and the trailing default branch.
// Both tests below take their expected count from here rather than writing an
// integer down, so the two cannot state it differently.
func multiIfArgCount(groups int) int { return 2*(groups+1) + 1 }

// capAfterPairedAppends replays the append sequence BOTH multiIf builders in
// detected_level.go share — `pairs` (condition, value) pairs appended two at a
// time, then one trailing default branch — against a slice pre-allocated with
// `hint`, and reports the length and the capacity the finished slice carries.
//
// The growth schedule depends on the exact order and grouping of the appends,
// not just on the final count, so the replay has to mirror the builder rather
// than append the total in one call.
//
// The element type has to be the real one. Go rounds a growing slice's
// capacity up to an allocator size class measured in BYTES, so a slice whose
// elements are a different width grows to different capacities and the
// simulation would answer a question nobody asked.
func capAfterPairedAppends(hint, pairs int) (length, capacity int) {
	args := make([]chplan.Expr, 0, hint)
	for i := 0; i < pairs; i++ {
		args = append(args, nil, nil) // (condition, value)
	}
	args = append(args, nil) // trailing default branch
	return len(args), cap(args)
}

// capAfterBuild replays [normaliseLevelExpr]'s exact append sequence: the
// leading (empty, unknown) pair, one (cond, canonical literal) pair per group,
// and the lowercased default — i.e. `groups+1` pairs and a trailing branch.
func capAfterBuild(hint, groups int) (length, capacity int) {
	return capAfterPairedAppends(hint, groups+1)
}

// TestNormaliseLevelExpr_CapHintMutantsAreKilled proves that the `cap`
// assertion in [TestNormaliseLevelExpr_MultiIfArgsCapacityAndShape] actually
// discriminates. For every ARITHMETIC_BASE operator substitution gremlins can
// make in detected_level.go:`(len(levelNormalizationGroups)+1)*2+1`, the
// capacity the finished slice ends up with must differ from the capacity the
// unmutated hint produces — otherwise that assertion passes on the mutant and
// the equivalence note above is claiming a kill it does not deliver.
//
// This is the claim the note used to make in prose, with the mutants'
// capacities written out by hand. Written out, they were free to drift from
// the arithmetic they described, and they did. Computed here they cannot: if a
// future edit to the append sequence, to the group table, or to the hint let
// some mutant land back on the true capacity, this test goes red and names it.
func TestNormaliseLevelExpr_CapHintMutantsAreKilled(t *testing.T) {
	t.Parallel()

	// The operators the unmutated hint uses, in the three positions
	// `(n + 1) * 2 + 1`.
	const (
		origInnerOp = "+"
		origMulOp   = "*"
		origTailOp  = "+"
	)
	positions := []struct{ name, orig string }{
		{"the inner `+1`", origInnerOp},
		{"the `*2`", origMulOp},
		{"the trailing `+1`", origTailOp},
	}

	n := len(canonicalLevelGroups)
	trueHint := capHint(t, n, origInnerOp, origMulOp, origTailOp)
	trueLen, trueCap := capAfterBuild(trueHint, n)

	// The premise of the whole equivalence argument: the appends FILL the
	// pre-allocation exactly — neither growing past it nor leaving slack. Only
	// then does the finished cap read back the hint, and only then does
	// asserting cap pin the hint's arithmetic. Checking capacity alone would
	// miss the under-filling half: a slice that never grows keeps the cap it
	// was made with however few elements are appended to it.
	if trueLen != trueHint || trueCap != trueHint {
		t.Fatalf("the unmutated hint %d does not exactly fit the append sequence "+
			"(finished len %d, cap %d): the appends no longer fill the "+
			"pre-allocation exactly, so cap() has stopped reading back the hint "+
			"and asserting it has stopped being a capacity kill",
			trueHint, trueLen, trueCap)
	}

	// …and the sequence replayed here has to be the one the sibling test pins
	// on the real multiIf, or this whole test measures a slice nobody builds.
	if want := multiIfArgCount(n); trueLen != want {
		t.Fatalf("the replayed append sequence produces %d args, but the multiIf "+
			"under test carries %d — the simulation has drifted from the builder "+
			"it stands in for", trueLen, want)
	}

	mutants, checked, negative := 0, 0, 0
	for i, pos := range positions {
		for _, op := range capHintOps {
			if op == pos.orig {
				continue
			}
			mutants++

			ops := []string{origInnerOp, origMulOp, origTailOp}
			ops[i] = op
			hint := capHint(t, n, ops[0], ops[1], ops[2])

			if hint < 0 {
				// `make` panics on a negative capacity, so this mutant dies in
				// any test that reaches normaliseLevelExpr at all.
				negative++
				continue
			}
			checked++

			if _, got := capAfterBuild(hint, n); got == trueCap {
				t.Errorf("mutating %s to %q gives capacity hint %d, and the finished "+
					"slice still ends at cap %d — identical to the unmutated build, so "+
					"the `cap(fn.Args) == wantLen` assertion does NOT kill this mutant",
					pos.name, op, hint, got)
			}
		}
	}

	// Anti-vacuity: a loop that enumerated nothing would report a clean run
	// while proving nothing at all.
	if want := len(positions) * (len(capHintOps) - 1); mutants != want {
		t.Fatalf("enumerated %d hint mutants, want %d (one per operator substitution "+
			"in each of the %d positions) — the enumeration is not covering the "+
			"mutant set it claims to", mutants, want, len(positions))
	}
	if checked == 0 {
		t.Fatalf("every one of the %d hint mutants was skipped as a negative capacity, "+
			"so the discriminating comparison never ran", mutants)
	}
	t.Logf("hint mutants: %d enumerated, %d distinguished by capacity, %d killed by a "+
		"negative make() capacity", mutants, checked, negative)
}

// TestNormaliseLevelExpr_CanonicalLevelOrder pins the exact (cond,
// literal) pair structure of the multiIf chain. After the leading
// (empty, unknown) pair comes one (cond, literal) pair per canonical
// group, in the enumeration order trace / debug / info / warn / error /
// critical / fatal, and the final slot is the lowercased pass-through
// default. A regression that drops a group, reorders the groups, or
// swaps an OR-chain for an unrelated condition will fail here.
// Combined with the cap test above this also serves as a structural
// backstop: it forces the `args` slice to actually be built
// end-to-end, so a capacity mutation can't quietly survive by also
// short-circuiting the loop.
func TestNormaliseLevelExpr_CanonicalLevelOrder(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	fn := detectedLevelExpr(s).(*chplan.FuncCall)

	// Leading pair: empty resolved source → "unknown" (reference Loki's
	// constants.LogLevelUnknown stamping for undetectable levels). The
	// source is the detected_level precedence cascade (see
	// detectedLevelSourceExpr); assertSourceCascade pins it.
	emptyCond, ok := fn.Args[0].(*chplan.Binary)
	if !ok || emptyCond.Op != chplan.OpEq {
		t.Fatalf("args[0] = %#v; want Binary{Op: Eq} comparing lower(<source>) to \"\"", fn.Args[0])
	}
	if rhs, ok := emptyCond.Right.(*chplan.LitString); !ok || rhs.V != "" {
		t.Fatalf("args[0] RHS = %#v; want empty-string literal", emptyCond.Right)
	}
	if lowerCall, ok := emptyCond.Left.(*chplan.FuncCall); !ok || lowerCall.Fn != chplan.FnLower {
		t.Fatalf("args[0] LHS = %#v; want lower(<source>)", emptyCond.Left)
	} else {
		assertSourceCascade(t, lowerCall.Args[0], s)
	}
	if lit, ok := fn.Args[1].(*chplan.LitString); !ok || lit.V != "unknown" {
		t.Fatalf("args[1] = %#v; want LitString \"unknown\"", fn.Args[1])
	}

	for i, g := range canonicalLevelGroups {
		// Each group emits two args after the leading (empty,
		// "unknown") pair: the OR-chain comparison at index 2*i+2,
		// the canonical literal at index 2*i+3.
		condIdx := 2*i + 2
		litIdx := 2*i + 3

		// The condition is either a plain `Binary{Op: Eq}` (for
		// single-variant groups like "critical" / "fatal") or a left-
		// folded OR-chain over the variants (multi-variant groups).
		// Either way the RHS of every leaf equality is one of the
		// variant LitStrings — collect them and compare set-wise.
		gotVariants := collectEqRHSLiterals(fn.Args[condIdx])
		if len(gotVariants) != len(g.variants) {
			t.Fatalf("group %d (%s): condition compares %d variants %v; want %d %v", i, g.canonical, len(gotVariants), gotVariants, len(g.variants), g.variants)
		}
		for _, want := range g.variants {
			if !containsString(gotVariants, want) {
				t.Errorf("group %d (%s): variant %q missing from condition (got %v)", i, g.canonical, want, gotVariants)
			}
		}

		// The canonical literal at the paired slot.
		canonLit, ok := fn.Args[litIdx].(*chplan.LitString)
		if !ok {
			t.Fatalf("group %d (%s): args[%d] = %T; want *chplan.LitString", i, g.canonical, litIdx, fn.Args[litIdx])
		}
		if canonLit.V != g.canonical {
			t.Errorf("group %d: canonical literal at args[%d] = %q; want %q", i, litIdx, canonLit.V, g.canonical)
		}
	}

	// The trailing default branch is the lowercased pass-through —
	// `lower(<source>)`, where <source> is the detected_level
	// precedence cascade. A mutation on the hint's trailing `+1` that
	// dropped the default slot would shorten Args by one and trip the
	// len check above; this assertion pins the SHAPE of the default
	// branch so a refactor that swaps it for something else (e.g. an
	// empty string fall-through) still trips a test.
	defaultIdx := 2 * (len(canonicalLevelGroups) + 1)
	defaultCall, ok := fn.Args[defaultIdx].(*chplan.FuncCall)
	if !ok {
		t.Fatalf("default branch at args[%d] = %T; want *chplan.FuncCall (lower(...))", defaultIdx, fn.Args[defaultIdx])
	}
	if defaultCall.Fn != chplan.FnLower {
		t.Errorf("default branch FuncCall.Fn = %q; want %q", defaultCall.Fn, chplan.FnLower)
	}
	if len(defaultCall.Args) != 1 {
		t.Fatalf("default branch lower() args = %d; want 1", len(defaultCall.Args))
	}
	assertSourceCascade(t, defaultCall.Args[0], s)
}

// assertSourceCascade verifies that `e` is the detected_level source
// precedence cascade [detectedLevelSourceExpr] produces for the default
// OTel schema: a `multiIf(...)` whose terminal fallback branch is the
// bare SeverityColumn ColumnRef. The intermediate branches resolve the
// structured-metadata level keys; the pin here is the final fallback,
// which is the load-bearing severity source.
func assertSourceCascade(t *testing.T, e chplan.Expr, s schema.Logs) {
	t.Helper()
	mi, ok := e.(*chplan.FuncCall)
	if !ok || mi.Fn != chplan.FnMultiIf {
		t.Fatalf("source = %#v; want multiIf(...) precedence cascade", e)
	}
	if len(mi.Args) == 0 {
		t.Fatalf("source multiIf has no args")
	}
	fallback, ok := mi.Args[len(mi.Args)-1].(*chplan.ColumnRef)
	if !ok || fallback.Name != s.SeverityColumn {
		t.Errorf("source fallback branch = %#v; want ColumnRef(%q)", mi.Args[len(mi.Args)-1], s.SeverityColumn)
	}
}

// collectEqRHSLiterals walks an OR-chain of `Binary{Op: Eq}`
// comparisons (the shape `anyEqual` emits) and returns every RHS
// LitString value. Single-variant groups bottom out at one leaf;
// multi-variant groups produce a left-folded chain whose leaves are
// the variant comparisons.
func collectEqRHSLiterals(e chplan.Expr) []string {
	var out []string
	var walk func(chplan.Expr)
	walk = func(node chplan.Expr) {
		bin, ok := node.(*chplan.Binary)
		if !ok {
			return
		}
		switch bin.Op {
		case chplan.OpOr:
			walk(bin.Left)
			walk(bin.Right)
		case chplan.OpEq:
			lit, ok := bin.Right.(*chplan.LitString)
			if ok {
				out = append(out, lit.V)
			}
		}
	}
	walk(e)
	return out
}

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// TestDetectedLevelSource_PrecedenceCascade pins reference Loki's
// extractLogLevel source precedence (pkg/distributor/field_detection.go)
// as cerberus encodes it in [detectedLevelSourceExpr]: the structured-
// metadata `detected_level` key wins, then the allowed level/severity
// keys, and the dedicated SeverityText column is the terminal fallback.
// The cascade is a multiIf whose (cond, value) pairs read the
// LogAttributes map and whose final branch is the bare SeverityColumn.
func TestDetectedLevelSource_PrecedenceCascade(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	cascade, ok := detectedLevelSourceExpr(s).(*chplan.FuncCall)
	if !ok || cascade.Fn != chplan.FnMultiIf {
		t.Fatalf("source = %#v; want multiIf cascade", detectedLevelSourceExpr(s))
	}

	// Expected key order: detected_level, then allowedLevelFields, each
	// contributing a (LogAttributes[key] != "", LogAttributes[key]) pair,
	// then the SeverityText fallback.
	wantKeys := append([]string{detectedLevelLabel}, allowedLevelFields...)
	wantArgs := len(wantKeys)*2 + 1
	if got := len(cascade.Args); got != wantArgs {
		t.Fatalf("cascade has %d args; want %d (%d keys × 2 + fallback)", got, wantArgs, len(wantKeys))
	}

	// Kill for the ARITHMETIC_BASE mutants on the slice-capacity hint
	// detected_level.go:`args := make([]chplan.Expr, 0, len(keys)*2+1)`. The
	// appends below fill that pre-allocation EXACTLY, so the finished slice
	// still reports the hint and `cap` reads the arithmetic straight back;
	// [TestDetectedLevelSource_CapHintMutantsAreKilled] proves no operator
	// substitution lands back on it. This works here — where the identical
	// argument fails for the sibling `keys := make([]string, 0,
	// len(allowedLevelFields)+1)` — because `args` ESCAPES: it becomes the
	// returned FuncCall's exported Args field, so its capacity is reachable
	// from a test, while `keys` never leaves the function.
	if got := cap(cascade.Args); got != wantArgs {
		t.Errorf("cap(cascade.Args) = %d; want %d — the capacity hint no longer "+
			"matches the args the cascade builds (mutant `*` → `+`/`-`/`/`/`%%` or "+
			"`+` → `-`/`*`/`/`/`%%` in detected_level.go:`len(keys)*2+1` shifts the "+
			"pre-allocation and append re-grows off the exact count)", got, wantArgs)
	}

	for i, key := range wantKeys {
		valIdx := 2*i + 1
		ma, ok := cascade.Args[valIdx].(*chplan.MapAccess)
		if !ok {
			t.Fatalf("value branch for %q (args[%d]) = %T; want MapAccess", key, valIdx, cascade.Args[valIdx])
		}
		col, ok := ma.Map.(*chplan.ColumnRef)
		if !ok || col.Name != s.AttributesColumn {
			t.Errorf("value branch for %q reads %#v; want ColumnRef(%q)", key, ma.Map, s.AttributesColumn)
		}
		lit, ok := ma.Key.(*chplan.LitString)
		if !ok || lit.V != key {
			t.Errorf("value branch %d key = %#v; want LitString(%q)", i, ma.Key, key)
		}
	}

	fallback, ok := cascade.Args[len(cascade.Args)-1].(*chplan.ColumnRef)
	if !ok || fallback.Name != s.SeverityColumn {
		t.Errorf("fallback branch = %#v; want ColumnRef(%q)", cascade.Args[len(cascade.Args)-1], s.SeverityColumn)
	}
}

// sourceCapHint evaluates the SHAPE of detected_level.go's cascade capacity
// hint `len(keys)*2+1` with each of its two operator positions supplied
// explicitly: `n mulOp 2 tailOp 1`. Passing the operators in is what lets the
// caller enumerate the hint's mutants without writing any of their values
// down.
func sourceCapHint(t *testing.T, n int, mulOp, tailOp string) int {
	t.Helper()

	return applyCapHintOp(t, applyCapHintOp(t, n, mulOp, 2), tailOp, 1)
}

// TestDetectedLevelSource_CapHintMutantsAreKilled proves that the `cap`
// assertion in [TestDetectedLevelSource_PrecedenceCascade] actually
// discriminates. For every ARITHMETIC_BASE operator substitution gremlins can
// make in detected_level.go:`args := make([]chplan.Expr, 0, len(keys)*2+1)`,
// the capacity the finished slice ends up with must differ from the capacity
// the unmutated hint produces — otherwise that assertion passes on the mutant
// and claims a kill it does not deliver.
//
// NOT KILLABLE — the third ARITHMETIC_BASE mutant this function carries, on
// the sibling hint detected_level.go:`keys := make([]string, 0,
// len(allowedLevelFields)+1)`, has no test that can observe it, and the reason
// is worth writing down because it is exactly what does NOT hold for the hint
// enumerated below. A capacity mutation is observable through `cap`, and `cap`
// is only reachable where the slice ESCAPES the builder. `args` escapes: it
// becomes the returned `*chplan.FuncCall`'s exported `Args` field, and the
// assertion above reads its capacity back. `keys` does not: it is ranged over
// and measured with `len` inside [detectedLevelSourceExpr] and never stored,
// returned or captured, so no caller — test or otherwise — holds the header
// whose capacity the mutation changes. Nor does that mutant die on a panic:
// `allowedLevelFields` carries four fixed entries, so every substitution
// (`-` → 3, `*` → 4, `/` → 4, `%` → 0) stays non-negative and `make` accepts
// it. It is an equivalent mutant, and the only honest thing to do with it is
// leave it counted as a survivor.
//
// So "an ARITHMETIC_BASE mutant on a `make` capacity argument is equivalent"
// is FALSE as a general claim: it holds for one of this function's two hints
// and fails for the other, on a property (escape) that is not visible in the
// mutated expression at all.
func TestDetectedLevelSource_CapHintMutantsAreKilled(t *testing.T) {
	t.Parallel()

	// The operators the unmutated hint uses, in the two positions `n * 2 + 1`.
	const (
		origMulOp  = "*"
		origTailOp = "+"
	)
	positions := []struct{ name, orig string }{
		{"the `*2`", origMulOp},
		{"the trailing `+1`", origTailOp},
	}

	// The cascade contributes one (condition, value) pair per source key and
	// one trailing severity-column fallback, so the pair count IS the key
	// count — the same `n` the hint measures with `len(keys)`.
	n := len(append([]string{detectedLevelLabel}, allowedLevelFields...))
	trueHint := sourceCapHint(t, n, origMulOp, origTailOp)
	trueLen, trueCap := capAfterPairedAppends(trueHint, n)

	// The premise of the whole argument: the appends FILL the pre-allocation
	// exactly — neither growing past it nor leaving slack. Only then does the
	// finished cap read back the hint, and only then does asserting cap pin the
	// hint's arithmetic.
	if trueLen != trueHint || trueCap != trueHint {
		t.Fatalf("the unmutated hint %d does not exactly fit the append sequence "+
			"(finished len %d, cap %d): the appends no longer fill the "+
			"pre-allocation exactly, so cap() has stopped reading back the hint "+
			"and asserting it has stopped being a capacity kill",
			trueHint, trueLen, trueCap)
	}

	// …and the sequence replayed here has to be the one the sibling test pins
	// on the real cascade, or this whole test measures a slice nobody builds.
	s := schema.DefaultOTelLogs()
	cascade, ok := detectedLevelSourceExpr(s).(*chplan.FuncCall)
	if !ok {
		t.Fatalf("source = %#v; want the multiIf cascade", detectedLevelSourceExpr(s))
	}
	if got := len(cascade.Args); got != trueLen {
		t.Fatalf("the replayed append sequence produces %d args, but the cascade "+
			"under test carries %d — the simulation has drifted from the builder "+
			"it stands in for", trueLen, got)
	}

	mutants, checked, negative := 0, 0, 0
	for i, pos := range positions {
		for _, op := range capHintOps {
			if op == pos.orig {
				continue
			}
			mutants++

			ops := []string{origMulOp, origTailOp}
			ops[i] = op
			hint := sourceCapHint(t, n, ops[0], ops[1])

			if hint < 0 {
				// `make` panics on a negative capacity, so this mutant dies in
				// any test that reaches detectedLevelSourceExpr at all.
				negative++
				continue
			}
			checked++

			if _, got := capAfterPairedAppends(hint, n); got == trueCap {
				t.Errorf("mutating %s to %q gives capacity hint %d, and the finished "+
					"slice still ends at cap %d — identical to the unmutated build, so "+
					"the `cap(cascade.Args) == wantArgs` assertion does NOT kill this mutant",
					pos.name, op, hint, got)
			}
		}
	}

	// Anti-vacuity: a loop that enumerated nothing would report a clean run
	// while proving nothing at all.
	if want := len(positions) * (len(capHintOps) - 1); mutants != want {
		t.Fatalf("enumerated %d hint mutants, want %d (one per operator substitution "+
			"in each of the %d positions) — the enumeration is not covering the "+
			"mutant set it claims to", mutants, want, len(positions))
	}
	if checked == 0 {
		t.Fatalf("every one of the %d hint mutants was skipped as a negative capacity, "+
			"so the discriminating comparison never ran", mutants)
	}
	t.Logf("hint mutants: %d enumerated, %d distinguished by capacity, %d killed by a "+
		"negative make() capacity", mutants, checked, negative)
}

// TestDetectedLevelSource_NoAttributesColumnCollapses verifies that a
// custom schema without a structured-metadata column resolves the level
// from the bare SeverityColumn only — byte-identical to the pre-cascade
// behaviour, so such schemas see zero churn.
func TestDetectedLevelSource_NoAttributesColumnCollapses(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	s.AttributesColumn = ""

	col, ok := detectedLevelSourceExpr(s).(*chplan.ColumnRef)
	if !ok || col.Name != s.SeverityColumn {
		t.Fatalf("source = %#v; want bare ColumnRef(%q) when AttributesColumn is empty", detectedLevelSourceExpr(s), s.SeverityColumn)
	}
}
