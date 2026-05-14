package promql_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestSyntheticFold_TimeTime_Instant pins the lowering+emit shape for
// `time() OP time()` and the bool-comparison variants in instant mode.
// Both legs of these binops are synthetic-scalar plans (Project over
// OneRow with empty MetricName / Attributes); the V-V VectorJoin path
// collapses each leg to one row via argMax and joins on
// `(MetricName, Attributes)`, leaving 1 × 1 = 1 row in range mode
// instead of Prom's N rows per step. The fix detects synthetic-on-both
// at lowering time and folds to a single Project that skips the join
// entirely.
//
// The instant-mode assertions here check the output SQL has no INNER
// JOIN (`AS L INNER JOIN`) and surfaces a single FROM (SELECT 1)
// source — that's the proxy for "the fold fired and the join was
// elided". Range-mode end-to-end coverage lives in the chDB step-loop
// test under internal/api/prom.
func TestSyntheticFold_TimeTime_Instant(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})
	instant := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

	cases := []struct {
		name  string
		query string
	}{
		// 6 arithmetic.
		{"add", "time() + time()"},
		{"sub", "time() - time()"},
		{"mul", "time() * time()"},
		{"div", "time() / time()"},
		{"mod", "time() % time()"},
		{"pow", "time() ^ time()"},
		// 6 bool comparisons.
		{"eq_bool", "time() == bool time()"},
		{"ne_bool", "time() != bool time()"},
		{"lt_bool", "time() < bool time()"},
		{"le_bool", "time() <= bool time()"},
		{"gt_bool", "time() > bool time()"},
		{"ge_bool", "time() >= bool time()"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr: %v", err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, instant, instant)
			if err != nil {
				t.Fatalf("LowerAt: %v", err)
			}
			sql, _, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit: %v", err)
			}
			if strings.Contains(sql, "INNER JOIN") {
				t.Fatalf("expected synthetic-fold to elide the V-V join; SQL still contains INNER JOIN:\n%s", sql)
			}
			if !strings.Contains(sql, "FROM (SELECT 1)") {
				t.Fatalf("expected folded plan to render over (SELECT 1); SQL:\n%s", sql)
			}
		})
	}
}

// TestSyntheticFold_TimeTime_Range pins the range-mode shape: the
// folded plan must thread through to a StepGrid source so each step
// in [start, end] emits its own row, instead of the 1-row collapse
// the VectorJoin path produced pre-fix.
func TestSyntheticFold_TimeTime_Range(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)
	step := 30 * time.Second

	expr, err := p.ParseExpr("time() + time()")
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := promql.LowerAtRange(context.Background(), expr, s, start, end, step)
	if err != nil {
		t.Fatalf("LowerAtRange: %v", err)
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if strings.Contains(sql, "INNER JOIN") {
		t.Fatalf("expected synthetic-fold to elide the V-V join in range mode; SQL still contains INNER JOIN:\n%s", sql)
	}
	// StepGrid emits a per-step `anchor_ts` column.
	if !strings.Contains(sql, "anchor_ts") {
		t.Fatalf("expected range-mode plan to reference anchor_ts; SQL:\n%s", sql)
	}
}

// TestSyntheticFold_TimeArith_LiteralAndStep keeps the existing
// time()-vs-literal binop path (lowerVectorScalar) byte-stable: the
// fold predicate must not misfire for vector-scalar shapes (`1 +
// time()`, `time() + 1`), which already work via the literal-scalar
// fold one level up.
func TestSyntheticFold_TimeArith_LiteralAndStep(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{})
	instant := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

	// `1 + time()` lowers via lowerVectorScalar; we don't want
	// the synthetic-fold to interfere with that path (lhs is a
	// scalar, not a synthetic vector).
	expr, err := p.ParseExpr("1 + time()")
	if err != nil {
		t.Fatalf("ParseExpr: %v", err)
	}
	plan, err := promql.LowerAt(context.Background(), expr, s, instant, instant)
	if err != nil {
		t.Fatalf("LowerAt: %v", err)
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	// Sanity: existing lowering path produced a single SELECT
	// without an INNER JOIN.
	if strings.Contains(sql, "INNER JOIN") {
		t.Fatalf("`1 + time()` regression: expected no INNER JOIN; SQL:\n%s", sql)
	}
}
