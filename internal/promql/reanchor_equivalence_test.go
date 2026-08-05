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
// epoch-aligned sub-step grid) is a shape widenSubquerySpine still declines
// (it carries no arm for RangeLWR / VectorJoin) — see reanchorCase.pinnedInner.
// ReanchorRange, on the production path, does not decline it: the solver's
// UnpinSpine zeroes the pinned grid before ReanchorRange ever sees it (see
// internal/solver.slice), and a StepAlign RangeLWR (#1556) re-derives its
// grid from each shard's own predicted bounds, floored to the same
// absolute-epoch phase every shard shares. pinnedInner exercises exactly
// that real sequence and checks the commensurate-grid property it exists
// to guarantee, rather than calling ReanchorRange directly on a still-
// pinned plan (which is not what production ever does).

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/optimizer"
	"github.com/tsouza/cerberus/internal/schema"
	"github.com/tsouza/cerberus/internal/solver"
)

// reanchorCase is one subquery shape plus how the two re-anchor passes are
// expected to treat its inner spine.
type reanchorCase struct {
	// query lowers (in range mode) to an inner subquery plan.
	query string
	// pinnedInner marks a shape whose inner grid is fixed at LOWERING time
	// onto the absolute-epoch sub-step grid PromQL evaluates a subquery's
	// inner on (chplan.RangeLWR.StepAlign). widenSubquerySpine carries no
	// arm for the node kinds that hold such a grid (RangeLWR / VectorJoin)
	// so it stays a no-op here regardless. ReanchorRange, called the way
	// production actually calls it — through internal/solver.UnpinSpine
	// first — re-anchors it per shard instead of failing closed (#1556);
	// this case asserts that sequence succeeds and produces commensurate
	// (same absolute-epoch phase) grids across independently re-anchored
	// shards. Shapes without pinnedInner keep their grid unpinned for the
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
		// Offset-carrying subquery (#1464): the outer reducer's own
		// `offset` modifier must widen the inner spine by Offset+Range,
		// not Range alone — this is what the RangeWindow arm of both
		// ReanchorRange and widenSubquerySpine silently dropped.
		{query: `max_over_time(rate(demo_cpu[1m])[5m:30s] offset 10m)`},
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

			// widenSubquerySpine is called with
			// [start.Add(-rw.Offset-sub.Range), end]
			// (lowerOuterRangeFnOverSubquery, #1464). rw.Offset is always
			// sub.OriginalOffset (subqueryAnchor sets it unconditionally).
			// Mirror that exactly.
			wStart := start.Add(-sub.OriginalOffset).Add(-sub.Range)

			widenClone := chplan.CloneNode(optimized)
			widenSubquerySpine(widenClone, wStart, end)

			if c.pinnedInner {
				// widenSubquerySpine still has no arm for RangeLWR /
				// VectorJoin — it must stay a no-op regardless of how
				// ReanchorRange itself is exercised below.
				if !widenClone.Equal(snapshot) {
					t.Fatalf("widenSubquerySpine re-gridded the pinned inner of %q", q)
				}

				// Run the REAL production sequence: internal/solver.slice
				// always unpins the spine (zeroing every windowed node's
				// Start/End) before calling ReanchorRange, once per shard.
				// Calling ReanchorRange directly on the still-pinned plan
				// (as this test did pre-#1556) never reaches that path and
				// so can't validate it.
				unpinned := solver.UnpinSpine(optimized)

				whole, err := chplan.ReanchorRange(unpinned, wStart, end)
				if err != nil {
					t.Fatalf("ReanchorRange(%q) via UnpinSpine, whole range: %v", q, err)
				}

				// Split [wStart, end] into two adjacent, non-overlapping
				// shards, mirroring what slice() hands ReanchorRange per
				// shard, and re-anchor each independently.
				mid := wStart.Add(end.Sub(wStart) / 2).Truncate(step)
				shardA, err := chplan.ReanchorRange(unpinned, wStart, mid)
				if err != nil {
					t.Fatalf("ReanchorRange(%q) shard A [%s,%s]: %v", q, wStart, mid, err)
				}
				shardB, err := chplan.ReanchorRange(unpinned, mid.Add(step), end)
				if err != nil {
					t.Fatalf("ReanchorRange(%q) shard B [%s,%s]: %v", q, mid.Add(step), end, err)
				}

				wholeLWR := findRangeLWR(t, whole, q)
				shardALWR := findRangeLWR(t, shardA, q)
				shardBLWR := findRangeLWR(t, shardB, q)

				// Every re-anchored grid must sit on the SAME
				// absolute-epoch (phase 0) floor — the property #1556 is
				// about. Before the fix, a shard's grid was floored
				// against that shard's OWN End, so different shards (and
				// the unsliced whole) landed on different phases.
				for name, r := range map[string]*chplan.RangeLWR{"whole": wholeLWR, "shard A": shardALWR, "shard B": shardBLWR} {
					if !r.End.Equal(epochFloor(r.End, r.Step)) {
						t.Errorf("%s: RangeLWR.End %s is not phase-0 aligned to step %s for %q", name, r.End, r.Step, q)
					}
				}

				// Per-shard anchor-set equality: every anchor a shard's
				// re-anchored grid produces must appear, at the exact same
				// timestamp, in the unsliced whole's grid. A phase-drifted
				// shard would produce anchors the whole grid never has —
				// exactly the incommensurate-grid bug #1556 fixes.
				wholeSet := make(map[time.Time]bool, len(lwrAnchors(wholeLWR)))
				for _, ts := range lwrAnchors(wholeLWR) {
					wholeSet[ts] = true
				}
				for _, ts := range lwrAnchors(shardALWR) {
					if !wholeSet[ts] {
						t.Errorf("shard A anchor %s absent from the whole grid (phase drift) for %q", ts, q)
					}
				}
				for _, ts := range lwrAnchors(shardBLWR) {
					if !wholeSet[ts] {
						t.Errorf("shard B anchor %s absent from the whole grid (phase drift) for %q", ts, q)
					}
				}
				return
			}

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

// TestReanchorRange_StepAlignCommensurateAcrossNonEpochAlignedEnd pins the
// #1556 fix against a request END that is deliberately NOT a multiple of
// the subquery inner's step from absolute epoch. This matters because the
// TXTAR spec corpus's shared grid end (2026-01-01T00:00:00Z, exactly
// midnight) sits ON that phase-0 grid for any reasonable step — an
// unfloored raw End and its floored twin are identical when the raw value
// already happens to be on-grid, so that corpus cannot discriminate a
// broken floor from a correct one. An end with an arbitrary phase (chosen
// below for its odd hour/minute/second) is exactly the shape that exposes
// drift: pre-#1556, ReanchorRange assigned a shard's raw End verbatim to
// the RangeLWR grid, so two shards computed against different raw Ends
// landed on different phases and produced anchor sets that don't tile the
// unsliced grid.
func TestReanchorRange_StepAlignCommensurateAcrossNonEpochAlignedEnd(t *testing.T) {
	t.Parallel()

	const q = `max_over_time((demo_cpu * 2)[5m:1m])`
	s := schema.DefaultOTelMetrics()
	driver := optimizer.Default()

	// end is deliberately off-grid: 26s past the minute, so it aligns to
	// neither the subquery's 1m sub-step nor the outer step below.
	end := time.Date(2026, 3, 14, 15, 9, 26, 0, time.UTC)
	step := time.Minute
	start := end.Add(-time.Hour)

	expr, err := parser.NewParser(parser.Options{EnableExperimentalFunctions: true}).ParseExpr(q)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", q, err)
	}
	sub, ok := outerSubquery(expr)
	if !ok {
		t.Fatalf("query %q has no recognizable subquery inner", q)
	}

	inner, err := lowerSubquery(sub, s, lowerCtx{
		start:    start,
		end:      end,
		step:     step,
		lowerers: RangeLowerers{}.withDefaults(),
	})
	if err != nil {
		t.Fatalf("lowerSubquery(%q): %v", q, err)
	}
	optimized := driver.Run(context.Background(), inner)

	// Mirrors lowerOuterRangeFnOverSubquery's own widening (#1464).
	wStart := start.Add(-sub.OriginalOffset).Add(-sub.Range)
	unpinned := solver.UnpinSpine(optimized)

	whole, err := chplan.ReanchorRange(unpinned, wStart, end)
	if err != nil {
		t.Fatalf("ReanchorRange(%q) whole range: %v", q, err)
	}

	// Two adjacent, non-overlapping shards, each with its own (equally
	// off-grid) End, mirroring what internal/solver.slice() would hand
	// ReanchorRange per shard.
	mid := wStart.Add(end.Sub(wStart) / 2)
	shardA, err := chplan.ReanchorRange(unpinned, wStart, mid)
	if err != nil {
		t.Fatalf("ReanchorRange(%q) shard A [%s,%s]: %v", q, wStart, mid, err)
	}
	shardB, err := chplan.ReanchorRange(unpinned, mid.Add(step), end)
	if err != nil {
		t.Fatalf("ReanchorRange(%q) shard B [%s,%s]: %v", q, mid.Add(step), end, err)
	}

	wholeLWR := findRangeLWR(t, whole, q)
	shardALWR := findRangeLWR(t, shardA, q)
	shardBLWR := findRangeLWR(t, shardB, q)

	for name, r := range map[string]*chplan.RangeLWR{"whole": wholeLWR, "shard A": shardALWR, "shard B": shardBLWR} {
		if !r.End.Equal(epochFloor(r.End, r.Step)) {
			t.Errorf("%s: RangeLWR.End %s is not phase-0 aligned to step %s (request end %s is off-grid)", name, r.End, r.Step, end)
		}
	}

	wholeSet := make(map[time.Time]bool, len(lwrAnchors(wholeLWR)))
	for _, ts := range lwrAnchors(wholeLWR) {
		wholeSet[ts] = true
	}
	for _, ts := range lwrAnchors(shardALWR) {
		if !wholeSet[ts] {
			t.Errorf("shard A anchor %s absent from the whole grid (phase drift against off-grid end %s)", ts, end)
		}
	}
	for _, ts := range lwrAnchors(shardBLWR) {
		if !wholeSet[ts] {
			t.Errorf("shard B anchor %s absent from the whole grid (phase drift against off-grid end %s)", ts, end)
		}
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

// findRangeLWR locates the single *chplan.RangeLWR node reachable from n,
// failing the test if there isn't exactly one. Used by the pinnedInner case
// to inspect the epoch-aligned subquery-inner grid a re-anchor pass produced.
func findRangeLWR(t *testing.T, n chplan.Node, query string) *chplan.RangeLWR {
	t.Helper()
	var found *chplan.RangeLWR
	chplan.Walk(n, func(node chplan.Node) bool {
		if r, ok := node.(*chplan.RangeLWR); ok {
			if found != nil {
				t.Fatalf("query %q lowers to more than one RangeLWR node", query)
			}
			found = r
		}
		return true
	})
	if found == nil {
		t.Fatalf("query %q has no RangeLWR node in its plan", query)
	}
	return found
}

// lwrAnchors enumerates the anchor timestamps a RangeLWR's grid covers:
// Start, Start+Step, ..., up to and including End.
func lwrAnchors(r *chplan.RangeLWR) []time.Time {
	var out []time.Time
	for ts := r.Start; !ts.After(r.End); ts = ts.Add(r.Step) {
		out = append(out, ts)
	}
	return out
}
