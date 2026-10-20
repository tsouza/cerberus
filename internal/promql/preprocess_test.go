package promql_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestPreprocess_SubMillisecondSubqueryStep pins that a subquery whose
// step is under a millisecond is rejected by every lowering entry point,
// as Prometheus fails the query on its zero-millisecond interval, instead
// of panicking, and that a step of one millisecond is lowered.
func TestPreprocess_SubMillisecondSubqueryStep(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	entries := map[string]func(parser.Expr) (chplan.Node, error){
		"Lower": func(e parser.Expr) (chplan.Node, error) {
			return promql.Lower(context.Background(), e, schema.DefaultOTelMetrics())
		},
		"LowerAt": func(e parser.Expr) (chplan.Node, error) {
			return promql.LowerAt(context.Background(), e, schema.DefaultOTelMetrics(), at, at)
		},
		"LowerAtRange": func(e parser.Expr) (chplan.Node, error) {
			return promql.LowerAtRange(context.Background(), e, schema.DefaultOTelMetrics(), at, at.Add(time.Hour), time.Minute)
		},
	}
	rejected := []string{
		`max_over_time(m[1m:0.0005])`,
		`count_over_time(m[1m:0.000999])`,
		`m[1m:0.0001]`,
		`max_over_time(max_over_time(m[1m:1m])[5m:0.0009])`,
		`sum(max_over_time(m[1m:0.0005])) + vector(1)`,
	}
	for name, lower := range entries {
		for _, q := range rejected {
			t.Run(name+"/"+q, func(t *testing.T) {
				t.Parallel()
				expr, err := parser.NewParser(parser.Options{}).ParseExpr(q)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := lower(expr); err == nil || !strings.Contains(err.Error(), "is under a millisecond") {
					t.Fatalf("%s(%q) = %v, want the sub-millisecond subquery step rejection", name, q, err)
				}
			})
		}
		t.Run(name+"/one_millisecond", func(t *testing.T) {
			t.Parallel()
			expr, err := parser.NewParser(parser.Options{}).ParseExpr(`max_over_time(m[1s:0.001])`)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := lower(expr); err != nil {
				t.Fatalf("%s: a one-millisecond subquery step: %v", name, err)
			}
		})
	}
}

// TestPreprocess_QueryContextFoldsToRequest pins that `start()`, `end()`,
// `step()` and `range()` name the request's start, end, step and span
// wherever they sit: inside a
// subquery, whose grid has a step and span of its own, in an isolated
// subquery's runs, and in each run of a range query split into runs.
// Every float the emitted SQL binds is the request's value.
func TestPreprocess_QueryContextFoldsToRequest(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const fiveMinutes = 5 * time.Minute
	end := start.Add(30 * time.Minute)
	// A range spanning more than one lowering run, about 146 years, is
	// split into runs of its steps.
	const splitSteps = 2500
	splitStep := 30 * 24 * time.Hour
	splitStart := time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)
	splitEnd := splitStart.Add(splitSteps * splitStep)
	cases := []struct {
		name       string
		query      string
		start, end time.Time
		step       time.Duration
		want       float64
		// literal is a number the query writes itself, bound beside the
		// folded value.
		literal float64
	}{
		{"subquery_step", `max_over_time(vector(step())[10m:1m])`, start, end, fiveMinutes, fiveMinutes.Seconds(), 0},
		{"subquery_range", `max_over_time(vector(range())[10m:1m])`, start, end, fiveMinutes, end.Sub(start).Seconds(), 0},
		{"subquery_step_scalar_operand", `max_over_time((vector(1) * step())[10m:1m])`, start, end, fiveMinutes, fiveMinutes.Seconds(), 1},
		{"subquery_step_instant", `max_over_time(vector(step())[10m:1m])`, end, end, 0, 0, 0},
		{"nested_subquery_step", `max_over_time(max_over_time(vector(step())[10m:1m])[20m:2m])`, start, end, fiveMinutes, fiveMinutes.Seconds(), 0},
		{"isolated_subquery_step", `max_over_time(vector(step())[5m:1m] @ 1e17)`, start, end, fiveMinutes, fiveMinutes.Seconds(), 0},
		{"scalar_argument_step", `clamp_max(vector(1000), step())`, start, end, fiveMinutes, fiveMinutes.Seconds(), 1000},
		{"split_range_step", `vector(step())`, splitStart, splitEnd, splitStep, splitStep.Seconds(), 0},
		{"split_range_range", `vector(range())`, splitStart, splitEnd, splitStep, splitEnd.Sub(splitStart).Seconds(), 0},
		{"split_range_root_range", `range()`, splitStart, splitEnd, splitStep, splitEnd.Sub(splitStart).Seconds(), 0},
		{"split_range_root_step", `step()`, splitStart, splitEnd, splitStep, splitStep.Seconds(), 0},
		{"root_start", `start()`, start, end, fiveMinutes, float64(start.Unix()), 0},
		{"root_end", `end()`, start, end, fiveMinutes, float64(end.Unix()), 0},
		{"root_start_instant", `start()`, end, end, 0, float64(end.Unix()), 0},
		{"subquery_start", `max_over_time(vector(start())[10m:1m])`, start, end, fiveMinutes, float64(start.Unix()), 0},
		{"subquery_end_instant", `max_over_time(vector(end())[10m:1m])`, end, end, 0, float64(end.Unix()), 0},
		{"nested_subquery_end", `max_over_time(max_over_time(vector(end())[10m:1m])[20m:2m])`, start, end, fiveMinutes, float64(end.Unix()), 0},
		{"isolated_subquery_start", `max_over_time(vector(start())[5m:1m] @ 1e17)`, start, end, fiveMinutes, float64(start.Unix()), 0},
		{"scalar_argument_start", `clamp_max(vector(1e12), start())`, start, end, fiveMinutes, float64(start.Unix()), 1e12},
		{"split_range_start", `vector(start())`, splitStart, splitEnd, splitStep, float64(splitStart.Unix()), 0},
		{"split_range_root_end", `end()`, splitStart, splitEnd, splitStep, float64(splitEnd.Unix()), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := queryContextParser().ParseExpr(tc.query)
			if err != nil {
				t.Fatal(err)
			}
			before := expr.String()
			plan, err := promql.LowerAtRange(context.Background(), expr, schema.DefaultOTelMetrics(), tc.start, tc.end, tc.step)
			if err != nil {
				t.Fatalf("LowerAtRange(%q): %v", tc.query, err)
			}
			if after := expr.String(); after != before {
				t.Fatalf("lowering modified the query: %q became %q", before, after)
			}
			if strings.HasPrefix(tc.name, "split_range") {
				if union, ok := plan.(*chplan.UnionAll); !ok || len(union.Inputs) < 2 {
					t.Fatalf("root %T, want a union of runs", plan)
				}
			}
			_, args, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatal(err)
			}
			folded := 0
			for _, a := range args {
				f, ok := a.(float64)
				switch {
				case !ok:
				case f == tc.want:
					folded++
				case tc.literal == 0 || f != tc.literal:
					t.Fatalf("%s: bound %v, want every folded value %v; args %v", tc.query, f, tc.want, args)
				}
			}
			if folded == 0 {
				t.Fatalf("%s: the folded value %v is not bound; args %v", tc.query, tc.want, args)
			}
		})
	}
}
