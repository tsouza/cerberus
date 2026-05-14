package promql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_Aggregate_Errors covers the aggregate paths whose error
// messages are observable contract (param / no-param mismatch, computed
// quantile phi, count_values argument-shape rejections). topk/bottomk
// now accept `without(...)` (lowered into a MapWithoutKeys partition
// expression on chplan.TopK.By); count_values `without` is still
// deferred.
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
			name:    "topk K must be scalar literal",
			query:   `topk(scalar(up), latency_seconds)`,
			wantErr: "requires a scalar literal K",
		},
		{
			name:    "topk K must be non-negative integer",
			query:   `topk(-1, up)`,
			wantErr: "non-negative integer literal",
		},
		{
			name:    "topk K must be > 0",
			query:   `topk(0, up)`,
			wantErr: "K must be > 0",
		},
		{
			name:    "count_values rejects empty label",
			query:   `count_values("", up)`,
			wantErr: "non-empty label name",
		},
		{
			name:    "count_values without not yet supported",
			query:   `count_values("v", up) without (instance)`,
			wantErr: "without(...) is not yet supported",
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
