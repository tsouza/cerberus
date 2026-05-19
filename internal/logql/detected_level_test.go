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
	var sawMapAccessLevel bool
	walkExprTree(filt.Predicate, func(e chplan.Expr) {
		bin, ok := e.(*chplan.Binary)
		if !ok || bin.Op != chplan.OpEq {
			return
		}
		rhs, ok := bin.Right.(*chplan.LitString)
		if !ok || rhs.V != "error" {
			return
		}
		ma, ok := bin.Left.(*chplan.MapAccess)
		if !ok {
			return
		}
		keyLit, ok := ma.Key.(*chplan.LitString)
		if !ok || keyLit.V != "level" {
			return
		}
		sawMapAccessLevel = true
	})
	if !sawMapAccessLevel {
		t.Errorf("expected MapAccess(<labels>, \"level\") = \"error\" filter; got plan %v", filt.Predicate)
	}
}

// TestDetectedLevel_NoColumnRefToDetectedLevelLabel verifies that no
// stray `ResourceAttributes["detected_level"]` MapAccess survives in
// the lowered tree — the synthesized normalisation should fully
// shadow the plain map lookup. A failure here would mean the
// dispatch missed a code path.
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
		lit, ok := ma.Key.(*chplan.LitString)
		if !ok {
			return
		}
		if lit.V == detectedLevelLabel {
			found = true
		}
	})
	if found {
		t.Errorf("plan still contains ResourceAttributes[\"detected_level\"] map lookup; want fully synthesized expression")
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
