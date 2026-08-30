package promql

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// Default-lane (no chDB) pins for cerberus issue #2728's triple-nested
// composition — `<outer-fn2>(<fn>(<inner-sub>)[<outer-range>:<step>])`.
// The chdb-tagged sibling proves the ANSWERS; these prove the lowering
// decisions that produce them, so the gremlins default build has real
// coverage over the gate and the grid arithmetic rather than reaching
// them only incidentally.

const (
	// outerFn2TestInnerRange / outerFn2TestOuterRange are the two brackets
	// every query below uses: `<fn>(<inner>[3m:1m])[4m:1m]`.
	outerFn2TestInnerRange = 3 * time.Minute
	outerFn2TestOuterRange = 4 * time.Minute
)

// outerFn2TestMixedInner is a mixed float/histogram `or`, which resolves
// the MID relation to MixedRowShape; outerFn2TestHistInner is an
// and-forwarded histogram, which resolves it to HistogramRowShape. A PURE
// two-histogram set op would be intercepted by an older root-level
// recognizer before the triple-nesting arm is reached at all.
const (
	outerFn2TestMixedInner = "((m_exp_hist) or (m_gauge))"
	outerFn2TestHistInner  = "((m_exp_hist) and (m_gauge))"
)

func outerFn2TestQuery(fn, inner, suffix string) string {
	return fn + "(rate(" + inner + "[3m:1m])[4m:1m]" + suffix + ")"
}

// outerFn2SelectFamily and outerFn2FoldFamily together are the fifteen
// names [histogramSubqueryOuterFnName] accepts.
var (
	outerFn2SelectFamily = []string{
		"count_over_time", "present_over_time", "ts_of_first_over_time", "ts_of_last_over_time",
		"last_over_time", "first_over_time", "resets", "changes",
	}
	outerFn2FoldFamily = []string{
		"rate", "increase", "delta", "irate", "idelta", "sum_over_time", "avg_over_time",
	}
)

func outerFn2AllNames() []string {
	return append(append([]string{}, outerFn2SelectFamily...), outerFn2FoldFamily...)
}

// TestHistogramSubqueryOuterFnName_Vocabulary pins the gate's membership
// exactly: every name [lowerHistogramOrMixedSubqueryOuterFnInput]'s switch
// answers must be accepted (or the widening the triple-nesting arm depends
// on never runs and the query falls through to a rejection), and every
// name it leaves unmatched must be refused (or the widening mutates a
// relation the caller then hands to its float-only-drop fallback).
func TestHistogramSubqueryOuterFnName_Vocabulary(t *testing.T) {
	t.Parallel()

	for _, fn := range outerFn2AllNames() {
		if !histogramSubqueryOuterFnName(fn) {
			t.Errorf("histogramSubqueryOuterFnName(%q) = false, want true — it is one of the fifteen names the dispatch answers", fn)
		}
	}
	// The eleven float-only-drop names plus a genuinely unknown one: none
	// of them reaches the dispatch's switch.
	for _, fn := range []string{
		"max_over_time", "min_over_time", "stddev_over_time", "stdvar_over_time",
		"quantile_over_time", "mad_over_time", "deriv", "predict_linear",
		"holt_winters", "ts_of_max_over_time", "ts_of_min_over_time", "not_a_function",
	} {
		if histogramSubqueryOuterFnName(fn) {
			t.Errorf("histogramSubqueryOuterFnName(%q) = true, want false — the dispatch's switch leaves it unmatched", fn)
		}
	}
}

// outerFn2NestedGrids collects the (Start, End, OuterRange) of every node
// on the MID relation's own independent outer-subquery grid — the
// OuterRange-mode fan-out the histogram half rides and the OuterRange-mode
// RangeWindow the float half rides. Both are the nodes
// [widenNestedCallSubqueryInner] must re-anchor; the enclosing function's
// own reduction grid is a CLASSIC (Start, End, Step) fan-out with
// OuterRange unset, so it never matches here.
type outerFn2Grid struct {
	kind       string
	start, end time.Time
	outerRange time.Duration
}

func outerFn2NestedGrids(plan chplan.Node) []outerFn2Grid {
	var out []outerFn2Grid
	chplan.Walk(plan, func(n chplan.Node) bool {
		switch v := n.(type) {
		case *chplan.RangeBucketFanout:
			if v.OuterRange > 0 {
				out = append(out, outerFn2Grid{"RangeBucketFanout", v.Start, v.End, v.OuterRange})
			}
		case *chplan.RangeWindow:
			if v.OuterRange > 0 && v.Range == outerFn2TestInnerRange {
				out = append(out, outerFn2Grid{"RangeWindow", v.Start, v.End, v.OuterRange})
			}
		}
		return true
	})
	return out
}

