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

// TestLower_HistogramQuantiles_SharedKernel asserts constant levels share one
// histogram preparation and expand only after the quantiles have been
// calculated.
func TestLower_HistogramQuantiles_SharedKernel(t *testing.T) {
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

	shared := findSharedHistogramQuantiles(plan)
	if len(shared) != 1 {
		t.Fatalf("shared histogram kernels = %d, want 1 (plan %T)", len(shared), plan)
	}
	if got := len(shared[0].Levels); got != 3 {
		t.Fatalf("shared levels = %d, want 3", got)
	}
	wantLevels := []chplan.HistogramQuantileLevel{
		{Phi: 0.5, Label: "0.5"},
		{Phi: 0.9, Label: "0.9"},
		{Phi: 0.99, Label: "0.99"},
	}
	for i, want := range wantLevels {
		if got := shared[0].Levels[i]; got != want {
			t.Fatalf("shared level[%d] = %#v, want %#v", i, got, want)
		}
	}

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
	for _, want := range []string{"mapConcat", "arrayZip"} {
		if !strings.Contains(sql, want) {
			t.Errorf("emitted SQL missing %q from shared expansion\n%s", want, sql)
		}
	}
	if strings.Contains(sql, "UNION ALL") {
		t.Errorf("shared constant quantiles unexpectedly re-scan through UNION ALL\n%s", sql)
	}
}

func findSharedHistogramQuantiles(n chplan.Node) []*chplan.HistogramQuantiles {
	var found []*chplan.HistogramQuantiles
	var walk func(chplan.Node)
	walk = func(node chplan.Node) {
		if shared, ok := node.(*chplan.HistogramQuantiles); ok {
			found = append(found, shared)
		}
		for _, child := range node.Children() {
			walk(child)
		}
	}
	walk(n)
	return found
}

func countSharedHistogramKernels(n chplan.Node) int {
	count := 0
	chplan.Walk(n, func(node chplan.Node) bool {
		switch node.(type) {
		case *chplan.HistogramQuantiles, *chplan.HistogramQuantilesNative:
			count++
		}
		return true
	})
	return count
}

func TestLower_HistogramQuantiles_SharesAcrossInputShapes(t *testing.T) {
	t.Parallel()

	queries := map[string]string{
		"classic bare":       `histogram_quantiles(http_server_request_duration, "q", 0.5, 0.95, 0.99)`,
		"parenthesized":      `histogram_quantiles((http_server_request_duration), "q", 0.5, 0.95, 0.99)`,
		"matcher order":      `histogram_quantiles(http_server_request_duration{service="api",env="prod"}, "q", 0.5, 0.95, 0.99)`,
		"sum by rate":        `histogram_quantiles(sum by (le, service) (rate(http_server_request_duration_seconds_bucket{env="prod"}[5m])), "q", 0.5, 0.95, 0.99)`,
		"sum without rate":   `histogram_quantiles(sum without (instance) (rate(http_server_request_duration_seconds_bucket{env="prod"}[5m])), "q", 0.5, 0.95, 0.99)`,
		"native exponential": `histogram_quantiles(http_server_request_duration_exp_hist, "q", 0.5, 0.95, 0.99)`,
	}
	for name, query := range queries {
		t.Run(name, func(t *testing.T) {
			s := schema.DefaultOTelMetrics()
			expr, err := newExperimentalParser().ParseExpr(query)
			if err != nil {
				t.Fatalf("ParseExpr: %v", err)
			}
			plan, err := promql.Lower(context.Background(), expr, s)
			if err != nil {
				t.Fatalf("Lower: %v", err)
			}
			if got := countSharedHistogramKernels(plan); got != 1 {
				t.Fatalf("shared histogram kernels = %d, want 1", got)
			}
			sql, _, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			if strings.Contains(sql, "UNION ALL") {
				t.Fatalf("constant quantiles did not share their input:\n%s", sql)
			}
		})
	}
}

