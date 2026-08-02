package promql

import (
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/parser"

	"github.com/tsouza/cerberus/internal/chplan"
	"github.com/tsouza/cerberus/internal/schema"
)

// The subquery evaluation grid is the whole of `<expr>[<range>:<step>]`
// semantics: WHICH instants the inner expression is re-evaluated at.
// Every anchor-dependent sub-expression — `time()`, an aggregation's
// per-step bucket, a comparison filter — reads it, so an off-by-one
// anchor or a grid snapped to the wrong phase is a wrong answer rather
// than a slow one. These tests pin the derivation directly, on the
// unexported helpers, because the shapes that distinguish the branches
// (a `@` pin whose instant differs from the query end, a pre-1970
// anchor, a sub-step window) are awkward to reach through a fixture.

// gridTestQueryStart / gridTestQueryEnd deliberately sit OFF the 1m
// subquery step so every assertion below observes the epoch snapping;
// a whole-minute query window would make floor() invisible.
var (
	gridTestQueryStart = time.Date(2026, time.January, 1, 0, 10, 7, 0, time.UTC)
	gridTestQueryEnd   = time.Date(2026, time.January, 1, 1, 4, 41, 0, time.UTC)
)

const (
	gridTestQueryStep = 30 * time.Second
	gridTestSubStep   = time.Minute
)

func TestEpochFloorSnapsToAbsoluteEpochMultiples(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   time.Time
		step time.Duration
		want time.Time
	}{
		{
			name: "mid-step snaps down",
			in:   time.Date(2026, time.January, 1, 1, 4, 41, 0, time.UTC),
			step: time.Minute,
			want: time.Date(2026, time.January, 1, 1, 4, 0, 0, time.UTC),
		},
		{
			name: "already on a multiple is unchanged",
			in:   time.Date(2026, time.January, 1, 1, 4, 0, 0, time.UTC),
			step: time.Minute,
			want: time.Date(2026, time.January, 1, 1, 4, 0, 0, time.UTC),
		},
		{
			name: "sub-second remainder snaps down",
			in:   time.Date(2026, time.January, 1, 1, 4, 0, 1, time.UTC),
			step: time.Minute,
			want: time.Date(2026, time.January, 1, 1, 4, 0, 0, time.UTC),
		},
		{
			name: "epoch itself is a multiple",
			in:   time.Unix(0, 0).UTC(),
			step: time.Minute,
			want: time.Unix(0, 0).UTC(),
		},
		{
			// Go's integer division truncates toward zero, so a pre-1970
			// instant needs the explicit decrement: -1.5s must floor to
			// -2s, not to -1s. `@ -100` is legal PromQL, so this is a
			// reachable shape, not a hypothetical.
			name: "pre-epoch mid-step floors away from zero",
			in:   time.Unix(0, -1_500_000_000).UTC(),
			step: time.Second,
			want: time.Unix(-2, 0).UTC(),
		},
		{
			// The mirror of the case above: an exact pre-1970 multiple
			// must NOT be decremented — it is already an anchor.
			name: "pre-epoch multiple is unchanged",
			in:   time.Unix(-2, 0).UTC(),
			step: time.Second,
			want: time.Unix(-2, 0).UTC(),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := epochFloor(tc.in, tc.step)
			if !got.Equal(tc.want) {
				t.Errorf("epochFloor(%s, %s) = %s, want %s", tc.in, tc.step, got, tc.want)
			}
		})
	}
}

