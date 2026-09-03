package chsql

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// This file kills the LIVED gremlins mutants in the range_window cluster
// (phase-2 mutation lane, ./internal/chsql, 95% efficacy floor). Each test
// is white-box (package chsql) so it can reach unexported emitter methods
// where the public Emit surface alone wouldn't isolate the boundary.
//
// Mutant inventory (construct → what the flip does):
//   - range_window.go:`InlineLit(sf), InlineLit(1-sf)` and
//     range_window.go:`InlineLit(tf), InlineLit(1-tf)` — `1 - sf` / `1 - tf` →
//     `1 + sf` / `1 + tf` (holt_winters smoothing weights): wrong (1∓w) factor
//     in the recurrence.
//   - range_window.go:emitWindowedArrayPairsMatrix:`minWindowSize > 0` → `>= 0`:
//     spuriously emits the window-length WHERE filter when no minimum was
//     requested.
//   - range_window.go, emitRangeWindowOverTimeDirect — `r.Step <= 0` → `< 0`: stops
//     rejecting OuterRange>0 subqueries that forgot Step (would divide by zero /
//     emit a degenerate grid).
//   - range_window.go, emitRangeWindowOverTimeDirect — `handled || err != nil` →
//     `handled && err != nil`: a successful fused emit (handled=true, err=nil) would
//     fall through to the materialized path, double-emitting / changing SQL.
//   - range_window.go, emitRangeWindowOverTimeDirectMatrix — `len(groupFrags)+1` →
//     `len(groupFrags)-1`: with no GroupBy this is `make([]Frag, 0, -1)` → runtime
//     panic (cap out of range).
//   - range_window_fused.go, tryEmitFusedSubquery — the outer-shape guard
//     (`instantOuter` / `matrixOuter`) that decides which shapes fuse at all.

func mutTestStart() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

// fusibleInner returns a fully-valid extrapolating MATRIX RangeWindow that
// satisfies every gate in tryEmitFusedSubquery (Func extrapolating,
// OuterRange>0, Step>0, columns set, Start/End non-zero) so that IF the
// instant-only self-guard passes, the fused emit returns handled=true.
func fusibleInner() *chplan.RangeWindow {
	start := mutTestStart()
	return &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_sum"},
		Func:            "rate",
		Range:           5 * time.Minute,
		OuterRange:      10 * time.Minute,
		Step:            1 * time.Minute,
		Start:           start,
		End:             start.Add(10 * time.Minute),
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
}

// fusedOuter wraps fusibleInner in an instant outer reducer (max_over_time)
// — the exact `max_over_time(rate(m[5m])[10m:1m])` instant-subquery shape.
func fusedOuter() *chplan.RangeWindow {
	start := mutTestStart()
	return &chplan.RangeWindow{
		Input:           fusibleInner(),
		Func:            "max_over_time",
		Range:           10 * time.Minute,
		Start:           start,
		End:             start.Add(10 * time.Minute),
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
	}
}

// TestHoltWintersSmoothingWeightsEmit kills the flips on
// range_window.go:`InlineLit(sf), InlineLit(1-sf)` and
// range_window.go:`InlineLit(tf), InlineLit(1-tf)`.
// With sf=0.25, tf=0.125 the (1−w) weights render as the exact literals
// 0.75 and 0.875; the ARITHMETIC_BASE / INVERT_NEGATIVES flips (1+w) would
// render 1.25 / 1.125 instead.
func TestHoltWintersSmoothingWeightsEmit(t *testing.T) {
	t.Parallel()
	plan := &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_gauge"},
		Func:            "holt_winters",
		Scalars:         []float64{0.25, 0.125},
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
	sql, _, err := Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(holt_winters): %v", err)
	}
	// 1 - sf = 0.75 must appear; the +sf flip's 1.25 must not.
	if !strings.Contains(sql, "0.75 *") {
		t.Errorf("missing `1 - sf` weight (0.75 *) — "+
			"range_window.go:`InlineLit(sf), InlineLit(1-sf)` flipped?\nSQL: %s", sql)
	}
	if strings.Contains(sql, "1.25") {
		t.Errorf("found 1.25 — `1 - sf` mutated to `1 + sf`\nSQL: %s", sql)
	}
	// 1 - tf = 0.875 must appear; the +tf flip's 1.125 must not.
	if !strings.Contains(sql, "0.875 *") {
		t.Errorf("missing `1 - tf` weight (0.875 *) — "+
			"range_window.go:`InlineLit(tf), InlineLit(1-tf)` flipped?\nSQL: %s", sql)
	}
	if strings.Contains(sql, "1.125") {
		t.Errorf("found 1.125 — `1 - tf` mutated to `1 + tf`\nSQL: %s", sql)
	}
}