// TestWidenNestedCallSubqueryInner_GridModes pins the window each of the
// three grid modes re-anchors the MID relation onto. Each mode is a
// separate branch of [widenNestedCallSubqueryInner], and each window is a
// separate arithmetic expression, so an assertion on the emitted
// (Start, End, OuterRange) triple reads each one back directly.
//
// The `offset` case exists because the offset term is the one part of
// that arithmetic no other case can distinguish: it shifts each anchor's
// own window back, so the grid has to reach a further `Offset` into the
// past for the oldest step to still find its samples.
func TestWidenNestedCallSubqueryInner_GridModes(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	pin := at.Add(-2 * time.Hour)
	rangeStart, rangeEnd := at.Add(-10*time.Minute), at
	const offset = 30 * time.Second

	cases := []struct {
		name       string
		suffix     string
		rangeMode  bool
		wantStart  time.Time
		wantEnd    time.Time
		wantOuterR time.Duration
	}{
		{
			name:      "instant",
			wantStart: at.Add(-outerFn2TestOuterRange), wantEnd: at,
			wantOuterR: outerFn2TestOuterRange,
		},
		{
			name:   "instant with offset",
			suffix: " offset 30s",
			// Each anchor's window is (t - Offset - Range, t - Offset], so
			// the grid reaches Offset further back than the bare case.
			wantStart: at.Add(-offset - outerFn2TestOuterRange), wantEnd: at,
			wantOuterR: offset + outerFn2TestOuterRange,
		},
		{
			name: "query_range fan-out", rangeMode: true,
			wantStart: rangeStart.Add(-outerFn2TestOuterRange), wantEnd: rangeEnd,
			wantOuterR: rangeEnd.Sub(rangeStart.Add(-outerFn2TestOuterRange)),
		},
		{
			name: "query_range pinned broadcast", rangeMode: true,
			suffix:    " @ " + strconv.FormatInt(pin.Unix(), 10),
			wantStart: pin.Add(-outerFn2TestOuterRange), wantEnd: pin,
			// The pin fixes the whole subquery evaluation, so the grid stays
			// exactly one sub.Range wide regardless of the ambient window.
			wantOuterR: outerFn2TestOuterRange,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			expr := mustParseExperimental(t, outerFn2TestQuery("count_over_time", outerFn2TestMixedInner, tc.suffix))
			var plan chplan.Node
			var err error
			if tc.rangeMode {
				plan, err = LowerAtRange(context.Background(), expr, s, rangeStart, rangeEnd, time.Minute)
			} else {
				plan, err = LowerAt(context.Background(), expr, s, at, at)
			}
			if err != nil {
				t.Fatalf("lower: %v", err)
			}
			grids := outerFn2NestedGrids(plan)
			if len(grids) == 0 {
				t.Fatalf("no OuterRange-mode grid node found — the MID relation was not built by the doubly-nested composition at all")
			}
			for _, g := range grids {
				if !g.start.Equal(tc.wantStart) || !g.end.Equal(tc.wantEnd) || g.outerRange != tc.wantOuterR {
					t.Errorf("%s grid = [%s, %s] outerRange=%s, want [%s, %s] outerRange=%s",
						g.kind, g.start, g.end, g.outerRange, tc.wantStart, tc.wantEnd, tc.wantOuterR)
				}
			}
		})
	}
}

// TestLowerOuterFn2_UnmatchedNameLeavesMidGridUntouched pins the gate's
// ORDER: [histogramSubqueryOuterFnName] is consulted BEFORE the widening
// runs, because the widening mutates the MID relation in place and an
// unmatched name hands that same relation to the caller's own
// float-only-drop fallback. A gate that widened first would re-anchor a
// relation nothing downstream re-reads — silently, since the drop path
// answers empty either way.
func TestLowerOuterFn2_UnmatchedNameLeavesMidGridUntouched(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	rangeStart := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	rangeEnd := rangeStart.Add(10 * time.Minute)

	// max_over_time has no native-histogram semantics in reference, so it
	// drops to empty rather than reducing — and must leave the grid alone.
	expr := mustParseExperimental(t, outerFn2TestQuery("max_over_time", outerFn2TestMixedInner, ""))
	plan, err := LowerAtRange(context.Background(), expr, s, rangeStart, rangeEnd, time.Minute)
	if err != nil {
		t.Fatalf("LowerAtRange: %v", err)
	}
	for _, g := range outerFn2NestedGrids(plan) {
		if g.outerRange != outerFn2TestOuterRange {
			t.Errorf("%s outerRange = %s, want %s (the un-widened bracket width — an unmatched name must not re-anchor the MID grid)",
				g.kind, g.outerRange, outerFn2TestOuterRange)
		}
	}
}

