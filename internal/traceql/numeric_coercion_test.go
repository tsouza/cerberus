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

// TestNumericAttrCoercion pins the toFloat64(...) wrap that lower.go
// emits around FieldAccess children of arithmetic / numeric-comparison
// Binary nodes. SpanAttributes / ResourceAttributes are
// Map(String, String) in OTel-CH, so without the wrap CH rejects
// numeric-literal comparisons with NO_COMMON_TYPE
// ("there is no supertype for types String, UInt8").
//
// The test asserts the rendered SQL substring so the wrap can't
// regress without a visible golden bump, and keeps the negative cases
// (string equality, regex, AND-tree of string predicates) as anchors
// that the coercion only fires when the comparison peer is numeric.
func TestNumericAttrCoercion(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelTraces()

	cases := []struct {
		name       string
		query      string
		wantSubstr string
		// notSubstr is non-empty when the coercion must NOT fire — the
		// test asserts the rendered SQL doesn't contain toFloat64. This
		// pins the negative half of the rule (string equality leaves the
		// FieldAccess as a bare map lookup).
		notSubstr string
	}{
		{
			name:       "int_eq_wraps_field_access",
			query:      `{ .attempt = 1 }`,
			wantSubstr: "toFloat64OrNull(`SpanAttributes`[?]) = ?",
		},
		{
			name:       "int_ge_wraps_field_access",
			query:      `{ span.http.status_code >= 500 }`,
			wantSubstr: "toFloat64OrNull(`SpanAttributes`[?]) >= ?",
		},
		{
			name:       "float_gt_wraps_field_access",
			query:      `{ span.cpu.usage > 0.5 }`,
			wantSubstr: "toFloat64OrNull(`SpanAttributes`[?]) > toFloat64(?)",
		},
		{
			name:       "resource_attr_coerces",
			query:      `{ resource.replicas <= 5 }`,
			wantSubstr: "toFloat64OrNull(`ResourceAttributes`[?]) <= ?",
		},
		{
			name:       "arithmetic_add_coerces_both_sides",
			query:      `{ .a + .b > 10 }`,
			wantSubstr: "(toFloat64OrNull(`SpanAttributes`[?]) + toFloat64OrNull(`SpanAttributes`[?])) > ?",
		},
		{
			name:       "arithmetic_mul_with_literal_coerces_field",
			query:      `{ .a * 2 > 10 }`,
			wantSubstr: "(toFloat64OrNull(`SpanAttributes`[?]) * ?) > ?",
		},
		{
			name:      "string_eq_leaves_field_access_bare",
			query:     `{ resource.service.name = "frontend" }`,
			notSubstr: "toFloat64(",
		},
		{
			name:      "regex_match_leaves_field_access_bare",
			query:     `{ .service.name =~ "front.*" }`,
			notSubstr: "toFloat64(",
		},
		{
			name: "intrinsic_duration_not_double_wrapped",
			// Duration is already Int64 in OTel-CH; the coercion guard
			// (FieldAccess-only, not ColumnRef) keeps the intrinsic
			// untouched even when the comparison is numeric.
			query:      `{ duration > 100ms }`,
			wantSubstr: "`Duration` > ?",
			notSubstr:  "toFloat64(`Duration`",
		},
		{
			// Aggregate-input coercion: `max(span.foo)` against a
			// Map(String, String) attribute reads as String; without the
			// wrap CH rejects `max(String) > 100` with NO_COMMON_TYPE.
			// coerceMapNumericAggInput wraps the FieldAccess with
			// toFloat64OrZero so the aggregate sees a Float64.
			name:       "max_span_attr_wraps_aggregate_input",
			query:      `{} | max(span.latency_ms) > 100`,
			wantSubstr: "max(toFloat64OrZero(`SpanAttributes`[?]))",
		},
		{
			name:       "min_span_attr_wraps_aggregate_input",
			query:      `{} | min(span.attempts) < 3`,
			wantSubstr: "min(toFloat64OrZero(`SpanAttributes`[?]))",
		},
		{
			name:       "sum_span_attr_wraps_aggregate_input",
			query:      `{} | sum(span.size) > 1000`,
			wantSubstr: "sum(toFloat64OrZero(`SpanAttributes`[?]))",
		},
		{
			name:       "avg_resource_attr_wraps_aggregate_input",
			query:      `{} | avg(resource.replicas) > 1`,
			wantSubstr: "avg(toFloat64OrZero(`ResourceAttributes`[?]))",
		},
		{
			// Intrinsic duration aggregates lower to a bare ColumnRef so
			// the coercion guard leaves them untouched. The existing
			// avg_duration / metrics_max_over_time fixtures pin this
			// shape; the unit case anchors the negative half of the
			// aggregate-input rule.
			name:      "avg_duration_intrinsic_not_wrapped",
			query:     `{} | avg(duration) > 100ms`,
			notSubstr: "toFloat64OrZero(`Duration`",
		},
		{
			// metrics-pipeline aggregates take the same coercion path
			// via metricsAggregateAttr — `max_over_time(span.foo)` over
			// a Map(String, String) carrier needs the same wrap.
			name:       "max_over_time_span_attr_wraps_aggregate_input",
			query:      `{} | max_over_time(span.latency_ms)`,
			wantSubstr: "max(toFloat64OrZero(`SpanAttributes`[?]))",
		},
		{
			name:       "quantile_over_time_span_attr_wraps_aggregate_input",
			query:      `{} | quantile_over_time(span.latency_ms, 0.95)`,
			wantSubstr: "quantile(toFloat64(?))(toFloat64OrZero(`SpanAttributes`[?]))",
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
				t.Fatalf("Emit: %v", err)
			}
			if tc.wantSubstr != "" && !strings.Contains(sql, tc.wantSubstr) {
				t.Fatalf("SQL missing wanted substring\n  want: %s\n   sql: %s", tc.wantSubstr, sql)
			}
			if tc.notSubstr != "" && strings.Contains(sql, tc.notSubstr) {
				t.Fatalf("SQL contains forbidden substring\n  not: %s\n  sql: %s", tc.notSubstr, sql)
			}
		})
	}
}
