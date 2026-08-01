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
//
// A subquery whose inner grid is already pinned at lowering time (the
// epoch-aligned sub-step grid) is the one shape the two passes must both
// decline rather than agree on — see reanchorCase.pinnedInner.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/schema"
)

// reanchorCase is one subquery shape plus how the two re-anchor passes are
// expected to treat its inner spine.
type reanchorCase struct {
	// query lowers (in range mode) to an inner subquery plan.
	query string
	// pinnedInner marks a shape whose inner grid is fixed at LOWERING time
	// onto the absolute-epoch sub-step grid PromQL evaluates a subquery's
	// inner on. Neither pass may re-grid it: widenSubquerySpine carries no
	// arm for the node kinds that hold such a grid (RangeLWR / VectorJoin)
	// so it is a no-op, and ReanchorRange must fail closed with
	// ErrReanchorGridMismatch so the sharded-pushdown solver routes A
	// instead of re-gridding an epoch-aligned inner onto a shard's own
	// request grid. Shapes without it keep their grid unpinned for the
	// widen pass to fill, and the two passes must agree exactly.
	pinnedInner bool
}

func TestReanchorRange_EquivalentToWidenSubquerySpine(t *testing.T) {
	t.Parallel()

	cases := []reanchorCase{
		// Bare range-vector subquery inner (Identity matrix wrap).
		{query: `max_over_time(rate(demo_cpu[1m])[5m:30s])`},
		// *_over_time over a bare-selector subquery.
		{query: `avg_over_time(demo_mem[10m:1m])`},
		// Aggregate-over-subquery: Project[Aggregate[matrix]].
		{query: `max_over_time(sum by(job)(rate(demo_cpu[1m]))[5m:1m])`},
		// without(...) aggregate spine.
		{query: `min_over_time(avg without(instance)(rate(demo_cpu[1m]))[10m:2m])`},
		// topk-over-subquery: TopK[matrix].
		{query: `max_over_time(topk(3, rate(demo_cpu[1m]))[5m:1m])`},
		// Nested matrix spine: stacked RangeWindows whose grids widen
		// cumulatively.
		{query: `max_over_time(rate(demo_cpu[1m])[5m:30s])`},
		// Binary-inner subquery: the inner is evaluated per anchor on the
		// epoch-aligned sub-step grid, so its grid is pinned at lowering.
		{query: `max_over_time((demo_cpu * 2)[5m:1m])`, pinnedInner: true},
	}

	s := schema.DefaultOTelMetrics()
	driver := optimizer.Default()
	start := time.Unix(1_700_000_000, 0).UTC()
	end := start.Add(time.Hour)
	step := time.Minute

	for _, c := range cases {
		c := c
		q := c.query
		t.Run(q, func(t *testing.T) {
			t.Parallel()

			expr, err := parser.NewParser(parser.Options{EnableExperimentalFunctions: true}).ParseExpr(q)
			if err != nil {
				t.Fatalf("ParseExpr(%q): %v", q, err)
			}
			sub, ok := outerSubquery(expr)
			if !ok {
				t.Fatalf("query %q has no recognizable subquery inner", q)
			}

			// Lower the inner subquery plan in range mode — the exact node
			// lowerOuterRangeFnOverSubquery feeds to widenSubquerySpine.
			// lowerers must be normalized exactly as the real entry
			// points do: this test calls lowerSubquery directly, and a
			// subquery inner now lowers on the range-mode selector seam,
			// which dispatches through the strategy table.
			inner, err := lowerSubquery(sub, s, lowerCtx{
				start:    start,
				end:      end,
				step:     step,
				lowerers: RangeLowerers{}.withDefaults(),
			})
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
			if c.pinnedInner {
				// Both passes must leave a pinned inner grid alone —
				// widenSubquerySpine by having no arm for the node kinds
				// that carry it, ReanchorRange by failing closed.
				if !errors.Is(err, chplan.ErrReanchorGridMismatch) {
					t.Fatalf("ReanchorRange(%q) error = %v, want ErrReanchorGridMismatch", q, err)
				}
				if !widenClone.Equal(snapshot) {
					t.Fatalf("widenSubquerySpine re-gridded the pinned inner of %q", q)
				}
				return
			}
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