func TestLower_HistogramQuantiles_PreservesDuplicateLevels(t *testing.T) {
	t.Parallel()
	expr, err := newExperimentalParser().ParseExpr(`histogram_quantiles(http_server_request_duration, "q", 0.5, 0.5, 0.99)`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := promql.Lower(context.Background(), expr, schema.DefaultOTelMetrics())
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	shared := findSharedHistogramQuantiles(plan)
	if len(shared) != 1 || len(shared[0].Levels) != 3 {
		t.Fatalf("duplicate levels were collapsed: %#v", shared)
	}
	if shared[0].Levels[0] != shared[0].Levels[1] {
		t.Fatalf("duplicate level order changed: %#v", shared[0].Levels)
	}
}

func TestLower_HistogramQuantiles_SharesWhenFirstLevelIsOutOfDomain(t *testing.T) {
	t.Parallel()
	expr, err := newExperimentalParser().ParseExpr(`histogram_quantiles(http_server_request_duration, "q", -1, 0.5, 2)`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := promql.Lower(context.Background(), expr, schema.DefaultOTelMetrics())
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	shared := findSharedHistogramQuantiles(plan)
	if len(shared) != 1 {
		t.Fatalf("shared histogram kernels = %d, want 1", len(shared))
	}
	if got := len(shared[0].Levels); got != 3 {
		t.Fatalf("shared levels = %d, want 3", got)
	}
}

func TestLower_HistogramQuantiles_SharesWithDomainEndpointKernel(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		query string
	}{
		{"zero", `histogram_quantiles(http_server_request_duration, "q", -1, 0)`},
		{"one", `histogram_quantiles(http_server_request_duration, "q", 2, 1)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expr, err := newExperimentalParser().ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr: %v", err)
			}
			plan, err := promql.Lower(context.Background(), expr, schema.DefaultOTelMetrics())
			if err != nil {
				t.Fatalf("Lower: %v", err)
			}
			if got := countSharedHistogramKernels(plan); got != 1 {
				t.Fatalf("shared histogram kernels = %d, want 1", got)
			}
		})
	}
}

func TestLower_HistogramQuantiles_FloatVectorFallsBack(t *testing.T) {
	t.Parallel()
	expr, err := newExperimentalParser().ParseExpr(
		`histogram_quantiles(vector(1), "q", 0.5, 0.9)`,
	)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := promql.Lower(context.Background(), expr, schema.DefaultOTelMetrics())
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	if _, ok := plan.(*chplan.UnionAll); !ok {
		t.Fatalf("float-vector fallback = %T, want *chplan.UnionAll", plan)
	}
}

func TestLower_HistogramQuantiles_AllOutOfDomainFallsBackWithoutRejecting(t *testing.T) {
	t.Parallel()
	expr, err := newExperimentalParser().ParseExpr(`histogram_quantiles(http_server_request_duration, "q", -1, 2)`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := promql.Lower(context.Background(), expr, schema.DefaultOTelMetrics())
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	if got := countSharedHistogramKernels(plan); got != 0 {
		t.Fatalf("shared histogram kernels = %d, want fallback", got)
	}
	if _, ok := plan.(*chplan.UnionAll); !ok {
		t.Fatalf("all-out-of-domain fallback = %T, want *chplan.UnionAll", plan)
	}
}

func TestLower_HistogramQuantiles_DynamicLevelDoesNotShare(t *testing.T) {
	t.Parallel()
	expr, err := newExperimentalParser().ParseExpr(`histogram_quantiles(http_server_request_duration, "q", 0.5, scalar(http_server_request_duration), 0.99)`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := promql.Lower(context.Background(), expr, schema.DefaultOTelMetrics())
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	if got := countSharedHistogramKernels(plan); got != 0 {
		t.Fatalf("dynamic level entered constant shared kernel: %d", got)
	}
	if _, ok := plan.(*chplan.UnionAll); !ok {
		t.Fatalf("dynamic-level fallback = %T, want *chplan.UnionAll", plan)
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
		if fc, ok := pr.Expr.(*chplan.FuncCall); ok && fc.Fn == chplan.FnMapMerge {
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
