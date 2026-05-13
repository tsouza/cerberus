package promql_test

import (
	"context"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// FuzzParse drives random inputs through the upstream PromQL parser and,
// on a successful parse, through cerberus's lowering. The goal is
// panic-freedom: any panic surfaces as a fuzz crash with the offending
// seed promoted to testdata/fuzz/FuzzParse/.
//
// RC3 R3.11 — seed corpus is intentionally small; CI runs `-fuzztime=120s`
// per parser weekly to grow the corpus organically.
func FuzzParse(f *testing.F) {
	seeds := []string{
		`up`,
		`up{job="api"}`,
		`rate(http_requests_total[5m])`,
		`sum by (job) (rate(http_requests_total[5m]))`,
		`histogram_quantile(0.9, sum by (le) (rate(http_request_duration_seconds_bucket[5m])))`,
		`up + on(job) up`,
		`up offset 5m`,
		`up @ 1700000000`,
		`max_over_time(rate(http_requests_total[5m])[1h:5m])`,
		`100 * (1 - avg(rate(node_cpu_seconds_total{mode="idle"}[5m])))`,
		`absent_over_time(up[5m])`,
		`label_replace(up, "new", "$1", "job", "(.+)")`,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})

	f.Fuzz(func(t *testing.T, q string) {
		expr, err := p.ParseExpr(q)
		if err != nil {
			return
		}
		_, _ = promql.Lower(context.Background(), expr, s)
	})
}
