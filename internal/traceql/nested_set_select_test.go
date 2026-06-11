package traceql_test

import (
	"context"
	"strings"
	"testing"

	tempo "github.com/grafana/tempo/pkg/traceql"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
	"github.com/tsouza/cerberus/test/spec"
)

// TestSelectNestedSet_WrapsAnnotate pins the `| select(nestedSet*)`
// lowering shape: the input spanset is wrapped in a NestedSetAnnotate
// (which recomputes Tempo's ingest-time nested-set numbering at query
// time) and each intrinsic projects the node's synthetic column under
// its TraceQL name. This is the projection clause Grafana Traces
// Drilldown's "Structure" tab sends; before this support landed it
// 422'd with "has no OTel ClickHouse backing column".
func TestSelectNestedSet_WrapsAnnotate(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()

	expr, err := tempo.Parse(`{ nestedSetParent < 0 } | select(nestedSetParent, nestedSetLeft, nestedSetRight)`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	plan, err := traceql.Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}

	p, ok := plan.(*chplan.Project)
	if !ok {
		t.Fatalf("plan root = %T, want *chplan.Project", plan)
	}
	ann, ok := p.Input.(*chplan.NestedSetAnnotate)
	if !ok {
		t.Fatalf("Project input = %T, want *chplan.NestedSetAnnotate", p.Input)
	}
	if ann.SpansTable != s.SpansTable || ann.TraceIDColumn != s.TraceIDColumn ||
		ann.SpanIDColumn != s.SpanIDColumn || ann.ParentSpanIDColumn != s.ParentSpanIDColumn ||
		ann.TimestampColumn != s.TimestampColumn {
		t.Fatalf("NestedSetAnnotate columns not threaded from schema: %+v", ann)
	}

	wantAlias := map[string]string{
		"nestedSetParent": chplan.NestedSetParentColumn,
		"nestedSetLeft":   chplan.NestedSetLeftColumn,
		"nestedSetRight":  chplan.NestedSetRightColumn,
	}
	found := 0
	for _, pr := range p.Projections {
		col, want := wantAlias[pr.Alias]
		if !want {
			continue
		}
		found++
		ref, ok := pr.Expr.(*chplan.ColumnRef)
		if !ok || ref.Name != col {
			t.Errorf("projection %q = %#v, want ColumnRef{%s}", pr.Alias, pr.Expr, col)
		}
	}
	if found != 3 {
		t.Fatalf("found %d nested-set projections, want 3 (projections: %+v)", found, p.Projections)
	}
}

// TestSelectWithoutNestedSet_NoAnnotate pins that plain select()
// queries keep their historical Project(Filter(Scan)) shape — the
// numbering join is strictly pay-for-what-you-select.
func TestSelectWithoutNestedSet_NoAnnotate(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()

	expr, err := tempo.Parse(`{ } | select(span.http.method)`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	plan, err := traceql.Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	p, ok := plan.(*chplan.Project)
	if !ok {
		t.Fatalf("plan root = %T, want *chplan.Project", plan)
	}
	if _, isAnnotate := p.Input.(*chplan.NestedSetAnnotate); isAnnotate {
		t.Fatal("select() without nested-set intrinsics must not wrap NestedSetAnnotate")
	}
}

// TestNestedSetOutsideSelectAnnotates pins the non-select bare
// reference positions: by() grouping and aggregate operands recompute
// the nested-set numbering via the same NestedSetAnnotate pass
// lowerSelect applies, then read the synthetic column. Reference Tempo
// materialises these positions and /api/search accepts the queries (the
// rejection-parity layer flagged the old 422s as wrong_rejections).
func TestNestedSetOutsideSelectAnnotates(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()

	for _, q := range []string{
		`{ } | by(nestedSetParent)`,
		`{ } | avg(nestedSetLeft) > 1`,
	} {
		expr, err := tempo.Parse(q)
		if err != nil {
			t.Fatalf("Parse(%q): %v", q, err)
		}
		plan, err := traceql.Lower(context.Background(), expr, s)
		if err != nil {
			t.Errorf("Lower(%q): want successful annotation-backed lowering, got: %v", q, err)
			continue
		}
		if !strings.Contains(spec.PrintChplan(plan), "NestedSetAnnotate") {
			t.Errorf("Lower(%q): plan must wrap a NestedSetAnnotate to materialise the position", q)
		}
	}
}

// TestUnionArmAlignment pins the `||` arm-shape alignment: a
// structural arm exposes the narrow span envelope while a plain
// filter arm is `SELECT *`; mixing them must wrap the plain arm in a
// Project of the same ordered column list or ClickHouse rejects the
// UNION DISTINCT with code 258 (UNION_ALL_RESULT_STRUCTURES_MISMATCH)
// — the exact failure hiding behind Traces Drilldown's structure-tab
// query once the select() 422 was fixed.
func TestUnionArmAlignment(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()

	t.Run("structural-or-plain wraps the plain arm", func(t *testing.T) {
		t.Parallel()
		expr, err := tempo.Parse(`({ nestedSetParent < 0 } &>> { kind = server }) || ({ nestedSetParent < 0 })`)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		plan, err := traceql.Lower(context.Background(), expr, s)
		if err != nil {
			t.Fatalf("Lower: %v", err)
		}
		setOp, ok := plan.(*chplan.SetOperation)
		if !ok {
			t.Fatalf("plan root = %T, want *chplan.SetOperation", plan)
		}
		if _, ok := setOp.Left.(*chplan.StructuralJoin); !ok {
			t.Fatalf("left arm = %T, want *chplan.StructuralJoin", setOp.Left)
		}
		proj, ok := setOp.Right.(*chplan.Project)
		if !ok {
			t.Fatalf("right arm = %T, want *chplan.Project (narrow alignment wrap)", setOp.Right)
		}
		// The wrap's column list must lead with the three join keys —
		// the structural arm's positional prefix.
		wantPrefix := []string{s.TraceIDColumn, s.SpanIDColumn, s.ParentSpanIDColumn}
		if len(proj.Projections) < len(wantPrefix) {
			t.Fatalf("alignment Project has %d projections, want >= %d", len(proj.Projections), len(wantPrefix))
		}
		for i, want := range wantPrefix {
			ref, ok := proj.Projections[i].Expr.(*chplan.ColumnRef)
			if !ok || ref.Name != want {
				t.Errorf("alignment projection[%d] = %#v, want ColumnRef{%s}", i, proj.Projections[i].Expr, want)
			}
		}
	})

	t.Run("plain-or-plain stays unwrapped", func(t *testing.T) {
		t.Parallel()
		expr, err := tempo.Parse(`{ kind = server } || { kind = client }`)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		plan, err := traceql.Lower(context.Background(), expr, s)
		if err != nil {
			t.Fatalf("Lower: %v", err)
		}
		setOp, ok := plan.(*chplan.SetOperation)
		if !ok {
			t.Fatalf("plan root = %T, want *chplan.SetOperation", plan)
		}
		if _, ok := setOp.Left.(*chplan.Project); ok {
			t.Error("plain || plain must not wrap the left arm")
		}
		if _, ok := setOp.Right.(*chplan.Project); ok {
			t.Error("plain || plain must not wrap the right arm")
		}
	})
}
