package chplan_test

import (
	"errors"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// This file gathers targeted tests that kill the LIVED gremlins mutants
// reported by the phase-1 mutation matrix in
// `.github/workflows/mutation.yml` (`./internal/chplan @ 95%`). Each
// test pins the exact behaviour a single mutation would break:
//
//   - An INVERT_LOGICAL on a `||`/`&&` inside an `Equal` method needs a
//     case where EXACTLY ONE operand is true (a single field differs,
//     all others equal). With the original operator the nodes are not
//     Equal; flipping `||`→`&&` would wrongly report Equal because the
//     other (equal) field makes the `&&` short-circuit elsewhere.
//   - An ARITHMETIC_BASE / INVERT_NEGATIVES on the membership-window
//     widen (`start - Offset - Lookback`) needs a nested node so the
//     widened start is recorded and asserted against the exact value.
//   - An INVERT_LOGICAL on the reanchor grid-prediction guards needs a
//     partially-pinned node (one bound zero / one bound on-grid) so the
//     `&&` and `||` forms diverge.
//
// These mirror the per-node `*_Equal_Negative_*` convention already in
// equal_invariants_test.go; they sit here as a single mutation-budget
// file so the kill set is easy to audit against the gremlins report.

// --- cross_join.go:27:30 — `Left.Equal(o.Left) && Right.Equal(o.Right)` ---

// TestCrossJoin_Equal_Negative_RightOnly / _LeftOnly exercise the
// `Left && Right` tail of CrossJoin.Equal. A mutant flipping `&&` to
// `||` would falsely report Equal when only one child differs, because
// the matching child's Equal returns true.
func TestCrossJoin_Equal_Negative_RightOnly(t *testing.T) {
	t.Parallel()
	a := &chplan.CrossJoin{Left: &chplan.Scan{Table: "shared"}, Right: &chplan.Scan{Table: "a"}}
	b := &chplan.CrossJoin{Left: &chplan.Scan{Table: "shared"}, Right: &chplan.Scan{Table: "b"}}
	if a.Equal(b) {
		t.Errorf("CrossJoin: different Right (Left equal) should not be Equal")
	}
}

func TestCrossJoin_Equal_Negative_LeftOnly(t *testing.T) {
	t.Parallel()
	a := &chplan.CrossJoin{Left: &chplan.Scan{Table: "a"}, Right: &chplan.Scan{Table: "shared"}}
	b := &chplan.CrossJoin{Left: &chplan.Scan{Table: "b"}, Right: &chplan.Scan{Table: "shared"}}
	if a.Equal(b) {
		t.Errorf("CrossJoin: different Left (Right equal) should not be Equal")
	}
}

// TestCrossJoin_Equal_Positive anchors the true case so the negatives
// above are genuine single-field divergences off an otherwise-equal pair.
func TestCrossJoin_Equal_Positive(t *testing.T) {
	t.Parallel()
	a := &chplan.CrossJoin{Left: &chplan.Scan{Table: "l"}, Right: &chplan.Scan{Table: "r"}}
	b := &chplan.CrossJoin{Left: &chplan.Scan{Table: "l"}, Right: &chplan.Scan{Table: "r"}}
	if !a.Equal(b) {
		t.Errorf("identical CrossJoin trees should be Equal")
	}
}

// --- nary_vector_set_op.go Equal disjunct chains ---

func naryFixture() *chplan.NaryVectorSetOp {
	return &chplan.NaryVectorSetOp{
		Arms:             []chplan.Node{&chplan.Scan{Table: "a"}, &chplan.Scan{Table: "b"}},
		Op:               chplan.VectorSetOr,
		Match:            chplan.VectorMatch{Labels: []string{"job"}, On: true},
		MetricNameColumn: "MetricName",
		AttributesColumn: "Attributes",
		TimestampColumn:  "TimeUnix",
		ValueColumn:      "Value",
	}
}

func TestNaryVectorSetOp_Equal_Positive(t *testing.T) {
	t.Parallel()
	if !naryFixture().Equal(naryFixture()) {
		t.Errorf("identical NaryVectorSetOp trees should be Equal")
	}
}

// TestNaryVectorSetOp_Equal_Negative_Fields exercises each disjunct in
// NaryVectorSetOp.Equal individually:
//   - nary_vector_set_op.go:72 — `Op != || !Match.Equal || StepAligned !=`
//   - nary_vector_set_op.go:75/76/77 — the
//     `MetricNameColumn != || AttributesColumn != || TimestampColumn !=
//     || ValueColumn !=` chain
//
// Each row diverges in exactly ONE field; with `||` → `&&` the mutant
// would require every column to differ before reporting not-Equal, so a
// single-field mismatch keeps the kill tight.
func TestNaryVectorSetOp_Equal_Negative_Fields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(n *chplan.NaryVectorSetOp)
	}{
		{"op", func(n *chplan.NaryVectorSetOp) { n.Op = chplan.VectorSetAnd }},
		{"match", func(n *chplan.NaryVectorSetOp) {
			n.Match = chplan.VectorMatch{Labels: []string{"instance"}, On: true}
		}},
		{"stepAligned", func(n *chplan.NaryVectorSetOp) { n.StepAligned = true }},
		{"metricNameColumn", func(n *chplan.NaryVectorSetOp) { n.MetricNameColumn = "Other" }},
		{"attributesColumn", func(n *chplan.NaryVectorSetOp) { n.AttributesColumn = "Other" }},
		{"timestampColumn", func(n *chplan.NaryVectorSetOp) { n.TimestampColumn = "Other" }},
		{"valueColumn", func(n *chplan.NaryVectorSetOp) { n.ValueColumn = "Other" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a, b := naryFixture(), naryFixture()
			tc.mutate(b)
			if a.Equal(b) || b.Equal(a) {
				t.Errorf("NaryVectorSetOp.Equal must detect %s divergence", tc.name)
			}
		})
	}
}

