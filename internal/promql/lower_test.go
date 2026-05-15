package promql_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/chsql"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/test/spec"
)

var fixtureDir = filepath.Join("..", "..", "test", "spec", "promql")

// TestLower walks every *.txtar fixture under test/spec/promql/, parses the
// PromQL in `query.promql`, lowers it to a chplan, emits SQL, and asserts
// the `sql` + `args` sections match what's recorded.
//
// To add a new case: create test/spec/promql/<name>.txtar with a
// `-- query.promql --` section, then run `just update-golden`.
func TestLower(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	p := parser.NewParser(parser.Options{EnableExperimentalFunctions: true})

	// Fixed start/end used when a fixture's query contains `@ start()`
	// or `@ end()`. Picking small Unix-epoch values keeps the generated
	// SQL literals short and deterministic.
	const (
		fixtureStartUnix = int64(100)
		fixtureEndUnix   = int64(500)
	)
	start := time.Unix(fixtureStartUnix, 0).UTC()
	end := time.Unix(fixtureEndUnix, 0).UTC()

	// Every other fixture uses a deterministic instant-eval anchor
	// just after the seed timestamps (the round-trip fixtures all use
	// `toDateTime64('2026-01-01 00:00:00', 9)`). One second after that
	// keeps the seed inside the 5-minute LWR staleness window the
	// instant-selector lowering applies. This is what gets passed to
	// LowerAt as both start and end — instant queries have
	// start == end == eval_ts.
	instantEval := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)

	// Deterministic range-mode anchor used by fixtures that opt into the
	// `range_step` section. The window `[2026-01-01 00:00:00, 00:05:00]`
	// keeps the SQL literals short and the step grid deterministic.
	rangeStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rangeEnd := rangeStart.Add(5 * time.Minute)

	spec.Walk(t, fixtureDir, func(t *testing.T, c *spec.Case) {
		query, ok := c.Section("query.promql")
		if !ok {
			t.Fatalf("fixture %s missing 'query.promql' section", c.Name)
		}
		query = strings.TrimSpace(query)

		expr, err := p.ParseExpr(query)
		if err != nil {
			t.Fatalf("ParseExpr(%q): %v", query, err)
		}
		// Fixtures that exercise `@ start()` / `@ end()` need the
		// special fixed range; every other fixture lowers with the
		// deterministic instant-eval anchor so the LWR staleness
		// predicate produces stable SQL literals.
		//
		// A `range_step:` section opts the fixture into range mode: the
		// section value is a duration parsed by time.ParseDuration, and
		// the lowering uses LowerAtRange with the deterministic
		// rangeStart/rangeEnd window. This is the fixture-side mechanism
		// for exercising query_range lowerings (histogram_quantile per-
		// step anchor, etc.) without requiring per-test Go scaffolding.
		var plan chplan.Node
		switch {
		case strings.Contains(query, "@ start()") || strings.Contains(query, "@ end()"):
			plan, err = promql.LowerAt(context.Background(), expr, s, start, end)
		default:
			if rs, ok := c.Section("range_step"); ok {
				stepDur, perr := time.ParseDuration(strings.TrimSpace(rs))
				if perr != nil {
					t.Fatalf("fixture %s: parse range_step %q: %v", c.Name, rs, perr)
				}
				plan, err = promql.LowerAtRange(context.Background(), expr, s, rangeStart, rangeEnd, stepDur)
			} else {
				plan, err = promql.LowerAt(context.Background(), expr, s, instantEval, instantEval)
			}
		}
		if err != nil {
			t.Fatalf("Lower(%q): %v", query, err)
		}
		sql, args, err := chsql.Emit(context.Background(), plan)
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}

		spec.Match(t, c, map[string]string{
			"sql":    sql,
			"args":   formatArgs(args),
			"chplan": spec.PrintChplan(plan),
		})
	})
}

// Per-op error-path coverage lives in op-specific *_test.go files now —
// see aggregate_test.go (topk/bottomk/count_values argument shapes) and
// binary_test.go (BinaryExpr vector-vector rejection paths). The
// previously-tested "topk without" rejection no longer exists: as of
// the topk/bottomk without(...) lowering, `topk(K, v) without (l)`
// lowers into chplan.TopK with a MapWithoutKeys partition expression.

func formatArgs(args []any) string {
	if len(args) == 0 {
		return "(none)\n"
	}
	var b strings.Builder
	for i, a := range args {
		fmt.Fprintf(&b, "[%d] %T = %#v\n", i, a, a)
	}
	return b.String()
}
