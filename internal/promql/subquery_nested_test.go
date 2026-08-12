package promql_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/promql"
	"github.com/tsouza/cerberus/internal/schema"
)

// TestLowerSubquery_DirectNested exercises
// `lowerSubqueryOverSubquery`, the defensive branch for a
// programmatically-constructed AST where `SubqueryExpr.Expr` is itself
// a `*parser.SubqueryExpr`. PromQL's parser type system forbids this
// shape — a SubqueryExpr produces a range vector, and a SubqueryExpr's
// body must be an instant vector — so this branch is unreachable
// through parsed PromQL. The test constructs the AST directly so the
// lowering path stays exercised in case an optimizer rewrite or future
// parser change produces this shape.
func TestLowerSubquery_DirectNested(t *testing.T) {
	t.Parallel()

	// Build `(rate(m[1m])[5m:30s])[1h:5m]` directly, skipping the
	// parser's range-vector-on-subquery check.
	innerMatrix := &parser.MatrixSelector{
		VectorSelector: &parser.VectorSelector{
			Name:          "m",
			LabelMatchers: []*labels.Matcher{labels.MustNewMatcher(labels.MatchEqual, "__name__", "m")},
		},
		Range: time.Minute,
	}
	rate := &parser.Call{
		Func: parser.MustGetFunction("rate"),
		Args: parser.Expressions{innerMatrix},
	}
	innerSub := &parser.SubqueryExpr{
		Expr:  rate,
		Range: 5 * time.Minute,
		Step:  30 * time.Second,
	}
	outerSub := &parser.SubqueryExpr{
		Expr:  innerSub,
		Range: time.Hour,
		Step:  5 * time.Minute,
	}

	plan, err := promql.Lower(context.Background(), outerSub, schema.DefaultOTelMetrics())
	if err != nil {
		t.Fatalf("Lower(direct-nested SubqueryExpr): %v", err)
	}

	// Outer is an Identity-mode RangeWindow with Step=5m and
	// OuterRange=1h. Inner widens to outer.Range + inner.Range = 1h5m
	// so every outer anchor's lookback finds inner anchors.
	outer, ok := plan.(*chplan.RangeWindow)
	if !ok {
		t.Fatalf("outer plan = %T; want *chplan.RangeWindow", plan)
	}
	if !outer.Identity {
		t.Errorf("outer.Identity = false; want true (no reducer for bare nested subquery)")
	}
	if outer.OuterRange != time.Hour {
		t.Errorf("outer.OuterRange = %v; want 1h", outer.OuterRange)
	}
	if outer.Step != 5*time.Minute {
		t.Errorf("outer.Step = %v; want 5m", outer.Step)
	}
	if outer.TimestampColumn != "anchor_ts" {
		t.Errorf("outer.TimestampColumn = %q; want anchor_ts (consumes inner matrix grid)", outer.TimestampColumn)
	}

	innerRW, ok := outer.Input.(*chplan.RangeWindow)
	if !ok {
		t.Fatalf("inner plan = %T; want *chplan.RangeWindow", outer.Input)
	}
	if innerRW.Func != "rate" {
		t.Errorf("inner.Func = %q; want rate", innerRW.Func)
	}
	if innerRW.OuterRange != 65*time.Minute {
		t.Errorf("inner.OuterRange = %v; want 65m (sub.Range + innerSub.Range widening)", innerRW.OuterRange)
	}
	if innerRW.Step != 30*time.Second {
		t.Errorf("inner.Step = %v; want 30s", innerRW.Step)
	}
}

