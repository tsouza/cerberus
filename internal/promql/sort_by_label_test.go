package promql_test

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// parseExprExp parses a PromQL expression with experimental functions
// enabled (sort_by_label / sort_by_label_desc are experimental in the
// reference parser).
func parseExprExp(t *testing.T, q string) parser.Expr {
	t.Helper()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
	expr, err := p.ParseExpr(q)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", q, err)
	}
	return expr
}

// TestLowerSortByLabel_OrderKeys pins the lowering shape: sort_by_label
// lowers to a chplan.OrderBy whose keys are the named label-value
// expressions (MapAccess on the Attributes column) in arg order, all
// ASC; sort_by_label_desc is identical but DESC. Later labels become
// additional tie-breaker keys. This is the pure-Go pin that runs in the
// default `check` lane (no chDB); the order-correctness against
// reference Prometheus is pinned by the chdb-tagged parity test.
func TestLowerSortByLabel_OrderKeys(t *testing.T) {
	s := schema.DefaultOTelMetrics()

	cases := []struct {
		name     string
		query    string
		wantDesc bool
		wantKeys []string // label names, in ORDER BY slot order
	}{
		{"asc_single", `sort_by_label(http_requests, "handler")`, false, []string{"handler"}},
		{"desc_single", `sort_by_label_desc(http_requests, "handler")`, true, []string{"handler"}},
		{"asc_variadic", `sort_by_label(http_requests, "handler", "method", "code")`, false, []string{"handler", "method", "code"}},
		{"desc_variadic", `sort_by_label_desc(http_requests, "handler", "method")`, true, []string{"handler", "method"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expr := parseExprExp(t, tc.query)
			plan, err := promql.Lower(context.Background(), expr, s)
			if err != nil {
				t.Fatalf("Lower(%q): %v", tc.query, err)
			}
			ob, ok := plan.(*chplan.OrderBy)
			if !ok {
				t.Fatalf("Lower(%q): top node = %T, want *chplan.OrderBy", tc.query, plan)
			}
			if len(ob.Keys) != len(tc.wantKeys) {
				t.Fatalf("Lower(%q): got %d order keys, want %d", tc.query, len(ob.Keys), len(tc.wantKeys))
			}
			for i, wantLabel := range tc.wantKeys {
				k := ob.Keys[i]
				if k.Desc != tc.wantDesc {
					t.Errorf("key[%d]: Desc=%v, want %v", i, k.Desc, tc.wantDesc)
				}
				ma, ok := k.Expr.(*chplan.MapAccess)
				if !ok {
					t.Fatalf("key[%d]: Expr=%T, want *chplan.MapAccess", i, k.Expr)
				}
				keyLit, ok := ma.Key.(*chplan.LitString)
				if !ok {
					t.Fatalf("key[%d]: MapAccess.Key=%T, want *chplan.LitString", i, ma.Key)
				}
				if keyLit.V != wantLabel {
					t.Errorf("key[%d]: label=%q, want %q", i, keyLit.V, wantLabel)
				}
				col, ok := ma.Map.(*chplan.ColumnRef)
				if !ok || col.Name != s.AttributesColumn {
					t.Errorf("key[%d]: MapAccess.Map=%v, want ColumnRef(%q)", i, ma.Map, s.AttributesColumn)
				}
			}
		})
	}
}

// TestLowerSortByLabel_MetricNameKey pins that a `__name__` sort label
// resolves to the dedicated MetricName column (a bare ColumnRef), not a
// MapAccess on Attributes — matching reference Prometheus, which sorts
// on the series' metric name.
func TestLowerSortByLabel_MetricNameKey(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	expr := parseExprExp(t, `sort_by_label(http_requests, "__name__")`)
	plan, err := promql.Lower(context.Background(), expr, s)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	ob, ok := plan.(*chplan.OrderBy)
	if !ok || len(ob.Keys) != 1 {
		t.Fatalf("plan = %T with keys; want OrderBy with 1 key", plan)
	}
	col, ok := ob.Keys[0].Expr.(*chplan.ColumnRef)
	if !ok || col.Name != s.MetricNameColumn {
		t.Errorf("key expr = %v, want ColumnRef(%q)", ob.Keys[0].Expr, s.MetricNameColumn)
	}
}

// TestLowerSortByLabel_Errors pins the argument-shape rejections.
func TestLowerSortByLabel_Errors(t *testing.T) {
	s := schema.DefaultOTelMetrics()
	cases := []struct {
		name    string
		query   string
		wantSub string
	}{
		{"no_label", `sort_by_label(http_requests)`, "at least 2 arguments"},
		{"no_label_desc", `sort_by_label_desc(http_requests)`, "at least 2 arguments"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expr := parseExprExp(t, tc.query)
			_, err := promql.Lower(context.Background(), expr, s)
			if err == nil {
				t.Fatalf("Lower(%q): want error, got nil", tc.query)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("Lower(%q): err=%q, want substring %q", tc.query, err, tc.wantSub)
			}
		})
	}
}
