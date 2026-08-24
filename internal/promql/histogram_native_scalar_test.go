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

// TestLower_ExpHistogram_ScalarAnswersNaN pins cerberus issue #2515:
// `scalar(v)` over a histogram-valued v used to hard-reject via
// expHistogramSelectorRouting's catch-all — lowerScalarArg's "scalar" case
// lowered v.Args[0] through the generic lower() dispatch with no
// lowerExpHistogramValuedShape check.
//
// Reference Prometheus's funcScalar (promql/functions.go) walks the
// argument vector's samples and skips every one whose H field is set —
// only a FLOAT sample can be "the" single sample scalar() reduces to.
// A bare exp-histogram selector's samples are ALL histogram-shaped
// (cerberus's schema model never mixes the two under one metric name), so
// funcScalar sees zero float samples and answers NaN, never an error —
// exactly the "zero or many samples → NaN" branch scalarValuePlan/
// scalarStepPlan already implement. The fix recognises the histogram shape
// via lowerExpHistogramValuedShape and routes it through
// dropExpHistogramSamples (the same "answer the canonical float shape with
// zero selected rows" helper the unary math functions (#2221), clamp
// (#2345), sort (#2456) and the date-component functions (#2498) reuse),
// which drives scalarValuePlan/scalarStepPlan's own NaN branch — no
// separate NaN literal needed here.
func TestLower_ExpHistogram_ScalarAnswersNaN(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

	queries := []string{
		`scalar(demo_latency_exp_hist)`,
		`scalar(demo_latency_exp_hist{service="api"})`,

		// The consumer sees histogram-valued results, not only selectors.
		`scalar(sum(demo_latency_exp_hist))`,
		`scalar(avg(demo_latency_exp_hist))`,
		`scalar(rate(demo_latency_exp_hist[5m]))`,
	}

	for _, query := range queries {
		query := query
		t.Run(query, func(t *testing.T) {
			t.Parallel()

			expr, err := p.ParseExpr(query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", query, err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
			if err != nil {
				t.Fatalf("LowerAt(%q): %v", query, err)
			}
			if shape := chplan.RowShapeOf(plan); shape != chplan.SampleRowShape {
				t.Fatalf("query=%q: plan publishes %s row shape, want sample", query, shape)
			}
			sql, _, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("chsql.Emit(%q): %v", query, err)
			}
			// The count()==1 ? value : NaN reduction's NaN branch must
			// still be present in the emitted SQL — the histogram-shaped
			// input is filtered to zero rows, so this branch is the ONLY
			// one that can ever fire, but it has to be there for the
			// "answer NaN, not an error" contract to hold at execution
			// time.
			const nanLiteral = "0.0/0"
			if !strings.Contains(sql, nanLiteral) {
				t.Fatalf("query=%q: emitted SQL does not contain the NaN literal %q; got:\n%s", query, nanLiteral, sql)
			}
		})
	}
}

// TestLower_ExpHistogram_ScalarRangeAnswersNaN covers range-query mode: a
// top-level `scalar(v)` over a bare exp-histogram selector binds
// scalar()'s NaN answer per evaluation step (lowerScalarArg's
// scalarsBindPerStep() branch, scalarStepPlan) rather than once for the
// whole statement — same NaN contract as the instant-mode
// scalarValuePlan branch, different chplan shape underneath.
func TestLower_ExpHistogram_ScalarRangeAnswersNaN(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(`scalar(demo_latency_exp_hist)`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	plan, err := promql.LowerAtRange(context.Background(), expr, s, start, start.Add(time.Minute), 15*time.Second)
	if err != nil {
		t.Fatalf("LowerAtRange: %v", err)
	}
	if shape := chplan.RowShapeOf(plan); shape != chplan.SampleRowShape {
		t.Fatalf("plan publishes %s row shape, want sample", shape)
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("chsql.Emit: %v", err)
	}
	const nanLiteral = "0.0/0"
	if !strings.Contains(sql, nanLiteral) {
		t.Fatalf("emitted SQL does not contain the NaN literal %q; got:\n%s", nanLiteral, sql)
	}
}

// TestLower_ExpHistogram_ScalarEmbeddedAnswersNaN covers scalar()
// EMBEDDED as a computed argument to another function — the
// lowerScalarArg "scalar" case is reached both as the whole top-level
// query (lowerScalarTopLevel) and nested inside a scalar-typed argument
// position (clamp's bound, predict_linear's horizon, …); both call
// sites share the same lowerScalarVectorArg histogram check.
func TestLower_ExpHistogram_ScalarEmbeddedAnswersNaN(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	at := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

	const query = `clamp_max(http_requests_total, scalar(demo_latency_exp_hist))`
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, at, at)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("chsql.Emit(%q): %v", query, err)
	}
	const nanLiteral = "0.0/0"
	if !strings.Contains(sql, nanLiteral) {
		t.Fatalf("query=%q: emitted SQL does not contain the NaN literal %q; got:\n%s", query, nanLiteral, sql)
	}
}
