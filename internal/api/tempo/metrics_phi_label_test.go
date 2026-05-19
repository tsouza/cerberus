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

// TestMetricsLabelNames_TempoCanonicalDisplay pins the
// metricsLabelNames contract on the chplan-IR side: when the
// MetricsAggregate carries GroupByDisplayNames, those names win over
// the SQL aliases — the handler surfaces the scope-prefixed wire form
// to Grafana, not the bare alias used inside the SQL emit.
//
// Mirrors upstream Tempo's response shape; see the long comment on
// metricsLabelNames for the upstream cross-references. The test is
// purely on the helper rather than the full handler so a regression
// surfaces at the right altitude (a wrong label-name selection inside
// the helper, not a wrong JSON marshal).
func TestMetricsLabelNames_TempoCanonicalDisplay(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		aliases []string
		display []string
		want    []string
	}{
		{
			name:    "resource scope: display wins, prefix appears",
			aliases: []string{"service.name"},
			display: []string{"resource.service.name"},
			want:    []string{"resource.service.name"},
		},
		{
			name:    "span scope: same shape, different prefix",
			aliases: []string{"http.method"},
			display: []string{"span.http.method"},
			want:    []string{"span.http.method"},
		},
		{
			name:    "intrinsic: display == alias (no scope prefix)",
			aliases: []string{"kind"},
			display: []string{"kind"},
			want:    []string{"kind"},
		},
		{
			name:    "mixed: order preserved across resource/intrinsic/span",
			aliases: []string{"service.name", "kind", "http.method"},
			display: []string{"resource.service.name", "kind", "span.http.method"},
			want:    []string{"resource.service.name", "kind", "span.http.method"},
		},
		{
			name:    "legacy / non-TraceQL: empty display, fallback to aliases",
			aliases: []string{"service.name"},
			display: nil,
			want:    []string{"service.name"},
		},
		{
			name:    "partial display: per-slot fallback to alias for blank slots",
			aliases: []string{"service.name", "http.method"},
			display: []string{"resource.service.name", ""},
			want:    []string{"resource.service.name", "http.method"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			groupBy := make([]chplan.Expr, len(tc.aliases))
			for i := range groupBy {
				groupBy[i] = &chplan.ColumnRef{Name: tc.aliases[i]}
			}
			got := metricsLabelNames(&chplan.MetricsAggregate{
				GroupBy:             groupBy,
				GroupByAliases:      tc.aliases,
				GroupByDisplayNames: tc.display,
			})
			if !equalSlice(got, tc.want) {
				t.Fatalf("metricsLabelNames = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestWrapMetricsForSample_DisplayNameKeysAttributesMap pins the
// Attributes-map projection contract on the matrix-shape wrap: the
// LHS (KEY) of each `map(<key>, toString(<col>))` pair uses the
// Tempo-canonical display name when one is set, while the RHS (VALUE)
// keeps referencing the SQL-side alias. The two slices decouple
// deliberately — the chsql emitter aliases the SELECT column as the
// bare path for compact column names, and the wire layer prefixes the
// scope for upstream Tempo parity.
func TestWrapMetricsForSample_DisplayNameKeysAttributesMap(t *testing.T) {
	t.Parallel()

	rw := &chplan.RangeWindow{}
	m := &chplan.MetricsAggregate{
		GroupBy:             []chplan.Expr{&chplan.ColumnRef{Name: "x"}},
		GroupByAliases:      []string{"service.name"},
		GroupByDisplayNames: []string{"resource.service.name"},
		ValueAlias:          "Value",
	}

	node := wrapMetricsForSample(rw, m)
	proj, ok := node.(*chplan.Project)
	if !ok {
		t.Fatalf("wrapMetricsForSample: got %T, want *chplan.Project", node)
	}
	if len(proj.Projections) < 2 {
		t.Fatalf("project has %d projections, want >= 2", len(proj.Projections))
	}
	call, ok := proj.Projections[1].Expr.(*chplan.FuncCall)
	if !ok || call.Name != "map" {
		t.Fatalf("Attributes expr = %T (%v), want *chplan.FuncCall(map)", proj.Projections[1].Expr, proj.Projections[1].Expr)
	}
	if len(call.Args) < 2 {
		t.Fatalf("map call has %d args, want >= 2", len(call.Args))
	}
	keyLit, ok := call.Args[0].(*chplan.LitString)
	if !ok {
		t.Fatalf("map arg[0] = %T, want *chplan.LitString (the KEY)", call.Args[0])
	}
	if keyLit.V != "resource.service.name" {
		t.Errorf("Attributes map key = %q, want %q (Tempo-canonical wire form)",
			keyLit.V, "resource.service.name")
	}
	// And the VALUE side wraps toString around the SQL-side alias, not
	// the display name.
	valueCall, ok := call.Args[1].(*chplan.FuncCall)
	if !ok || valueCall.Name != "toString" {
		t.Fatalf("map arg[1] = %T (%v), want toString(<alias>)", call.Args[1], call.Args[1])
	}
	if len(valueCall.Args) != 1 {
		t.Fatalf("toString call has %d args, want 1", len(valueCall.Args))
	}
	colRef, ok := valueCall.Args[0].(*chplan.ColumnRef)
	if !ok {
		t.Fatalf("toString arg = %T, want *chplan.ColumnRef (the SQL alias)", valueCall.Args[0])
	}
	if colRef.Name != "service.name" {
		t.Errorf("toString column ref = %q, want %q (the bare SQL alias)",
			colRef.Name, "service.name")
	}
}