// TestWindowedArrayPairsMatrixMinWindowBoundary kills
// range_window.go:emitWindowedArrayPairsMatrix:`minWindowSize > 0`.
// minWindowSize=0 must NOT emit the `length(window_pairs) >= 0` WHERE filter;
// minWindowSize=2 must. The `> 0` → `>= 0` boundary flip adds the no-op
// filter at 0, so the filter's presence is pinned to the boundary.
func TestWindowedArrayPairsMatrixMinWindowBoundary(t *testing.T) {
	t.Parallel()
	start := mutTestStart()
	base := func() *chplan.RangeWindow {
		return &chplan.RangeWindow{
			Input:           &chplan.Scan{Table: "otel_metrics_sum"},
			Func:            "irate",
			Range:           5 * time.Minute,
			OuterRange:      5 * time.Minute,
			Step:            30 * time.Second,
			Start:           start,
			End:             start.Add(5 * time.Minute),
			TimestampColumn: "TimeUnix",
			ValueColumn:     "Value",
			GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
		}
	}
	writer := func(_ Frag) Frag { return Col("Value") }

	const lenFilterPrefix = "length(`window_pairs`)"

	// minWindowSize = 0 → no WHERE window-length filter at all.
	e0 := &emitter{}
	if err := e0.emitWindowedArrayPairsMatrix(base(), writer, 0); err != nil {
		t.Fatalf("emitWindowedArrayPairsMatrix(min=0): %v", err)
	}
	if got := e0.b.String(); strings.Contains(got, lenFilterPrefix) {
		t.Errorf("min=0 emitted a window-length filter (%q) — `minWindowSize > 0` flipped to `>= 0`\nSQL: %s",
			lenFilterPrefix, got)
	}

	// minWindowSize = 2 → the filter must be present with the exact bound.
	e2 := &emitter{}
	if err := e2.emitWindowedArrayPairsMatrix(base(), writer, 2); err != nil {
		t.Fatalf("emitWindowedArrayPairsMatrix(min=2): %v", err)
	}
	if got := e2.b.String(); !strings.Contains(got, lenFilterPrefix+" >= 2") {
		t.Errorf("min=2 dropped the window-length filter\nSQL: %s", got)
	}
}

// TestOverTimeDirectRejectsZeroStepSubquery kills
// range_window.go:`r.OuterRange > 0 && r.Step <= 0`.
// OuterRange>0 with Step==0 is the exact boundary: original (`<= 0`) returns
// ErrUnsupported; the `< 0` flip would accept Step==0 and divide by zero.
func TestOverTimeDirectRejectsZeroStepSubquery(t *testing.T) {
	t.Parallel()
	start := mutTestStart()
	plan := &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_sum"},
		Func:            "count_over_time",
		Range:           5 * time.Minute,
		OuterRange:      5 * time.Minute,
		Step:            0, // boundary
		Start:           start,
		End:             start.Add(5 * time.Minute),
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		GroupBy:         []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
	_, _, err := Emit(context.Background(), plan)
	if err == nil {
		t.Fatalf("Emit(OuterRange>0, Step=0) returned nil error — range_window.go:`if r.OuterRange > 0 && r.Step <= 0 {` flipped to `< 0`")
	}
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
}

// TestFusedInstantSubqueryTaken kills the `handled || err != nil` conjunction in
// emitRangeWindowOverTimeDirect (and, via the public path, the instant arm of
// tryEmitFusedSubquery's outer-shape guard). A fusible instant subquery must
// emit the fused shape — `arrayReduce('max', vals)` over a per-series `samples`
// array. The `||` → `&&` flip would drop handled=true on the floor and fall
// through to the materialized regroup, which reduces with `max(Value)` over an
// `anchor_ts` group and never builds the `vals` / `samples` arrays.
func TestFusedInstantSubqueryTaken(t *testing.T) {
	t.Parallel()
	sql, _, err := Emit(context.Background(), fusedOuter())
	if err != nil {
		t.Fatalf("Emit(fused instant subquery): %v", err)
	}
	for _, want := range []string{"arrayReduce('max', vals)", "AS `samples`", "WHERE length(vals) > 0"} {
		if !strings.Contains(sql, want) {
			t.Errorf("fused shape not emitted (missing %q) — the `handled || err != nil` "+
				"conjunction or the fused outer-shape guard flipped\nSQL: %s", want, sql)
		}
	}
	// The materialized fall-through regroup must NOT be what we emitted.
	if strings.Contains(sql, "anchor_ts") {
		t.Errorf("emitted the materialized regroup (anchor_ts) instead of the fused shape\nSQL: %s", sql)
	}
}

