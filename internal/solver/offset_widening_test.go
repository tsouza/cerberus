package solver

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// The two spans the fixtures below keep apart. Both differ from every per-kind
// lookback in carrier_geometry_test.go, so an assertion on a sum can name which
// term is missing rather than only that the total is wrong.
const (
	// geomCarrierOffset is the `offset` modifier stamped onto every
	// offset-bearing carrier.
	geomCarrierOffset = 11 * time.Minute
	// geomNestedRange is the [range] of the nested probe window the
	// walk-arithmetic fixtures place below a native / absent carrier.
	geomNestedRange = 3 * time.Minute
)

// stampOffset sets the PromQL `offset` modifier on a grid carrier, reporting
// whether the kind carries one at all. A kind without an Offset field (StepGrid
// — a bare, data-free anchor axis) returns false, and
// TestCarrierGeometry_OffsetTableCoversEveryOffsetCarrier proves that answer is
// a property of chplan rather than of this switch.
func stampOffset(n chplan.Node, off time.Duration) bool {
	switch v := n.(type) {
	case *chplan.RangeWindow:
		v.Offset = off
	case *chplan.RangeLWR:
		v.Offset = off
	case *chplan.RangeWindowGridNative:
		v.Offset = off
	case *chplan.RangeWindowStaleResample:
		v.Offset = off
	case *chplan.RangeBucketFanout:
		v.Offset = off
	case *chplan.RangeBucketGridNative:
		v.Offset = off
	case *chplan.AbsentOverTime:
		v.Offset = off
	default:
		return false
	}
	return true
}

// TestCarrierGeometry_OffsetChangesNeitherFanoutNorD pins the REFUTATION of
// issue #1732's item 2, which proposed that carrierGeometryOf's `lookback:
// v.Range` should become `v.Offset + v.Range` for consistency with
// [chplan.RangeWindow.InputWindow]'s widening arithmetic.
//
// InputWindow answers "how far back must this carrier's INPUT SPINE reach",
// and Offset genuinely belongs there. carrierGeometry.lookback answers two
// different questions, and Offset belongs in neither:
//
//   - F = lookback/Step is how many samples ONE anchor reduces. Shifting a
//     window does not change how many samples it holds. F gates routing under
//     ModeAuto (`route iff F >= MinFanout AND N*F >= MinAnchorPairs`), so an
//     inflated F routes plans on a sample count nobody reads.
//   - D = Σ lookback drives the high-D floor `K <= OuterRange / max(D, Step)`,
//     which measures per-slice REDUNDANCY. Adjacent slices' input windows
//     overlap by exactly the window span; an offset shifts every slice's
//     window back by the same amount and changes that overlap by nothing.
//
// So the whole geometry must be offset-INVARIANT, which is what this asserts:
// stamping an offset onto each carrier moves neither feature, nor the anchor
// grid. Applying #1732's proposal makes every row here fail — and, downstream,
// makes TestPlan_RangeLWRSpineRoutes/positive_offset refuse a routable plan as
// high-D.
func TestCarrierGeometry_OffsetChangesNeitherFanoutNorD(t *testing.T) {
	t.Parallel()

	stamped := 0
	for _, tc := range carrierCases() {
		if stampOffset(tc.plan(), geomCarrierOffset) {
			stamped++
		}
	}
	if stamped == 0 {
		t.Fatal("no carrier in the table accepted an offset; every subtest below would return " +
			"early and this pin would be vacuous")
	}

	for _, tc := range carrierCases() {
		tc := tc
		t.Run(tc.kind, func(t *testing.T) {
			t.Parallel()

			plan := tc.plan()
			if !stampOffset(plan, geomCarrierOffset) {
				// A carrier with no `offset` modifier at all: its geometry must
				// be identical to the un-stamped row, which the base table
				// already pins. Nothing to add here.
				return
			}

			start, end, step := GridOf(plan)
			if step != gridStep || !start.Equal(gridStart) || !end.Equal(gridEnd) {
				t.Fatalf("stamping an offset moved the eval grid: GridOf = (%s, %s, %s), "+
					"want (%s, %s, %s) — offset shifts WHICH samples an anchor reads, never "+
					"where the anchors sit", start, end, step, gridStart, gridEnd, gridStep)
			}
			meta := RequestMeta{Lang: LangPromQL, Start: start, End: end, Step: step}

			d, _ := (&Planner{Cfg: autoCfg()}).Plan(plan, meta)

			if d.CumulativeD != tc.wantD {
				t.Errorf("CumulativeD = %s, want %s — D is the per-slice overlap the high-D floor "+
					"prices, and an offset shifts every slice's window back equally, so it adds "+
					"no overlap at all", d.CumulativeD, tc.wantD)
			}
			if d.Fanout != tc.wantFanout {
				t.Errorf("Fanout = %d, want %d — shifting a window does not change how many "+
					"samples it holds, and F gates routing in ModeAuto", d.Fanout, tc.wantFanout)
			}
			if d.NAnchors != geomAnchors {
				t.Errorf("NAnchors = %d, want %d", d.NAnchors, geomAnchors)
			}
		})
	}
}

