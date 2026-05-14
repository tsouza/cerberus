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
		var plan chplan.Node
		if strings.Contains(query, "@ start()") || strings.Contains(query, "@ end()") {
			plan, err = promql.LowerAt(context.Background(), expr, s, start, end)
		} else {
			plan, err = promql.LowerAt(context.Background(), expr, s, instantEval, instantEval)
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
