package promql

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// narrowedNestedMixed nests a mixed `or` under a label-sort wrapper, so a
// float-narrowing consumer over it reaches the generic lowering rather than
// a root recognizer composed directly over the `or`.
const narrowedNestedMixed = `sort_by_label(latency_exp_hist or num_cpus, "job")`

// TestLower_NarrowedNestedMixed_AsFloatOperand pins cerberus issue #3853: a
// float-narrowing consumer over a nested mixed relation — a comparison, a
// sort, or a degenerate-bounds clamp, each of which keeps the mixed payload
// columns beside its float-row proof — lowers as the float arm of a mixed
// `or` (at the root and under the aggregate, label and subquery recognizers)
// and as the plain operand of a mixed vector join, in instant and range
// mode. Every arm of every mixed `or` in the plan has a float or a histogram
// physical schema: none carries the mixed payload of a narrowed operand.
func TestLower_NarrowedNestedMixed_AsFloatOperand(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := narrowedNestedMixed

	queries := []string{
		// The three shapes cerberus issue #3853 lists.
		`latency_exp_hist or sort(sort_by_label(latency_exp_hist or num_cpus, "job"))`,
		`(` + m + ` > 1) or latency_exp_hist`,
		`clamp(` + m + `, 2, 1) or latency_exp_hist`,
		// The remaining row-filtering float narrowings.
		`sort_desc(` + m + `) or latency_exp_hist`,
		`(1 < ` + m + `) or latency_exp_hist`,
		`(sort(` + m + `) > 1) or latency_exp_hist`,
		`(sort_by_label(` + m + `, "a") > 1) or latency_exp_hist`,
		`(` + m + ` > 1) or on(job) latency_exp_hist`,
		// The narrowed arm under the recognizers that reuse the mixed `or`
		// operand lowering.
		`sum((` + m + ` > 1) or latency_exp_hist)`,
		`avg(sort(` + m + `) or latency_exp_hist)`,
		`count((` + m + ` > 1) or latency_exp_hist)`,
		`histogram_count((` + m + ` > 1) or latency_exp_hist)`,
		`sort_by_label((` + m + ` > 1) or latency_exp_hist, "job")`,
		`label_replace(sort(` + m + `) or latency_exp_hist, "a", "$1", "b", "(.*)")`,
		`max_over_time((sort(` + m + `) or latency_exp_hist)[5m:1m])`,
		`last_over_time((sort(` + m + `) or latency_exp_hist)[5m:1m])`,
		// The narrowed relation as the plain operand of a mixed vector join.
		`(latency_exp_hist or num_cpus) + sort(` + m + `)`,
		`(latency_exp_hist or num_cpus) * (` + m + ` > 1)`,
		`sort(` + m + `) + (latency_exp_hist or num_cpus)`,
	}

	for _, query := range queries {
		for _, step := range []time.Duration{0, time.Minute} {
			t.Run(query+"/step="+step.String(), func(t *testing.T) {
				t.Parallel()
				expr, err := p.ParseExpr(query)
				if err != nil {
					t.Fatalf("ParseExpr(%q): %v", query, err)
				}
				plan, err := LowerAtRange(context.Background(), expr, s, at, at.Add(step), step)
				if err != nil {
					t.Fatalf("LowerAtRange(%q): %v", query, err)
				}
				chplan.WalkDeep(plan, func(n chplan.Node) bool {
					union, ok := n.(*chplan.VectorSetOp)
					if !ok || !union.Mixed {
						return true
					}
					for _, arm := range []chplan.Node{union.Left, union.Right} {
						if kind := arm.RowType().SampleKind(); kind != chplan.SampleKindFloat && kind != chplan.SampleKindHistogram {
							t.Errorf("%q: mixed `or` arm has %s physical schema, want float or histogram", query, kind)
						}
					}
					return true
				})
			})
		}
	}
}

// TestProvenFloatRowsSchema pins the projection the float-operand sites
// apply: only a physically mixed relation with a float-row proof loses its
// payload columns; a live mixed relation and an ordinary float relation are
// returned unchanged.
func TestProvenFloatRowsSchema(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelMetrics()
	mixedColumns := append(metricRoles(s), chplan.HistogramPayloadColumns()...)
	mixedColumns = append(mixedColumns, chplan.Column{Name: mixedDiscriminatorColumn, Role: chplan.RoleDiscriminator})
	mixed := sampleForwardTestInput(mixedColumns...)
	float := sampleForwardTestInput(metricRoles(s)...)

	for _, tc := range []struct {
		name      string
		input     chplan.Node
		unchanged bool
	}{
		{"live_mixed", mixed, true},
		{"float", float, true},
		{"discriminator_narrowed", mixedRowsFloatOnly(mixed), false},
		{"ordered_narrowed", &chplan.OrderBy{Input: mixedRowsFloatOnly(mixed)}, false},
		{"empty_filter", &chplan.Filter{Input: mixed, Predicate: &chplan.LitBool{V: false}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := provenFloatRowsSchema(tc.input, s)
			if tc.unchanged {
				if got != tc.input {
					t.Fatalf("provenFloatRowsSchema rewrapped %T", tc.input)
				}
				return
			}
			project, ok := got.(*chplan.Project)
			if !ok || project.Input != tc.input {
				t.Fatalf("provenFloatRowsSchema = %T, want a Project over the input", got)
			}
			if row := got.RowType(); !row.Equal(float.RowType()) {
				t.Fatalf("projected schema = %#v, want the canonical float schema %#v", row, float.RowType())
			}
		})
	}
}
