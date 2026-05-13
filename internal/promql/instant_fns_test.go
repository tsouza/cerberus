package promql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_InstantFn_Errors covers the unsupported instant-fn shapes so
// the error messages stay observable as the surface grows.
func TestLower_InstantFn_Errors(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})

	cases := []struct {
		name    string
		query   string
		wantErr string
	}{
		{
			name:    "round 2-arg requires scalar bound",
			query:   `round(temperature, scalar(other))`,
			wantErr: "requires a scalar literal to_nearest",
		},
		{
			name:    "histogram_quantile non-scalar phi",
			query:   `histogram_quantile(scalar(other), foo)`,
			wantErr: "requires a scalar-literal phi",
		},
		{
			name:    "histogram_quantile non-selector arg",
			query:   `histogram_quantile(0.9, vector(1))`,
			wantErr: "second argument must be a histogram VectorSelector",
		},
		{
			name:    "clamp_max needs scalar bound",
			query:   `clamp_max(up, scalar(other))`,
			wantErr: "requires a scalar-literal bound",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr: %v", err)
			}
			_, err = promql.Lower(context.Background(), expr, s)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}
