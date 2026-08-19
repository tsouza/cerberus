package chsql

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tsouza/cerberus/internal/chplan"
)

// deltaCapableInstantWindow returns a minimal instant (OuterRange == 0)
// rate() RangeWindow over a Sum-typed counter table — the exact shape
// instantDeltaPrefixSource governs: TemporalityColumn set, Func a
// counter function, OuterRange zero.
func deltaCapableInstantWindow() *chplan.RangeWindow {
	end := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return &chplan.RangeWindow{
		Input:             &chplan.Scan{Table: "otel_metrics_sum"},
		Func:              "rate",
		Range:             5 * time.Minute,
		End:               end,
		TimestampColumn:   "TimeUnix",
		ValueColumn:       "Value",
		TemporalityColumn: "AggregationTemporality",
		GroupBy:           []chplan.Expr{&chplan.ColumnRef{Name: "Attributes"}},
	}
}

// deltaCapableMatrixWindow is deltaCapableInstantWindow's OuterRange > 0
// sibling — the shape emitWindowedArrayExtrapolatedMatrix governs.
func deltaCapableMatrixWindow() *chplan.RangeWindow {
	r := deltaCapableInstantWindow()
	r.Start = r.End.Add(-10 * time.Minute)
	r.Step = time.Minute
	r.OuterRange = 10 * time.Minute
	return r
}

func TestDeltaPrefixLookback_InstantScan_AddsLowerBound(t *testing.T) {
	t.Parallel()

	r := deltaCapableInstantWindow()

	disabledCtx := WithDeltaPrefixLookback(context.Background(), 0)
	disabledSQL, _, err := Emit(disabledCtx, r)
	if err != nil {
		t.Fatalf("Emit (disabled): %v", err)
	}

	boundedCtx := WithDeltaPrefixLookback(context.Background(), 2*time.Hour)
	boundedSQL, _, err := Emit(boundedCtx, r)
	if err != nil {
		t.Fatalf("Emit (bounded): %v", err)
	}

	// The prefix scan's own WHERE renders `<rangeNS bound> - <lookback
	// bound>` chained: rangeNS is 5m (300000000000ns), the configured
	// lookback is 2h (7200000000000ns). Chaining rather than counting
	// occurrences pins that the NEW bound sits on the SAME `TimeUnix <=
	// rangeStart` predicate deltaPrefixLowerBoundFrag derives its floor
	// from, not some unrelated interval elsewhere in the statement.
	const chained = "toIntervalNanosecond(300000000000) - toIntervalNanosecond(7200000000000)"
	if !strings.Contains(boundedSQL, chained) {
		t.Errorf("bounded SQL missing the chained rangeNS/lookback bound %q\nSQL: %s", chained, boundedSQL)
	}
	if strings.Contains(disabledSQL, chained) {
		t.Errorf("disabled SQL unexpectedly carries the lookback bound %q\nSQL: %s", chained, disabledSQL)
	}
	// Sanity: the prefix subquery (the ONE place this bound applies) is
	// present exactly once in both forms — the chained-substring assertion
	// above would be vacuous if it happened to match zero or multiple
	// unrelated sites.
	if got := strings.Count(boundedSQL, "AS `delta_prefix_key_0`"); got != 1 {
		t.Fatalf("expected exactly one delta-prefix subquery, found shape %d times\nSQL: %s", got, boundedSQL)
	}
}

