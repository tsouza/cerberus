package chsql_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	promparser "github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/test/spec"
)

// fixedAccumCounterDeltaShape is the reset-corrected counter delta the
// fixed-accumulator rate/increase emitter hands extrapolatedValueExpr as
// ONE operand of its `*` (the raw-result multiplication) and `/` (the
// zero-crossing clamp's denominator). Both bind tighter than the `+`
// inside, so the whole term has to arrive parenthesised — bare, it
// re-associates into `last_val - first_val + reset_sum * factor` and the
// extrapolation factor silently drops off the reset-corrected term.
const fixedAccumCounterDeltaShape = "((last_val - first_val) + `reset_sum`) *"

// fixedAccumulatorFixtures are the promql spec fixtures that opt into the
// fixed-accumulator strategy alone, unshadowed by the native ts_grid path.
// Each one is named here with the RangeLowerers its marker section selects
// in internal/promql's own TestLower (wireNativeStrategies), so this test
// lowers exactly what that lane lowers.
var fixedAccumulatorFixtures = []struct {
	name     string
	lowerers promql.RangeLowerers
}{
	{
		name:     "fixed_accumulator_rate_range_step",
		lowerers: promql.RangeLowerers{Rate: promql.FixedAccumulatorRateLowerer{Fallback: promql.FanoutRateLowerer{}}},
	},
	{
		name:     "fixed_accumulator_increase_range_step",
		lowerers: promql.RangeLowerers{Increase: promql.FixedAccumulatorIncreaseLowerer{Fallback: promql.FanoutIncreaseLowerer{}}},
	},
	{
		name:     "fixed_accumulator_delta_range_step",
		lowerers: promql.RangeLowerers{Delta: promql.FixedAccumulatorDeltaLowerer{Fallback: promql.FanoutDeltaLowerer{}}},
	},
}

// The deterministic range-mode window internal/promql's TestLower lowers a
// `range_step` fixture over; the fixtures' `-- sql --` goldens embed its
// literals, so this test has to use the same one.
var (
	fixedAccumRangeStart = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fixedAccumRangeEnd   = fixedAccumRangeStart.Add(5 * time.Minute)
)

// TestFixedAccumulator_HoldsToFixtureGoldens lowers the three
// fixed-accumulator fixtures the way internal/promql's TestLower does and
// holds the emitted SQL — pre-optimizer and optimized — to the fixtures'
// own `-- sql --` / `-- sql_optimized --` goldens, from THIS package.
//
// The promql lane already pins those goldens, but it does so from the
// promql package: a mutation run scoped to ./internal/chsql never executes
// it, and every other test in this package that reaches the fixed-
// accumulator emitter renders its SQL without holding it to a full
// expected string. This is the in-package pin that turns a wrong shape
// check, a wrong anchor count, or a wrong delta-prefix gate into a byte
// diff. It only ever COMPARES: regeneration belongs to the promql shard's
// TestLower, and a second writer of the same fixture from a package that
// `go test` runs concurrently would race it.
//
// The render-time precedence guard is installed around the walk. These
// fixtures scan a temporality-bearing table, so their counter delta sits
// inside an if(...) call whose own parentheses would mask a missing pair;
// TestFixedAccumulator_CounterDeltaIsParenthesised pins that operand
// (fixedAccumCounterDeltaShape) over a gauge table where it is bare.
func TestFixedAccumulator_HoldsToFixtureGoldens(t *testing.T) {
	report := chsql.InstallPrecedenceGuard()
	p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
	for _, tc := range fixedAccumulatorFixtures {
		t.Run(tc.name, func(t *testing.T) {
			c, err := spec.Load(filepath.Join("..", "..", "test", "spec", "promql", tc.name+".txtar"))
			if err != nil {
				t.Fatalf("load fixture: %v", err)
			}
			query, ok := c.Section("query.promql")
			if !ok {
				t.Fatalf("fixture %s has no query.promql section", c.Name)
			}
			rs, ok := c.Section("range_step")
			if !ok {
				t.Fatalf("fixture %s has no range_step section", c.Name)
			}
			step, err := time.ParseDuration(strings.TrimSpace(rs))
			if err != nil {
				t.Fatalf("parse range_step %q: %v", rs, err)
			}
			expr, err := p.ParseExpr(strings.TrimSpace(query))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			plan, err := promql.LowerAtRangeOpts(context.Background(), expr, spec.FixtureMetrics(),
				fixedAccumRangeStart, fixedAccumRangeEnd, step, promql.LowerOpts{Lowerers: tc.lowerers})
			if err != nil {
				t.Fatalf("lower: %v", err)
			}
			sqlStr, _, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("emit: %v", err)
			}
			optSQL, _, err := chsql.Emit(context.Background(), optimizer.Default().Run(context.Background(), plan))
			if err != nil {
				t.Fatalf("emit optimized: %v", err)
			}
			if !strings.Contains(sqlStr, "`last_val`") {
				t.Fatalf("%s did not take the fixed-accumulator strategy:\n%s", c.Name, sqlStr)
			}
			assertMatchesSection(t, c, "sql", sqlStr)
			assertMatchesSection(t, c, "sql_optimized", optSQL)
		})
	}
	if violations := report(); len(violations) > 0 {
		t.Fatalf("%d operand(s) ClickHouse would re-associate:\n%s", len(violations), strings.Join(violations, "\n"))
	}
}

// TestFixedAccumulator_CounterDeltaIsParenthesised renders the
// fixed-accumulator rate and increase shapes over a gauge-table series (no
// temporality column, so the delta is embedded bare rather than inside an
// if(...) call whose own parentheses would hide a missing pair) and pins
// the parenthesised counter delta in the emitted SQL, with the render-time
// precedence guard installed so any other operand the shape re-associates
// is reported as well.
func TestFixedAccumulator_CounterDeltaIsParenthesised(t *testing.T) {
	report := chsql.InstallPrecedenceGuard()
	p := promparser.NewParser(promparser.Options{EnableExperimentalFunctions: true})
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
			plan, err := promql.LowerAtRangeOpts(context.Background(), expr, spec.FixtureMetrics(),
				fixedAccumRangeStart, fixedAccumRangeEnd, 30*time.Second, promql.LowerOpts{Lowerers: tc.lowerers})
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

// assertMatchesSection holds got to the fixture's named golden section,
// comparing modulo trailing whitespace exactly as spec.Match does, but
// never rewriting the fixture.
func assertMatchesSection(t *testing.T, c *spec.Case, section, got string) {
	t.Helper()
	want, ok := c.Section(section)
	if !ok {
		t.Fatalf("fixture %s has no %s section", c.Name, section)
	}
	if strings.TrimSpace(got) != strings.TrimSpace(want) {
		t.Errorf("[%s] section %q differs from the fixture golden\n--- want ---\n%s\n--- got ---\n%s", c.Name, section, want, got)
	}
}
