package logql_test

import (
	"context"
	"testing"

	"github.com/grafana/loki/v3/pkg/logql/syntax"

	"github.com/tsouza/cerberus/internal/logql"
	"github.com/tsouza/cerberus/internal/schema"
)

// FuzzParse drives random inputs through the upstream LogQL parser and,
// on a successful parse, through cerberus's lowering. The goal is
// panic-freedom: any panic surfaces as a fuzz crash with the offending
// seed promoted to testdata/fuzz/FuzzParse/.
//
// RC3 R3.11 — seed corpus is intentionally small; CI runs `-fuzztime=120s`
// per parser weekly to grow the corpus organically.
func FuzzParse(f *testing.F) {
	seeds := []string{
		`{job="api"}`,
		`{job="api"} |= "ERROR"`,
		`{job="api"} | json | level="ERROR"`,
		`{job="api"} | logfmt | latency > 100ms`,
		`rate({job="api"}[5m])`,
		`sum by (level) (rate({job="api"} |= "error" [5m]))`,
		`count_over_time({job="api"}[5m])`,
		`sum_over_time({job="api"} | unwrap latency [5m])`,
		`topk(5, sum by (level) (rate({job="api"}[5m])))`,
		`{job="api"} | line_format "{{.message}}"`,
		`{job="api"} | label_format new_level="{{.level}}"`,
		`{job="api"} | regexp "(?P<status>\\d+)"`,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	s := schema.DefaultOTelLogs()

	f.Fuzz(func(t *testing.T, q string) {
		expr, err := syntax.ParseExpr(q)
		if err != nil {
			return
		}
		_, _ = logql.Lower(context.Background(), expr, s)
	})
}
