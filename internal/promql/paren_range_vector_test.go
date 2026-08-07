package promql_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestParenRangeVectorMatchesBareSpelling pins #1896 directly: a
// parenthesised range vector must lower to BYTE-IDENTICAL SQL and args as
// its bare-selector twin, at every site that reads a range-vector argument
// (lowerCall's dispatch, lowerRangeVectorCall, matrixAndSelector,
// lowerSubqueryOverCall, lowerAbsentOverTime). A fixture that only records
// whatever SQL a paren spelling happens to produce would still pass if the
// paren form silently diverged from the bare form's semantics — this test
// asserts the relationship, not just that each spelling lowers.
func TestParenRangeVectorMatchesBareSpelling(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	anchor := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

	cases := []struct {
		name string
		bare string
		// paren spellings that must all match `bare` byte-for-byte.
		paren []string
	}{
		{
			name: "rate",
			bare: `rate(http_requests_total[5m])`,
			paren: []string{
				`rate((http_requests_total[5m]))`,
				`rate(((http_requests_total[5m])))`,
			},
		},
		{
			name:  "quantile_over_time",
			bare:  `quantile_over_time(0.5, latency_ms[5m])`,
			paren: []string{`quantile_over_time(0.5, (latency_ms[5m]))`},
		},
		{
			name:  "absent_over_time",
			bare:  `absent_over_time(temperature[5m])`,
			paren: []string{`absent_over_time((temperature[5m]))`},
		},
		{
			name:  "subquery_inner_rate",
			bare:  `max_over_time(rate(http_requests_total[5m])[10m:1m])`,
			paren: []string{`max_over_time(rate((http_requests_total[5m]))[10m:1m])`},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			wantSQL, wantArgs := lowerAndEmit(t, tc.bare, s, anchor)

			for _, spelling := range tc.paren {
				gotSQL, gotArgs := lowerAndEmit(t, spelling, s, anchor)
				if gotSQL != wantSQL {
					t.Errorf("SQL for %q does not match bare spelling %q:\n  bare:  %s\n  paren: %s",
						spelling, tc.bare, wantSQL, gotSQL)
				}
				if len(gotArgs) != len(wantArgs) {
					t.Fatalf("args length for %q (%d) does not match bare spelling %q (%d)",
						spelling, len(gotArgs), tc.bare, len(wantArgs))
				}
				for i := range wantArgs {
					if gotArgs[i] != wantArgs[i] {
						t.Errorf("arg[%d] for %q = %v, bare spelling %q has %v",
							i, spelling, gotArgs[i], tc.bare, wantArgs[i])
					}
				}
			}
		})
	}
}

func lowerAndEmit(t *testing.T, query string, s schema.Metrics, anchor time.Time) (string, []any) {
	t.Helper()

	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, anchor, anchor)
	if err != nil {
		t.Fatalf("LowerAt(%q): %v", query, err)
	}
	sql, args, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(%q): %v", query, err)
	}
	return sql, args
}
