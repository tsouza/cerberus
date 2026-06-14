package traceql_test

import (
	"context"
	"strings"
	"testing"

	tempo "github.com/grafana/tempo/pkg/traceql"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/traceql"
)

// TestLowerUnaryMinus pins the arithmetic-negation lowering
// (UnaryOperation{OpSub}). Reference Tempo (pkg/traceql
// ast_execute.go UnaryOperation.execute OpSub) negates a numeric
// operand and returns `-1 * n`; cerberus lowers it as `0 - <operand>`
// (chplan.Binary{OpSub}) so the existing arithmetic emit + numeric
// coercion paths apply. The parser constant-folds any non-span operand
// (newUnaryOperation), so every shape exercised here references a span.
//
// This is the non-chdb companion to the txtar roundtrip fixtures
// (test/spec/traceql/unary_minus_*.txtar) — those pin the
// RESULT against real ClickHouse execution; this pins the SQL SHAPE so
// a coercion or operator regression surfaces as a visible substring
// failure even when libchdb is unavailable.
func TestLowerUnaryMinus(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()

	cases := []struct {
		name       string
		query      string
		wantSubstr string
	}{
		{
			// Bare numeric attribute operand: the operand FieldAccess is
			// wrapped in toFloat64OrNull by the outer comparison's numeric
			// coercion recursing into the `0 - x` arithmetic Binary.
			name:       "bare_attr_negated_gt",
			query:      `{ -.payload_bytes > -100 }`,
			wantSubstr: "(? - toFloat64OrNull(`SpanAttributes`[?])) > ?",
		},
		{
			name:       "bare_attr_negated_lt",
			query:      `{ -.a < 5 }`,
			wantSubstr: "(? - toFloat64OrNull(`SpanAttributes`[?])) < ?",
		},
		{
			// Nested arithmetic operand: coercion recurses through both the
			// negation Binary and the inner `+` Binary, wrapping the leaf
			// FieldAccess.
			name:       "nested_arith_operand",
			query:      `{ -(.a + 1) < -10 }`,
			wantSubstr: "(? - (toFloat64OrNull(`SpanAttributes`[?]) + ?)) < ?",
		},
		{
			// Negated resource-scoped attribute resolves against the
			// ResourceAttributes carrier.
			name:       "resource_attr_negated",
			query:      `{ -resource.replicas > -5 }`,
			wantSubstr: "(? - toFloat64OrNull(`ResourceAttributes`[?])) > ?",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := tempo.Parse(tc.query)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.query, err)
			}
			plan, err := traceql.Lower(context.Background(), expr, s)
			if err != nil {
				t.Fatalf("Lower(%q): %v", tc.query, err)
			}
			sql, _, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit(%q): %v", tc.query, err)
			}
			if !strings.Contains(sql, tc.wantSubstr) {
				t.Fatalf("SQL missing wanted substring\n  want: %s\n   sql: %s", tc.wantSubstr, sql)
			}
		})
	}
}