// --- range_bucket_fanout.go Equal disjunct chains ---

func fanoutFixture() *chplan.RangeBucketFanout {
	return &chplan.RangeBucketFanout{
		Input:          &chplan.Scan{Table: "metrics"},
		Start:          time.Unix(1000, 0).UTC(),
		End:            time.Unix(4600, 0).UTC(),
		Step:           30 * time.Second,
		Lookback:       5 * time.Minute,
		Offset:         time.Minute,
		GroupBy:        []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		GroupByAliases: []string{"g0"},
		AggFuncs:       []chplan.AggFunc{{Name: "sum", Args: []chplan.Expr{&chplan.ColumnRef{Name: "Value"}}, Alias: "BucketCounts"}},
		AnchorAlias:    "anchor_ts",
		TimestampCol:   "TimeUnix",
	}
}

func TestRangeBucketFanout_Equal_Positive(t *testing.T) {
	t.Parallel()
	if !fanoutFixture().Equal(fanoutFixture()) {
		t.Errorf("identical RangeBucketFanout trees should be Equal")
	}
}

// TestRangeBucketFanout_Equal_Negative_Fields exercises each `||`
// disjunct in RangeBucketFanout.Equal one field at a time:
//   - 99  — `!Start.Equal || !End.Equal`
//   - 102 — `Step != || Lookback != || Offset !=`
//   - 105 — `AnchorAlias != || TimestampCol !=`
//   - 108 — `len(GroupBy) != || len(AggFuncs) !=`
//
// Single-field divergence means `||` → `&&` (which needs both sides of
// a disjunct true) wrongly reports Equal — caught here.
func TestRangeBucketFanout_Equal_Negative_Fields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(r *chplan.RangeBucketFanout)
	}{
		{"start", func(r *chplan.RangeBucketFanout) { r.Start = r.Start.Add(time.Second) }},
		{"end", func(r *chplan.RangeBucketFanout) { r.End = r.End.Add(time.Second) }},
		{"step", func(r *chplan.RangeBucketFanout) { r.Step = time.Minute }},
		{"lookback", func(r *chplan.RangeBucketFanout) { r.Lookback = time.Hour }},
		{"offset", func(r *chplan.RangeBucketFanout) { r.Offset = 2 * time.Minute }},
		{"anchorAlias", func(r *chplan.RangeBucketFanout) { r.AnchorAlias = "other_anchor" }},
		{"timestampCol", func(r *chplan.RangeBucketFanout) { r.TimestampCol = "Other" }},
		{"groupByLen", func(r *chplan.RangeBucketFanout) {
			r.GroupBy = append(r.GroupBy, &chplan.ColumnRef{Name: "Extra"})
		}},
		{"aggFuncsLen", func(r *chplan.RangeBucketFanout) {
			r.AggFuncs = append(r.AggFuncs, chplan.AggFunc{Name: "count", Alias: "Extra"})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a, b := fanoutFixture(), fanoutFixture()
			tc.mutate(b)
			if a.Equal(b) || b.Equal(a) {
				t.Errorf("RangeBucketFanout.Equal must detect %s divergence", tc.name)
			}
		})
	}
}

