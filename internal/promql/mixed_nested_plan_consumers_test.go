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

// A mixed float/histogram plan produced ONE LEVEL DOWN — by a
// payload-preserving wrapper (sort_by_label, limitk, a further set operator,
// a subquery select) over a mixed `or` — reaches the scale,
// unary and vector-vector consumers as an already-lowered plan, where the
// root recognisers of lowerMixedExpHistogramFamily never see it. Reference
// Prometheus scales histograms under `*`, `/` and unary `-`, merges two
// histograms under `+`/`-`, and emits no sample at all for a histogram-vs-
// float `+`/`-`/comparison — so the generic float consumers (which narrow to
// float rows or read the histogram placeholder Value) can never be right for
// it. Every such shape must lower through the same discriminator-aware folds
// its direct root already uses, and its plan must still be a live mixed
// relation (the histogram rows survive) or, for the comparison and `+`/`-`
// shapes, a fold over a MixedVectorJoin rather than a plain VectorJoin. A
// histogram-valued partner — one lowerRoot's histogram-vs-float-vector
// recognisers would otherwise claim, reading the nested plan as a float
// vector — joins through the same fold with its rows stamped histogram.
// The histogram value functions (histogram_count/sum/avg/fraction/
// quantile) answer floats over the plan's histogram partition rather than
// folding it to the empty vector as a float pipeline.
func TestNestedMixedPlanAtScaleUnaryAndVectorConsumersLowersLikeItsRoot(t *testing.T) {
	const direct = `(latency_exp_hist or num_cpus)`
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	wrappers := []string{
		`sort_by_label(` + direct + `, "job")`,
		`limitk(5, ` + direct + `)`,
		`last_over_time(` + direct + `[5m:1m])`,
		`(` + direct + ` and up)`,
	}
	consumers := []struct {
		name string
		wrap func(nested string) string
		// mixed reports whether the answer keeps histogram rows, so the
		// lowered plan must publish the live mixed contract; the other
		// consumers answer floats only after the discriminator fold.
		mixed bool
	}{
		{"scale_right", func(n string) string { return n + ` * 2` }, true},
		{"scale_left", func(n string) string { return `2 * ` + n }, true},
		{"scale_div", func(n string) string { return n + ` / 2` }, true},
		{"scale_computed", func(n string) string { return n + ` * scalar(vector(2))` }, true},
		{"unary_minus", func(n string) string { return `-` + n }, true},
		{"unary_plus", func(n string) string { return `+` + n }, true},
		{"vector_add", func(n string) string { return n + ` + up` }, true},
		{"vector_sub_left", func(n string) string { return `up - ` + n }, true},
		{"vector_mul", func(n string) string { return n + ` * up` }, true},
		{"vector_pow", func(n string) string { return n + ` ^ up` }, true},
		{"vector_add_both_nested", func(n string) string { return n + ` + ` + n }, true},
		{"vector_add_forwarded_histogram", func(n string) string { return n + ` + (other_exp_hist and up)` }, true},
		{"vector_mul_forwarded_histogram", func(n string) string { return `(other_exp_hist and up) * ` + n }, true},
		{"vector_add_histogram", func(n string) string { return n + ` + other_exp_hist` }, true},
		{"vector_sub_histogram_left", func(n string) string { return `other_exp_hist - ` + n }, true},
		{"vector_mul_histogram", func(n string) string { return n + ` * other_exp_hist` }, true},
		{"vector_div_histogram_group_left", func(n string) string { return `other_exp_hist / on(job) group_left() ` + n }, true},
		{"compare_histogram_filter", func(n string) string { return n + ` == other_exp_hist` }, true},
		{"compare_histogram_bool", func(n string) string { return n + ` != bool other_exp_hist` }, false},
		{"compare_bool", func(n string) string { return n + ` > bool up` }, false},
		{"compare_filter", func(n string) string { return n + ` == up` }, true},
		{"compare_both_nested", func(n string) string { return n + ` != ` + n }, true},
		{"histogram_count", func(n string) string { return `histogram_count(` + n + `)` }, false},
		{"histogram_sum_of_scaled", func(n string) string { return `histogram_sum(` + n + ` * 2)` }, false},
		{"histogram_avg", func(n string) string { return `histogram_avg(` + n + `)` }, false},
		{"histogram_fraction", func(n string) string { return `histogram_fraction(0, 10, ` + n + `)` }, false},
		{"histogram_quantile", func(n string) string { return `histogram_quantile(0.5, ` + n + `)` }, false},
	}
	for _, nested := range wrappers {
		for _, c := range consumers {
			for _, step := range []time.Duration{0, time.Minute} {
				query := c.wrap(nested)
				t.Run(fmt.Sprintf("%s/step=%s", query, step), func(t *testing.T) {
					expr, err := p.ParseExpr(query)
					if err != nil {
						t.Fatal(err)
					}
					plan, err := LowerAtRange(context.Background(), expr, s, at, at.Add(step), step)
					if err != nil {
						t.Fatalf("nested mixed plan rejected at a consumer its root already answers: %v", err)
					}
					kind := chplan.LiveSampleKind(plan)
					if c.mixed && kind != chplan.SampleKindMixed {
						t.Fatalf("consumer dropped the histogram rows: live sample kind %s, want %s", kind, chplan.SampleKindMixed)
					}
					if !c.mixed && kind != chplan.SampleKindFloat {
						t.Fatalf("float-answering consumer: live sample kind %s, want %s", kind, chplan.SampleKindFloat)
					}
					if !c.mixed {
						if emptied := countConstantFalseFilters(plan); emptied != 0 {
							t.Fatalf("consumer folded the nested mixed plan to the empty vector %d time(s) instead of reading its histogram rows", emptied)
						}
						return
					}
					if narrowed := countMixedFloatNarrowing(plan); narrowed != 0 {
						t.Fatalf("plan narrows the mixed relation to float rows %d time(s); the fold must keep both row shapes", narrowed)
					}
					if joins := countPlainVectorJoins(plan); joins != 0 {
						t.Fatalf("plan joins the mixed relation through %d plain VectorJoin(s), which reads the histogram placeholder Value", joins)
					}
				})
			}
		}
	}
}

func countMixedFloatNarrowing(plan chplan.Node) int {
	n := 0
	chplan.Walk(plan, func(node chplan.Node) bool {
		if chplan.IsMixedFloatNarrowing(node) {
			n++
		}
		return true
	})
	return n
}

func countConstantFalseFilters(plan chplan.Node) int {
	n := 0
	chplan.Walk(plan, func(node chplan.Node) bool {
		if filter, ok := node.(*chplan.Filter); ok {
			if predicate, ok := filter.Predicate.(*chplan.LitBool); ok && !predicate.V {
				n++
			}
		}
		return true
	})
	return n
}

func countPlainVectorJoins(plan chplan.Node) int {
	n := 0
	chplan.Walk(plan, func(node chplan.Node) bool {
		if _, ok := node.(*chplan.VectorJoin); ok {
			n++
		}
		return true
	})
	return n
}
