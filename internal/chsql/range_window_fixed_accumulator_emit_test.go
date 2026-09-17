package chsql_test

import (
	"context"
	"strings"
	"testing"
	"time"

	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// fixedAccumCounterDeltaShape is the reset-corrected counter delta the
// fixed-accumulator rate/increase emitter hands extrapolatedValueExpr as
// ONE operand of its `*` (the raw-result multiplication) and `/` (the
// zero-crossing clamp's denominator). Both bind tighter than the `+`
// inside, so the whole term has to arrive parenthesised — bare, it
// re-associates into `last_val - first_val + reset_sum * factor` and the
// extrapolation factor silently drops off the reset-corrected term.
const fixedAccumCounterDeltaShape = "((last_val - first_val) + `reset_sum`) *"

// TestFixedAccumulator_CounterDeltaIsParenthesised renders the
// fixed-accumulator rate and increase shapes over a gauge-table series (no
// temporality column, so the delta is embedded bare rather than inside an
// if(...) call) and pins the parenthesised counter delta in the emitted
// SQL, with the render-time precedence guard installed so any other
// operand the shape re-associates is reported as well. The corpora never
// render this strategy (its fixtures are chdb-only), so this is the
// untagged pin for it.
func TestFixedAccumulator_CounterDeltaIsParenthesised(t *testing.T) {
	report := chsql.InstallPrecedenceGuardForTest()
	p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, query string
		lowerers    promql.RangeLowerers
	}{
		{
			name: "rate", query: "rate(cpu_temp_celsius[5m])",
			lowerers: promql.RangeLowerers{Rate: promql.FixedAccumulatorRateLowerer{Fallback: promql.FanoutRateLowerer{}}},
		},
		{
			name: "increase", query: "increase(cpu_temp_celsius[5m])",
			lowerers: promql.RangeLowerers{Increase: promql.FixedAccumulatorIncreaseLowerer{Fallback: promql.FanoutIncreaseLowerer{}}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			plan, err := promql.LowerAtRangeOpts(context.Background(), expr, schema.DefaultOTelMetrics(),
				start, start.Add(5*time.Minute), 30*time.Second, promql.LowerOpts{Lowerers: tc.lowerers})
			if err != nil {
				t.Fatalf("lower: %v", err)
			}
			plan = optimizer.Default().Run(context.Background(), plan)
			sqlStr, _, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("emit: %v", err)
			}
			if !strings.Contains(sqlStr, "`last_val`") || strings.Contains(sqlStr, "`temporality`") {
				t.Fatalf("%s did not take the fixed-accumulator strategy on a temporality-less table:\n%s", tc.query, sqlStr)
			}
			if !strings.Contains(sqlStr, fixedAccumCounterDeltaShape) {
				t.Fatalf("%s: the reset-corrected counter delta must be parenthesised as an operand of `*`, want %q in:\n%s",
					tc.query, fixedAccumCounterDeltaShape, sqlStr)
			}
		})
	}
	if violations := report(); len(violations) > 0 {
		t.Fatalf("%d operand(s) ClickHouse would re-associate:\n%s", len(violations), strings.Join(violations, "\n"))
	}
}