// fusedMatrixOuter is the `/api/v1/query_range` sibling of fusedOuter: the same
// fusible inner under an outer reducer that carries the request's own step grid.
func fusedMatrixOuter() *chplan.RangeWindow {
	r := fusedOuter()
	r.Step = 1 * time.Minute
	r.OuterRange = 10 * time.Minute
	return r
}

// TestFusedMatrixSubqueryTaken pins #1505: a range-mode outer reducer over a
// fusible subquery must take the FUSED matrix shape, not the materialized
// regroup it used to fall through to. The call-site ordering is what this
// catches — returning emitRangeWindowOverTimeDirectMatrix before attempting
// fusion (the pre-#1505 order) puts every marker below on the wrong side.
func TestFusedMatrixSubqueryTaken(t *testing.T) {
	t.Parallel()
	sql, _, err := Emit(context.Background(), fusedMatrixOuter())
	if err != nil {
		t.Fatalf("Emit(fused matrix subquery): %v", err)
	}
	// The fused matrix markers: the per-series inner grid, the per-outer-anchor
	// arrayJoin, and the outer reduce over the covered run.
	for _, want := range []string{
		"AS `samples`",
		"AS `" + fusedAnchorGridAlias + "`",
		"AS `" + fusedOuterAnchorAlias + "`",
		"arrayJoin(arrayFilter(",
		"arraySlice(" + fusedAnchorGridAlias,
		"arrayReduce('max', q)",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("fused matrix shape not emitted (missing %q) — fusion is still "+
				"attempted AFTER the materialized matrix delegation\nSQL: %s", want, sql)
		}
	}
	// The materialized inner matrix must be gone: no per-(series, anchor)
	// regroup and no window_pairs array.
	for _, unwanted := range []string{"window_pairs", "GROUP BY `Attributes`, `anchor_ts`"} {
		if strings.Contains(sql, unwanted) {
			t.Errorf("materialized inner matrix still emitted (found %q)\nSQL: %s", unwanted, sql)
		}
	}
	// The matrix contract still holds: one row per (series, anchor), with the
	// anchor surfaced under both the internal alias and the schema timestamp
	// column the wrapping projection reads.
	if !strings.Contains(sql, "AS `anchor_ts`") || !strings.Contains(sql, "AS `TimeUnix`") {
		t.Errorf("fused matrix must project anchor_ts and the schema timestamp column\nSQL: %s", sql)
	}
}

// TestNonFusibleMatrixSubqueryFallsThrough is the negative control for the
// call-site reordering: a range-mode outer over a NON-extrapolating inner
// (irate is pairwise, never fusible) must still reach the materialized matrix
// emitter. Without this, "fusion first" could be satisfied by a guard that
// swallows shapes it cannot render.
func TestNonFusibleMatrixSubqueryFallsThrough(t *testing.T) {
	t.Parallel()
	r := fusedMatrixOuter()
	r.Input.(*chplan.RangeWindow).Func = "irate"
	sql, _, err := Emit(context.Background(), r)
	if err != nil {
		t.Fatalf("Emit(non-fusible matrix subquery): %v", err)
	}
	if strings.Contains(sql, fusedAnchorGridAlias) {
		t.Errorf("irate inner must not fuse\nSQL: %s", sql)
	}
	if !strings.Contains(sql, "GROUP BY `Attributes`, `anchor_ts`") {
		t.Errorf("non-fusible matrix subquery must fall through to the materialized "+
			"regroup\nSQL: %s", sql)
	}
}

// TestFusedSubqueryOuterShapeGuard pins tryEmitFusedSubquery's outer-shape
// guard. Two outer shapes fuse — INSTANT (OuterRange == 0 && Step == 0) and
// MATRIX (OuterRange > 0 && Step > 0) — and the two half-set pairs must not:
// an OuterRange with no Step has no grid spacing and a Step with no OuterRange
// has no span, so neither can name an anchor grid. Calling the entry point
// directly with a valid fusible inner pins every arm:
//   - OuterRange>0, Step==0 → handled=false (a `&&`→`||` on matrixOuter, or
//     dropping the Step conjunct, would fuse a grid-less shape).
//   - OuterRange==0, Step>0 → handled=false (same, on the other conjunct).
//   - OuterRange==0, Step==0 → handled=true (instant).
//   - OuterRange>0, Step>0  → handled=true (matrix, #1505).
func TestFusedSubqueryOuterShapeGuard(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		outerRange  time.Duration
		step        time.Duration
		wantHandled bool
	}{
		{"OuterRange without Step", 1 * time.Minute, 0, false},
		{"Step without OuterRange", 0, 1 * time.Minute, false},
		{"instant fuses", 0, 0, true},
		{"matrix fuses", 10 * time.Minute, 1 * time.Minute, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := fusedOuter()
			r.OuterRange = tc.outerRange
			r.Step = tc.step
			e := &emitter{}
			handled, err := e.tryEmitFusedSubquery(r)
			if handled != tc.wantHandled {
				t.Errorf("tryEmitFusedSubquery(OuterRange=%v, Step=%v) handled=%v, want %v",
					tc.outerRange, tc.step, handled, tc.wantHandled)
			}
			if tc.wantHandled && err != nil {
				t.Errorf("fusible case returned err: %v", err)
			}
		})
	}
}

