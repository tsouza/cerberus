package promql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// newExperimentalParser builds a parser that admits the experimental
// histogram_quantiles function (gated behind EnableExperimentalFunctions
// upstream).
func newExperimentalParser() parser.Parser {
	return parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
}

// TestLower_HistogramQuantiles_FanOut asserts the variadic
// histogram_quantiles(<vector>, "<label>", phi...) lowers into a UnionAll
// with one arm per phi, each arm injecting the quantile label into the
// Attributes map via mapConcat. This is the structural counterpart to the
// chDB-pinned parity fixtures under test/spec/promql.
func TestLower_HistogramQuantiles_FanOut(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := newExperimentalParser()

	expr, err := p.ParseExpr(`histogram_quantiles(http_server_request_duration, "q", 0.5, 0.9, 0.99)`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := promql.Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}

	union, ok := plan.(*chplan.UnionAll)
	if !ok {
		t.Fatalf("expected top-level *chplan.UnionAll, got %T", plan)
	}
	if len(union.Inputs) != 3 {
		t.Fatalf("expected 3 union arms (one per phi), got %d", len(union.Inputs))
	}

	// Each arm must be a Project that overrides Attributes with a
	// mapConcat injecting the q label. The label VALUE is the OpenMetrics
	// rendering of the phi: 0.5 -> "0.5", 0.9 -> "0.9", 0.99 -> "0.99".
	wantPhiStr := []string{"0.5", "0.9", "0.99"}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	for _, want := range wantPhiStr {
		if !strings.Contains(sql, want) {
			t.Errorf("emitted SQL missing phi-label literal %q\n%s", want, sql)
		}
	}
	if !strings.Contains(sql, "mapConcat") {
		t.Errorf("emitted SQL missing mapConcat label injection\n%s", sql)
	}
	if !strings.Contains(sql, "UNION ALL") {
		t.Errorf("emitted SQL missing UNION ALL across phi arms\n%s", sql)
	}
}

// TestLower_HistogramQuantiles_SinglePhi asserts that a single-phi call
// collapses to the lone (label-injecting) arm rather than a degenerate
// one-arm UnionAll — UnionAll rejects single-arm unions at emit time.
func TestLower_HistogramQuantiles_SinglePhi(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := newExperimentalParser()

	expr, err := p.ParseExpr(`histogram_quantiles(http_server_request_duration, "q", 0.5)`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := promql.Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	if _, ok := plan.(*chplan.UnionAll); ok {
		t.Fatalf("single-phi histogram_quantiles must not produce a UnionAll, got %T", plan)
	}
	proj, ok := plan.(*chplan.Project)
	if !ok {
		t.Fatalf("expected top-level *chplan.Project for single-phi, got %T", plan)
	}
	// The outermost Project overrides Attributes with the mapConcat
	// label-injection expression.
	var foundInjection bool
	for _, pr := range proj.Projections {
		if pr.Alias != s.AttributesColumn {
			continue
		}
		if fc, ok := pr.Expr.(*chplan.FuncCall); ok && fc.Name == "mapConcat" {
			foundInjection = true
		}
	}
	if !foundInjection {
		t.Errorf("single-phi arm did not inject the quantile label via mapConcat")
	}
}

// TestLower_HistogramQuantiles_ComputedPhi pins the computed-phi
// acceptance. Reference Prometheus type-checks the phi arguments as
// scalar-valued and reads phi[0].F per step, so
// `histogram_quantiles(v, "q", scalar(x))` is an ordinary query there.
// Cerberus renders the quantile label's OpenMetrics float formatting at
// query time instead of folding it at lowering time, so the lowered
// tree still injects the label via mapConcat — just with a computed
// value rather than a chplan.LitString.
func TestLower_HistogramQuantiles_ComputedPhi(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := newExperimentalParser()

	expr, err := p.ParseExpr(`histogram_quantiles(http_server_request_duration, "q", scalar(http_server_request_duration))`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := promql.Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	// The runtime formatter is the tell: a folded literal phi would
	// never reach for arrayMap/multiIf to build the label value.
	for _, want := range []string{"mapConcat", "arrayMap", "multiIf"} {
		if !strings.Contains(sql, want) {
			t.Errorf("emitted SQL missing %q for the computed-phi label; got:\n%s", want, sql)
		}
	}
}