// TestRangeBucketFanout_Equal_Negative_InputOneNil pins the `Input ==
// nil || o.Input == nil` short-circuit (range_bucket_fanout.go:129). A
// `||` → `&&` flip would skip the early-out when exactly one Input is
// nil, walking into Input.Equal on a nil receiver / mis-reporting.
func TestRangeBucketFanout_Equal_Negative_InputOneNil(t *testing.T) {
	t.Parallel()
	a := fanoutFixture()
	b := fanoutFixture()
	b.Input = nil
	if a.Equal(b) {
		t.Errorf("non-nil Input vs nil Input should not be Equal")
	}
	if b.Equal(a) {
		t.Errorf("nil Input vs non-nil Input should not be Equal (reverse)")
	}
}

// --- step_grid.go:38 — `Start.Equal && End.Equal && Step ==` ---

func TestStepGrid_Equal_Positive(t *testing.T) {
	t.Parallel()
	g := func() *chplan.StepGrid {
		return &chplan.StepGrid{
			Start: time.Unix(1000, 0).UTC(),
			End:   time.Unix(4600, 0).UTC(),
			Step:  time.Minute,
		}
	}
	if !g().Equal(g()) {
		t.Errorf("identical StepGrid should be Equal")
	}
}

// TestStepGrid_Equal_Negative_Fields exercises each `&&` conjunct in
// StepGrid.Equal singly. A `&&` → `||` flip would report Equal when two
// of the three fields match and one differs; single-field divergence
// catches each conjunct (Start, End, Step).
func TestStepGrid_Equal_Negative_Fields(t *testing.T) {
	t.Parallel()
	base := chplan.StepGrid{
		Start: time.Unix(1000, 0).UTC(),
		End:   time.Unix(4600, 0).UTC(),
		Step:  time.Minute,
	}
	cases := []struct {
		name   string
		mutate func(g *chplan.StepGrid)
	}{
		{"start", func(g *chplan.StepGrid) { g.Start = g.Start.Add(time.Second) }},
		{"end", func(g *chplan.StepGrid) { g.End = g.End.Add(time.Second) }},
		{"step", func(g *chplan.StepGrid) { g.Step = 2 * time.Minute }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := base
			b := base
			tc.mutate(&b)
			if a.Equal(&b) || b.Equal(&a) {
				t.Errorf("StepGrid.Equal must detect %s divergence", tc.name)
			}
		})
	}
}

// --- reanchor.go widen arithmetic + grid-prediction guards ---

// TestReanchorRange_LWRWidenArithmetic kills the membership-window widen
// math at reanchor.go:126 — `start.Add(-v.Offset - v.Lookback)`. The
// outer RangeLWR's Input is itself an (unpinned) RangeLWR, so the
// widened start is RECORDED as the inner node's Start and can be
// asserted exactly. Offset and Lookback are distinct non-zero durations
// so that:
//   - ARITHMETIC_BASE (`-` → `+`) on either subtraction shifts the
//     widened start by 2*Offset or 2*Lookback, and
//   - INVERT_NEGATIVES (`-v.Offset` → `v.Offset`, `-v.Lookback` →
//     `v.Lookback`) flips the sign of one term,
//
// all of which move the inner Start off the asserted value.
func TestReanchorRange_LWRWidenArithmetic(t *testing.T) {
	t.Parallel()

	const (
		offset   = 90 * time.Second
		lookback = 7 * time.Minute
	)
	inner := &chplan.RangeLWR{
		Input:         &chplan.Scan{Table: "metrics"},
		Step:          time.Minute, // Step > 0 so the inner participates in the grid
		Lookback:      2 * time.Minute,
		Offset:        0,
		MetricNameCol: "MetricName",
		AttributesCol: "Attributes",
		TimestampCol:  "TimeUnix",
		ValueCol:      "Value",
		// Start/End left zero → unpinned, accepted by the grid check.
	}
	outer := &chplan.RangeLWR{
		Input:         inner,
		Step:          time.Minute,
		Lookback:      lookback,
		Offset:        offset,
		MetricNameCol: "MetricName",
		AttributesCol: "Attributes",
		TimestampCol:  "TimeUnix",
		ValueCol:      "Value",
		// Outer unpinned too.
	}

	start := time.Unix(100_000, 0).UTC()
	end := time.Unix(200_000, 0).UTC()
	out, err := chplan.ReanchorRange(outer, start, end)
	if err != nil {
		t.Fatalf("ReanchorRange: %v", err)
	}
	gotOuter := out.(*chplan.RangeLWR)
	gotInner := gotOuter.Input.(*chplan.RangeLWR)

	// The inner spine is reanchored to start - Offset - Lookback (and end).
	wantInnerStart := start.Add(-offset - lookback)
	if !gotInner.Start.Equal(wantInnerStart) {
		t.Fatalf("inner widen start wrong: want %v (start - %v - %v), got %v",
			wantInnerStart, offset, lookback, gotInner.Start)
	}
	if !gotInner.End.Equal(end) {
		t.Fatalf("inner end wrong: want %v, got %v", end, gotInner.End)
	}
}

