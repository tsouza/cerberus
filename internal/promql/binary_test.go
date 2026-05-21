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

// TestLower_Binary_VectorSetOps covers the happy path for the vector
// set operators (`and` / `or` / `unless`). Each shape lowers to a
// chplan.VectorSetOp; the chsql emitter renders the right
// IN / NOT IN / UNION ALL shape.
//
// Pure scalar-scalar is folded at lowering time and emits a synthetic
// 1-row vector — see TestLower_Binary_ScalarOnly_Folds.
//
// group_left / group_right are honoured for arithmetic / comparison
// V-V binops — see vector_match_test.go. The `bool` modifier on
// vector-vector binops is exercised by TestLower_Binary_VV_Bool and
// the dedicated bool_vv_*.txtar fixtures.
func TestLower_Binary_VectorSetOps(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})

	cases := []struct {
		name      string
		query     string
		wantInSQL []string
	}{
		{
			name:      "and default match",
			query:     `up and up`,
			wantInSQL: []string{"`Attributes` IN (", "DISTINCT `Attributes`"},
		},
		{
			name:      "unless default match",
			query:     `up unless up`,
			wantInSQL: []string{"`Attributes` NOT IN (", "DISTINCT `Attributes`"},
		},
		{
			name:      "or default match",
			query:     `up or up`,
			wantInSQL: []string{"UNION ALL", "NOT IN ("},
		},
		{
			name:      "and ignoring",
			query:     `up and ignoring(instance) up`,
			wantInSQL: []string{"mapFilter((k, v) -> NOT (k IN (?)), `Attributes`) IN ("},
		},
		{
			name:      "unless on",
			query:     `up unless on(job) up`,
			wantInSQL: []string{"mapFilter((k, v) -> k IN (?), `Attributes`) NOT IN ("},
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

// TestLower_Binary_VectorSetOps_RejectsBool covers the parser-level
// guard: the `bool` modifier is only allowed on comparison ops; set
// ops should reject it with the same wording as the arithmetic V-V
// path.
func TestLower_Binary_VectorSetOps_RejectsBool(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})

	// The PromQL parser doesn't itself reject `up and bool up` —
	// it's a semantic error caught at lowering. We hand-craft the
	// BinaryExpr to bypass any parser-level guard so the lowering
	// path can be exercised in isolation.
	expr, err := p.ParseExpr(`up and up`)
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	be, ok := expr.(*parser.BinaryExpr)
	if !ok {
		t.Fatalf("expected *parser.BinaryExpr, got %T", expr)
	}
	be.ReturnBool = true
	_, err = promql.Lower(context.Background(), be, s)
	if err == nil {
		t.Fatal("expected error for set op + bool, got nil")
	}
	want := "'bool' modifier is only allowed on comparison binary ops"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not contain %q", err.Error(), want)
	}
}

// TestLower_Binary_ScalarOnly_Folds end-to-end checks the synthetic
// 1-row vector path: a pure scalar-scalar BinaryExpr folds to a single
// literal at lowering time and the emitted SQL has the folded value
// bound as the Value column on a no-FROM SELECT.
func TestLower_Binary_ScalarOnly_Folds(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})

	cases := []struct {
		name      string
		query     string
		wantInSQL []string
	}{
		{
			name:      "simple add",
			query:     `1 + 2`,
			wantInSQL: []string{"SELECT 1", "AS `Value`"},
		},
		{
			name:      "precedence",
			query:     `1 + 2 * 3`,
			wantInSQL: []string{"SELECT 1", "AS `Value`"},
		},
		{
			name:      "parens",
			query:     `(1 + 2) * (3 + 4)`,
			wantInSQL: []string{"SELECT 1", "AS `Value`"},
		},
		{
			name:      "bool eq",
			query:     `1 == bool 2`,
			wantInSQL: []string{"SELECT 1", "AS `Value`"},
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
			wantInSQL: []string{"`Value` * toFloat64(?)", "AS `Value`"},
		},
		{
			name:      "scalar minus vector preserves order",
			query:     `100 - up`,
			wantInSQL: []string{"toFloat64(?) - `Value`"},
		},
		{
			name:      "vector div scalar",
			query:     `metric / 1000`,
			wantInSQL: []string{"`Value` / toFloat64(?)"},
		},
		{
			name:      "negated scalar unwraps",
			query:     `up * -1`,
			wantInSQL: []string{"`Value` * toFloat64(?)"},
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
