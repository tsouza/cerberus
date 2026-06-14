package logql

import (
	"testing"

	"github.com/grafana/loki/v3/pkg/logql/syntax"

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
			if fn.Name != "multiIf" {
				t.Errorf("matcher %q: LHS func = %q; want %q", tc.name, fn.Name, "multiIf")
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
	// projection's expression. It should be a `map(...)` whose value
	// for `level` is the multiIf normalisation (a FuncCall named
	// multiIf), not a MapAccess on ResourceAttributes.
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
	mapCall, ok := proj.Projections[0].Expr.(*chplan.FuncCall)
	if !ok || mapCall.Name != "map" {
		t.Fatalf("identity projection = %v; want FuncCall(map, ...)", proj.Projections[0].Expr)
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
	if !ok || multiIf.Name != "multiIf" {
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
		if !ok || lhs.Name != "if" {
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
		if !ok || fn.Name != "multiIf" {
			continue
		}
		if _, ok := b.Right.(*chplan.LitString); ok {
			return b
		}
	}
	t.Fatalf("no detected_level Binary found in plan; bins=%d", len(bins))
	return nil
}

// walkChplanExpr is a minimal sibling of the cross-package
// chplan.WalkExpr used by collectColumnRefs. Visits every Expr node
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

// canonicalLevelGroups mirrors the (variants, canonical) table inside
// [normaliseLevelExpr] so the assertions below can pin the documented
// 7-level order without re-importing the unexported `group` struct.
// Order matches the source — upstream Loki's `normalizeLogLevel` switch
// (trace / debug / info / warn / error / critical / fatal).
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

// TestNormaliseLevelExpr_MultiIfArgsCapacityAndShape kills the two
// adjacent ARITHMETIC_BASE mutants reported by the gremlins phase-4
// run at `detected_level.go:143:44` (the `*` in `len(groups)*2+1`)
// and `:143:46` (the `+` in the same expression). The mutation site
// is the slice-capacity hint passed to `make([]chplan.Expr, 0,
// len(groups)*2+1)` — `*` flipping to `/` (or `%`) and `+` flipping
// to `-` (or `*`) change the pre-allocated capacity but `append`
// silently grows past it, so a semantic-only test cannot observe the
// difference. This test pins:
//
//  1. `len(Args) == 2*len(canonicalLevelGroups)+1` (the load-bearing
//     count: 7 (cond, literal) pairs plus the trailing default
//     branch) — documents the arithmetic so a structural mutation
//     elsewhere shows up immediately.
//  2. `cap(Args) == 2*len(canonicalLevelGroups)+1` (the direct kill:
//     `make([]T, 0, N)` returns a slice with `cap == N` before any
//     append, and the 15 subsequent appends never exceed that
//     capacity — so the final `cap` equals the hint). Any
//     ARITHMETIC_BASE mutation on `*` or `+` at col 44/46 shifts the
//     allocated capacity, triggers an `append`-driven re-allocation
//     with Go's runtime growth strategy, and produces a final `cap`
//     that is NOT 15 (the original mutants would yield e.g. 4, 8,
//     16, or 26 depending on the operator). Asserting `cap == 15`
//     exactly catches each shift.
//
// The cap assertion is the only direct kill for a slice-capacity
// arithmetic mutant — append's growth strategy hides the change from
// length-only tests, and the SQL the emitter prints from the
// resulting FuncCall is byte-identical regardless of how the
// underlying slice was allocated.
func TestNormaliseLevelExpr_MultiIfArgsCapacityAndShape(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelLogs()
	expr := detectedLevelExpr(s)

	fn, ok := expr.(*chplan.FuncCall)
	if !ok {
		t.Fatalf("detectedLevelExpr returned %T; want *chplan.FuncCall (multiIf)", expr)
	}
	if fn.Name != "multiIf" {
		t.Fatalf("FuncCall.Name = %q; want %q", fn.Name, "multiIf")
	}

	// The leading (empty → "unknown") pair — reference Loki stamps
	// `detected_level="unknown"` when no level is detectable
	// (pkg/distributor/field_detection.go, constants.LogLevelUnknown)
	// — precedes the 7 canonical groups, then the lowercased default.
	wantLen := 2*(len(canonicalLevelGroups)+1) + 1
	if got := len(fn.Args); got != wantLen {
		t.Fatalf("len(multiIf.Args) = %d; want %d (1 (empty, unknown) pair + 7 (cond, literal) pairs + 1 default)", got, wantLen)
	}

	// Kill: any arithmetic mutation on the capacity hint either
	// over-allocates (appends fit, final cap = the mutated hint) or
	// under-allocates (append re-grows via the runtime's schedule) —
	// both produce a final cap different from the exact arg count.
	// Asserting cap == wantLen pins the arithmetic.
	if got, want := cap(fn.Args), wantLen; got != want {
		t.Fatalf("cap(multiIf.Args) = %d; want %d (mutant `*` → `/`/`%%` or `+` → `-`/`*` at detected_level.go:143:44 / :143:46 would shift the capacity hint and re-allocate via append's growth schedule)", got, want)
	}
}

// TestNormaliseLevelExpr_CanonicalLevelOrder pins the exact (cond,
// literal) pair structure of the multiIf chain. The 14 paired slots
// follow the canonical 7-level enumeration trace / debug / info /
// warn / error / critical / fatal — in that order — and the 15th
// (default) slot is the lowercased pass-through. A regression that
// drops a group, reorders the groups, or swaps an OR-chain for an
// unrelated condition will fail here. Combined with the cap test
// above this also serves as a structural backstop: it forces the
// `args` slice to actually be built end-to-end, so a capacity
// mutation can't quietly survive by also short-circuiting the loop.
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
	if lowerCall, ok := emptyCond.Left.(*chplan.FuncCall); !ok || lowerCall.Name != "lower" {
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
	// precedence cascade. A mutation on `+1` that drops the default
	// slot would shorten Args to 14 and trip the len check above; this
	// assertion pins the SHAPE of the default branch so a refactor that
	// swaps it for something else (e.g. an empty string fall-through)
	// still trips a test.
	defaultIdx := 2 * (len(canonicalLevelGroups) + 1)
	defaultCall, ok := fn.Args[defaultIdx].(*chplan.FuncCall)
	if !ok {
		t.Fatalf("default branch at args[%d] = %T; want *chplan.FuncCall (lower(...))", defaultIdx, fn.Args[defaultIdx])
	}
	if defaultCall.Name != "lower" {
		t.Errorf("default branch FuncCall.Name = %q; want %q", defaultCall.Name, "lower")
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
	if !ok || mi.Name != "multiIf" {
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
	if !ok || cascade.Name != "multiIf" {
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
