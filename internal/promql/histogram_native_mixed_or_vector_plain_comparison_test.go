package promql_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_ExpHistogram_MixedSetOpOr_VectorPlainCompareFilter pins the
// non-`bool` comparison shape for a mixed `or` paired with a plain
// vector — mirrors [TestLower_ExpHistogram_MixedSetOpOr_VectorVectorCompareFilter].
func TestLower_ExpHistogram_MixedSetOpOr_VectorPlainCompareFilter(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for _, op := range []string{"==", "!=", "<", "<=", ">", ">="} {
		for _, mixedOnLeft := range []bool{true, false} {
			op, mixedOnLeft := op, mixedOnLeft
			name := op
			if !mixedOnLeft {
				name += "_plain_left"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				query := mixedOrExpr + " " + op + " " + mvpPlainVector
				if !mixedOnLeft {
					query = mvpPlainVector + " " + op + " " + mixedOrExpr
				}
				expr, err := p.ParseExpr(query)
				if err != nil {
					t.Fatalf("ParseExpr(%q): %v", query, err)
				}
				plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
				if err != nil {
					t.Fatalf("LowerAt(%q): unexpected error: %v", query, err)
				}
				if shape := chplan.RowShapeOf(plan); shape != chplan.MixedRowShape {
					t.Fatalf("lower(%q): plan root publishes %s, want %s", query, shape, chplan.MixedRowShape)
				}
				proj, ok := plan.(*chplan.Project)
				if !ok {
					t.Fatalf("lower(%q): plan root is %T, want *chplan.Project", query, plan)
				}
				if len(proj.Projections) != 14 {
					t.Fatalf("lower(%q): plan root projects %d columns, want 14", query, len(proj.Projections))
				}
				filter, ok := proj.Input.(*chplan.Filter)
				if !ok {
					t.Fatalf("lower(%q): Project.Input is %T, want *chplan.Filter", query, proj.Input)
				}
				join, ok := filter.Input.(*chplan.MixedVectorJoin)
				if !ok {
					t.Fatalf("lower(%q): Filter.Input is %T, want *chplan.MixedVectorJoin", query, filter.Input)
				}
				assertMixedJoinSides(t, query, join, mixedOnLeft)
			})
		}
	}
}

// TestLower_ExpHistogram_MixedSetOpOr_VectorPlainCompareBool pins the
// `bool`-modified comparison shape: always float-valued
// ([chplan.SampleRowShape]).
func TestLower_ExpHistogram_MixedSetOpOr_VectorPlainCompareBool(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	query := mixedOrExpr + " == bool " + mvpPlainVector
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
	if err != nil {
		t.Fatalf("LowerAt(%q): unexpected error: %v", query, err)
	}
	if shape := chplan.RowShapeOf(plan); shape != chplan.SampleRowShape {
		t.Fatalf("lower(%q): plan root publishes %s, want %s", query, shape, chplan.SampleRowShape)
	}
	proj, ok := plan.(*chplan.Project)
	if !ok {
		t.Fatalf("lower(%q): plan root is %T, want *chplan.Project", query, plan)
	}
	if len(proj.Projections) != 4 {
		t.Fatalf("lower(%q): plan root projects %d columns, want 4", query, len(proj.Projections))
	}
	filter, ok := proj.Input.(*chplan.Filter)
	if !ok {
		t.Fatalf("lower(%q): Project.Input is %T, want *chplan.Filter", query, proj.Input)
	}
	if _, ok := filter.Input.(*chplan.MixedVectorJoin); !ok {
		t.Fatalf("lower(%q): Filter.Input is %T, want *chplan.MixedVectorJoin", query, filter.Input)
	}
}
