package promql

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

func TestComputedClampNarrowsMixedBeforeBoundFilter(t *testing.T) {
	t.Parallel()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, sortFn := range []string{"sort_by_label", "sort_by_label_desc"} {
		for _, union := range []string{"latency_exp_hist or num_cpus", "num_cpus or latency_exp_hist"} {
			for _, bounds := range []string{"scalar(vector(0)), scalar(vector(10))", "scalar(vector(2)), scalar(vector(1))"} {
				query := fmt.Sprintf(`clamp(%s(%s, "job"), %s)`, sortFn, union, bounds)
				t.Run(query, func(t *testing.T) {
					expr, err := p.ParseExpr(query)
					if err != nil {
						t.Fatal(err)
					}
					plan, err := LowerAt(context.Background(), expr, s, at, at)
					if err != nil {
						t.Fatal(err)
					}
					bound := computedClampBoundFilter(t, plan)
					if !chplan.IsMixedFloatNarrowing(bound.Input) {
						t.Fatalf("bound Filter input %T must first narrow mixed rows to floats", bound.Input)
					}
					narrow := bound.Input.(*chplan.Filter)
					if chplan.RowShapeOf(narrow.Input) != chplan.MixedRowShape {
						t.Fatalf("narrowing input shape = %v, want mixed", chplan.RowShapeOf(narrow.Input))
					}
					if chplan.RowShapeOf(plan) != chplan.SampleRowShape {
						t.Fatalf("clamp result shape = %v, want sample", chplan.RowShapeOf(plan))
					}
				})
			}
		}
	}
}

func TestComputedClampRangeNarrowsBeforeBoundFilter(t *testing.T) {
	t.Parallel()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(`clamp(sort_by_label_desc(latency_exp_hist or num_cpus, "job"), scalar(vector(0)), scalar(vector(10)))`)
	if err != nil {
		t.Fatal(err)
	}
	end := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const queryStep = 15 * time.Second
	plan, err := LowerAtRange(context.Background(), expr, schema.DefaultOTelMetrics(), end.Add(-time.Minute), end, queryStep)
	if err != nil {
		t.Fatal(err)
	}
	bound := computedClampBoundFilter(t, plan)
	if !chplan.IsMixedFloatNarrowing(bound.Input) {
		t.Fatalf("range bound Filter input %T must first narrow mixed rows to floats", bound.Input)
	}
}

func TestComputedClampKeepsOrdinaryFloatInput(t *testing.T) {
	t.Parallel()
	p := parser.NewParser(parser.Options{})
	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, bounds := range []string{"scalar(vector(0)), scalar(vector(10))", "scalar(vector(2)), scalar(vector(1))"} {
		t.Run(bounds, func(t *testing.T) {
			expr, err := p.ParseExpr(`clamp(up, ` + bounds + `)`)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := LowerAt(context.Background(), expr, s, at, at)
			if err != nil {
				t.Fatal(err)
			}
			bound := computedClampBoundFilter(t, plan)
			ordinaryExpr, err := p.ParseExpr("up")
			if err != nil {
				t.Fatal(err)
			}
			ordinary, err := LowerAt(context.Background(), ordinaryExpr, s, at, at)
			if err != nil {
				t.Fatal(err)
			}
			if !bound.Input.Equal(ordinary) {
				t.Fatalf("ordinary float input was changed before bound Filter: %T", bound.Input)
			}
		})
	}
}

func computedClampBoundFilter(t *testing.T, plan chplan.Node) *chplan.Filter {
	t.Helper()
	var found *chplan.Filter
	chplan.Walk(plan, func(node chplan.Node) bool {
		filter, ok := node.(*chplan.Filter)
		if !ok {
			return true
		}
		predicate, ok := filter.Predicate.(*chplan.FuncCall)
		if !ok || predicate.Fn != chplan.FnNot || len(predicate.Args) != 1 {
			return true
		}
		comparison, ok := predicate.Args[0].(*chplan.Binary)
		if !ok || comparison.Op != chplan.OpLt {
			return true
		}
		if found != nil {
			t.Fatal("multiple computed clamp bound filters")
		}
		found = filter
		return true
	})
	if found == nil {
		t.Fatal("computed clamp bound Filter not found")
	}
	return found
}
