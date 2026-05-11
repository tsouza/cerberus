package logql

import (
	"testing"

	"github.com/grafana/loki/v3/pkg/logql/syntax"
)

// TestParserSmoke confirms that the upstream Loki LogQL parser loads and
// parses a trivial expression. Real lowering lands after the PromQL slice.
func TestParserSmoke(t *testing.T) {
	t.Parallel()

	cases := []string{
		`{job="api"}`,
		`{job="api"} |= "error"`,
		`rate({job="api"} |= "error" [5m])`,
	}

	for _, q := range cases {
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			expr, err := syntax.ParseExpr(q)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", q, err)
			}
			if expr == nil {
				t.Fatalf("ParseExpr(%q) returned nil expression", q)
			}
		})
	}
}