// TestDeltaPrefixLookback_Unset_FallsBackToDefault pins that a caller who
// never threads WithDeltaPrefixLookback at all (the spec/golden lane, and
// any pre-fix caller of chsql.Emit) gets defaultDeltaPrefixLookback rather
// than reverting to the unbounded pre-fix scan — the fail-closed-by-default
// posture this bound exists for.
func TestDeltaPrefixLookback_Unset_FallsBackToDefault(t *testing.T) {
	t.Parallel()

	r := deltaCapableInstantWindow()

	unsetSQL, _, err := Emit(context.Background(), r)
	if err != nil {
		t.Fatalf("Emit (unset): %v", err)
	}
	explicitDefaultSQL, _, err := Emit(WithDeltaPrefixLookback(context.Background(), defaultDeltaPrefixLookback), r)
	if err != nil {
		t.Fatalf("Emit (explicit default): %v", err)
	}
	if unsetSQL != explicitDefaultSQL {
		t.Errorf("unset ctx did not fall back to defaultDeltaPrefixLookback (%s):\nunset:            %s\nexplicit default: %s",
			defaultDeltaPrefixLookback, unsetSQL, explicitDefaultSQL)
	}

	disabledSQL, _, err := Emit(WithDeltaPrefixLookback(context.Background(), 0), r)
	if err != nil {
		t.Fatalf("Emit (disabled): %v", err)
	}
	if unsetSQL == disabledSQL {
		t.Error("unset ctx rendered the same (unbounded) SQL as an explicit disable — the default must be a bound, not a no-op")
	}
}

func TestDeltaPrefixLookback_MatrixScan_AddsLowerBound(t *testing.T) {
	t.Parallel()

	r := deltaCapableMatrixWindow()

	disabledSQL, _, err := Emit(WithDeltaPrefixLookback(context.Background(), 0), r)
	if err != nil {
		t.Fatalf("Emit (disabled): %v", err)
	}
	boundedSQL, _, err := Emit(WithDeltaPrefixLookback(context.Background(), 3*time.Hour), r)
	if err != nil {
		t.Fatalf("Emit (bounded): %v", err)
	}

	// rangeNS is 5m (300000000000ns); the configured lookback is 3h
	// (10800000000000ns). Same chaining rationale as the instant-scan test.
	const chained = "toIntervalNanosecond(300000000000) - toIntervalNanosecond(10800000000000)"
	if !strings.Contains(boundedSQL, chained) {
		t.Errorf("bounded matrix SQL missing the chained rangeNS/lookback bound %q\nSQL: %s", chained, boundedSQL)
	}
	if strings.Contains(disabledSQL, chained) {
		t.Errorf("disabled matrix SQL unexpectedly carries the lookback bound %q\nSQL: %s", chained, disabledSQL)
	}

	// The DELTA branch must stay an OR arm alongside the ordinary per-anchor
	// window condition — a series with genuinely no DELTA temporality must
	// still take the unmodified, unrelated branch. Regression pin against a
	// rewrite that accidentally ANDs the lookback bound onto the WHOLE
	// predicate instead of just the DELTA arm.
	if !strings.Contains(boundedSQL, " OR ") {
		t.Errorf("bounded matrix SQL lost the DELTA-vs-in-window OR structure\nSQL: %s", boundedSQL)
	}
}

// TestDeltaPrefixLookback_CumulativeOnlyOutputUnaffected proves the lookback
// bound changes ZERO output for a purely CUMULATIVE-temporality series — the
// overwhelmingly common real-world case (verified live: ~99.99% of one
// production otel_metrics_sum table) — by asserting the RangeWindow's own
// InstantScanBounded / emitted shape for a plan with NO TemporalityColumn is
// byte-identical regardless of the configured lookback: instantDeltaPrefixSource
// / emitWindowedArrayExtrapolatedMatrix are only reachable when
// TemporalityColumn is set, so a plan without one never touches this code
// path at all, and the lookback ctx value is simply never consulted.
func TestDeltaPrefixLookback_CumulativeOnlyOutputUnaffected(t *testing.T) {
	t.Parallel()

	r := deltaCapableInstantWindow()
	r.TemporalityColumn = "" // no AggregationTemporality column at all

	shortSQL, _, err := Emit(WithDeltaPrefixLookback(context.Background(), time.Minute), r)
	if err != nil {
		t.Fatalf("Emit (1m lookback): %v", err)
	}
	disabledSQL, _, err := Emit(WithDeltaPrefixLookback(context.Background(), 0), r)
	if err != nil {
		t.Fatalf("Emit (disabled): %v", err)
	}
	if shortSQL != disabledSQL {
		t.Errorf("a plan with no TemporalityColumn must be lookback-invariant (it never reaches instantDeltaPrefixSource):\n1m lookback: %s\ndisabled:    %s",
			shortSQL, disabledSQL)
	}
}
