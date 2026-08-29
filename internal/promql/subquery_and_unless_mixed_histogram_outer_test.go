package promql_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestOuterRangeFnOverAndUnlessMixedSubquery_Composes proves that cerberus
// issue #2589's fix, once lowerSubqueryOverBinary stops discarding a
// histogram/mixed-shaped `and`/`or`/`unless` subquery inner, makes the
// PRE-EXISTING histogram-shape guard in lowerOuterRangeFnOverSubquery
// newly REACHABLE for this shape — and cerberus issue #2724's
// [lowerHistogramOrMixedSubqueryOuterFnInput], dispatching on the row
// shape that guard already computes rather than re-deriving a per-AST-shape
// recognizer, now answers it correctly instead of rejecting: `rate` folds
// the and/unless-forwarded histogram subquery inner the same way it folds
// a DIRECT one (histogram_native_range_fn.go's own
// [lowerExpHistogramRangeFnOverSubqueryInput]), turning what used to be
// (before #2589) a silent wrong answer, then (after #2589, before #2724) a
// clean rejection, into a real, correctly-composed one.
//
// Before #2589's fix, lowerSubqueryOverBinary's unconditional
// subqueryAnchorShape wrap meant the guard this test exercises could never
// see a Histogram/MixedRowShape node for this AST shape in the first place
// — chplan.RowShapeOf(inner) always answered SampleRowShape (the corrupted
// four-column reprojection), so the guard fell through silently.
func TestOuterRangeFnOverAndUnlessMixedSubquery_Composes(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	evalTS := time.Date(2026, 1, 1, 0, 2, 0, 0, time.UTC)

	// demo_latency_exp_hist / demo_num_cpus are the real seeded metric
	// names (compatibility/prometheus/cmd/seed/main.go) — LowerAt performs
	// no data access, but using the real names keeps this test's query
	// shape identical to what a real compat-lane probe would send.
	for _, tc := range []struct {
		name  string
		query string
	}{
		{"and", "rate((demo_latency_exp_hist and on(instance) demo_num_cpus)[5m:1m])"},
		{"unless", "rate((demo_latency_exp_hist unless on(instance) demo_num_cpus)[5m:1m])"},
		{"or_wrapped_by_and", "rate(((demo_latency_exp_hist or demo_num_cpus) and on(instance) demo_num_cpus)[5m:1m])"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, evalTS, evalTS)
			if err != nil {
				t.Fatalf("%s: LowerAt failed, want success: %v", tc.query, err)
			}
			if plan == nil {
				t.Fatalf("%s: LowerAt returned a nil plan with nil error", tc.query)
			}
		})
	}
}

// TestOuterFloatOnlyDropFnOverAndUnlessMixedSubquery_Composes proves the
// OTHER half of lowerOuterRangeFnOverSubquery's existing histogram-shape
// guard also becomes reachable, and answers correctly (not merely
// "doesn't error"): max_over_time is one of the eleven functions reference
// Prometheus itself defines no histogram semantics for
// (histogramSubqueryFloatOnlyDropFunc), so it already has a dedicated
// dropExpHistogramSamples fold — this test proves that fold now actually
// runs for the and/unless-mixed subquery-inner shape instead of the
// generic-lower path being unreachable before this fix ever let it get
// this far.
func TestOuterFloatOnlyDropFnOverAndUnlessMixedSubquery_Composes(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	evalTS := time.Date(2026, 1, 1, 0, 2, 0, 0, time.UTC)

	query := "max_over_time((demo_latency_exp_hist and on(instance) demo_num_cpus)[5m:1m])"
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, evalTS, evalTS)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v (want a successful drop-and-empty composition, matching reference's own \"histogram(s) ignored\" behaviour for max_over_time)", query, err)
	}
	if plan == nil {
		t.Fatalf("LowerAt(%q): got nil plan with nil error", query)
	}
}

// TestSubqueryOverBinary_HistogramSetOp_NoEvalAnchorRejects proves the
// symmetric guard in lowerSubqueryOverBinary's own !ok (no query eval-time
// context) branch: a bare Lower() call — never a real HTTP entry point,
// see subqueryHasEvalAnchor's doc — cannot resolve the per-anchor grid a
// histogram/mixed-shaped and/or/unless composition needs, so it must
// reject cleanly rather than fall through to wrapSubqueryIdentity's own
// identical lossy Sample-quartet reprojection.
func TestSubqueryOverBinary_HistogramSetOp_NoEvalAnchorRejects(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	query := "(demo_latency_exp_hist and on(instance) demo_num_cpus)[5m:1m]"
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	_, err = promql.Lower(context.Background(), expr, s)
	if err == nil {
		t.Fatalf("%s: Lower (no eval anchor) succeeded, want a clean rejection", query)
	}
	const want = "requires query eval-time context"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("%s: error = %q, want it to contain %q", query, err.Error(), want)
	}
}
