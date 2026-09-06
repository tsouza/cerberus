package chsql

import (
	"strings"
	"testing"
)

// TestInSubquery_GlobalFollowsPhysicalScans pins the render-time rule
// InSubquery / NotInSubquery document (cerberus issues #3128 / #3141): a
// subquery that scans a physical table is written GLOBAL — so a pushed-down
// outer statement never re-executes it on every shard — and a subquery that
// scans none (an external temporary table, a literal set) stays a plain IN.
// Both keywords and both directions are pinned, each against the rendered
// text, so the rule cannot silently invert or apply to the wrong side.
func TestInSubquery_GlobalFollowsPhysicalScans(t *testing.T) {
	t.Parallel()
	physical := func() Frag {
		return NewQuery().Select(Col("TraceId")).From(physicalTableFrag("otel_traces")).Frag()
	}
	external := func() Frag {
		return NewQuery().Select(Col("TraceId")).From(Col("_cerberus_ext_ids")).Frag()
	}
	cases := []struct {
		name string
		frag Frag
		want string
		not  string
	}{
		{"IN over a physical scan is GLOBAL", InSubquery(Col("TraceId"), physical()), "`TraceId` GLOBAL IN (SELECT `TraceId` FROM `otel_traces`)", " NOT IN"},
		{"IN over an external table stays plain", InSubquery(Col("TraceId"), external()), "`TraceId` IN (SELECT `TraceId` FROM `_cerberus_ext_ids`)", "GLOBAL"},
		{"NOT IN over a physical scan is GLOBAL NOT IN", NotInSubquery(Col("sig"), physical()), "`sig` GLOBAL NOT IN ((SELECT `TraceId` FROM `otel_traces`))", ""},
		{"NOT IN over an external table stays plain", NotInSubquery(Col("sig"), external()), "`sig` NOT IN ((SELECT `TraceId` FROM `_cerberus_ext_ids`))", "GLOBAL"},
		{"tuple IN over a pre-rendered physical subquery is GLOBAL", InSubquery(Tuple(Col("TraceId"), Col("SpanId")), Subquery(PreRenderedSQL{SQL: "SELECT 1", PhysicalScans: 2})), "(`TraceId`, `SpanId`) GLOBAL IN (SELECT 1)", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := NewBuilder()
			tc.frag(b)
			sql, _, err := b.Build()
			if err != nil {
				t.Fatal(err)
			}
			if sql != tc.want {
				t.Fatalf("rendered %q, want %q", sql, tc.want)
			}
			if tc.not != "" && strings.Contains(sql, tc.not) {
				t.Fatalf("rendered %q must not contain %q", sql, tc.not)
			}
		})
	}
}

// TestInSubquery_ScanCountAndArgsSurvive pins that routing the subquery
// through the scratch Builder loses nothing: its positional args land in
// order and its physical-scan count still reaches the enclosing Builder.
func TestInSubquery_ScanCountAndArgsSurvive(t *testing.T) {
	t.Parallel()
	sub := NewQuery().Select(Col("TraceId")).From(physicalTableFrag("otel_traces")).Where(Eq(Col("ServiceName"), Lit("svc"))).Frag()
	b := NewBuilder()
	And(Eq(Col("x"), Lit(1)), InSubquery(Col("TraceId"), sub))(b)
	sql, args, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sql, "GLOBAL IN") {
		t.Fatalf("expected GLOBAL IN in %q", sql)
	}
	if len(args) != 2 || args[0] != 1 || args[1] != "svc" {
		t.Fatalf("args = %#v, want [1 svc] in render order", args)
	}
	if b.PhysicalScans() != 1 {
		t.Fatalf("PhysicalScans = %d, want 1", b.PhysicalScans())
	}
}