func TestSubqueryGridCtxDerivesTheAnchorGrid(t *testing.T) {
	t.Parallel()

	// The `@` pin below is deliberately far from both query bounds, so a
	// grid that ignored the pin lands on visibly different anchors.
	pin := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	pinMillis := pin.UnixMilli()

	rangeCtx := lowerCtx{start: gridTestQueryStart, end: gridTestQueryEnd, step: gridTestQueryStep}
	instantCtx := lowerCtx{end: gridTestQueryEnd}

	cases := []struct {
		name  string
		query string
		// pinAt, when non-nil, is spliced into the parsed subquery as an
		// `@ <ts>` modifier. Written as a field rather than into the
		// query text so the instant stays a named time.Time.
		pinAt     *int64
		ctx       lowerCtx
		wantStart time.Time
		wantEnd   time.Time
	}{
		{
			// Range mode widens the grid to [start-Range, end] so every
			// outer anchor's lookback finds inner anchors.
			name:      "range mode covers every outer anchor's lookback",
			query:     `up[5m:1m]`,
			ctx:       rangeCtx,
			wantStart: time.Date(2026, time.January, 1, 0, 6, 0, 0, time.UTC),
			wantEnd:   time.Date(2026, time.January, 1, 1, 4, 0, 0, time.UTC),
		},
		{
			// A `@` pin overrides the range-mode window entirely: the
			// subquery evaluates its own [pin-Range, pin], not the
			// query's.
			name:      "@ timestamp pin wins over the range-mode window",
			query:     `up[5m:1m]`,
			pinAt:     &pinMillis,
			ctx:       rangeCtx,
			wantStart: time.Date(2025, time.December, 31, 23, 56, 0, 0, time.UTC),
			wantEnd:   pin,
		},
		{
			// `@ start()` pins to the query start — same override path,
			// reached through StartOrEnd rather than Timestamp.
			name:      "@ start() pins to the query start",
			query:     `up[5m:1m] @ start()`,
			ctx:       rangeCtx,
			wantStart: time.Date(2026, time.January, 1, 0, 6, 0, 0, time.UTC),
			wantEnd:   time.Date(2026, time.January, 1, 0, 10, 0, 0, time.UTC),
		},
		{
			// Instant eval has no query start; the grid is the
			// subquery's own [end-Range, end] window.
			name:      "instant eval anchors on the eval timestamp",
			query:     `up[5m:1m]`,
			ctx:       instantCtx,
			wantStart: time.Date(2026, time.January, 1, 1, 0, 0, 0, time.UTC),
			wantEnd:   time.Date(2026, time.January, 1, 1, 4, 0, 0, time.UTC),
		},
		{
			// `offset` shifts WHICH instants are evaluated, and the grid
			// is snapped AFTER the shift so anchors stay on phase 0.
			name:      "offset shifts the whole grid, snapped after the shift",
			query:     `up[5m:1m] offset 2m`,
			ctx:       rangeCtx,
			wantStart: time.Date(2026, time.January, 1, 0, 4, 0, 0, time.UTC),
			wantEnd:   time.Date(2026, time.January, 1, 1, 2, 0, 0, time.UTC),
		},
		{
			// A window narrower than one step would otherwise produce
			// start > end; reference clamps to the single snapped anchor.
			name:      "sub-step window clamps to one anchor",
			query:     `up[1s:1m]`,
			ctx:       instantCtx,
			wantStart: time.Date(2026, time.January, 1, 1, 4, 0, 0, time.UTC),
			wantEnd:   time.Date(2026, time.January, 1, 1, 4, 0, 0, time.UTC),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			sub := parseSubquery(t, tc.query)
			if tc.pinAt != nil {
				sub.Timestamp = tc.pinAt
			}
			got, ok, err := subqueryGridCtx(sub, gridTestSubStep, tc.ctx)
			if err != nil {
				t.Fatalf("subqueryGridCtx(%q): %v", tc.query, err)
			}
			if !ok {
				t.Fatalf("subqueryGridCtx(%q) reported no grid, want one", tc.query)
			}
			if !got.start.Equal(tc.wantStart) {
				t.Errorf("grid start = %s, want %s", got.start, tc.wantStart)
			}
			if !got.end.Equal(tc.wantEnd) {
				t.Errorf("grid end = %s, want %s", got.end, tc.wantEnd)
			}
			if got.step != gridTestSubStep {
				t.Errorf("grid step = %s, want %s", got.step, gridTestSubStep)
			}
			if got.inRangeVector {
				t.Error("grid context is in range-vector mode; the inner is evaluated as an INSTANT query per anchor")
			}
		})
	}
}

// TestSubqueryGridCtxNeedsACompleteEvalWindow pins the fallback: without
// a full [start, end, step] triple AND no `@` pin there is no anchor to
// build a grid from, and the caller must keep the raw-sample-stream
// lowering rather than derive a grid off a zero time.
func TestSubqueryGridCtxNeedsACompleteEvalWindow(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		ctx  lowerCtx
	}{
		{
			name: "no eval context at all",
			ctx:  lowerCtx{},
		},
		{
			// A step without bounds is not a range window. Deriving one
			// anyway would take the grid start from the zero time.
			name: "step without query bounds",
			ctx:  lowerCtx{step: gridTestQueryStep, end: gridTestQueryEnd},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, ok, err := subqueryGridCtx(parseSubquery(t, `up[5m:1m]`), gridTestSubStep, tc.ctx)
			if err != nil {
				t.Fatalf("subqueryGridCtx: %v", err)
			}
			if ok {
				t.Fatalf("subqueryGridCtx reported a grid [%s, %s], want the raw-stream fallback", got.start, got.end)
			}
		})
	}
}

// TestSubqueryGridCtxPropagatesAnchorErrors pins that an unusable
// modifier surfaces as an error rather than as a silently grid-free
// lowering — `@ start()` without a query range has no instant to pin to.
func TestSubqueryGridCtxPropagatesAnchorErrors(t *testing.T) {
	t.Parallel()

	_, ok, err := subqueryGridCtx(parseSubquery(t, `up[5m:1m] @ start()`), gridTestSubStep, lowerCtx{})
	if err == nil {
		t.Fatalf("subqueryGridCtx(`@ start()`, no range context) returned ok=%v, want an error", ok)
	}
}

