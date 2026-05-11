package promql_test

import (
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_Aggregate_Errors covers the M1.4 surface that intentionally
// stays out of scope (output-shape-changing aggregates) plus the param /
// no-param mismatch paths so the error messages remain observable.
func TestLower_Aggregate_Errors(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})

	cases := []struct {
		name    string
		query   string
		wantErr string
	}{
		{
			name:    "topk shape change deferred",
			query:   `topk(3, up)`,
			wantErr: "changes output shape and lands with M1.7",
		},
		{
			name:    "bottomk shape change deferred",
			query:   `bottomk(3, up)`,
			wantErr: "changes output shape and lands with M1.7",
		},
		{
			name:    "count_values shape change deferred",
			query:   `count_values("v", up)`,
			wantErr: "changes output shape and lands with M1.7",
		},
		{
			name:    "quantile needs scalar literal phi",
			query:   `quantile(scalar(up), latency_seconds)`,
			wantErr: "scalar literal phi",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr: %v", err)
			}
			_, err = promql.Lower(expr, s)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}
