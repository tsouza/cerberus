package promql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLower_Binary_Errors covers the binary-expression shapes lowerBinary
// still rejects: pure scalar/scalar (deferred until scalars are first-class
// chplan nodes) and logical ops (`and`/`or`/`unless`, deferred to a later
// milestone).
//
// group_left / group_right are no longer rejected — RC2 cardinality-edge
// support added them with explicit cardinality enforcement; see
// vector_match_test.go for the lowering coverage. The `bool` modifier on
// vector-vector binops is also no longer rejected — see TestLower_Binary_VV_Bool
// below for the happy path, and the dedicated V-V `bool` TXTAR fixtures
// (bool_vv_*.txtar) for end-to-end SQL coverage.
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
			plan, err := promql.Lower(context.Background(), expr, s)
			if err != nil {
				t.Fatalf("Lower: %v", err)
			}
			sql, _, err := chsql.Emit(context.Background(), plan)
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

// TestLower_Binary_VV_Bool covers the happy path for the `bool` modifier
// on vector-vector comparison ops. The emitter must wrap the per-pair
// binary result in `toFloat64(...)` so non-matching pairs surface as 0.0
// rather than being dropped by the comparison.
//
// Non-comparison ops with `bool` come back as a clear "only allowed on
// comparison ops" error, matching Prometheus's parser-level guard.
func TestLower_Binary_VV_Bool(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})

	cases := []struct {
		name      string
		query     string
		wantInSQL []string
	}{
		{
			name:      "gt bool vv",
			query:     `up > bool up`,
			wantInSQL: []string{"toFloat64((L.`Value` > R.`Value`)) AS `Value`"},
		},
		{
			name:      "eq bool vv",
			query:     `up == bool up`,
			wantInSQL: []string{"toFloat64((L.`Value` = R.`Value`)) AS `Value`"},
		},
		{
			name:      "ne bool vv on labels",
			query:     `up != bool on(instance) up`,
			wantInSQL: []string{"toFloat64((L.`Value` != R.`Value`)) AS `Value`"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr: %v", err)
			}
			plan, err := promql.Lower(context.Background(), expr, s)
			if err != nil {
				t.Fatalf("Lower: %v", err)
			}
			sql, _, err := chsql.Emit(context.Background(), plan)
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
