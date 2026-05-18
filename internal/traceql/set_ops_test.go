package traceql_test

import (
	"context"
	"testing"

	tempo "github.com/grafana/tempo/pkg/traceql"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
)

// TestLowerSetOps exercises the spanset set-op (`&&`, `||`) and the
// sibling (`~`) lowering directly. Parses each query, lowers it, and
// walks the resulting chplan tree to confirm the expected node kind +
// identity columns.
func TestLowerSetOps(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()

	t.Run("set_intersect", func(t *testing.T) {
		t.Parallel()
		expr, err := tempo.Parse(`{ resource.service.name = "a" } && { duration > 1s }`)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		plan, err := traceql.Lower(context.Background(), expr, s)
		if err != nil {
			t.Fatalf("Lower: %v", err)
		}
		so, ok := plan.(*chplan.SetOperation)
		if !ok {
			t.Fatalf("expected *chplan.SetOperation, got %T", plan)
		}
		if so.Op != chplan.SetIntersect {
			t.Errorf("Op = %q, want %q", so.Op, chplan.SetIntersect)
		}
		if so.TraceIDColumn != s.TraceIDColumn || so.SpanIDColumn != s.SpanIDColumn {
			t.Errorf("identity columns wrong: TraceID=%q SpanID=%q",
				so.TraceIDColumn, so.SpanIDColumn)
		}
	})

	t.Run("set_union", func(t *testing.T) {
		t.Parallel()
		expr, err := tempo.Parse(`{ resource.service.name = "a" } || { resource.service.name = "b" }`)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		plan, err := traceql.Lower(context.Background(), expr, s)
		if err != nil {
			t.Fatalf("Lower: %v", err)
		}
		so, ok := plan.(*chplan.SetOperation)
		if !ok {
			t.Fatalf("expected *chplan.SetOperation, got %T", plan)
		}
		if so.Op != chplan.SetUnion {
			t.Errorf("Op = %q, want %q", so.Op, chplan.SetUnion)
		}
	})

	t.Run("sibling", func(t *testing.T) {
		t.Parallel()
		expr, err := tempo.Parse(`{ name = "parent" } ~ { name = "sibling" }`)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		plan, err := traceql.Lower(context.Background(), expr, s)
		if err != nil {
			t.Fatalf("Lower: %v", err)
		}
		sj, ok := plan.(*chplan.StructuralJoin)
		if !ok {
			t.Fatalf("expected *chplan.StructuralJoin, got %T", plan)
		}
		if sj.Op != chplan.StructuralSibling {
			t.Errorf("Op = %q, want %q", sj.Op, chplan.StructuralSibling)
		}
		if sj.ParentSpanIDColumn != s.ParentSpanIDColumn {
			t.Errorf("ParentSpanIDColumn = %q, want %q",
				sj.ParentSpanIDColumn, s.ParentSpanIDColumn)
		}
	})
}

// TestLowerNegatedAndUnionStructural exercises the negated (`!>`,
// `!<`, `!~`, `!>>`, `!<<`) and union-prefixed (`&>`, `&<`, `&~`,
// `&>>`, `&<<`) structural variants. Each query should parse, lower
// to a chplan.StructuralJoin, and carry the corresponding negated /
// union Op constant — the emitter then turns the negated forms into a
// LEFT ANTI JOIN and the union forms into a UNION DISTINCT pair (see
// internal/chsql/structural_join.go).
func TestLowerNegatedAndUnionStructural(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()

	cases := []struct {
		name   string
		query  string
		wantOp chplan.StructuralOp
	}{
		{"not_child", `{ name = "a" } !> { name = "b" }`, chplan.StructuralNotChild},
		{"not_parent", `{ name = "a" } !< { name = "b" }`, chplan.StructuralNotParent},
		{"not_sibling", `{ name = "a" } !~ { name = "b" }`, chplan.StructuralNotSibling},
		{"not_descendant", `{ name = "a" } !>> { name = "b" }`, chplan.StructuralNotDescendant},
		{"not_ancestor", `{ name = "a" } !<< { name = "b" }`, chplan.StructuralNotAncestor},
		{"union_child", `{ name = "a" } &> { name = "b" }`, chplan.StructuralUnionChild},
		{"union_parent", `{ name = "a" } &< { name = "b" }`, chplan.StructuralUnionParent},
		{"union_sibling", `{ name = "a" } &~ { name = "b" }`, chplan.StructuralUnionSibling},
		{"union_descendant", `{ name = "a" } &>> { name = "b" }`, chplan.StructuralUnionDescendant},
		{"union_ancestor", `{ name = "a" } &<< { name = "b" }`, chplan.StructuralUnionAncestor},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := tempo.Parse(tc.query)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.query, err)
			}
			plan, err := traceql.Lower(context.Background(), expr, s)
			if err != nil {
				t.Fatalf("Lower(%q): %v", tc.query, err)
			}
			sj, ok := plan.(*chplan.StructuralJoin)
			if !ok {
				t.Fatalf("Lower(%q): expected *chplan.StructuralJoin, got %T", tc.query, plan)
			}
			if sj.Op != tc.wantOp {
				t.Errorf("Op = %q, want %q", sj.Op, tc.wantOp)
			}
			if sj.TraceIDColumn != s.TraceIDColumn ||
				sj.SpanIDColumn != s.SpanIDColumn ||
				sj.ParentSpanIDColumn != s.ParentSpanIDColumn {
				t.Errorf("identity columns wrong: TraceID=%q SpanID=%q ParentSpanID=%q",
					sj.TraceIDColumn, sj.SpanIDColumn, sj.ParentSpanIDColumn)
			}
		})
	}
}
