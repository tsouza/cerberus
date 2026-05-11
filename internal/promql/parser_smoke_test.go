package promql

import (
	"testing"

	"github.com/prometheus/prometheus/promql/parser"
)

// TestParserSmoke confirms that the upstream Prometheus PromQL parser loads
// and parses a trivial expression. Real lowering tests land in PR5.
func TestParserSmoke(t *testing.T) {
	t.Parallel()

	cases := []string{
		`up`,
		`rate(http_requests_total{job="api"}[5m])`,
		`sum by (job) (rate(http_requests_total[1m]))`,
	}

	p := parser.NewParser(parser.Options{})
	for _, q := range cases {
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(q)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", q, err)
			}
			if expr == nil {
				t.Fatalf("ParseExpr(%q) returned nil expression", q)
			}
		})
	}
}