// TestLowerOuterFn2_EveryNameBothShapesAllGridModes sweeps the whole
// dispatch surface: all fifteen names, over both a MixedRowShape and a
// HistogramRowShape MID, at instant / `@`-pinned / query_range. Before
// cerberus issue #2728 every one of these rejected with "over a subquery
// wrapping a native-histogram-valued shape is unsupported".
func TestLowerOuterFn2_EveryNameBothShapesAllGridModes(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	at := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	pin := at.Add(-2 * time.Hour)

	for _, inner := range []string{outerFn2TestMixedInner, outerFn2TestHistInner} {
		for _, fn := range outerFn2AllNames() {
			for _, mode := range []string{"instant", "pinned", "range"} {
				suffix := ""
				if mode == "pinned" {
					suffix = " @ " + strconv.FormatInt(pin.Unix(), 10)
				}
				name := fn + "/" + mode + "/" + inner
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					expr := mustParseExperimental(t, outerFn2TestQuery(fn, inner, suffix))
					var plan chplan.Node
					var err error
					if mode == "range" {
						plan, err = LowerAtRange(context.Background(), expr, s, at.Add(-10*time.Minute), at, time.Minute)
					} else {
						plan, err = LowerAt(context.Background(), expr, s, at, at)
					}
					if err != nil {
						t.Fatalf("lower: %v", err)
					}
					// The pre-#2728 fallback for these names was an ERROR, so
					// reaching a plan at all is the headline. Assert it is a
					// real reduction and not the drop-to-empty shape, which
					// would answer without erroring but read nothing.
					if planDropsEverything(plan) {
						t.Fatalf("lowered to the drop-to-empty shape (Filter predicate=false), want a real reduction")
					}
				})
			}
		}
	}
}

// planDropsEverything reports whether plan contains the constant-false
// Filter [dropExpHistogramSamples] builds — the "answer empty" shape the
// float-only-drop fallback lowers to.
func planDropsEverything(plan chplan.Node) bool {
	found := false
	chplan.Walk(plan, func(n chplan.Node) bool {
		f, ok := n.(*chplan.Filter)
		if !ok {
			return true
		}
		if lit, ok := f.Predicate.(*chplan.LitBool); ok && !lit.V {
			found = true
		}
		return true
	})
	return found
}

// TestCombineMixedAggregateBranches_ReadsEachBranchOnce is the direct pin
// on why [chplan.VectorSetOp.MixedDropCollisions] exists: the recombination
// must name each already-reduced branch EXACTLY once. The equivalent
// `(hist unless float) or (float unless hist)` tree names each one twice,
// which squares the emitted SQL of a stacked composition and doubles the
// reads — cerberus issue #2728's triple nesting stacks two of these and
// went past ClickHouse's max_ast_elements ceiling because of it.
func TestCombineMixedAggregateBranches_ReadsEachBranchOnce(t *testing.T) {
	t.Parallel()

	s := schema.DefaultOTelMetrics()
	histBranch := &chplan.Scan{Table: "hist_branch"}
	floatBranch := &chplan.Scan{Table: "float_branch"}

	combined := combineMixedAggregateBranches(histBranch, floatBranch, s, true)

	counts := map[chplan.Node]int{}
	chplan.Walk(combined, func(n chplan.Node) bool {
		counts[n]++
		return true
	})
	if got := counts[histBranch]; got != 1 {
		t.Errorf("histBranch appears %d times in the recombination, want exactly 1", got)
	}
	if got := counts[floatBranch]; got != 1 {
		t.Errorf("floatBranch appears %d times in the recombination, want exactly 1", got)
	}

	setOp, ok := combined.(*chplan.VectorSetOp)
	if !ok {
		t.Fatalf("combineMixedAggregateBranches returned %T, want *chplan.VectorSetOp", combined)
	}
	if !setOp.MixedDropCollisions {
		t.Error("MixedDropCollisions = false — without it the union is left-biased and a key both branches claim survives through the histogram side instead of being dropped")
	}
	if !setOp.Mixed || setOp.Op != chplan.VectorSetOr {
		t.Errorf("Mixed=%v Op=%v, want Mixed=true Op=or (MixedDropCollisions is only meaningful there)", setOp.Mixed, setOp.Op)
	}
}
