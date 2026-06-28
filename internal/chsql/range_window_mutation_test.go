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
// Mutant inventory (file:line:col → what the flip does):
//   - range_window.go:423:28 / 425:28 — `1 - sf` / `1 - tf` → `1 + sf` / `1 + tf`
//     (holt_winters smoothing weights): wrong (1∓w) factor in the recurrence.
//   - range_window.go:662:19 — `minWindowSize > 0` → `>= 0`: spuriously emits the
//     window-length WHERE filter when no minimum was requested.
//   - range_window.go:2346:13 — `r.Step <= 0` → `< 0`: stops rejecting OuterRange>0
//     subqueries that forgot Step (would divide by zero / emit a degenerate grid).
//   - range_window.go:2357:63 — `handled || err != nil` → `handled && err != nil`:
//     a successful fused emit (handled=true, err=nil) would fall through to the
//     materialized path, double-emitting / changing SQL.
//   - range_window.go:2465:48 — `len(groupFrags)+1` → `len(groupFrags)-1`: with no
//     GroupBy this is `make([]Frag, 0, -1)` → runtime panic (cap out of range).
//   - range_window_fused.go:93:18 / 93:23 / 93:33 — the self-guard
//     `if r.OuterRange != 0 || r.Step != 0` that keeps the fused entry instant-only.

func mutTestStart() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

// fusibleInner returns a fully-valid extrapolating MATRIX RangeWindow that
// satisfies every gate in tryEmitFusedInstantSubquery (Func extrapolating,
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

// TestHoltWintersSmoothingWeightsEmit kills range_window.go:423 + 425.
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
		t.Errorf("missing `1 - sf` weight (0.75 *) — line 423 flipped?\nSQL: %s", sql)
	}
	if strings.Contains(sql, "1.25") {
		t.Errorf("found 1.25 — `1 - sf` mutated to `1 + sf` (line 423)\nSQL: %s", sql)
	}
	// 1 - tf = 0.875 must appear; the +tf flip's 1.125 must not.
	if !strings.Contains(sql, "0.875 *") {
		t.Errorf("missing `1 - tf` weight (0.875 *) — line 425 flipped?\nSQL: %s", sql)
	}
	if strings.Contains(sql, "1.125") {
		t.Errorf("found 1.125 — `1 - tf` mutated to `1 + tf` (line 425)\nSQL: %s", sql)
	}
}

// TestWindowedArrayPairsMatrixMinWindowBoundary kills range_window.go:662.
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
		t.Errorf("min=0 emitted a window-length filter (%q) — line 662 `> 0` flipped to `>= 0`\nSQL: %s",
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

// TestOverTimeDirectRejectsZeroStepSubquery kills range_window.go:2346.
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
		t.Fatalf("Emit(OuterRange>0, Step=0) returned nil error — line 2346 `<= 0` flipped to `< 0`")
	}
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
}

// TestFusedInstantSubqueryTaken kills range_window.go:2357 (and, via the
// public path, fused.go 93:18 + 93:33). A fusible instant subquery must emit
// the fused shape — `arrayReduce('max', vals)` over a per-series `samples`
// array. The `||` → `&&` flip at 2357 would drop handled=true on the floor and
// fall through to the materialized regroup, which reduces with `max(Value)`
// over an `anchor_ts` group and never builds the `vals` / `samples` arrays.
func TestFusedInstantSubqueryTaken(t *testing.T) {
	t.Parallel()
	sql, _, err := Emit(context.Background(), fusedOuter())
	if err != nil {
		t.Fatalf("Emit(fused instant subquery): %v", err)
	}
	for _, want := range []string{"arrayReduce('max', vals)", "AS `samples`", "WHERE length(vals) > 0"} {
		if !strings.Contains(sql, want) {
			t.Errorf("fused shape not emitted (missing %q) — line 2357 `||`→`&&` "+
				"or fused.go:93 self-guard flipped\nSQL: %s", want, sql)
		}
	}
	// The materialized fall-through regroup must NOT be what we emitted.
	if strings.Contains(sql, "anchor_ts") {
		t.Errorf("emitted the materialized regroup (anchor_ts) instead of the fused shape\nSQL: %s", sql)
	}
}

// TestFusedInstantSelfGuard kills range_window_fused.go:93:18 / 93:23 / 93:33.
// The self-guard `OuterRange != 0 || Step != 0` returns handled=false for any
// non-instant outer. Calling the entry point directly with a valid fusible
// inner pins all three sub-mutations:
//   - OuterRange!=0, Step==0 → handled=false (kills `!=`→`==` on OuterRange, and
//     `||`→`&&`, both of which would proceed to fuse → handled=true).
//   - OuterRange==0, Step!=0 → handled=false (kills `!=`→`==` on Step, and
//     `||`→`&&`).
//   - OuterRange==0, Step==0 → handled=true (the genuinely-fusible case).
func TestFusedInstantSelfGuard(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		outerRange  time.Duration
		step        time.Duration
		wantHandled bool
	}{
		{"non-instant OuterRange", 1 * time.Minute, 0, false},
		{"non-instant Step", 0, 1 * time.Minute, false},
		{"instant fuses", 0, 0, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := fusedOuter()
			r.OuterRange = tc.outerRange
			r.Step = tc.step
			e := &emitter{}
			handled, err := e.tryEmitFusedInstantSubquery(r)
			if handled != tc.wantHandled {
				t.Errorf("tryEmitFusedInstantSubquery(OuterRange=%v, Step=%v) handled=%v, want %v",
					tc.outerRange, tc.step, handled, tc.wantHandled)
			}
			if tc.wantHandled && err != nil {
				t.Errorf("fusible case returned err: %v", err)
			}
		})
	}
}

// TestOverTimeDirectMatrixNoGroupBy kills range_window.go:2465. With an empty
// GroupBy the regroup-key slice cap is `len(groupFrags)+1` = 1; the
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
		// GroupBy intentionally empty → make-cap boundary at line 2465.
	}
	sql, _, err := Emit(context.Background(), plan)
	if err != nil {
		t.Fatalf("Emit(count_over_time matrix, no GroupBy): %v", err)
	}
	if !strings.Contains(sql, "anchor_ts") {
		t.Errorf("expected matrix anchor fan-out, got:\n%s", sql)
	}
}
