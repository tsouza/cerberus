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

// TestLower_PredictLinear_OK covers the happy-path lowering: an
// instant `predict_linear(metric[range], t)` produces a RangeWindow
// with Func="predict_linear" and Scalars=[t].
func TestLower_PredictLinear_OK(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})

	expr, err := p.ParseExpr(`predict_linear(http_requests_total[5m], 3600)`)
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
	if rw.Func != "predict_linear" {
		t.Fatalf("Func = %q, want predict_linear", rw.Func)
	}
	if len(rw.Scalars) != 1 || rw.Scalars[0] != 3600 {
		t.Fatalf("Scalars = %v, want [3600]", rw.Scalars)
	}

	// Sanity check that the SQL emitter produces a non-empty,
	// `simpleLinearRegression`-flavoured query.
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if !strings.Contains(sql, "simpleLinearRegression") {
		t.Fatalf("emitted SQL does not contain simpleLinearRegression:\n%s", sql)
	}
}

// TestLower_PredictLinear_Errors covers the rejected shapes so the
// error messages stay observable as the surface grows.
func TestLower_PredictLinear_Errors(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})

	cases := []struct {
		name    string
		query   string
		wantErr string
	}{
		{
			name:    "missing second arg",
			query:   `predict_linear(http_requests_total[5m])`,
			wantErr: "expects 2 arguments",
		},
		{
			name:    "non-scalar horizon",
			query:   `predict_linear(http_requests_total[5m], scalar(other))`,
			wantErr: "requires a scalar-literal predict horizon",
		},
		{
			name:    "first arg must be range-vector",
			query:   `predict_linear(up, 60)`,
			wantErr: "must be a range-vector selector",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				// Some of these may fail at parse time (predict_linear
				// is a known function so the parser checks arity); treat
				// the parse error as the surfaced error.
				if !strings.Contains(err.Error(), tc.wantErr) &&
					!strings.Contains(err.Error(), "expected") {
					t.Fatalf("ParseExpr: %v", err)
				}
				return
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