// TestCarrierGeometry_OffsetTableCoversEveryOffsetCarrier closes stampOffset
// against chplan itself: every grid carrier that declares an `Offset
// time.Duration` field must be stamped by the switch above, and nothing else
// may be.
//
// Without this, a carrier kind that grows an offset later would silently fall
// into stampOffset's default arm, its subtest above would return early, and the
// offset-invariance pin would quietly stop covering it.
func TestCarrierGeometry_OffsetTableCoversEveryOffsetCarrier(t *testing.T) {
	t.Parallel()

	declared := offsetDeclaringChplanCarriers(t)
	if len(declared) == 0 {
		t.Fatal("scanned internal/chplan and found no grid carrier declaring an Offset field; " +
			"the scan is broken, not the package")
	}

	stampedKinds := make(map[string]bool)
	for _, tc := range carrierCases() {
		if stampOffset(tc.plan(), geomCarrierOffset) {
			stampedKinds[tc.kind] = true
		}
	}

	for _, name := range declared {
		if !stampedKinds[name] {
			t.Errorf("chplan.%s declares an eval grid AND an Offset, but stampOffset does not set "+
				"it — that kind's cost geometry is never checked for offset-invariance", name)
		}
		delete(stampedKinds, name)
	}
	for name := range stampedKinds {
		t.Errorf("stampOffset sets an Offset on chplan.%s, which no longer declares one", name)
	}
}

// offsetDeclaringChplanCarriers returns the names of every internal/chplan
// struct that declares BOTH the eval-grid field signature and an `Offset
// time.Duration`, sorted. It reuses declaresEvalGrid so "is a grid carrier"
// means the same thing here as in the completeness ratchet next door.
func offsetDeclaringChplanCarriers(t *testing.T) []string {
	t.Helper()

	const chplanDir = "../chplan"
	entries, err := os.ReadDir(chplanDir)
	if err != nil {
		t.Fatalf("read internal/chplan: %v", err)
	}

	fset := token.NewFileSet()
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(chplanDir, e.Name()), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				return true
			}
			if declaresEvalGrid(st) && declaresOffset(st) {
				names = append(names, ts.Name.Name)
			}
			return true
		})
	}
	sort.Strings(names)
	return names
}

// declaresOffset reports whether a struct carries an `Offset time.Duration`
// field — the PromQL modifier that shifts a carrier's per-anchor window.
func declaresOffset(st *ast.StructType) bool {
	for _, f := range st.Fields.List {
		if exprTypeName(f.Type) != "time.Duration" {
			continue
		}
		for _, nm := range f.Names {
			if nm.Name == "Offset" {
				return true
			}
		}
	}
	return false
}

// offsetWalkCase is one grid-carrier kind whose walkNode arm widens its own
// inner spine, built at a chosen predicted inner start.
type offsetWalkCase struct {
	kind string
	// plan wraps a nested probe window, pinned at innerStart..gridEnd, under
	// the carrier this row is about.
	plan func(innerStart time.Time) chplan.Node
}

