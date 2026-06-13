package promql

// This test lives in internal/promql (NOT internal/chplan) for two
// reasons: (1) it must call the unexported widenSubquerySpine to pin
// ReanchorRange against the in-place mutator it generalizes; (2) it builds
// POST-OPTIMIZER plans, and chplan cannot import internal/optimizer (which
// imports chplan) without an import cycle.
//
// The contract: for every subquery shape widenSubquerySpine handles, run
// the optimizer over the lowered inner plan, then apply both
//   - widenSubquerySpine (mutates a clone in place), and
//   - chplan.ReanchorRange (returns a fresh deep copy),
// to the SAME [start, end] and assert the resulting geometries are Equal.
// Optimizer-substituted shapes are therefore what gets validated.

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/schema"
)

func TestReanchorRange_EquivalentToWidenSubquerySpine(t *testing.T) {
	t.Parallel()

	// Every query here lowers (in range mode) to an inner subquery plan
	// whose spine widenSubquerySpine walks: matrix RangeWindow, optionally
	// wrapped in Project / Aggregate / TopK / Filter by the aggregate /
	// topk / count_values lowerings, and nested matrix spines.
	queries := []string{
		// Bare range-vector subquery inner (Identity matrix wrap).
		`max_over_time(rate(demo_cpu[1m])[5m:30s])`,
		// *_over_time over a bare-selector subquery.
		`avg_over_time(demo_mem[10m:1m])`,
		// Aggregate-over-subquery: Project[Aggregate[matrix]].
		`max_over_time(sum by(job)(rate(demo_cpu[1m]))[5m:1m])`,
		// without(...) aggregate spine.
		`min_over_time(avg without(instance)(rate(demo_cpu[1m]))[10m:2m])`,
		// topk-over-subquery: TopK[matrix].
		`max_over_time(topk(3, rate(demo_cpu[1m]))[5m:1m])`,
		// Nested matrix spine: stacked RangeWindows whose grids widen
		// cumulatively.
		`max_over_time(rate(demo_cpu[1m])[5m:30s])`,
		// Binary-inner subquery (Identity wrap over a per-sample rewrite).
		`max_over_time((demo_cpu * 2)[5m:1m])`,
	}

	s := schema.DefaultOTelMetrics()
	driver := optimizer.Default()
	start := time.Unix(1_700_000_000, 0).UTC()
	end := start.Add(time.Hour)
	step := time.Minute

	for _, q := range queries {
		q := q
		t.Run(q, func(t *testing.T) {
			t.Parallel()

			expr, err := parser.ParseExpr(q)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", q, err)
			}
			sub, ok := outerSubquery(expr)
			if !ok {
				t.Fatalf("query %q has no recognizable subquery inner", q)
			}

			// Lower the inner subquery plan in range mode — the exact node
			// lowerOuterRangeFnOverSubquery feeds to widenSubquerySpine.
			inner, err := lowerSubquery(sub, s, lowerCtx{start: start, end: end, step: step})
			if err != nil {
				t.Fatalf("lowerSubquery(%q): %v", q, err)
			}

			// Optimize, so optimizer-substituted shapes are validated.
			optimized := driver.Run(context.Background(), inner)
			snapshot := chplan.CloneNode(optimized) // pre-pass reference

			// widenSubquerySpine is called with [start.Add(-sub.Range), end]
			// (lowerOuterRangeFnOverSubquery). Mirror that exactly.
			wStart := start.Add(-sub.Range)

			widenClone := chplan.CloneNode(optimized)
			widenSubquerySpine(widenClone, wStart, end)

			reanchored, err := chplan.ReanchorRange(optimized, wStart, end)
			if err != nil {
				t.Fatalf("ReanchorRange(%q): %v", q, err)
			}

			if !widenClone.Equal(reanchored) {
				t.Fatalf("ReanchorRange geometry differs from widenSubquerySpine for %q", q)
			}

			// ReanchorRange must not have mutated its input.
			if !optimized.Equal(snapshot) {
				t.Fatalf("ReanchorRange mutated its input plan for %q", q)
			}
		})
	}
}

// outerSubquery extracts the *parser.SubqueryExpr that the outer
// range-vector function wraps (e.g. the `[5m:30s]` subquery inside
// `max_over_time(...)`), or the top-level subquery itself.
func outerSubquery(expr parser.Expr) (*parser.SubqueryExpr, bool) {
	switch e := expr.(type) {
	case *parser.SubqueryExpr:
		return e, true
	case *parser.Call:
		for _, a := range e.Args {
			if sub, ok := outerSubquery(a); ok {
				return sub, true
			}
		}
	case *parser.ParenExpr:
		return outerSubquery(e.Expr)
	}
	return nil, false
}
