package traceql_test

import (
	"context"
	"strings"
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

// TestLowerSetOpsUnsupported confirms that negated / union-prefixed
// structural variants surface as clean errors rather than panicking
// (they're parser-legal but cerberus doesn't lower them yet).
func TestLowerSetOpsUnsupported(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()

	cases := []struct {
		name  string
		query string
	}{
		{"not_child", `{ name = "a" } !> { name = "b" }`},
		{"not_parent", `{ name = "a" } !< { name = "b" }`},
		{"not_sibling", `{ name = "a" } !~ { name = "b" }`},
		{"union_child", `{ name = "a" } &> { name = "b" }`},
		{"union_sibling", `{ name = "a" } &~ { name = "b" }`},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := tempo.Parse(tc.query)
			if err != nil {
				// Some shapes may fail at parse time on certain Tempo
				// versions; the test documents lowering behavior so a
				// parse-time rejection is acceptable.
				return
			}
			_, err = traceql.Lower(context.Background(), expr, s)
			if err == nil {
				t.Fatalf("Lower(%q): expected error, got nil", tc.query)
			}
			if !strings.Contains(err.Error(), "not yet supported") {
				t.Errorf("Lower(%q) error = %q, want substring %q",
					tc.query, err.Error(), "not yet supported")
			}
		})
	}
}