// nestedProbeWindow is a fully pinned matrix RangeWindow sitting at
// [innerStart, gridEnd]. Placed below another carrier it is the ONLY way the
// walk's predicted inner bounds become observable: checkRangeWindowGrid
// compares this node's own Start/End/OuterRange against what walkNode predicted
// at its depth, and records sawGridMismatch when they disagree.
func nestedProbeWindow(innerStart time.Time) *chplan.RangeWindow {
	return &chplan.RangeWindow{
		Input:           leafScan(),
		Func:            "sum_over_time",
		Range:           time.Minute,
		Step:            gridStep,
		OuterRange:      gridEnd.Sub(innerStart),
		Start:           innerStart,
		End:             gridEnd,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
}

func offsetWalkCases() []offsetWalkCase {
	return []offsetWalkCase{
		{
			// The LIVE shape: `absent_over_time(<subquery>)` lowers to an
			// AbsentOverTime whose Input is a Project over the subquery's own
			// RangeWindow grid (internal/promql/subquery.go's
			// lowerAbsentOverTimeOverSubquery), so this walk really does reach a
			// carrier whose predicted bounds the arm's arithmetic decides.
			kind: "AbsentOverTime",
			plan: func(innerStart time.Time) chplan.Node {
				return &chplan.AbsentOverTime{
					Input: &chplan.Project{
						Input: nestedProbeWindow(innerStart),
						Projections: []chplan.Projection{
							{Expr: &chplan.ColumnRef{Name: "Attributes"}, Alias: "Attributes"},
							{Expr: &chplan.ColumnRef{Name: "TimeUnix"}, Alias: "TimeUnix"},
							{Expr: &chplan.ColumnRef{Name: "Value"}, Alias: "Value"},
						},
					},
					Range:            geomNestedRange,
					Offset:           geomCarrierOffset,
					Start:            gridStart,
					End:              gridEnd,
					Step:             gridStep,
					TimestampColumn:  "TimeUnix",
					ValueColumn:      "Value",
					MetricNameColumn: "MetricName",
					AttributesColumn: "Attributes",
				}
			},
		},
		{
			// The natively-lowered rate(): today its Input is always a plain
			// scan, so this nesting is synthetic. The arm's arithmetic is still
			// the thing under test — it is what would decide the predicted
			// bounds the day the node is made re-anchorable, and the whole point
			// of closing the class is that the fix lands before that, not after.
			kind: "RangeWindowGridNative",
			plan: func(innerStart time.Time) chplan.Node {
				return &chplan.RangeWindowGridNative{
					Input:           nestedProbeWindow(innerStart),
					Func:            "rate",
					Range:           geomNestedRange,
					Offset:          geomCarrierOffset,
					Step:            gridStep,
					Start:           gridStart,
					End:             gridEnd,
					TimestampColumn: "TimeUnix",
					ValueColumn:     "Value",
					GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
				}
			},
		},
	}
}

// TestWalkNode_OffsetWidensNativeAndAbsentInnerSpine pins the grid-prediction
// arithmetic of the two walkNode arms that were still widening blind by Range
// alone, dropping the Offset term the sibling RangeWindow arm got right in
// #1464.
//
// Both directions are asserted, because either alone is satisfiable by a wrong
// walk: a probe pinned at `gridStart - Offset - Range` must MATCH (the correct
// prediction), and one pinned at the pre-fix `gridStart - Range` must MISMATCH.
// Without the second half, an arm that widened by some huge constant would pass.
func TestWalkNode_OffsetWidensNativeAndAbsentInnerSpine(t *testing.T) {
	t.Parallel()

	meta := RequestMeta{Lang: LangPromQL, Start: gridStart, End: gridEnd, Step: gridStep}

	// The window the carrier's own anchors read: `(anchor - Offset - Range,
	// anchor - Offset]`. Its far edge is what the inner spine must reach.
	wantInnerStart := gridStart.Add(-geomCarrierOffset - geomNestedRange)
	// What the arms predicted before the fix: Range alone, Offset dropped.
	offsetBlindInnerStart := gridStart.Add(-geomNestedRange)

	for _, tc := range offsetWalkCases() {
		tc := tc
		t.Run(tc.kind, func(t *testing.T) {
			t.Parallel()

			sig := (&Planner{Cfg: autoCfg()}).analyze(tc.plan(wantInnerStart), meta)
			if sig.sawGridMismatch {
				t.Errorf("a probe pinned at Start-Offset-Range (%s) was reported off-grid: the %s "+
					"arm widens its inner spine by less than the Offset+Range its own anchors read, "+
					"so every nested carrier below it is measured against bounds the plan never uses",
					wantInnerStart, tc.kind)
			}

			blind := (&Planner{Cfg: autoCfg()}).analyze(tc.plan(offsetBlindInnerStart), meta)
			if !blind.sawGridMismatch {
				t.Errorf("a probe pinned at the pre-fix Start-Range (%s) was accepted as on-grid, so "+
					"the %s arm is still dropping the Offset term (or the probe never reached the "+
					"grid check at all)", offsetBlindInnerStart, tc.kind)
			}
		})
	}
}
