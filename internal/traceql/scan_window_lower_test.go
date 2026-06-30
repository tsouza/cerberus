package traceql

import (
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// Phase-4 lowering coverage for the spans-scan WINDOW invariant
// (stampRecursiveScanWindow + stampNestedSetTraceLimit). The /api/search
// request window must reach the EMITTER-SYNTHETIC recursive spans scans — the
// nested-set numbering CTE (NestedSetAnnotate) and the structural-closure step
// arm (StructuralJoin) — which scan the physical spans table directly and so
// are NOT chplan leaves the leaf-predicate push can reach. Without the window
// on these nodes the emitter renders `TraceId IN (<seed>)` membership that
// prunes NO toDate(Timestamp) partitions, and the recursive walk reads full
// retention (the traces-drilldown OOM).
//
// The tests assert: (1) a windowed search stamps Window* (and, for
// StructuralJoin, TimestampColumn) on every recursive node; (2) the metrics /
// non-search path (no limit) leaves them unstamped so that output stays
// byte-identical; (3) compare() is metrics-routed, so lowering does NOT stamp
// its root-lookup (the emit path windows it) — verified by lower-output
// invariance to the window.

const (
	scanWindowLowerLimit = 20
	// A historical window well away from now, so a regression that swaps the
	// request window for a now-relative default would diverge from these exact
	// nanos.
	scanWindowLowerStartNano int64 = 1782571392_000000000
	scanWindowLowerEndNano   int64 = 1782573192_000000000
)

func scanWindowLowerBounds() (time.Time, time.Time) {
	return time.Unix(0, scanWindowLowerStartNano).UTC(), time.Unix(0, scanWindowLowerEndNano).UTC()
}

// collectScanWindowNodes gathers every NestedSetAnnotate and StructuralJoin
// reachable from plan — the two recursive-spans-scan node families the window
// stamp must reach.
func collectScanWindowNodes(plan chplan.Node) (nsa []*chplan.NestedSetAnnotate, sj []*chplan.StructuralJoin) {
	chplan.Walk(plan, func(n chplan.Node) bool {
		switch v := n.(type) {
		case *chplan.NestedSetAnnotate:
			nsa = append(nsa, v)
		case *chplan.StructuralJoin:
			sj = append(sj, v)
		}
		return true
	})
	return nsa, sj
}

// TestLowerStampsRequestWindowOnRecursiveScans is the positive case: every
// recursive spans-scan node a windowed search produces carries the exact
// request window (and TimestampColumn for the structural join).
func TestLowerStampsRequestWindowOnRecursiveScans(t *testing.T) {
	t.Parallel()
	s := schema.DefaultOTelTraces()
	start, end := scanWindowLowerBounds()

	cases := []struct {
		name    string
		query   string
		wantNSA int // minimum NestedSetAnnotate count
		wantSJ  int // minimum StructuralJoin count
	}{
		{
			name:    "select_nested_set",
			query:   `{ nestedSetParent < 0 } | select(nestedSetParent, nestedSetLeft, nestedSetRight)`,
			wantNSA: 1,
		},
		{
			name:    "group_by_nested_set",
			query:   `{ } | by(nestedSetParent)`,
			wantNSA: 1,
		},
		{
			// A position comparison (not the `nestedSetParent < 0` root idiom)
			// forces a real NestedSetAnnotate, here under an Aggregate +
			// scalar-filter — exercising the window stamp's descent THROUGH the
			// aggregate spine, not just a top-level select.
			name:    "aggregate_over_nested_set",
			query:   `{ nestedSetLeft > 0 } | count() > 0`,
			wantNSA: 1,
		},
		{
			name:   "structural_descendant",
			query:  `{ .service.name = "a" } >> { .http.status_code = 500 }`,
			wantSJ: 1,
		},
		{
			name:   "structural_union_descendant",
			query:  `{ .service.name = "a" } &>> { .http.status_code = 500 }`,
			wantSJ: 1,
		},
		{
			// The Grafana Traces Drilldown structure-tab query — both a
			// NestedSetAnnotate (the select numbering) and a StructuralJoin
			// (the &>> closure).
			name:    "structure_tab_union",
			query:   `({ nestedSetParent < 0 } &>> { kind = server }) || ({ nestedSetParent < 0 }) | select(nestedSetParent, nestedSetLeft, nestedSetRight)`,
			wantNSA: 1,
			wantSJ:  1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			plan := lowerSearchWindowed(t, tc.query, scanWindowLowerLimit, start, end)
			nsa, sj := collectScanWindowNodes(plan)
			if len(nsa) < tc.wantNSA {
				t.Fatalf("got %d NestedSetAnnotate nodes, want >= %d — query shape changed, assertion vacuous", len(nsa), tc.wantNSA)
			}
			if len(sj) < tc.wantSJ {
				t.Fatalf("got %d StructuralJoin nodes, want >= %d — query shape changed, assertion vacuous", len(sj), tc.wantSJ)
			}
			for i, n := range nsa {
				if n.WindowStartNano != scanWindowLowerStartNano || n.WindowEndNano != scanWindowLowerEndNano {
					t.Errorf("NestedSetAnnotate[%d] window = [%d,%d]; want [%d,%d] — the numbering CTE would read full retention behind the inert TraceId-IN",
						i, n.WindowStartNano, n.WindowEndNano, scanWindowLowerStartNano, scanWindowLowerEndNano)
				}
				if n.TimestampColumn != s.TimestampColumn {
					t.Errorf("NestedSetAnnotate[%d] TimestampColumn = %q; want %q", i, n.TimestampColumn, s.TimestampColumn)
				}
			}
			for i, j := range sj {
				if j.TimestampColumn != s.TimestampColumn {
					t.Errorf("StructuralJoin[%d] TimestampColumn = %q; want %q — the recursive step scan would not partition-prune",
						i, j.TimestampColumn, s.TimestampColumn)
				}
				if j.WindowStartNano != scanWindowLowerStartNano || j.WindowEndNano != scanWindowLowerEndNano {
					t.Errorf("StructuralJoin[%d] window = [%d,%d]; want [%d,%d]",
						i, j.WindowStartNano, j.WindowEndNano, scanWindowLowerStartNano, scanWindowLowerEndNano)
				}
			}
		})
	}
}