// TestFusedInstantInnerGate kills the inner-matrix gate in
// tryEmitFusedSubquery. Each case starts from the fully-fusible fusedOuter()
// and perturbs ONE inner field so the gate should reject (handled=false); the
// flip would proceed to fuse (handled=true). The clean baseline fuses
// (handled=true).
//
//   - range_window_fused.go:`inner.Identity || inner.OuterRange <= 0 || inner.Step <= 0`:
//     the leading `||`→`&&` (Identity=true still rejects), the
//     `inner.OuterRange <= 0` `<=`→`<` (OuterRange==0 still rejects), the
//     second `||`→`&&` and the `inner.Step <= 0` `<=`→`<` (Step==0 still
//     rejects).
//   - range_window_fused.go:`inner.TimestampColumn == "" || inner.ValueColumn == ""`
//     `||`→`&&`: an empty TimestampColumn (ValueColumn still set) still
//     rejects.
//   - range_window_fused.go:`inner.Start.IsZero() || inner.End.IsZero()`
//     `||`→`&&`: a zero inner Start (End still set) still rejects.
func TestFusedInstantInnerGate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		perturb func(inner *chplan.RangeWindow)
	}{
		{"identity inner", func(in *chplan.RangeWindow) { in.Identity = true }},
		{"zero OuterRange", func(in *chplan.RangeWindow) { in.OuterRange = 0 }},
		{"zero Step", func(in *chplan.RangeWindow) { in.Step = 0 }},
		{"empty TimestampColumn", func(in *chplan.RangeWindow) { in.TimestampColumn = "" }},
		{"zero inner Start", func(in *chplan.RangeWindow) { in.Start = time.Time{} }},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := fusedOuter()
			tc.perturb(r.Input.(*chplan.RangeWindow))
			e := &emitter{}
			handled, err := e.tryEmitFusedSubquery(r)
			if err != nil {
				t.Fatalf("gate rejection must not error, got %v", err)
			}
			if handled {
				t.Errorf("perturbed inner (%s) must not fuse (handled=true) — the inner gate flip is alive", tc.name)
			}
		})
	}
	// Positive control: the untouched fusible shape fuses.
	e := &emitter{}
	if handled, err := e.tryEmitFusedSubquery(fusedOuter()); err != nil || !handled {
		t.Fatalf("clean fusible inner must fuse: handled=%v err=%v", handled, err)
	}
}

// TestFusedInstantAnchorCount kills
// range_window_fused.go:`inner.OuterRange.Nanoseconds()/stepNS + 1`. With
// OuterRange=10m and Step=1m the fused anchor grid is `range(11)`. The `/`→`*`
// flip overflows to a wildly different count and the `+`→`-` flip yields
// `range(9)`, so pinning the exact `range(11)` literal kills both arithmetic
// mutants.
func TestFusedInstantAnchorCount(t *testing.T) {
	t.Parallel()
	sql, _, err := Emit(context.Background(), fusedOuter())
	if err != nil {
		t.Fatalf("Emit(fused instant subquery): %v", err)
	}
	// OuterRange 10m / Step 1m + 1 = 11 anchors.
	if !strings.Contains(sql, "range(11)") {
		t.Errorf("fused anchor grid must be range(11) (OuterRange/Step + 1) — "+
			"`inner.OuterRange.Nanoseconds()/stepNS + 1` arithmetic flipped\nSQL: %s", sql)
	}
	if strings.Contains(sql, "range(9)") {
		t.Errorf("found range(9) — `+ 1` mutated to `- 1`\nSQL: %s", sql)
	}
}