// TestLowerSubqueryRejectsNonPositiveRange pins the range guard. A
// zero-range subquery has no window to evaluate anchors across; lowering
// it would emit a query whose window is empty by construction.
func TestLowerSubqueryRejectsNonPositiveRange(t *testing.T) {
	t.Parallel()

	for _, r := range []time.Duration{0, -time.Minute} {
		sub := &parser.SubqueryExpr{
			Expr:  &parser.VectorSelector{Name: "up"},
			Range: r,
			Step:  gridTestSubStep,
		}
		plan, err := lowerSubquery(sub, schema.DefaultOTelMetrics(), lowerCtx{
			start:    gridTestQueryStart,
			end:      gridTestQueryEnd,
			step:     gridTestQueryStep,
			lowerers: RangeLowerers{}.withDefaults(),
		})
		if err == nil {
			t.Errorf("lowerSubquery(range=%s) lowered to %T, want an error", r, plan)
		}
	}
}

// TestWidenSubquerySpineSkipsInstantWindows pins the spine walk's
// terminator. An instant-shape RangeWindow (Step == 0) resolves a single
// anchor by contract; re-anchoring it across the range-mode window would
// fan a per-statement subplan out over the whole query range.
func TestWidenSubquerySpineSkipsInstantWindows(t *testing.T) {
	t.Parallel()

	instant := &chplan.RangeWindow{Input: &chplan.Scan{}, Range: 5 * time.Minute}
	widenSubquerySpine(instant, gridTestQueryStart, gridTestQueryEnd)

	if !instant.Start.IsZero() || !instant.End.IsZero() {
		t.Errorf("instant RangeWindow was re-anchored to [%s, %s], want it left alone", instant.Start, instant.End)
	}
	if instant.StepAlign {
		t.Error("instant RangeWindow was epoch-aligned; only matrix windows carry a subquery grid")
	}

	matrix := &chplan.RangeWindow{Input: &chplan.Scan{}, Range: 5 * time.Minute, Step: gridTestSubStep}
	widenSubquerySpine(matrix, gridTestQueryStart, gridTestQueryEnd)

	if !matrix.Start.Equal(gridTestQueryStart) || !matrix.End.Equal(gridTestQueryEnd) {
		t.Errorf("matrix RangeWindow anchored to [%s, %s], want [%s, %s]",
			matrix.Start, matrix.End, gridTestQueryStart, gridTestQueryEnd)
	}
	if !matrix.StepAlign {
		t.Error("matrix RangeWindow is not epoch-aligned; PromQL evaluates inner samples on a phase-0 grid")
	}
}

// TestSubqueryOverAbsentGridCoversEveryOuterAnchor pins where
// `absent(<v>)[5m:1m]` starts fanning its per-anchor absence indicator.
// In range mode the grid has to reach back a full subquery range before
// the QUERY start, or the leading outer anchors have no inner anchors to
// reduce and the series silently begins late; instant eval has no query
// start and reaches back from the eval timestamp instead.
func TestSubqueryOverAbsentGridCoversEveryOuterAnchor(t *testing.T) {
	t.Parallel()

	const query = `absent(up)[5m:1m]`
	const subRange = 5 * time.Minute

	cases := []struct {
		name string
		ctx  lowerCtx
		want time.Time
	}{
		{
			name: "range mode reaches back from the query start",
			ctx:  lowerCtx{start: gridTestQueryStart, end: gridTestQueryEnd, step: gridTestQueryStep},
			want: gridTestQueryStart.Add(-subRange),
		},
		{
			name: "instant eval reaches back from the eval timestamp",
			ctx:  lowerCtx{end: gridTestQueryEnd},
			want: gridTestQueryEnd.Add(-subRange),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := tc.ctx
			ctx.lowerers = RangeLowerers{}.withDefaults()
			plan, err := lowerSubquery(parseSubquery(t, query), schema.DefaultOTelMetrics(), ctx)
			if err != nil {
				t.Fatalf("lowerSubquery(%q): %v", query, err)
			}
			project, ok := plan.(*chplan.Project)
			if !ok {
				t.Fatalf("lowered %q to %T, want *chplan.Project", query, plan)
			}
			absent, ok := project.Input.(*chplan.AbsentOverTime)
			if !ok {
				t.Fatalf("lowered %q over %T, want *chplan.AbsentOverTime", query, project.Input)
			}
			if !absent.Start.Equal(tc.want) {
				t.Errorf("AbsentOverTime.Start = %s, want %s", absent.Start, tc.want)
			}
			if !absent.End.Equal(tc.ctx.end) {
				t.Errorf("AbsentOverTime.End = %s, want %s", absent.End, tc.ctx.end)
			}
		})
	}
}

func parseSubquery(t *testing.T, query string) *parser.SubqueryExpr {
	t.Helper()

	expr, err := parser.NewParser(parser.Options{}).ParseExpr(query)
	if err != nil {
		t.Fatalf("ParseExpr(%q): %v", query, err)
	}
	sub, ok := expr.(*parser.SubqueryExpr)
	if !ok {
		t.Fatalf("ParseExpr(%q) returned %T, want *parser.SubqueryExpr", query, expr)
	}
	return sub
}
