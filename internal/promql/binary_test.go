package promql_test

import (
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_Binary_Errors covers the cases lowerBinary rejects today —
// vector/vector (deferred to M1.6 vector matching), the `bool` modifier
// on comparisons, and pure scalar/scalar (deferred until scalars are
// first-class chplan nodes).
func TestLower_Binary_Errors(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})

	cases := []struct {
		name    string
		query   string
		wantErr string
	}{
		{
			name:    "vector OP vector deferred",
			query:   `up + up`,
			wantErr: "vector OP vector binary expressions require vector matching",
		},
		{
			name:    "scalar OP scalar deferred",
			query:   `1 + 2`,
			wantErr: "scalar-only binary expressions not yet lowered",
		},
		{
			name:    "logical and deferred",
			query:   `up and up`,
			wantErr: "binary op and not yet supported",
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

// TestLower_Binary_VectorScalar end-to-end checks the happy path: lowering
// produces a chplan with a Project node, and chsql.Emit produces SQL that
// references the schema's Value column with the scalar operation applied.
func TestLower_Binary_VectorScalar(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})

	cases := []struct {
		name      string
		query     string
		wantInSQL []string
	}{
		{
			name:      "vector times scalar",
			query:     `up * 2`,
			wantInSQL: []string{"`Value` * ?", "AS `Value`"},
		},
		{
			name:      "scalar minus vector preserves order",
			query:     `100 - up`,
			wantInSQL: []string{"? - `Value`"},
		},
		{
			name:      "vector div scalar",
			query:     `metric / 1000`,
			wantInSQL: []string{"`Value` / ?"},
		},
		{
			name:      "negated scalar unwraps",
			query:     `up * -1`,
			wantInSQL: []string{"`Value` * ?"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr: %v", err)
			}
			plan, err := promql.Lower(expr, s)
			if err != nil {
				t.Fatalf("Lower: %v", err)
			}
			sql, _, err := chsql.Emit(plan)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			for _, want := range tc.wantInSQL {
				if !strings.Contains(sql, want) {
					t.Errorf("expected SQL to contain %q; full SQL:\n%s", want, sql)
				}
			}
		})
	}
}