// TestOverTimeDirectMatrixNoGroupBy kills
// range_window.go:emitRangeWindowOverTimeDirectMatrix:`len(groupFrags)+1`.
// With an empty GroupBy the regroup-key slice cap is `len(groupFrags)+1` = 1; the
// ARITHMETIC_BASE flip to `-1` makes `make([]Frag, 0, -1)`, which panics
// (cap out of range) on this exact path. The original must emit clean SQL.
func TestOverTimeDirectMatrixNoGroupBy(t *testing.T) {
	t.Parallel()
	start := mutTestStart()
	plan := &chplan.RangeWindow{
		Input:           &chplan.Scan{Table: "otel_metrics_sum"},
		Func:            "count_over_time",
		Range:           5 * time.Minute,
		OuterRange:      5 * time.Minute,
		Step:            30 * time.Second,
		Start:           start,
		End:             start.Add(5 * time.Minute),
		TimestampColumn: "TimeUnix",
		ValueColumn:     "Value",
		// GroupBy intentionally empty → the `len(groupFrags)+1` make-cap boundary.
	}
	sql, _, err := Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(count_over_time matrix, no GroupBy): %v", err)
	}
	if !strings.Contains(sql, "anchor_ts") {
		t.Errorf("expected matrix anchor fan-out, got:\n%s", sql)
	}
}

// TestFusedSamplesQueryTemporalityGate kills the CONDITIONALS_NEGATION
// mutant at range_window_fused.go:`g.temporality != nil`, inside
// samplesQuery. When the inner window carries a TemporalityColumn, the
// fused samples layer must read the series' single AggregationTemporality
// via `any(...)` under windowTemporalityAlias; when it doesn't, that
// projection must be absent entirely. The `!=` → `==` flip would invert
// both cases: absent when needed (the DELTA/CUMULATIVE split in
// fusedExtrapolatedValueFrag then reads an unprojected column) and present
// when not (an any() over a column no plan ever set).
func TestFusedSamplesQueryTemporalityGate(t *testing.T) {
	t.Parallel()
	const wantProjection = "any(`AggregationTemporality`) AS `temporality`"

	t.Run("TemporalityColumn set → any() projected", func(t *testing.T) {
		t.Parallel()
		r := fusedOuter()
		r.Input.(*chplan.RangeWindow).TemporalityColumn = "AggregationTemporality"
		sql, _, err := Emit(context.Background(), r)
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
		if !strings.Contains(sql, wantProjection) {
			t.Errorf("expected %q in the fused samples layer "+
				"(`g.temporality != nil` flipped?)\nSQL: %s", wantProjection, sql)
		}
	})

	t.Run("TemporalityColumn unset → no any() projected", func(t *testing.T) {
		t.Parallel()
		sql, _, err := Emit(context.Background(), fusedOuter())
		if err != nil {
			t.Fatalf("Emit: %v", err)
		}
		if strings.Contains(sql, wantProjection) {
			t.Errorf("unexpected %q with no TemporalityColumn set "+
				"(`g.temporality != nil` flipped?)\nSQL: %s", wantProjection, sql)
		}
	})
}

// TestFusedMatrixOuterAnchorCount kills the ARITHMETIC_BASE mutants at
// range_window_fused.go:`r.OuterRange.Nanoseconds()/outerStepNS + 1`,
// emitFusedMatrixSubquery's OUTER anchor grid — distinct from the INNER
// grid TestFusedInstantAnchorCount already pins. Choosing an
// outer OuterRange/Step pair (6m / 2m) that differs from fusibleInner's
// (10m / 1m) makes the outer count's literal unambiguous: a `/`→`*` flip
// or a `+`→`-` flip on the outer arithmetic cannot hide behind the inner
// grid's own (unrelated) range(11) literal.
func TestFusedMatrixOuterAnchorCount(t *testing.T) {
	t.Parallel()
	r := fusedOuter()
	r.Step = 2 * time.Minute
	r.OuterRange = 6 * time.Minute
	sql, _, err := Emit(context.Background(), r)
	if err != nil {
		t.Fatalf("Emit(fused matrix subquery): %v", err)
	}
	// OuterRange 6m / Step 2m + 1 = 4 outer anchors.
	if !strings.Contains(sql, "range(4)") {
		t.Errorf("outer anchor grid must be range(4) (OuterRange/Step + 1) — range_window_fused.go:`numAnchors := inner.OuterRange.Nanoseconds()/stepNS + 1` arithmetic flipped\nSQL: %s", sql)
	}
	if strings.Contains(sql, "range(2)") {
		t.Errorf("found range(2) — `+ 1` mutated to `- 1` in range_window_fused.go:`numAnchors := inner.OuterRange.Nanoseconds()/stepNS + 1`\nSQL: %s", sql)
	}
}
