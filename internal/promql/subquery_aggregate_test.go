package promql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLowerSubquery_Aggregate_Errors covers the error contracts on the
// subquery-over-aggregation paths: computed K / phi / label-name shapes
// are rejected the same way they are in instant mode (see
// aggregate_test.go) so callers see a clear error rather than a SQL
// runtime failure.
func TestLowerSubquery_Aggregate_Errors(t *testing.T) {
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
			query:   `max_over_time(topk(scalar(up), rate(http_requests_total[1m]))[1h:30s])`,
			wantErr: "requires a scalar literal K",
		},
		{
			name:    "topk K must be non-negative integer",
			query:   `max_over_time(topk(-1, rate(http_requests_total[1m]))[1h:30s])`,
			wantErr: "non-negative integer literal",
		},
		{
			name:    "topk K must be > 0",
			query:   `max_over_time(topk(0, rate(http_requests_total[1m]))[1h:30s])`,
			wantErr: "K must be > 0",
		},
		{
			name:    "bottomk K must be scalar literal",
			query:   `max_over_time(bottomk(scalar(up), rate(http_requests_total[1m]))[1h:30s])`,
			wantErr: "requires a scalar literal K",
		},
		{
			name:    "count_values requires non-empty label",
			query:   `max_over_time(count_values("", up)[5m:1m])`,
			wantErr: "non-empty label name",
		},
		{
			name:    "quantile needs scalar literal phi",
			query:   `max_over_time(quantile(scalar(up), rate(http_requests_total[1m]))[1h:30s])`,
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
