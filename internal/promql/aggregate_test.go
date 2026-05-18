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
// and count_values now both accept `without(...)`: topk lowers into a
// MapWithoutKeys partition expression on chplan.TopK.By, count_values
// into a MapWithoutKeys group key + mapConcat overlay (see
// lowerCountValues).
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
			// `topk(scalar(<vector>), v)` and `bottomk(scalar(<vector>), v)`
			// are now lowered into the computed-K shape (KExpr on
			// chplan.TopK). Other computed-K shapes — `topk(2 + scalar(x), v)`,
			// `topk(time(), v)`, etc. — still error since the lowering
			// only recognises `scalar(<vector>)` as a K source.
			name:    "topk K must be scalar literal or scalar(...)",
			query:   `topk(time(), latency_seconds)`,
			wantErr: "must be a scalar literal or scalar(<vector>)",
		},
		{
			// Mixed arithmetic around a scalar() subquery is still
			// rejected: `2 + scalar(x)` lowers as a vector-scalar binop
			// at parse time, so tryScalarLiteral returns false and the
			// computed-K path's scalar(...) detector also fails.
			name:    "topk K rejects mixed scalar arithmetic",
			query:   `topk(2 + scalar(latency_seconds), up)`,
			wantErr: "must be a scalar literal or scalar(<vector>)",
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
