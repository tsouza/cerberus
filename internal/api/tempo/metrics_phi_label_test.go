package tempo

import (
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestMetricsLabelNames_PhiLabelAddedForMultiQuantile pins the contract
// that the response label list ends with `p` for every
// `quantile_over_time` MetricsAggregate (single or multi-phi, with or
// without a `by(...)` clause). Tempo's HistogramAggregator
// (engine_metrics.go) appends `Label{"p", NewStaticFloat(q)}` to every
// per-phi series; cerberus surfaces the same wire key so the
// tempo-compat differ canonicalises both backends identically.
//
// Non-quantile ops keep the existing behaviour: ungrouped surfaces
// `["__name__"]` (Tempo's UngroupedAggregator wire shape), grouped
// surfaces the by(...) display names only.
func TestMetricsLabelNames_PhiLabelAddedForMultiQuantile(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		m    *chplan.MetricsAggregate
		want []string
	}{
		{
			// Ungrouped quantile_over_time with a single phi: Tempo's
			// HistogramAggregator surfaces `{p="<phi>"}`, NOT
			// `{__name__="quantile_over_time"}`, because quantile is
			// routed through HistogramAggregator rather than
			// UngroupedAggregator.
			name: "no_groupby_single_quantile",
			m: &chplan.MetricsAggregate{
				Op:        chplan.MetricsOpQuantileOverTime,
				Quantiles: []float64{0.95},
			},
			want: []string{"p"},
		},
		{
			name: "no_groupby_multi_quantile",
			m: &chplan.MetricsAggregate{
				Op:        chplan.MetricsOpQuantileOverTime,
				Quantiles: []float64{0.5, 0.9, 0.99},
			},
			want: []string{"p"},
		},
		{
			name: "groupby_single_quantile",
			m: &chplan.MetricsAggregate{
				Op:             chplan.MetricsOpQuantileOverTime,
				GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: "x"}},
				GroupByAliases: []string{"service"},
				Quantiles:      []float64{0.95},
			},
			want: []string{"service", "p"},
		},
		{
			name: "groupby_multi_quantile",
			m: &chplan.MetricsAggregate{
				Op:             chplan.MetricsOpQuantileOverTime,
				GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: "x"}},
				GroupByAliases: []string{"service"},
				Quantiles:      []float64{0.5, 0.99},
			},
			want: []string{"service", "p"},
		},
		{
			// Grouped non-quantile ops drop the `p` label entirely —
			// the `by(...)` labels stand alone.
			name: "non_quantile_op_with_quantiles_unset",
			m: &chplan.MetricsAggregate{
				Op:             chplan.MetricsOpAvgOverTime,
				GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: "x"}},
				GroupByAliases: []string{"region"},
			},
			want: []string{"region"},
		},
		{
			// Ungrouped non-quantile ops surface `["__name__"]`
			// (Tempo's UngroupedAggregator wire shape — the `__name__`
			// value is filled in by wrapMetricsForSample via
			// m.Op.String()).
			name: "no_groupby_non_quantile",
			m: &chplan.MetricsAggregate{
				Op: chplan.MetricsOpRate,
			},
			want: []string{"__name__"},
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
// column produced by the chsql fan-out — surfaced on the wire under
// the Tempo-canonical `p` key (matching HistogramAggregator's
// `Label{"p", NewStaticFloat(q)}` per-series label).
func TestWrapMetricsForSample_MultiQuantileAttrsIncludePhi(t *testing.T) {
	t.Parallel()

	rw := &chplan.RangeWindow{}
	m := &chplan.MetricsAggregate{
		Op:             chplan.MetricsOpQuantileOverTime,
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

	// Args are (key, value) pairs. The wire key is `p`; the SQL column
	// alias is the unexported `__phi__` constant — they intentionally
	// diverge so the wire shape mirrors Tempo while the SQL stream stays
	// recognisable as the multi-phi fan-out output.
	foundPKey := false
	foundPhiCol := false
	for i, arg := range call.Args {
		if lit, ok := arg.(*chplan.LitString); ok && lit.V == "p" {
			foundPKey = true
			if i+1 < len(call.Args) {
				if col, ok := call.Args[i+1].(*chplan.ColumnRef); ok && col.Name == "__phi__" {
					foundPhiCol = true
				}
			}
		}
	}
	if !foundPKey {
		t.Errorf("map args do not include 'p' literal key:\n  %v", call.Args)
	}
	if !foundPhiCol {
		t.Errorf("map args do not pair 'p' wire key with the __phi__ SQL ColumnRef")
	}
}

// TestWrapMetricsForSample_SingleQuantilePLabelLiteral pins the
// single-phi quantile_over_time contract: the Attributes map carries a
// `('p', '<phi_str>')` pair with the phi as an inline literal, because
// the chsql emitter doesn't project a per-row phi column in the
// single-phi path. The wire shape stays consistent with the multi-phi
// branch — both surface a `p` label per series.
func TestWrapMetricsForSample_SingleQuantilePLabelLiteral(t *testing.T) {
	t.Parallel()

	rw := &chplan.RangeWindow{}
	m := &chplan.MetricsAggregate{
		Op:             chplan.MetricsOpQuantileOverTime,
		GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: "x"}},
		GroupByAliases: []string{"service"},
		Quantiles:      []float64{0.95},
		ValueAlias:     "Value",
	}

	node := wrapMetricsForSample(rw, m)
	proj := node.(*chplan.Project)
	call := proj.Projections[1].Expr.(*chplan.FuncCall)

	foundPLit := false
	for i, arg := range call.Args {
		if lit, ok := arg.(*chplan.LitString); ok && lit.V == "p" {
			if i+1 < len(call.Args) {
				if v, ok := call.Args[i+1].(*chplan.LitString); ok && v.V == "0.95" {
					foundPLit = true
				}
			}
		}
		// Reject any leak of the SQL-side `__phi__` alias into the
		// single-phi path — the chsql emitter doesn't project that
		// column here.
		if lit, ok := arg.(*chplan.LitString); ok && strings.Contains(lit.V, "__phi__") {
			t.Errorf("single-quantile map args unexpectedly include __phi__ key: %v", call.Args)
		}
		if col, ok := arg.(*chplan.ColumnRef); ok && col.Name == "__phi__" {
			t.Errorf("single-quantile map args unexpectedly reference __phi__ ColumnRef: %v", call.Args)
		}
	}
	if !foundPLit {
		t.Errorf("single-phi quantile_over_time: expected ('p', '0.95') literal pair, got %v", call.Args)
	}
}

