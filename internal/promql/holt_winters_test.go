package promql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_HoltWinters_OK covers the happy-path lowering: an instant
// `double_exponential_smoothing(metric[range], sf, tf)` produces a RangeWindow with
// Func="holt_winters" and Scalars=[sf, tf].
func TestLower_HoltWinters_OK(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	expr, err := p.ParseExpr(`double_exponential_smoothing(http_requests_total[10m], 0.5, 0.1)`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := promql.Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	rw, ok := plan.(*chplan.RangeWindow)
	if !ok {
		t.Fatalf("expected *chplan.RangeWindow, got %T", plan)
	}
	if rw.Func != "holt_winters" {
		t.Fatalf("Func = %q, want holt_winters", rw.Func)
	}
	if len(rw.Scalars) != 2 || rw.Scalars[0] != 0.5 || rw.Scalars[1] != 0.1 {
		t.Fatalf("Scalars = %v, want [0.5, 0.1]", rw.Scalars)
	}

	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "arrayFold") {
		t.Fatalf("emitted SQL does not contain arrayFold:\n%s", sql)
	}
}

// TestLower_HoltWinters_Errors covers rejected shapes.
func TestLower_HoltWinters_Errors(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	cases := []struct {
		name    string
		query   string
		wantErr string
	}{
		{
			name:    "sf out of range (zero)",
			query:   `double_exponential_smoothing(up[5m], 0, 0.5)`,
			wantErr: "smoothing factor must be in (0, 1)",
		},
		{
			name:    "sf out of range (one)",
			query:   `double_exponential_smoothing(up[5m], 1, 0.5)`,
			wantErr: "smoothing factor must be in (0, 1)",
		},
		{
			name:    "tf out of range",
			query:   `double_exponential_smoothing(up[5m], 0.5, 1.2)`,
			wantErr: "trend factor must be in (0, 1)",
		},
		{
			name:    "non-scalar factor",
			query:   `double_exponential_smoothing(up[5m], scalar(other), 0.5)`,
			wantErr: "requires scalar-literal smoothing and trend factors",
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