// TestReanchorRange_RejectsPartialPin kills the matrix-window grid guards
// at reanchor.go:185 (`Start.IsZero() && End.IsZero()`). A partially-
// pinned node — Start zero, End pinned OFF the predicted grid — must be
// rejected with ErrReanchorGridMismatch. With `&&` → `||` the unpinned
// early-return would fire (Start.IsZero() alone is enough), wrongly
// accepting the off-grid End.
func TestReanchorRange_RejectsPartialPin(t *testing.T) {
	t.Parallel()

	in := matrixWindowKill(5*time.Minute, time.Minute, time.Hour)
	in.Start = time.Time{}            // zero
	in.End = time.Unix(9999, 0).UTC() // pinned, NOT the predicted grid end

	start := time.Unix(1000, 0).UTC()
	end := time.Unix(4600, 0).UTC()
	if _, err := chplan.ReanchorRange(in, start, end); !errors.Is(err, chplan.ErrReanchorGridMismatch) {
		t.Fatalf("partially-pinned matrix window should be rejected, got %v", err)
	}
}

// TestReanchorRange_LWRRejectsPartialPinZeroStart kills the LWR
// unpinned guard at reanchor.go:209 (`Start.IsZero() && End.IsZero()`):
// Start zero, End off-grid → reject. `&&` → `||` would accept on the
// zero Start alone.
func TestReanchorRange_LWRRejectsPartialPinZeroStart(t *testing.T) {
	t.Parallel()

	in := lwrNodeKill(time.Time{}, time.Unix(9999, 0).UTC(), time.Minute, 5*time.Minute, 0)
	start := time.Unix(1000, 0).UTC()
	end := time.Unix(4600, 0).UTC()
	if _, err := chplan.ReanchorRange(in, start, end); !errors.Is(err, chplan.ErrReanchorGridMismatch) {
		t.Fatalf("LWR with zero Start + off-grid End should be rejected, got %v", err)
	}
}

// TestReanchorRange_LWRRejectsGriddedStartOffGridEnd kills the LWR
// already-gridded guard at reanchor.go:212 (`Start.Equal(predStart) &&
// End.Equal(predEnd)`): Start sits exactly on the predicted grid but End
// diverges → reject. With `&&` → `||` the matching Start alone would
// satisfy the guard and wrongly accept the off-grid End.
func TestReanchorRange_LWRRejectsGriddedStartOffGridEnd(t *testing.T) {
	t.Parallel()

	start := time.Unix(1000, 0).UTC()
	end := time.Unix(4600, 0).UTC()
	// Start == the predicted grid start; End pinned off-grid.
	in := lwrNodeKill(start, time.Unix(9999, 0).UTC(), time.Minute, 5*time.Minute, 0)
	if _, err := chplan.ReanchorRange(in, start, end); !errors.Is(err, chplan.ErrReanchorGridMismatch) {
		t.Fatalf("LWR with on-grid Start + off-grid End should be rejected, got %v", err)
	}
}

// matrixWindowKill / lwrNodeKill are local fixture builders for the
// reanchor kills above — kept self-contained in this file so the kill
// set does not depend on helper signatures in reanchor_test.go.
func matrixWindowKill(rang, step, outerRange time.Duration) *chplan.RangeWindow {
	return &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "metrics", Columns: []string{"Value", "TimeUnix"}},
		Func:            "rate",
		Range:           rang,
		Step:            step,
		OuterRange:      outerRange,
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
}

func lwrNodeKill(start, end time.Time, step, lookback, offset time.Duration) *chplan.RangeLWR {
	return &chplan.RangeLWR{
		Input:         &chplan.Scan{Table: "metrics", Columns: []string{"Value", "TimeUnix"}},
		Start:         start,
		End:           end,
		Step:          step,
		Lookback:      lookback,
		Offset:        offset,
		MetricNameCol: "MetricName",
		AttributesCol: "Attributes",
		TimestampCol:  "TimeUnix",
		ValueCol:      "Value",
	}
}