// TestLowerSubquery_DirectNested_ZeroRange pins the zero-range
// rejection in `lowerSubqueryOverSubquery` — the inner subquery's
// range must be positive, mirroring `lowerSubqueryOverCallSubquery`.
func TestLowerSubquery_DirectNested_ZeroRange(t *testing.T) {
	t.Parallel()

	vs := &parser.VectorSelector{
		Name:          "m",
		LabelMatchers: []*labels.Matcher{labels.MustNewMatcher(labels.MatchEqual, "__name__", "m")},
	}
	innerSub := &parser.SubqueryExpr{Expr: vs, Range: 0, Step: 30 * time.Second}
	outerSub := &parser.SubqueryExpr{Expr: innerSub, Range: time.Hour, Step: 5 * time.Minute}

	_, err := promql.Lower(context.Background(), outerSub, schema.DefaultOTelMetrics())
	if err == nil {
		t.Fatal("Lower(direct-nested zero-range): want error, got nil")
	}
	if !strings.Contains(err.Error(), "inner subquery range must be positive") {
		t.Errorf("Lower error = %q; want 'inner subquery range must be positive'", err)
	}
}

// TestLowerSubquery_CallNested_NonPositiveRange pins the same
// positive-inner-range guard on the OTHER nested shape,
// `<fn>(<inner-sub>)[<outer-range>:<step>]`. Both shapes widen the
// inner range to `sub.Range + innerSub.Range`, so a non-positive inner
// range degenerates the widening — every outer anchor would reduce over
// the identical sample set instead of its own lookback slice. PromQL's
// parser rejects a non-positive range literal, so the AST is built
// directly, matching the defensive-branch style above.
func TestLowerSubquery_CallNested_NonPositiveRange(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		iRange time.Duration
	}{
		{name: "zero", iRange: 0},
		{name: "negative", iRange: -time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			matrix := &parser.MatrixSelector{
				VectorSelector: &parser.VectorSelector{
					Name:          "m",
					LabelMatchers: []*labels.Matcher{labels.MustNewMatcher(labels.MatchEqual, "__name__", "m")},
				},
				Range: time.Minute,
			}
			innerSub := &parser.SubqueryExpr{
				Expr:  &parser.Call{Func: parser.MustGetFunction("rate"), Args: parser.Expressions{matrix}},
				Range: tc.iRange,
				Step:  30 * time.Second,
			}
			outerSub := &parser.SubqueryExpr{
				Expr:  &parser.Call{Func: parser.MustGetFunction("max_over_time"), Args: parser.Expressions{innerSub}},
				Range: time.Hour,
				Step:  5 * time.Minute,
			}

			_, err := promql.Lower(context.Background(), outerSub, schema.DefaultOTelMetrics())
			if err == nil {
				t.Fatalf("Lower(max_over_time over %v-range subquery): want error, got nil", tc.iRange)
			}
			if !strings.Contains(err.Error(), "inner subquery range must be positive") {
				t.Errorf("Lower error = %q; want 'inner subquery range must be positive'", err)
			}
		})
	}
}

// nestedOffsetProbe is the shape the audit below is about:
// `<reducer>(<inner-sub>)[<outer-range>:<step>]`, whose lowering is
// lowerSubqueryOverCallSubquery. innerSubRange / midRange name the two
// windows so the widening arithmetic can be written out rather than
// hard-coded.
const (
	nestedInnerSubRange = 5 * time.Minute  // the `[5m:30s]` inner subquery
	nestedInnerFnRange  = time.Minute      // the `rate(m[1m])` matrix selector
	nestedOuterSubRange = time.Hour        // the `[1h:5m]` outer subquery
	nestedOffset        = 10 * time.Minute // the outer subquery's `offset`
)

// collectRangeWindows returns every *chplan.RangeWindow in the plan in
// depth-first order, outermost first.
func collectRangeWindows(n chplan.Node) []*chplan.RangeWindow {
	var out []*chplan.RangeWindow
	var walk func(chplan.Node)
	walk = func(node chplan.Node) {
		if node == nil {
			return
		}
		if rw, ok := node.(*chplan.RangeWindow); ok {
			out = append(out, rw)
		}
		for _, c := range node.Children() {
			walk(c)
		}
	}
	walk(n)
	return out
}

