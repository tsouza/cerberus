package promql

import (
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

func TestClassicBucketRateGridReduction(t *testing.T) {
	t.Parallel()
	const bucketRate = `rate(core_http_request_duration_seconds_bucket[5m])`
	for _, tc := range []struct {
		name, query    string
		native, vector bool
	}{
		{"bucket sum", `sum by(le, k8s_deployment_name)(` + bucketRate + `)`, true, true},
		{"bucket sum without", `sum without(instance)(` + bucketRate + `)`, true, true},
		{"quantile", `histogram_quantile(0.95, sum by(le, k8s_deployment_name)(` + bucketRate + `))`, true, true},
		{"ordinary counter unchanged", `sum by(job)(rate(requests_total[5m]))`, true, false},
		{"bucket average unchanged", `avg by(le)(` + bucketRate + `)`, true, false},
		{"bucket increase unchanged", `sum by(le)(increase(core_http_request_duration_seconds_bucket[5m]))`, true, false},
		{"native unavailable", `sum by(le)(` + bucketRate + `)`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := parser.NewParser(parser.Options{}).ParseExpr(tc.query)
			if err != nil {
				t.Fatal(err)
			}
			var lowerers RangeLowerers
			if tc.native {
				lowerers.Rate = NativeRateLowerer{Fallback: FanoutRateLowerer{}}
			}
			start := time.Date(2026, 8, 18, 9, 5, 0, 0, time.UTC)
			plan, err := lowerRoot(expr, schema.DefaultOTelMetrics(), lowerCtx{
				start: start, end: start.Add(time.Hour), step: 30 * time.Second,
				lowerers: lowerers.withDefaults(), resourceBounds: DefaultResourceBounds(),
			})
			if err != nil {
				t.Fatal(err)
			}
			var vectors int
			chplan.Walk(plan, func(node chplan.Node) bool {
				if grid, ok := node.(*chplan.RangeWindowGridNativeVectorAgg); ok {
					vectors++
					if grid.Fn != chplan.FnSum || !isClassicBucketRateGrid(grid.Input, schema.DefaultOTelMetrics()) {
						t.Errorf("fold must consume completed original-source bucket rates: %#v", grid)
					}
				}
				return true
			})
			want := 0
			if tc.vector {
				want = 1
			}
			if vectors != want {
				t.Fatalf("vector reductions=%d, want %d", vectors, want)
			}
		})
	}
}

func TestClassicBucketRateGridRejectsOtherNodes(t *testing.T) {
	t.Parallel()
	for _, node := range []chplan.Node{
		&chplan.Scan{}, &chplan.Limit{}, &chplan.Aggregate{},
		&chplan.RangeWindowGridNative{Func: "increase", Input: &chplan.Scan{}},
		&chplan.RangeWindowGridNative{Func: "rate", Input: &chplan.Scan{}},
	} {
		if isClassicBucketRateGrid(node, schema.DefaultOTelMetrics()) {
			t.Errorf("accepted non-bucket grid %T", node)
		}
	}
}

func TestQuantileRankWalkComputedPhiRetainsRuntimeSemantics(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		phi    chplan.Expr
		native bool
	}{
		{"literal", nil, true},
		{"runtime column", &chplan.ColumnRef{Name: "phi"}, false},
		{"computed expression", &chplan.LitFloat{V: 0.95}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := &chplan.HistogramQuantile{Phi: 0.5, PhiExpr: tc.phi}
			got := (NativeQuantileRankWalkLowerer{}).LowerQuantileRankWalk(plan)
			if got != plan || got.UseNativeQuantileAggregate != tc.native {
				t.Fatalf("native=%v, want %v, same node=%v", got.UseNativeQuantileAggregate, tc.native, got == plan)
			}
		})
	}
}
