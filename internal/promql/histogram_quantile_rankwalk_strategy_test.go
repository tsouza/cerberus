package promql

import (
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestQuantileRankWalk_ReachesEveryConstructionSite pins the ONE thing
// promql.QuantileRankWalkLowerer's boot-wired seam depends on that no
// TXTAR fixture can reach: every one of the FOUR places that build a
// chplan.HistogramQuantile node propagates ctx.lowerers.QuantileRankWalk
// into UseNativeQuantileAggregate. Unlike the ClickHouse-native
// timeSeries*ToGrid family, the spec lane's marker-section mechanism
// (internal/promql/lower_test.go's wireNativeStrategies) only threads
// LowerOpts.Lowerers through the RANGE-mode branch — the two INSTANT
// construction sites (lowerHistogramQuantileClassicBare,
// lowerHistogramQuantileAgg) and the float-domain one
// (lowerHistogramQuantileClassicFloat) are unreachable from a TXTAR
// fixture's marker section, since promql.LowerAt itself takes no
// LowerOpts. This test is what closes that gap for the wiring; the
// emitted SQL shape those four sites feed is independently proven correct
// by internal/chsql's own real-CH differential
// (TestHistogramQuantile_RankWalkNative_DifferentialRealCH) and by
// test/spec/promql/histogram_quantile_classic_range_step_native.txtar
// (the one shape reachable through the marker-section mechanism).
func TestQuantileRankWalk_ReachesEveryConstructionSite(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})
	rangeStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name      string
		query     string
		rangeMode bool
	}{
		{name: "instant bare selector", query: `histogram_quantile(0.9, http_server_request_duration_bucket)`},
		{name: "instant cross-series merge", query: `histogram_quantile(0.5, quantile by (le) (0.9, rate(http_server_request_duration_bucket[5m])))`},
		{name: "float-domain (topk)", query: `histogram_quantile(0.5, topk by(le) (2, sum_over_time(http_server_request_duration_bucket[5m])))`},
		{name: "range bare selector", query: `histogram_quantile(0.9, http_server_request_duration_bucket)`, rangeMode: true},
		{name: "range aggregated", query: `histogram_quantile(0.9, sum by(le)(rate(http_server_request_duration_bucket[5m])))`, rangeMode: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tt.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tt.query, err)
			}

			for _, native := range []bool{false, true} {
				var lowerers RangeLowerers
				if native {
					lowerers.QuantileRankWalk = NativeQuantileRankWalkLowerer{}
				}
				lowerers = lowerers.withDefaults()

				ctx := lowerCtx{lowerers: lowerers, resourceBounds: DefaultResourceBounds()}
				if tt.rangeMode {
					ctx.start = rangeStart
					ctx.end = rangeStart.Add(5 * time.Minute)
					ctx.step = time.Minute
				}

				plan, err := lowerRoot(expr, s, ctx)
				if err != nil {
					t.Fatalf("lower(%q, native=%v): %v", tt.query, native, err)
				}

				got, found := findHistogramQuantileNative(plan)
				if !found {
					t.Fatalf("lower(%q, native=%v): plan has no HistogramQuantile node", tt.query, native)
				}
				if got != native {
					t.Errorf("lower(%q, native=%v): HistogramQuantile.UseNativeQuantileAggregate = %v, want %v",
						tt.query, native, got, native)
				}
			}
		})
	}
}

// findHistogramQuantileNative walks root for a *chplan.HistogramQuantile
// node and returns its UseNativeQuantileAggregate flag, or ok=false if the
// plan carries none.
func findHistogramQuantileNative(root chplan.Node) (native, ok bool) {
	var walk func(chplan.Node)
	walk = func(n chplan.Node) {
		if n == nil || ok {
			return
		}
		if hq, isHQ := n.(*chplan.HistogramQuantile); isHQ {
			native, ok = hq.UseNativeQuantileAggregate, true
			return
		}
		for _, c := range n.Children() {
			walk(c)
		}
	}
	walk(root)
	return native, ok
}