// TestLowerNestedSubquery_OuterOffsetReachesInnerSpine is the REFUTATION pin for
// issue #1732's item 4, which suspected the one live wrong-answer left in the
// defect-D2 family.
//
// The suspicion: lowerSubqueryOverCallSubquery (and its parser-unreachable
// sibling lowerSubqueryOverSubquery) build their widened inner grid with
// `lowerSubquery(&widened, s, ctx)` using a ctx that was never adjusted for the
// OUTER subquery's own Offset. Read locally that is true — and it is also
// harmless, because neither function is the last pass over that spine.
//
// Every reachable path re-anchors the spine afterwards, and the re-anchoring
// carries the Offset through chplan.RangeWindow.InputWindow — the single owner
// #1464 introduced:
//
//   - A bare top-level instant subquery is widened by lower()'s SubqueryExpr
//     arm (internal/promql/lower.go), which calls widenSubquerySpine.
//   - A subquery under an outer range-vector reducer is widened by
//     lowerOuterRangeFnOverSubquery's three arms, likewise.
//
// widenSubquerySpine overwrites the middle RangeWindow's Start/End/OuterRange
// and then recurses via `v.InputWindow(start, end)` — i.e. `start - Offset -
// Range`. So the outer offset reaches the inner grid through the middle node's
// own Offset, one level down, rather than through the ctx the lowering used.
//
// This asserts that end to end and in the ONE form that cannot pass by
// accident: the inner spine's Start must move back by EXACTLY the outer
// subquery's offset when the offset is added, and the middle node's bounds must
// satisfy InputWindow exactly. A widening that dropped the Offset term leaves
// the inner Start unmoved and fails the first check; one that widened by some
// other amount fails the second.
//
// It also closes the issue's second question — "neither function sets rw.Start,
// is that a range-mode gap?" — in the negative: Start is populated on every
// node here, filled by the same re-anchoring pass, exactly as the well-covered
// lowerSubqueryOverVectorSelector sibling relies on.
func TestLowerNestedSubquery_OuterOffsetReachesInnerSpine(t *testing.T) {
	t.Parallel()

	const (
		plain     = `max_over_time(rate(demo_cpu[1m])[5m:30s])[1h:5m]`
		offsetted = `max_over_time(rate(demo_cpu[1m])[5m:30s])[1h:5m] offset 10m`
	)
	s := schema.DefaultOTelMetrics()
	evalTS := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

	// The two reachable contexts for a nested call-subquery. A range query over
	// a matrix-typed expression is rejected upstream, so the bare form is
	// instant-only; the reducer form covers both.
	for _, mode := range []struct {
		name string
		// wrap turns the subquery source into the query actually lowered.
		wrap  func(string) string
		lower func(parser.Expr) (chplan.Node, error)
		// midIdx is the index of the nested-call RangeWindow in depth-first
		// order: the bare form IS it, the reducer form has the reducer above it.
		midIdx int
	}{
		{
			name:   "bare top-level instant subquery",
			wrap:   func(q string) string { return q },
			midIdx: 0,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, evalTS, evalTS)
			},
		},
		{
			name:   "under an outer reducer, instant",
			wrap:   func(q string) string { return "max_over_time(" + q + ")" },
			midIdx: 1,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAt(context.Background(), e, s, evalTS, evalTS)
			},
		},
		{
			name:   "under an outer reducer, range",
			wrap:   func(q string) string { return "max_over_time(" + q + ")" },
			midIdx: 1,
			lower: func(e parser.Expr) (chplan.Node, error) {
				return promql.LowerAtRange(
					context.Background(), e, s, evalTS.Add(-2*time.Hour), evalTS, time.Minute,
				)
			},
		},
	} {
		mode := mode
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()

			innerStart := make(map[string]time.Time, 2)
			for _, tc := range []struct {
				name  string
				query string
			}{
				{"no offset", mode.wrap(plain)},
				{"offset 10m", mode.wrap(offsetted)},
			} {
				expr, err := parser.NewParser(parser.Options{}).ParseExpr(tc.query)
				if err != nil {
					t.Fatalf("ParseExpr(%q): %v", tc.query, err)
				}
				plan, err := mode.lower(expr)
				if err != nil {
					t.Fatalf("lower(%q): %v", tc.query, err)
				}

				windows := collectRangeWindows(plan)
				if len(windows) != mode.midIdx+2 {
					t.Fatalf("lowered %q to %d RangeWindows, want %d — the nested shape no longer "+
						"routes through lowerSubqueryOverCallSubquery and this pin has no subject",
						tc.query, len(windows), mode.midIdx+2)
				}
				mid, inner := windows[mode.midIdx], windows[mode.midIdx+1]

				// The middle node is the nested-call window: its per-anchor
				// window is the INNER subquery's range, fanned across the OUTER
				// subquery's range.
				if mid.Range != nestedInnerSubRange || mid.OuterRange <= 0 {
					t.Fatalf("middle RangeWindow is range=%s outerRange=%s, want range=%s and a "+
						"non-zero outer range — the fixture is not the nested shape",
						mid.Range, mid.OuterRange, nestedInnerSubRange)
				}
				if mid.Start.IsZero() || mid.End.IsZero() {
					t.Errorf("middle RangeWindow bounds are (%s, %s); both must be filled by the "+
						"re-anchoring pass, which is what the lowering leaves to it", mid.Start, mid.End)
				}

				// (1) The structural relation: the inner spine reaches back
				// Offset+Range from the middle node's own start — chplan's
				// RangeWindow.InputWindow, applied by widenSubquerySpine.
				wantInner := mid.Start.Add(-mid.Offset - mid.Range)
				if !inner.Start.Equal(wantInner) {
					t.Errorf("[%s] inner spine Start = %s, want %s (mid.Start - Offset - Range) — "+
						"the widening pass is not carrying the outer offset down through "+
						"RangeWindow.InputWindow", tc.name, inner.Start, wantInner)
				}
				if inner.Range != nestedInnerFnRange {
					t.Errorf("[%s] inner spine range = %s, want %s", tc.name, inner.Range, nestedInnerFnRange)
				}

				// (2) The SEMANTIC property, and the one a wrong answer would
				// break: every anchor the middle node actually emits must find
				// its whole window on the inner spine. A matrix RangeWindow
				// emits anchors across [End - Offset - OuterRange, End -
				// Offset] (the Offset shifts the anchors themselves — see
				// test/spec/promql/subquery_offset.txtar, whose emitted
				// timestamps are the shifted ones), and each anchor reduces
				// `(anchor - Range, anchor]`. So the oldest sample any anchor
				// needs sits at:
				oldestAnchor := mid.End.Add(-mid.Offset - mid.OuterRange)
				deepestRead := oldestAnchor.Add(-mid.Range)
				if inner.Start.After(deepestRead) {
					t.Errorf("[%s] inner spine starts at %s but the middle node's oldest anchor "+
						"(%s) reduces back to %s — the leading anchors reduce over a truncated "+
						"window and report a wrong value", tc.name, inner.Start, oldestAnchor, deepestRead)
				}
				innerStart[tc.name] = inner.Start
			}

			// Non-vacuity: the outer subquery's offset must REACH the inner
			// grid. If the un-adjusted ctx that issue #1732 item 4 flagged were
			// the last word on these bounds, the two would be identical.
			//
			// The size of the shift is deliberately not pinned to the offset
			// itself: under an outer reducer the offset legitimately arrives
			// twice over — once because the reducer's own widen extends the
			// middle node's OuterRange to cover the shifted anchor range it
			// selects, and once because the middle node's Offset then moves its
			// emitted anchors a further Offset back. Coverage above is the
			// property that actually has to hold; this only proves the offset
			// is not being dropped.
			if shift := innerStart["no offset"].Sub(innerStart["offset 10m"]); shift <= 0 {
				t.Errorf("adding `offset 10m` moved the inner spine Start by %s — a zero or forward "+
					"shift is exactly the live bug issue #1732 item 4 suspected", shift)
			}
		})
	}
}