// TestLowerLeavesRecursiveScansUnwindowedWithoutLimit pins the no-op edge: the
// metrics / spec / property paths (no /api/search limit on the context) must
// leave the recursive nodes unstamped, so their emitted SQL stays
// byte-identical to the pre-window output. A regression that unconditionally
// stamped the window would churn every non-search golden and silently change
// metrics-path numbering.
func TestLowerLeavesRecursiveScansUnwindowedWithoutLimit(t *testing.T) {
	t.Parallel()

	queries := []string{
		`{ nestedSetParent < 0 } | select(nestedSetParent, nestedSetLeft, nestedSetRight)`,
		`{ .service.name = "a" } >> { .http.status_code = 500 }`,
		`({ nestedSetParent < 0 } &>> { kind = server }) || ({ nestedSetParent < 0 }) | select(nestedSetParent, nestedSetLeft, nestedSetRight)`,
	}
	for _, q := range queries {
		plan := lowerSearchWindowed(t, q, 0, time.Time{}, time.Time{})
		nsa, sj := collectScanWindowNodes(plan)
		for i, n := range nsa {
			if n.WindowStartNano != 0 || n.WindowEndNano != 0 {
				t.Errorf("no-limit %q: NestedSetAnnotate[%d] window = [%d,%d]; want [0,0]", q, i, n.WindowStartNano, n.WindowEndNano)
			}
		}
		for i, j := range sj {
			if j.WindowStartNano != 0 || j.WindowEndNano != 0 || j.TimestampColumn != "" {
				t.Errorf("no-limit %q: StructuralJoin[%d] window = [%d,%d] tsCol=%q; want [0,0] tsCol=\"\"",
					q, i, j.WindowStartNano, j.WindowEndNano, j.TimestampColumn)
			}
		}
	}
}

// TestLowerCompareNotWindowStampedAtLowering documents the design split for the
// compare() root-lookup: compare is metrics-routed (MetricsPipeline), so the
// /api/search stamp block is skipped entirely and the window has NO effect at
// lowering — the emit path (windowRootLookupScan, gated on the context-threaded
// spans table) is what folds the window onto the root scan. Asserting the
// lowered plan is invariant to the window proves no stray search-window stamp
// leaks onto the metrics path (which would double-window or churn the metrics
// goldens).
func TestLowerCompareNotWindowStampedAtLowering(t *testing.T) {
	t.Parallel()
	start, end := scanWindowLowerBounds()
	const q = `{ } | compare({ status = error })`

	windowed := lowerSearchWindowed(t, q, scanWindowLowerLimit, start, end)
	bare := lowerSearchWindowed(t, q, 0, time.Time{}, time.Time{})
	if !windowed.Equal(bare) {
		t.Errorf("compare() lowering differs with vs without a search window — the metrics path must ignore the search-window stamp (the emitter owns the root-lookup window)")
	}

	// And the root lookup the emitter windows must actually be present, so the
	// invariance above is non-vacuous.
	var foundRootLookup bool
	chplan.Walk(windowed, func(n chplan.Node) bool {
		if c, ok := n.(*chplan.MetricsCompare); ok && c.RootLookup != nil {
			foundRootLookup = true
		}
		return true
	})
	if !foundRootLookup {
		t.Fatal("compare() plan carries no MetricsCompare.RootLookup — the emit-time window push has nothing to bound")
	}
}
