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

// TestLower_ExpHistogram_Timestamp pins cerberus issue #2474:
// `timestamp(v)` reads only the selected sample's own timestamp, never its
// value, so it is defined for a histogram-valued argument exactly as it is
// for a float-valued one — unlike the float-only date-component functions
// (year/month/hour/…) and unlike sort/clamp/absent's "drop the histogram
// samples" posture. Every representative shape below must lower
// successfully to the canonical float [chplan.SampleRowShape], both in
// instant and range mode, and the produced SQL must actually compile
// through the chsql emitter.
func TestLower_ExpHistogram_Timestamp(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	start := at.Add(-time.Hour)

	cases := []struct {
		name  string
		query string
	}{
		{
			name:  "issue representative query: bare exp-histogram selector",
			query: `timestamp(demo_latency_exp_hist)`,
		},
		{
			name:  "bare selector with label matcher",
			query: `timestamp(demo_latency_exp_hist{service="api"})`,
		},
		{
			name:  "parenthesised bare selector",
			query: `timestamp((demo_latency_exp_hist))`,
		},
		{
			name:  "sum() wrapping a bare exp-histogram selector",
			query: `timestamp(sum(demo_latency_exp_hist))`,
		},
		{
			name:  "avg() wrapping a bare exp-histogram selector",
			query: `timestamp(avg(demo_latency_exp_hist))`,
		},
		{
			name:  "rate() over an exp-histogram range selector",
			query: `timestamp(rate(demo_latency_exp_hist[5m]))`,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name+"/instant", func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
			if err != nil {
				t.Fatalf("LowerAt(%q): unexpected error: %v", tc.query, err)
			}
			if shape := chplan.RowShapeOf(plan); shape != chplan.SampleRowShape {
				t.Fatalf("lower(%q): plan root publishes %s, want %s", tc.query, shape, chplan.SampleRowShape)
			}
			if _, _, err := chsql.Emit(context.Background(), plan); err != nil {
				t.Fatalf("chsql.Emit(%q): %v", tc.query, err)
			}
		})
		t.Run(tc.name+"/range", func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			plan, err := promql.LowerAtRange(context.Background(), expr, s, start, at, 30*time.Second)
			if err != nil {
				t.Fatalf("LowerAtRange(%q): unexpected error: %v", tc.query, err)
			}
			if shape := chplan.RowShapeOf(plan); shape != chplan.SampleRowShape {
				t.Fatalf("lower(%q) range: plan root publishes %s, want %s", tc.query, shape, chplan.SampleRowShape)
			}
			if _, _, err := chsql.Emit(context.Background(), plan); err != nil {
				t.Fatalf("chsql.Emit(%q) range: %v", tc.query, err)
			}
		})
	}
}

// TestLower_ExpHistogram_Timestamp_AtPin covers the range-query, `@`-pinned
// (gridBroadcast) shape for a bare exp-histogram selector: the pinned
// window is resolved once and its selected sample's timestamp is fanned
// across the step grid, at each step's own anchor position.
func TestLower_ExpHistogram_Timestamp_AtPin(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	start := at.Add(-time.Hour)

	query := `timestamp(demo_latency_exp_hist @ 1735689600)`
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAtRange(context.Background(), expr, s, start, at, 30*time.Second)
	if err != nil {
		t.Fatalf("LowerAtRange(%q): unexpected error: %v", query, err)
	}
	if shape := chplan.RowShapeOf(plan); shape != chplan.SampleRowShape {
		t.Fatalf("lower(%q): plan root publishes %s, want %s", query, shape, chplan.SampleRowShape)
	}
	if _, _, err := chsql.Emit(context.Background(), plan); err != nil {
		t.Fatalf("chsql.Emit(%q): %v", query, err)
	}
}

// TestLower_ExpHistogram_Timestamp_OtherDateFnsStillRejectHistograms
// guards the fix's scope: `timestamp()` is the ONLY date-component
// function whose value is independent of the sample's payload (reference
// reads only Point.T). Every sibling (year/month/day_of_month/…) computes
// its value FROM the sample's Value column via [valueAsDateTime], which a
// native histogram sample never populates meaningfully — those must keep
// hitting expHistogramSelectorRouting's rejection over a bare exp-histogram
// selector, exactly as before this fix.
func TestLower_ExpHistogram_Timestamp_OtherDateFnsStillRejectHistograms(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	for _, query := range []string{
		`year(demo_latency_exp_hist)`,
		`month(demo_latency_exp_hist)`,
		`hour(demo_latency_exp_hist)`,
		`minute(demo_latency_exp_hist)`,
		`day_of_week(demo_latency_exp_hist)`,
	} {
		query := query
		t.Run(query, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", query, err)
			}
			_, err = promql.LowerAt(context.Background(), expr, s, at, at)
			if err == nil {
				t.Fatalf("LowerAt(%q): expected the exponential-histogram routing rejection, got nil error", query)
			}
			if !strings.Contains(err.Error(), "is an exponential histogram metric") {
				t.Fatalf("LowerAt(%q): unexpected error: %v", query, err)
			}
		})
	}
}