// -----------------------------------------------------------------------
// Nil-child guards on the two experimental grid nodes.
//
// RangeWindowResample and RangeWindowNative are the only Equal methods that
// tolerate a nil Input, and they do it with an explicit `r.Input == nil ||
// o.Input == nil` guard. TestNodeEqual_DiscriminatesEveryField cannot reach
// the guard — it compares fully-populated nodes — so the contract is pinned
// here: a nil Input on one side alone is a difference, and the comparison
// must answer it rather than dereference the nil.
// -----------------------------------------------------------------------

func TestRangeWindowResample_Equal_NilInputIsADifference(t *testing.T) {
	t.Parallel()

	populated := func(input chplan.Node) *chplan.RangeWindowResample {
		return &chplan.RangeWindowResample{
			Input:         input,
			Start:         time.Unix(1_700_000_000, 0).UTC(),
			End:           time.Unix(1_700_003_600, 0).UTC(),
			Step:          30 * time.Second,
			Lookback:      5 * time.Minute,
			MetricNameCol: "MetricName",
			AttributesCol: "Attributes",
			TimestampCol:  "TimeUnix",
			ValueCol:      "Value",
		}
	}

	withInput := populated(&chplan.Scan{Table: "metrics"})
	noInput := populated(nil)

	if withInput.Equal(noInput) {
		t.Error("a node with an Input must not equal one without")
	}
	if noInput.Equal(withInput) {
		t.Error("the nil-Input guard must answer symmetrically, not dereference the nil Input")
	}
	if !noInput.Equal(populated(nil)) {
		t.Error("two nil-Input nodes with identical scalars must be Equal")
	}
}

func TestRangeWindowNative_Equal_NilInputIsADifference(t *testing.T) {
	t.Parallel()

	populated := func(input chplan.Node) *chplan.RangeWindowNative {
		return &chplan.RangeWindowNative{
			Input:           input,
			Func:            "rate",
			Range:           5 * time.Minute,
			Step:            30 * time.Second,
			Start:           time.Unix(1_700_000_000, 0).UTC(),
			End:             time.Unix(1_700_003_600, 0).UTC(),
			TimestampColumn: "TimeUnix",
			ValueColumn:     "Value",
		}
	}

	withInput := populated(&chplan.Scan{Table: "metrics"})
	noInput := populated(nil)

	if withInput.Equal(noInput) {
		t.Error("a node with an Input must not equal one without")
	}
	if noInput.Equal(withInput) {
		t.Error("the nil-Input guard must answer symmetrically, not dereference the nil Input")
	}
	if !noInput.Equal(populated(nil)) {
		t.Error("two nil-Input nodes with identical scalars must be Equal")
	}
}

// -----------------------------------------------------------------------
// RewriteChildren's InfoJoin arm rebuilds on a ONE-SIDED change.
//
// The exhaustiveness table plants its sentinel in every child slot at once,
// so both sides always change together and the arm's `!inCh && !infoCh`
// early-out is never exercised with a mixed verdict. Widening that to `||`
// makes the arm drop a rewrite that reached only one side — the optimizer
// then silently discards a rule's result on the base vector or the info
// scan, whichever the other side did not also touch.
// -----------------------------------------------------------------------

func TestRewriteChildren_InfoJoin_KeepsAOneSidedRewrite(t *testing.T) {
	t.Parallel()

	base := &chplan.Scan{Table: "base"}
	info := &chplan.Scan{Table: "info"}
	rewritten := &chplan.Scan{Table: "rewritten"}

	rewriteOnly := func(table string) func(chplan.Node) (chplan.Node, bool) {
		return func(c chplan.Node) (chplan.Node, bool) {
			if s, ok := c.(*chplan.Scan); ok && s.Table == table {
				return rewritten, true
			}
			return c, false
		}
	}

	for _, tc := range []struct {
		name         string
		side         string
		wantInput    chplan.Node
		wantInfoNode chplan.Node
	}{
		{name: "input only", side: "base", wantInput: rewritten, wantInfoNode: info},
		{name: "info only", side: "info", wantInput: base, wantInfoNode: rewritten},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			in := &chplan.InfoJoin{Input: base, Info: info}
			got, changed := chplan.RewriteChildren(in, rewriteOnly(tc.side))
			if !changed {
				t.Fatalf("rewriting the %s side must report changed=true", tc.side)
			}
			out, ok := got.(*chplan.InfoJoin)
			if !ok {
				t.Fatalf("RewriteChildren returned %T, want *chplan.InfoJoin", got)
			}
			if out == in {
				t.Fatal("a changed InfoJoin must be rebuilt, not returned in place")
			}
			if out.Input != tc.wantInput {
				t.Errorf("Input = %#v, want %#v", out.Input, tc.wantInput)
			}
			if out.Info != tc.wantInfoNode {
				t.Errorf("Info = %#v, want %#v", out.Info, tc.wantInfoNode)
			}
		})
	}
}

