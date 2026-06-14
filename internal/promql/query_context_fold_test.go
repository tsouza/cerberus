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

// queryContextParser enables experimental functions so the parser
// accepts the query-context functions (start/end/range/step are flagged
// Experimental in the upstream function table).
func queryContextParser() parser.Parser {
	return parser.NewParser(parser.Options{EnableExperimentalFunctions: true})
}

// TestQueryContextFold_Instant pins the instant-mode (start == end)
// fold for the four query-context functions. The reference engine
// (engine.foldQueryContextFunctions) folds:
//
//   - start() / end() → the eval timestamp in Unix seconds
//   - range()         → (end - start) seconds → 0 for an instant query
//   - step()          → 0 for an instant query (start == end)
//
// Cerberus folds these at lowering into a single synthetic scalar row
// (Project over OneRow), so the emitted SQL must render over the
// `(SELECT 1)` OneRow source with no series scan and no range-window
// machinery. The folded value appears as a bound `toFloat64(?)` arg.
func TestQueryContextFold_Instant(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := queryContextParser()
	// 2026-01-01T00:00:01Z = 1767225601.
	instant := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)
	const instantEpoch = 1767225601.0

	cases := []struct {
		name  string
		query string
		want  float64
	}{
		{"start", "start()", instantEpoch},
		{"end", "end()", instantEpoch},
		{"range", "range()", 0},
		{"step", "step()", 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			plan, err := promql.LowerAt(context.Background(), expr, s, instant, instant)
			if err != nil {
				t.Fatalf("LowerAt(%q): %v", tc.query, err)
			}
			sql, args, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit(%q): %v", tc.query, err)
			}
			// Synthetic-scalar instant shape: a OneRow source.
			if !strings.Contains(sql, "FROM (SELECT 1)") {
				t.Fatalf("%s: expected folded plan over (SELECT 1); SQL:\n%s", tc.query, sql)
			}
			// No data scan / range-window machinery in a pure fold.
			for _, banned := range []string{"otel_metrics", "arrayJoin", "INNER JOIN"} {
				if strings.Contains(sql, banned) {
					t.Fatalf("%s: folded plan must not reference %q; SQL:\n%s", tc.query, banned, sql)
				}
			}
			assertHasFloatArg(t, tc.query, args, tc.want)
		})
	}
}

// TestQueryContextFold_Range pins the range-mode (start != end) fold.
// The reference values over the window [2026-01-01 00:00:00,
// 00:05:00] with a 30s step are:
//
//   - start() → 1767225600 (00:00:00Z)
//   - end()   → 1767225900 (00:05:00Z)
//   - range() → 300
//   - step()  → 30
//
// In range mode the constant is fanned across a StepGrid (one row per
// anchor), so the emitted SQL must carry the arrayJoin step-grid source
// rather than the OneRow `(SELECT 1)`.
func TestQueryContextFold_Range(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := queryContextParser()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)
	step := 30 * time.Second

	cases := []struct {
		name  string
		query string
		want  float64
	}{
		{"start", "start()", 1767225600},
		{"end", "end()", 1767225900},
		{"range", "range()", 300},
		{"step", "step()", 30},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr, err := p.ParseExpr(tc.query)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", tc.query, err)
			}
			plan, err := promql.LowerAtRange(context.Background(), expr, s, start, end, step)
			if err != nil {
				t.Fatalf("LowerAtRange(%q): %v", tc.query, err)
			}
			sql, args, err := chsql.Emit(context.Background(), plan)
			if err != nil {
				t.Fatalf("Emit(%q): %v", tc.query, err)
			}
			// Range-mode synthetic shape: a StepGrid fans the constant
			// via arrayJoin over the [start, end] anchors.
			if !strings.Contains(sql, "arrayJoin") {
				t.Fatalf("%s: expected range-mode fold over a StepGrid (arrayJoin); SQL:\n%s", tc.query, sql)
			}
			if strings.Contains(sql, "otel_metrics") {
				t.Fatalf("%s: folded plan must not reference a data table; SQL:\n%s", tc.query, sql)
			}
			assertHasFloatArg(t, tc.query, args, tc.want)
		})
	}
}

// assertHasFloatArg asserts that the bound-args list carries the
// expected folded float value (the central Builder.Expr LitFloat path
// binds the value as a positional arg behind `toFloat64(?)`).
func assertHasFloatArg(t *testing.T, query string, args []any, want float64) {
	t.Helper()
	for _, a := range args {
		if f, ok := a.(float64); ok && f == want {
			return
		}
	}
	t.Fatalf("%s: expected folded value %v in bound args %v", query, want, args)
}
