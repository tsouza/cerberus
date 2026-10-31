package traceql_test

import (
	"context"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
	tempo "github.com/tsouza/cerberus/internal/traceql/ast"
)

// TestTraceScopedRootlessMembershipForm pins which membership form a
// root-identity comparison lowers to. The per-trace aggregate runs over
// root spans only, so a trace with no root span has no aggregate row;
// reference Tempo answers such a trace with an empty rootName /
// rootServiceName. A comparison the empty string satisfies must therefore
// lower to `NOT (TraceId IN (<rooted traces failing it>))`, which a
// rootless trace passes, and every other comparison to the plain
// `TraceId IN (<rooted traces satisfying it>)`, which a rootless trace
// fails. The chDB-executed answers for the rootless trace are pinned by
// test/spec/traceql/trace_scoped_root_name_rootless_*.txtar and
// trace_scoped_root_name_regex_none.txtar, each checked against the
// reference engine through its parity section.
func TestTraceScopedRootlessMembershipForm(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()

	for _, tc := range []struct {
		query           string
		rootlessMatches bool
	}{
		{`{ rootName = "GET /home" }`, false},
		{`{ rootName != "GET /home" }`, true},
		{`{ rootName = "" }`, true},
		{`{ rootName != "" }`, false},
		{`{ rootServiceName =~ "front.*" }`, false},
		{`{ rootServiceName =~ ".*" }`, true},
		{`{ rootServiceName !~ "front.*" }`, true},
		{`{ rootServiceName !~ ".*" }`, false},
		{`{ rootName < "a" }`, true},
		{`{ rootName > "a" }`, false},
		{`{ "GET /home" != trace:rootName }`, true},
		{`{ trace:rootService = "checkout" || trace:rootService = "billing" }`, false},
		{`{ trace:rootService != "checkout" && trace:rootService != "billing" }`, true},
		{`{ rootName =~ "GET .*" || rootName =~ "POST .*" }`, false},
		{`{ rootName !~ "GET .*" && rootName !~ "POST .*" }`, true},
	} {
		expr, err := tempo.Parse(tc.query)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.query, err)
		}
		plan, err := traceql.Lower(context.Background(), expr, s)
		if err != nil {
			t.Fatalf("Lower(%q): %v", tc.query, err)
		}
		filter, ok := plan.(*chplan.Filter)
		if !ok {
			t.Fatalf("Lower(%q): want *chplan.Filter, got %T", tc.query, plan)
		}
		in, negated := membershipOf(filter.Predicate)
		if in == nil {
			t.Errorf("Lower(%q): predicate %T is neither TraceId IN (...) nor its negation", tc.query, filter.Predicate)
			continue
		}
		if negated != tc.rootlessMatches {
			t.Errorf("Lower(%q): negated membership = %v, want %v (a rootless trace must match exactly when the empty root identity satisfies the comparison)",
				tc.query, negated, tc.rootlessMatches)
		}
		if !aggregatesRootSpansOnly(in.Subquery) {
			t.Errorf("Lower(%q): per-trace aggregate does not read a root-only (ParentSpanId = '') span scan", tc.query)
		}
	}
}

// membershipOf unwraps the trace-scoped membership predicate: a bare
// *chplan.InSubquery, or one under a single NOT.
func membershipOf(e chplan.Expr) (in *chplan.InSubquery, negated bool) {
	if not, ok := e.(*chplan.FuncCall); ok && not.Fn == chplan.FnNot && len(not.Args) == 1 {
		in, _ = not.Args[0].(*chplan.InSubquery)
		return in, true
	}
	in, _ = e.(*chplan.InSubquery)
	return in, false
}

// aggregatesRootSpansOnly reports whether the subquery's Aggregate reads
// a Filter(ParentSpanId = "") directly over the span scan.
func aggregatesRootSpansOnly(n chplan.Node) bool {
	for n != nil {
		if agg, ok := n.(*chplan.Aggregate); ok {
			f, ok := agg.Input.(*chplan.Filter)
			if !ok {
				return false
			}
			if _, ok := f.Input.(*chplan.Scan); !ok {
				return false
			}
			cond, ok := f.Predicate.(*chplan.Binary)
			if !ok || cond.Op != chplan.OpEq {
				return false
			}
			col, colOK := cond.Left.(*chplan.ColumnRef)
			lit, litOK := cond.Right.(*chplan.LitString)
			return colOK && litOK && col.Name == "ParentSpanId" && lit.V == ""
		}
		children := n.Children()
		if len(children) != 1 {
			return false
		}
		n = children[0]
	}
	return false
}