// -----------------------------------------------------------------------
// inspectExpr's InSubquery arm.
//
// The arm carries the same `nodeVisit != nil && v.Subquery != nil` guard as
// the ScalarSubquery arm above it, but only ScalarSubquery had a test. Each
// half of the guard is load-bearing: InspectExpr passes a nil nodeVisit, and
// an InSubquery built by a partially-lowered plan can carry a nil Subquery.
// -----------------------------------------------------------------------

func TestInspectExprNodes_InSubquery_SurfacesTheSubqueryPlan(t *testing.T) {
	t.Parallel()

	inner := &chplan.Scan{Table: "trace_ids"}
	expr := &chplan.InSubquery{Left: &chplan.ColumnRef{Name: "TraceId"}, Subquery: inner}

	var seen []chplan.Node
	chplan.InspectExprNodes(expr, func(chplan.Expr) bool { return true }, func(n chplan.Node) {
		seen = append(seen, n)
	})

	if len(seen) != 1 || seen[0] != chplan.Node(inner) {
		t.Fatalf("nodeVisit saw %#v, want exactly the InSubquery's embedded plan Node", seen)
	}
}

func TestInspectExprNodes_InSubquery_SkipsANilSubquery(t *testing.T) {
	t.Parallel()

	expr := &chplan.InSubquery{Left: &chplan.ColumnRef{Name: "TraceId"}}

	chplan.InspectExprNodes(expr, func(chplan.Expr) bool { return true }, func(n chplan.Node) {
		t.Fatalf("nodeVisit must not be invoked for a nil Subquery; got %#v", n)
	})
}

func TestInspectExpr_InSubquery_StaysExprOnlyWithoutANodeVisitor(t *testing.T) {
	t.Parallel()

	expr := &chplan.InSubquery{
		Left:     &chplan.ColumnRef{Name: "TraceId"},
		Subquery: &chplan.Scan{Table: "trace_ids"},
	}

	// InspectExpr supplies a nil nodeVisit. The guard is what stops the arm
	// calling through it; without it this panics rather than fails.
	var visited []chplan.Expr
	chplan.InspectExpr(expr, func(e chplan.Expr) bool {
		visited = append(visited, e)
		return true
	})

	if len(visited) != 2 {
		t.Fatalf("visited %d expressions, want the InSubquery and its Left operand", len(visited))
	}
}

// -----------------------------------------------------------------------
// CanonicalAttributesExpr's idempotence check.
//
// The check tests the call's NAME, so it must wrap every OTHER Map-valued
// call and skip only an already-canonical one. Inverting it turns the
// helper into a no-op for exactly the calls that need the wrap.
// -----------------------------------------------------------------------

func TestCanonicalAttributesExpr_WrapsAnotherMapValuedCall(t *testing.T) {
	t.Parallel()

	inner := &chplan.FuncCall{
		Name: "mapConcat",
		Args: []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}

	got := chplan.CanonicalAttributesExpr(inner)

	call, ok := got.(*chplan.FuncCall)
	if !ok || call.Name != chplan.CanonicalMapFunc {
		t.Fatalf("got %#v, want a %s call wrapping the mapConcat", got, chplan.CanonicalMapFunc)
	}
	if len(call.Args) != 1 || call.Args[0] != chplan.Expr(inner) {
		t.Fatalf("%s args = %#v, want exactly the wrapped expression", chplan.CanonicalMapFunc, call.Args)
	}
}

func TestCanonicalAttributesExpr_LeavesACanonicalCallAlone(t *testing.T) {
	t.Parallel()

	inner := &chplan.FuncCall{
		Name: chplan.CanonicalMapFunc,
		Args: []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}

	if got := chplan.CanonicalAttributesExpr(inner); got != chplan.Expr(inner) {
		t.Fatalf("got %#v, want the already-canonical call back by identity", got)
	}
}
