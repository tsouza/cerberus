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
			// Ungrouped (no by(...) + single quantile) is the
			// UngroupedAggregator-equivalent shape — Tempo's wire
			// surface for that case is `{__name__="<op>"}`. We surface
			// the same `__name__` label key (the value is injected by
			// wrapMetricsForSample via m.Op.String()).
			name: "no_groupby_single_quantile",
			m:    &chplan.MetricsAggregate{Quantiles: []float64{0.95}},
			want: []string{"__name__"},
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

// TestWrapMetricsForSample_UngroupedAttachesMetricName pins the
// UngroupedAggregator parity contract: when a TraceQL metrics-pipeline
// query carries no `by(...)` clause and isn't a multi-quantile fan-out,
// the Attributes projection must emit `map('__name__', '<op>')` rather
// than an empty Map(String,String). That mirrors Tempo's reference
// engine (pkg/traceql.UngroupedAggregator.Series) which attaches a
// single `__name__=<op_name>` label per series, and stops cerberus
// from emitting the divergent empty-labels series the Tempo compat
// differ flagged as `missing_in_a series key e3b0c44298fc1c14` (the
// sha256 prefix of the empty string).
func TestWrapMetricsForSample_UngroupedAttachesMetricName(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		op      chplan.MetricsOp
		wantVal string
	}{
		{name: "rate", op: chplan.MetricsOpRate, wantVal: "rate"},
		{name: "count_over_time", op: chplan.MetricsOpCountOverTime, wantVal: "count_over_time"},
		{name: "avg_over_time", op: chplan.MetricsOpAvgOverTime, wantVal: "avg_over_time"},
		{name: "sum_over_time", op: chplan.MetricsOpSumOverTime, wantVal: "sum_over_time"},
		{name: "quantile_over_time", op: chplan.MetricsOpQuantileOverTime, wantVal: "quantile_over_time"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rw := &chplan.RangeWindow{}
			m := &chplan.MetricsAggregate{
				Op:         tc.op,
				ValueAlias: "Value",
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
			if len(call.Args) != 2 {
				t.Fatalf("map args = %d, want 2 ({__name__, <op>}): %+v", len(call.Args), call.Args)
			}
			keyLit, ok := call.Args[0].(*chplan.LitString)
			if !ok || keyLit.V != "__name__" {
				t.Errorf("map arg[0] = %v, want LitString(__name__)", call.Args[0])
			}
			valLit, ok := call.Args[1].(*chplan.LitString)
			if !ok || valLit.V != tc.wantVal {
				t.Errorf("map arg[1] = %v, want LitString(%q)", call.Args[1], tc.wantVal)
			}
		})
	}
}

// TestMetricsLabelNames_UngroupedIncludesMetricName pins the helper-side
// contract: an ungrouped MetricsAggregate (no GroupBy + no multi-phi)
// surfaces `["__name__"]` so labelsFromSample orders the synthetic
// label deterministically before any other key the SQL projection
// might surface. Tempo's UngroupedAggregator wire shape is the upstream
// reference (pkg/traceql.UngroupedAggregator.Series).
func TestMetricsLabelNames_UngroupedIncludesMetricName(t *testing.T) {
	t.Parallel()

	got := metricsLabelNames(&chplan.MetricsAggregate{Op: chplan.MetricsOpRate})
	if !equalSlice(got, []string{"__name__"}) {
		t.Fatalf("metricsLabelNames(ungrouped rate) = %v, want [__name__]", got)
	}
	// multi-phi takes precedence (the fan-out adds its own column);
	// the __name__ injection only fires when there's truly nothing
	// else on the wire.
	got = metricsLabelNames(&chplan.MetricsAggregate{
		Op:        chplan.MetricsOpQuantileOverTime,
		Quantiles: []float64{0.5, 0.9},
	})
	if !equalSlice(got, []string{"__phi__"}) {
		t.Fatalf("metricsLabelNames(ungrouped multi-quantile) = %v, want [__phi__]", got)
	}
}
