package logql

import (
	"testing"

	syntax "github.com/tsouza/cerberus/internal/logql/lsyntax"
)

// TestParserSmoke pins that the upstream LogQL parser accepts every
// shape cerberus claims to handle, plus a wider catalog of real-world
// queries from the Loki test corpus and Grafana dashboards.
func TestParserSmoke(t *testing.T) {
	t.Parallel()

	cases := []string{
		// Stream selectors — basic + label matcher variants.
		// Loki rejects selectors with only-negation / only-empty matchers
		// ("at least one regexp or equality matcher that does not have
		// an empty-compatible value"); we test negation only paired with
		// an equality anchor.
		`{job="api"}`,
		`{ job = "api" }`, // whitespace inside braces
		`{job=~"api|web"}`,
		`{job="api", env!="prod"}`,
		`{job="api", env!~"prod.*"}`,
		`{job="api", env="prod"}`,
		`{job="api", env=~"prod|stage"}`,
		"{job=~`api\\w+`}", // backtick raw string

		// Line filters — all 4 operators
		`{job="api"} |= "ERROR"`,
		`{job="api"} != "DEBUG"`,
		`{job="api"} |~ "fail.*"`,
		`{job="api"} !~ "health.*ok"`,

		// Chained line filters (AND fold)
		`{job="api"} |= "ERROR" |~ "5\\d\\d"`,
		`{job="api"} |= "a" != "b" |~ "c" !~ "d"`,

		// Line filters with `or` (OR fold)
		`{job="api"} |= "error" or "warn"`,
		`{job="api"} !~ "health" or "metrics"`,

		// Label filters
		`{job="api"} | level="ERROR"`,
		`{job="api"} | level!="DEBUG"`,
		`{job="api"} | level=~"WARN|ERROR"`,
		`{job="api"} | status_code >= 500`,
		`{job="api"} | latency > 250ms`,
		`{job="api"} | latency > 1h15m30s`,
		`{job="api"} | size > 250KB`,

		// Parser stages
		`{job="api"} | json`,
		`{job="api"} | logfmt`,
		`{job="api"} | regexp "(?P<status>\\d+)"`,
		`{job="api"} | pattern "<_> <status>"`,
		`{job="api"} | unpack`,
		`{job="api"} | json code="response.code"`,

		// Decolorize / strip
		`{job="api"} | decolorize`,

		// Multiple stages chained
		`{job="api"} | json | level="ERROR" | line_format "{{.message}}"`,
		`{job="api"} |= "error" | json | latency > 100ms`,

		// Range aggregations
		`rate({job="api"}[5m])`,
		`count_over_time({job="api"}[5m])`,
		`bytes_rate({job="api"}[5m])`,
		`bytes_over_time({job="api"}[5m])`,
		`rate({job="api"} |= "error" [5m])`,

		// Unwrap-based range aggregations
		`sum_over_time({job="api"} | unwrap latency [5m])`,
		`avg_over_time({job="api"} | unwrap duration(latency) [5m])`,
		`quantile_over_time(0.95, {job="api"} | unwrap latency [5m])`,
		`max_over_time({job="api"} | unwrap bytes_received [5m])`,

		// Vector aggregations
		`sum(rate({job="api"}[5m]))`,
		`sum by (level) (rate({job="api"}[5m]))`,
		`sum without (instance) (rate({job="api"}[5m]))`,
		`avg by (level) (count_over_time({job="api"}[5m]))`,
		`topk(5, sum by (level) (rate({job="api"}[5m])))`,
		`bottomk(3, sum by (level) (rate({job="api"}[5m])))`,

		// Binary ops between metric queries
		`sum(rate({job="api"}[5m])) / sum(rate({job="api"}[1h]))`,
		`sum by (level) (rate({job="api"} |= "error" [5m])) / sum(rate({job="api"}[5m]))`,
		`rate({job="api"}[5m]) > 0`,
		`rate({job="api"}[5m]) > bool 0`,

		// Label formatting
		`{job="api"} | line_format "{{.message}}"`,
		`{job="api"} | label_format new_level="{{.level}}"`,
		`{job="api"} | json | line_format "{{.foo}} {{.bar}}"`,

		// Real-world dashboard queries
		`sum by (level) (rate({app="frontend"} | json | level=~"WARN|ERROR" [5m]))`,
		`topk(10, sum by (path) (rate({app="api"} | json | __error__ = "" [5m])))`,
		`count_over_time({namespace="prod", level="error"} [5m])`,
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

// TestParserSmoke_Rejected pins that obviously-broken LogQL is
// rejected by the parser, so cerberus never has to defend.
func TestParserSmoke_Rejected(t *testing.T) {
	t.Parallel()

	cases := []string{
		`{job="api"`,        // unterminated brace
		`{job="api`,         // unterminated string
		`{job=}`,            // missing matcher value
		`{}`,                // empty matcher set (Loki rejects this)
		`{=` + `"api"}`,     // missing matcher name
		`rate({job="api"})`, // missing range
		`rate({job="api"}[`, // unterminated range
		`{job="api"} | unknown_parser_stage`,
		`{job="api"} |= `,   // missing filter pattern
		`{job="api"} |~ "(`, // unterminated regex group
	}

	for _, q := range cases {
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			_, err := syntax.ParseExpr(q)
			if err == nil {
				t.Fatalf("ParseExpr(%q) should have rejected; accepted instead", q)
			}
		})
	}
}
