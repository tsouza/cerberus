package promql

import (
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestEmptyVectorFold_DropsTheArgumentsPlan pins that a histogram function
// whose argument provably carries none of the samples it reads answers the
// empty vector without the argument's plan: no table scan survives beneath
// the fold, which is still the constant-false Filter planProvablyEmpty
// recognises. A constant-false Filter over the argument's own plan answered
// the same empty vector, but ClickHouse still built the argument's whole
// pipeline for it, the classic-bucket fan-out of a rate included.
func TestEmptyVectorFold_DropsTheArgumentsPlan(t *testing.T) {
	t.Parallel()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	for _, query := range []string{
		// A float pipeline under a native-histogram value function.
		`histogram_fraction(0, 0.1, sum by (le) (rate(http_server_request_duration_bucket[5m])))`,
		`histogram_count(sum(rate(http_server_request_duration_bucket[5m])))`,
		// An argument that carries no le label under histogram_quantile.
		`histogram_quantile(0.9, rate(up[5m]) * 2)`,
		`histogram_quantile(0.5, count_values("hist", latency_exp_hist))`,
	} {
		for _, mode := range []hqClassMode{hqClassInstant, hqClassRangeFanout, hqClassRangeNative} {
			t.Run(string(mode)+" "+query, func(t *testing.T) {
				t.Parallel()
				expr, err := p.ParseExpr(query)
				if err != nil {
					t.Fatalf("ParseExpr(%q): %v", query, err)
				}
				plan := hqClassLowerExpr(t, expr, mode)
				scans := 0
				chplan.Walk(plan, func(n chplan.Node) bool {
					if _, ok := n.(*chplan.Scan); ok {
						scans++
					}
					return true
				})
				if scans != 0 {
					t.Errorf("the empty answer still carries %d table scans of its argument\nSQL: %s", scans, hqClassSQL(t, plan))
				}
				if !planProvablyEmpty(plan) {
					t.Errorf("the fold is not a constant-false Filter planProvablyEmpty recognises\nSQL: %s", hqClassSQL(t, plan))
				}
			})
		}
	}
}
