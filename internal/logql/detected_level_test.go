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
