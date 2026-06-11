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
			name:    "histogram_quantile non-selector arg",
			query:   `histogram_quantile(0.9, vector(1))`,
			wantErr: "second argument must be a histogram VectorSelector",
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

// TestLower_InstantFn_ComputedScalarArgs pins the computed-scalar
// acceptance for the instant-fn family: `scalar(<vector>)` (and any
// scalar-typed composition) is a valid bound / to_nearest / phi
// argument — reference Prometheus evaluates these per query, so a
// lowering-time rejection was a wrong rejection (rejection-parity
// catalogue: clamp / clamp_min / round / histogram_quantile entries).
// The bound rides a chplan.ScalarSubquery; the chdb round-trip
// fixtures (clamp_min_scalar_bound.txtar & friends) pin the values.
func TestLower_InstantFn_ComputedScalarArgs(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})

	for _, q := range []string{
		`clamp_max(up, scalar(other))`,
		`clamp_min(up, scalar(other))`,
		`clamp(up, scalar(other), 1)`,
		`clamp(up, 0, scalar(other) * 2)`,
		`round(temperature, scalar(other))`,
		`histogram_quantile(scalar(other), foo_bucket)`,
		`vector(scalar(up))`,
		`quantile(scalar(up), up)`,
	} {
		expr, err := p.ParseExpr(q)
		if err != nil {
			t.Fatalf("ParseExpr(%q): %v", q, err)
		}
		if _, err := promql.Lower(context.Background(), expr, s); err != nil {
			t.Fatalf("Lower(%q): %v", q, err)
		}
	}
}
