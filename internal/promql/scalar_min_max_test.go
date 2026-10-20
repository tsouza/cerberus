package promql

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/schema"
)

// parseExperimental parses q with the experimental functions enabled, as
// the HTTP handler's parser does; min_of and max_of are experimental.
func parseExperimental(t *testing.T, q string) parser.Expr {
	t.Helper()
	expr, err := parser.NewParser(parser.Options{EnableExperimentalFunctions: true}).ParseExpr(q)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", q, err)
	}
	return expr
}

// TestTryFoldScalar_MinOfMaxOf pins the literal fold against math.Min and
// math.Max, including the cases where ClickHouse's least / greatest
// disagree with them: NaN against a finite value, NaN against the
// dominating infinity, and the sign of zero.
func TestTryFoldScalar_MinOfMaxOf(t *testing.T) {
	t.Parallel()
	negZero := math.Copysign(0, -1)
	for _, tc := range []struct {
		query string
		want  float64
	}{
		{"min_of(1, 2)", 1},
		{"max_of(1, 2)", 2},
		{"min_of(2, -3)", -3},
		{"max_of(1+1, 2*3)", 6},
		{"min_of(NaN, 1)", math.NaN()},
		{"max_of(1, NaN)", math.NaN()},
		{"max_of(+Inf, NaN)", math.Inf(1)},
		{"min_of(NaN, -Inf)", math.Inf(-1)},
		{"max_of(NaN, -Inf)", math.NaN()},
		{"min_of(NaN, +Inf)", math.NaN()},
		{"min_of(0, -0)", negZero},
		{"max_of(-0, 0)", 0},
		{"max_of(-0, -0)", negZero},
		{"min_of(max_of(1, 5), pi())", math.Pi},
	} {
		got, ok := TryFoldScalar(parseExperimental(t, tc.query))
		if !ok {
			t.Fatalf("TryFoldScalar(%q) did not fold", tc.query)
		}
		if math.Float64bits(got) != math.Float64bits(tc.want) && !(math.IsNaN(got) && math.IsNaN(tc.want)) {
			t.Fatalf("TryFoldScalar(%q) = %v (bits %x), want %v (bits %x)",
				tc.query, got, math.Float64bits(got), tc.want, math.Float64bits(tc.want))
		}
	}
}

// TestTryFoldScalar_MinOfMaxOfComputedDoesNotFold pins that a computed
// argument leaves the call for ClickHouse to evaluate.
func TestTryFoldScalar_MinOfMaxOfComputedDoesNotFold(t *testing.T) {
	t.Parallel()
	for _, q := range []string{"min_of(scalar(up), 1)", "max_of(1, time())"} {
		if v, ok := TryFoldScalar(parseExperimental(t, q)); ok {
			t.Fatalf("TryFoldScalar(%q) folded to %v; a computed argument must be lowered", q, v)
		}
	}
}

// TestLowerMinOfMaxOf_ComputedArgumentsEmitTheGoSpecialCases lowers each
// position a scalar call can take and checks that a computed argument
// reaches the SQL through the expression that reproduces math.Min /
// math.Max (NaN checks, the signed-zero negation for min_of, least /
// greatest), bound once through arrayMap, and that the scalar subquery
// appears once per argument rather than once per read.
func TestLowerMinOfMaxOf_ComputedArgumentsEmitTheGoSpecialCases(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)
	for _, tc := range []struct {
		query string
		pick  string
		step  time.Duration
		// scalarReads is how often the SQL evaluates scalar(bound): once
		// per occurrence in the query, except that clamp's computed-bound
		// path reads its bound twice (isNaN(bound) and least(v, bound)).
		scalarReads int
	}{
		{"min_of(scalar(bound), 45)", "least(", 0, 1},
		{"max_of(scalar(bound), 45)", "greatest(", 0, 1},
		{"max_of(scalar(bound), 45)", "greatest(", time.Minute, 1},
		{"clamp_max(cpu, max_of(scalar(bound), 45))", "greatest(", 0, 2},
		{"cpu * min_of(time(), scalar(bound))", "least(", 0, 1},
		{"vector(min_of(scalar(bound), 1))", "least(", time.Minute, 1},
	} {
		expr := parseExperimental(t, tc.query)
		start := at
		if tc.step > 0 {
			start = at.Add(-5 * time.Minute)
		}
		plan, err := LowerAtRange(context.Background(), expr, schema.DefaultOTelMetrics(), start, at, tc.step)
		if err != nil {
			t.Fatalf("LowerAtRange(%q, step=%s): %v", tc.query, tc.step, err)
		}
		sql, _, err := chsql.Emit(context.Background(), plan)
		if err != nil {
			t.Fatalf("Emit(%q): %v", tc.query, err)
		}
		wants := []string{tc.pick, "isNaN(", "arrayMap(", "_cerb_mma"}
		if tc.pick == "least(" {
			wants = append(wants, "* toFloat64(?)")
		}
		for _, want := range wants {
			if !strings.Contains(sql, want) {
				t.Fatalf("SQL for %q (step=%s) lacks %q:\n%s", tc.query, tc.step, want, sql)
			}
		}
		if n := strings.Count(sql, "`_cerb_scnt` = ?") + strings.Count(sql, "_cerb_sks"); n < 1 {
			t.Fatalf("SQL for %q (step=%s) never evaluates scalar(bound):\n%s", tc.query, tc.step, sql)
		} else if tc.step == 0 && n != tc.scalarReads {
			t.Fatalf("SQL for %q evaluates scalar(bound) %d times, want %d:\n%s", tc.query, n, tc.scalarReads, sql)
		}
	}
}

// TestLowerMinOfMaxOf_LiteralArgumentsFold pins that two literal
// arguments reach the plan as the folded constant, exactly as `1+1` does.
func TestLowerMinOfMaxOf_LiteralArgumentsFold(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)
	plan, err := LowerAt(context.Background(), parseExperimental(t, "cpu * min_of(2, 3)"), schema.DefaultOTelMetrics(), at, at)
	if err != nil {
		t.Fatal(err)
	}
	sql, _, err := chsql.Emit(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{"least(", "greatest(", "isNaN("} {
		if strings.Contains(sql, absent) {
			t.Fatalf("literal min_of(2, 3) was not folded; SQL contains %q:\n%s", absent, sql)
		}
	}
}
