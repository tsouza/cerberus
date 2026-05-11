package traceql

import (
	"testing"

	"github.com/grafana/tempo/pkg/traceql"
)

// TestParserSmoke confirms that the upstream Tempo TraceQL parser loads and
// parses a trivial expression. Real lowering lands after the PromQL slice.
func TestParserSmoke(t *testing.T) {
	t.Parallel()

	cases := []string{
		`{ .service.name = "frontend" }`,
		`{ duration > 100ms }`,
		`{ .http.status_code >= 500 } | count() > 0`,
	}

	for _, q := range cases {
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			expr, err := traceql.Parse(q)
			if err != nil {
				t.Fatalf("Parse(%q): %v", q, err)
			}
			if expr == nil {
				t.Fatalf("Parse(%q) returned nil expression", q)
			}
		})
	}
}
