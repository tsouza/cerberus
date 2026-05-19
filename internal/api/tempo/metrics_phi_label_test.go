package tempo

import (
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestMetricsLabelNames_PhiLabelAddedForMultiQuantile pins the contract
// that the response label list includes the synthetic `__phi__` label
// when MetricsAggregate.Quantiles carries more than one phi value. The
// chsql multi-quantile fan-out projects an extra column with that
// alias; the handler must surface it as a wire-format label so each
// (group × phi) becomes its own response series.
func TestMetricsLabelNames_PhiLabelAddedForMultiQuantile(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		m    *chplan.MetricsAggregate
		want []string
	}{
		{
			name: "no_groupby_single_quantile",
			m:    &chplan.MetricsAggregate{Quantiles: []float64{0.95}},
			want: nil,
		},
		{
			name: "no_groupby_multi_quantile",
			m:    &chplan.MetricsAggregate{Quantiles: []float64{0.5, 0.9, 0.99}},
			want: []string{"__phi__"},
		},
		{
			name: "groupby_single_quantile",
			m: &chplan.MetricsAggregate{
				GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: "x"}},
				GroupByAliases: []string{"service"},
				Quantiles:      []float64{0.95},
			},
			want: []string{"service"},
		},
		{
			name: "groupby_multi_quantile",
			m: &chplan.MetricsAggregate{
				GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: "x"}},
				GroupByAliases: []string{"service"},
				Quantiles:      []float64{0.5, 0.99},
			},
			want: []string{"service", "__phi__"},
		},
		{
			name: "non_quantile_op_with_quantiles_unset",
			m: &chplan.MetricsAggregate{
				GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: "x"}},
				GroupByAliases: []string{"region"},
			},
			want: []string{"region"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := metricsLabelNames(tc.m)
			if !equalSlice(got, tc.want) {
				t.Fatalf("metricsLabelNames = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestWrapMetricsForSample_MultiQuantileAttrsIncludePhi pins the
// Attributes map's shape: when len(Quantiles) > 1 the projected map
// must reference both the group columns and the synthetic `__phi__`
// column produced by the chsql fan-out.
func TestWrapMetricsForSample_MultiQuantileAttrsIncludePhi(t *testing.T) {
	t.Parallel()

	rw := &chplan.RangeWindow{}
	m := &chplan.MetricsAggregate{
		GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: "x"}},
		GroupByAliases: []string{"service"},
		Quantiles:      []float64{0.5, 0.9},
		ValueAlias:     "Value",
	}

	node := wrapMetricsForSample(rw, m)
	proj, ok := node.(*chplan.Project)
	if !ok {
		t.Fatalf("wrapMetricsForSample: got %T, want *chplan.Project", node)
	}
	// The Attributes projection is at index 1 (after MetricName).
	if len(proj.Projections) < 2 {
		t.Fatalf("project has %d projections, want >= 2", len(proj.Projections))
	}
	attrsExpr := proj.Projections[1].Expr
	call, ok := attrsExpr.(*chplan.FuncCall)
	if !ok || call.Name != "map" {
		t.Fatalf("Attributes expr = %T (%v), want *chplan.FuncCall(map)", attrsExpr, attrsExpr)
	}

	// Args are (key, value) pairs. Look for the __phi__ key + matching value.
	foundPhiKey := false
	foundPhiCol := false
	for i, arg := range call.Args {
		if lit, ok := arg.(*chplan.LitString); ok && lit.V == "__phi__" {
			foundPhiKey = true
			if i+1 < len(call.Args) {
				if col, ok := call.Args[i+1].(*chplan.ColumnRef); ok && col.Name == "__phi__" {
					foundPhiCol = true
				}
			}
		}
	}
	if !foundPhiKey {
		t.Errorf("map args do not include '__phi__' literal key:\n  %v", call.Args)
	}
	if !foundPhiCol {
		t.Errorf("map args do not pair '__phi__' key with __phi__ ColumnRef")
	}
}

// TestWrapMetricsForSample_SingleQuantileNoPhiKey pins the inverse:
// single-quantile or non-quantile aggregates must NOT inject __phi__
// into the Attributes map (the chsql emitter doesn't project the
// column in that path).
func TestWrapMetricsForSample_SingleQuantileNoPhiKey(t *testing.T) {
	t.Parallel()

	rw := &chplan.RangeWindow{}
	m := &chplan.MetricsAggregate{
		GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: "x"}},
		GroupByAliases: []string{"service"},
		Quantiles:      []float64{0.95},
		ValueAlias:     "Value",
	}

	node := wrapMetricsForSample(rw, m)
	proj := node.(*chplan.Project)
	attrsExpr := proj.Projections[1].Expr.(*chplan.FuncCall)

	for _, arg := range attrsExpr.Args {
		if lit, ok := arg.(*chplan.LitString); ok && strings.Contains(lit.V, "__phi__") {
			t.Errorf("single-quantile map args unexpectedly include __phi__ key: %v", attrsExpr.Args)
		}
	}
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
