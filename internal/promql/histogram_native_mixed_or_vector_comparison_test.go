package promql_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_ExpHistogram_MixedSetOpOr_VectorVectorCompareFilter pins
// cerberus issue #2449's eighth wrapper family without `bool`: the
// lowered plan stays the full fourteen-column Mixed shape
// ([chplan.MixedRowShape]), since the surviving combinations
// (float,float always; histogram,histogram for `==`/`!=` only) can each
// independently be float- or histogram-payload rows at runtime — see
// histogram_native_mixed_or_vector_comparison.go's header for the
// four-combination accounting.
func TestLower_ExpHistogram_MixedSetOpOr_VectorVectorCompareFilter(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for _, op := range []string{"==", "!=", "<", "<=", ">", ">="} {
		op := op
		t.Run(op, func(t *testing.T) {
			t.Parallel()
			query := mixedOrExpr + " " + op + " " + mixedOrExpr
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
				t.Fatalf("lower(%q): plan root projects %d columns, want 14 (the fourteen-column Mixed shape)", query, len(proj.Projections))
			}
			if proj.Projections[0].Alias != s.MetricNameColumn {
				t.Fatalf("lower(%q): first projection alias = %q, want %q", query, proj.Projections[0].Alias, s.MetricNameColumn)
			}
			if _, ok := proj.Projections[0].Expr.(*chplan.LitString); ok {
				t.Fatalf("lower(%q): MetricName projection is a forced empty literal, want the forwarded L column (comparisons never changesMetricSchema)", query)
			}
			last := proj.Projections[len(proj.Projections)-1]
			if last.Alias != chplan.MixedDiscriminatorColumn {
				t.Fatalf("lower(%q): last projection alias = %q, want %q", query, last.Alias, chplan.MixedDiscriminatorColumn)
			}
			filter, ok := proj.Input.(*chplan.Filter)
			if !ok {
				t.Fatalf("lower(%q): Project.Input is %T, want *chplan.Filter", query, proj.Input)
			}
			if _, ok := filter.Input.(*chplan.MixedVectorJoin); !ok {
				t.Fatalf("lower(%q): Filter.Input is %T, want *chplan.MixedVectorJoin", query, filter.Input)
			}
		})
	}
}

// TestLower_ExpHistogram_MixedSetOpOr_VectorVectorCompareBool pins
// cerberus issue #2449's eighth wrapper family WITH `bool`: the output is
// always float-valued ([chplan.SampleRowShape]) — reference forces the
// histogram payload to nil and the name to "" for every surviving row —
// regardless of which op is used.
func TestLower_ExpHistogram_MixedSetOpOr_VectorVectorCompareBool(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for _, op := range []string{"==", "!=", "<", "<=", ">", ">="} {
		op := op
		t.Run(op, func(t *testing.T) {
			t.Parallel()
			query := mixedOrExpr + " " + op + " bool " + mixedOrExpr
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
				t.Fatalf("lower(%q): plan root projects %d columns, want 4 (the plain canonical quartet)", query, len(proj.Projections))
			}
			lit, ok := proj.Projections[0].Expr.(*chplan.LitString)
			if !ok || lit.V != "" {
				t.Fatalf("lower(%q): __name__ projection = %#v, want an empty LitString", query, proj.Projections[0].Expr)
			}
			filter, ok := proj.Input.(*chplan.Filter)
			if !ok {
				t.Fatalf("lower(%q): Project.Input is %T, want *chplan.Filter", query, proj.Input)
			}
			if _, ok := filter.Input.(*chplan.MixedVectorJoin); !ok {
				t.Fatalf("lower(%q): Filter.Input is %T, want *chplan.MixedVectorJoin", query, filter.Input)
			}
		})
	}
}

// TestLower_ExpHistogram_MixedSetOpOr_VectorVectorCompareGroupLeftRight
// pins that group_left()/group_right() between two mixed `or` operands
// now lowers for comparisons too, the same
// [chplan.MixedVectorJoin] Card/Include support
// [vectorVectorArithmeticOverMixedExpHistogramSetOp]'s own doc describes
// for arithmetic (cerberus issue #2449's ninth wrapper family).
func TestLower_ExpHistogram_MixedSetOpOr_VectorVectorCompareGroupLeftRight(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name        string
		query       string
		wantCard    chplan.VectorCard
		wantInclude []string
	}{
		{
			name:     "group_left, bool, no Include",
			query:    mixedOrExpr + ` == bool on(job) group_left() ` + mixedOrExpr,
			wantCard: chplan.CardManyToOne,
		},
		{
			name:        "group_right, no bool, with Include",
			query:       mixedOrExpr + ` == on(job) group_right(instance) ` + mixedOrExpr,
			wantCard:    chplan.CardOneToMany,
			wantInclude: []string{"instance"},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
			if err != nil {
				t.Fatalf("LowerAt(%q): unexpected error: %v", tc.query, err)
			}
			proj, ok := plan.(*chplan.Project)
			if !ok {
				t.Fatalf("lower(%q): plan root is %T, want *chplan.Project", tc.query, plan)
			}
			filter, ok := proj.Input.(*chplan.Filter)
			if !ok {
				t.Fatalf("lower(%q): Project.Input is %T, want *chplan.Filter", tc.query, proj.Input)
			}
			join, ok := filter.Input.(*chplan.MixedVectorJoin)
			if !ok {
				t.Fatalf("lower(%q): Filter.Input is %T, want *chplan.MixedVectorJoin", tc.query, filter.Input)
			}
			if join.Card != tc.wantCard {
				t.Errorf("lower(%q): Card = %v, want %v", tc.query, join.Card, tc.wantCard)
			}
			if !slices.Equal(join.Include, tc.wantInclude) {
				t.Errorf("lower(%q): Include = %v, want %v", tc.query, join.Include, tc.wantInclude)
			}
		})
	}
}

// TestLower_ExpHistogram_MixedSetOpOr_VectorVectorCompareOnIgnoring pins
// that on()/ignoring() vector matching (still CardOneToOne) is supported
// for comparisons the same way it is for arithmetic: the output
// Attributes projection is a mapFilter reduction rather than a bare
// forwarded column.
func TestLower_ExpHistogram_MixedSetOpOr_VectorVectorCompareOnIgnoring(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name  string
		query string
	}{
		{"on", mixedOrExpr + ` == on(job) ` + mixedOrExpr},
		{"ignoring", mixedOrExpr + ` != ignoring(job) ` + mixedOrExpr},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
			if err != nil {
				t.Fatalf("LowerAt(%q): unexpected error: %v", tc.query, err)
			}
			proj, ok := plan.(*chplan.Project)
			if !ok {
				t.Fatalf("lower(%q): plan root is %T, want *chplan.Project", tc.query, plan)
			}
			attrsProj := proj.Projections[1]
			if attrsProj.Alias != s.AttributesColumn {
				t.Fatalf("lower(%q): second projection alias = %q, want %q", tc.query, attrsProj.Alias, s.AttributesColumn)
			}
			call, ok := attrsProj.Expr.(*chplan.FuncCall)
			if !ok || call.Fn != chplan.FnMapFilter {
				t.Fatalf("lower(%q): Attributes projection = %#v, want a mapFilter FuncCall (on()/ignoring() reduction)", tc.query, attrsProj.Expr)
			}
		})
	}
}