// TestWrapMetricsForSample_UngroupedQuantileSurfacesPLabel pins the
// ungrouped quantile path: no `by(...)` clause and a single phi must
// emit `{p="<phi>"}` (NOT `{__name__="quantile_over_time"}`) because
// Tempo routes quantile_over_time through HistogramAggregator rather
// than UngroupedAggregator. This is the case the tempo-compat differ
// caught with `missing_in_a` for the metrics_quantile_over_time_p95
// case before the fix.
func TestWrapMetricsForSample_UngroupedQuantileSurfacesPLabel(t *testing.T) {
	t.Parallel()

	rw := &chplan.RangeWindow{}
	m := &chplan.MetricsAggregate{
		Op:         chplan.MetricsOpQuantileOverTime,
		Quantiles:  []float64{0.95},
		ValueAlias: "Value",
	}

	node := wrapMetricsForSample(rw, m)
	proj := node.(*chplan.Project)
	call := proj.Projections[1].Expr.(*chplan.FuncCall)

	// Expect exactly one (key, value) pair: ('p', '0.95').
	if len(call.Args) != 2 {
		t.Fatalf("ungrouped quantile_over_time: expected 2 map args (one pair), got %d: %v",
			len(call.Args), call.Args)
	}
	key, ok := call.Args[0].(*chplan.LitString)
	if !ok || key.V != "p" {
		t.Errorf("Attributes map[0] (key) = %v, want LitString{p}", call.Args[0])
	}
	val, ok := call.Args[1].(*chplan.LitString)
	if !ok || val.V != "0.95" {
		t.Errorf("Attributes map[1] (value) = %v, want LitString{0.95}", call.Args[1])
	}

	// And the `__name__` synthetic label MUST NOT appear — that's the
	// UngroupedAggregator path, which quantile_over_time skips.
	for _, arg := range call.Args {
		if lit, ok := arg.(*chplan.LitString); ok && lit.V == "__name__" {
			t.Errorf("ungrouped quantile_over_time leaked __name__ label: %v", call.Args)
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
// query carries no `by(...)` clause and isn't a quantile aggregate,
// the Attributes projection must emit `map('__name__', '<op>')` rather
// than an empty Map(String,String). That mirrors Tempo's reference
// engine (pkg/traceql.UngroupedAggregator.Series) which attaches a
// single `__name__=<op_name>` label per series, and stops cerberus
// from emitting the divergent empty-labels series the Tempo compat
// differ flagged as `missing_in_a series key e3b0c44298fc1c14` (the
// sha256 prefix of the empty string).
//
// quantile_over_time is excluded here on purpose — Tempo routes it
// through HistogramAggregator rather than UngroupedAggregator, so the
// ungrouped wire shape is `{p="<phi>"}`. See
// TestWrapMetricsForSample_UngroupedQuantileSurfacesPLabel for that
// branch.
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
// contract: an ungrouped non-quantile MetricsAggregate surfaces
// `["__name__"]` so labelsFromSample orders the synthetic label
// deterministically before any other key the SQL projection might
// surface. Tempo's UngroupedAggregator wire shape is the upstream
// reference (pkg/traceql.UngroupedAggregator.Series).
//
// Ungrouped quantile_over_time surfaces `["p"]` instead — see
// TestMetricsLabelNames_PhiLabelAddedForMultiQuantile / the dedicated
// wrap test (HistogramAggregator parity).
func TestMetricsLabelNames_UngroupedIncludesMetricName(t *testing.T) {
	t.Parallel()

	got := metricsLabelNames(&chplan.MetricsAggregate{Op: chplan.MetricsOpRate})
	if !equalSlice(got, []string{"__name__"}) {
		t.Fatalf("metricsLabelNames(ungrouped rate) = %v, want [__name__]", got)
	}
	// quantile_over_time takes the `p` branch (HistogramAggregator
	// parity); the `__name__` injection only fires when the op is
	// routed through UngroupedAggregator upstream.
	got = metricsLabelNames(&chplan.MetricsAggregate{
		Op:        chplan.MetricsOpQuantileOverTime,
		Quantiles: []float64{0.5, 0.9},
	})
	if !equalSlice(got, []string{"p"}) {
		t.Fatalf("metricsLabelNames(ungrouped multi-quantile) = %v, want [p]", got)
	}
}
