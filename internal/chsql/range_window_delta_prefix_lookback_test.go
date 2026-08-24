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

// deltaPrefixAggregateCapableWindow is deltaCapableInstantWindow with
// DeltaPrefixAggregateInput populated — the shape deltaPrefixAggregateSource
// governs (only reachable when both DeltaPrefixAggregateInput != nil AND
// WithDeltaPrefixReadEnabled(ctx, true) — see that function's doc).
func deltaPrefixAggregateCapableWindow() *chplan.RangeWindow {
	r := deltaCapableInstantWindow()
	r.DeltaPrefixAggregateInput = &chplan.Scan{Table: "otel_metrics_sum_delta_prefix"}
	return r
}

// TestDeltaPrefixAggregateSource_PresenceGuardWiredIntoBothTerms pins that
// deltaPrefixAggregateSource applies the SAME CUMULATIVE-only presence guard
// (deltaPresenceGuardFrag) instantDeltaPrefixSource's own prefix scan already
// carries, to BOTH of its two summed terms — the aggregate-table scan and the
// raw-remainder scan — not just one. An adversarial review of PR #2514 found
// this guard silently absent from the new mechanism entirely, reintroducing
// the exact unguarded-scan cost (measured at 181x on deltaPresenceGuardFrag's
// own doc) for the ~99.99% of production traffic that is CUMULATIVE-only.
// It also pins that the summed join carries the ifNull guard the same review
// found missing — see TestDeltaPrefixAggregateSource_JoinUseNulls1DoesNotPropagateNull
// (chdb) for the behavioural proof.
func TestDeltaPrefixAggregateSource_PresenceGuardWiredIntoBothTerms(t *testing.T) {
	t.Parallel()

	r := deltaPrefixAggregateCapableWindow()
	ctx := WithDeltaPrefixReadEnabled(context.Background(), true)
	sqlText, _, err := Emit(ctx, r)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	// AggregationTemporality = 1 is schema.AggregationTemporalityDelta.
	const guard = "(SELECT max(`AggregationTemporality` = 1)"
	if got := strings.Count(sqlText, guard); got != 2 {
		t.Errorf("expected the presence guard exactly twice (aggregate term + raw-remainder term), found %d\nSQL: %s", got, sqlText)
	}

	// Both LEFT JOIN reads feeding the summed delta_prefix_before_window must
	// be ifNull-guarded: a join_use_nulls=1 deployment (a legitimate
	// ClickHouse setting cerberus does not itself pin) returns NULL, not the
	// numeric-type default, on a LEFT JOIN miss, and NULL + x is NULL in
	// ClickHouse.
	const summed = "ifNull(`a`.`delta_prefix_aggregate_before_window`, 0) + ifNull(`p`.`delta_prefix_raw_remainder`, 0)"
	if !strings.Contains(sqlText, summed) {
		t.Errorf("expected the summed join to ifNull-guard both terms\nwant substring: %s\nSQL: %s", summed, sqlText)
	}
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
// TestExtrapolatedInstantDeltaPrefixAggregateGate is
// TestExtrapolatedMatrixDeltaPrefixAggregateGate's instant-mode sibling,
// pinning emitWindowedArrayExtrapolated's own
//
//	if r.DeltaPrefixAggregateInput != nil && e.deltaPrefixReadEnabled {
//
// gate independently on each operand — gremlins found this exact shape's
// `&&` survives mutation to `||` with no instant-path test distinguishing
// "populated but not enabled" / "enabled but not populated" from "both",
// even though TestExtrapolatedMatrixDeltaPrefixAggregateGate already pins
// the identical three-case pattern for the matrix-mode gate a few lines
// away. Mirrors that test's byte-for-byte comparison strategy exactly, so
// a boolean-operator mutation on either operand is guaranteed to flip at
// least one case's expected outcome.
func TestExtrapolatedInstantDeltaPrefixAggregateGate(t *testing.T) {
	t.Parallel()

	baseline, _, err := Emit(context.Background(), deltaCapableInstantWindow())
	if err != nil {
		t.Fatalf("Emit(aggInput=nil, readEnabled=false): %v", err)
	}

	withReadEnabledNoInput, _, err := Emit(
		WithDeltaPrefixReadEnabled(context.Background(), true),
		deltaCapableInstantWindow(),
	)
	if err != nil {
		t.Fatalf("Emit(aggInput=nil, readEnabled=true): %v", err)
	}
	if withReadEnabledNoInput != baseline {
		t.Errorf("DeltaPrefixAggregateInput=nil, readEnabled=true must match the baseline SQL exactly "+
			"(no DELTA-prefix table named by this schema means nothing to read) — got a DIFFERENT query:\nbaseline: %s\ngot:      %s",
			baseline, withReadEnabledNoInput)
	}

	withInputNoReadEnabled, _, err := Emit(context.Background(), deltaPrefixAggregateCapableWindow())
	if err != nil {
		t.Fatalf("Emit(aggInput=set, readEnabled=false): %v", err)
	}
	if withInputNoReadEnabled != baseline {
		t.Errorf("DeltaPrefixAggregateInput populated but readEnabled=false must match the baseline SQL exactly "+
			"(the flag, not the field, gates consumption) — got a DIFFERENT query:\nbaseline: %s\ngot:      %s",
			baseline, withInputNoReadEnabled)
	}

	withBoth, _, err := Emit(
		WithDeltaPrefixReadEnabled(context.Background(), true),
		deltaPrefixAggregateCapableWindow(),
	)
	if err != nil {
		t.Fatalf("Emit(aggInput=set, readEnabled=true): %v", err)
	}
	if withBoth == baseline {
		t.Error("DeltaPrefixAggregateInput populated AND readEnabled=true must emit a DIFFERENT query than the " +
			"baseline — the exact-aggregate mechanism never fired")
	}
	if !strings.Contains(withBoth, "otel_metrics_sum_delta_prefix") {
		t.Errorf("DeltaPrefixAggregateInput populated AND readEnabled=true must scan the aggregate table by name\nSQL: %s", withBoth)
	}
}

// TestDeltaPrefixAggregateSource_GroupColumnsBranch pins
// deltaPrefixAggregateSource's `if len(groupColumns) == 0` branch in BOTH
// directions: an empty GroupBy (e.g. `sum(rate(m[5m]))` with no `by(...)`)
// must CROSS JOIN the aggregate/raw-remainder terms, and a non-empty
// GroupBy (every other test in this file) must LEFT JOIN them keyed on the
// group columns. Gremlins found the `== 0` survives mutation to `!= 0`
// with no test exercising the empty-GroupBy shape at all — every other
// case in this file sets a non-empty GroupBy, so a mutant that swaps which
// branch fires for which case still emits valid, plausible-looking SQL and
// was never caught.
func TestDeltaPrefixAggregateSource_GroupColumnsBranch(t *testing.T) {
	t.Parallel()

	keyed := deltaPrefixAggregateCapableWindow()
	keyedSQL, _, err := Emit(WithDeltaPrefixReadEnabled(context.Background(), true), keyed)
	if err != nil {
		t.Fatalf("Emit (non-empty GroupBy): %v", err)
	}
	if !strings.Contains(keyedSQL, "LEFT JOIN") {
		t.Errorf("non-empty GroupBy must LEFT JOIN the aggregate/raw-remainder terms\nSQL: %s", keyedSQL)
	}
	if strings.Contains(keyedSQL, "CROSS JOIN") {
		t.Errorf("non-empty GroupBy must not CROSS JOIN\nSQL: %s", keyedSQL)
	}

	unkeyed := deltaPrefixAggregateCapableWindow()
	unkeyed.GroupBy = nil
	unkeyedSQL, _, err := Emit(WithDeltaPrefixReadEnabled(context.Background(), true), unkeyed)
	if err != nil {
		t.Fatalf("Emit (empty GroupBy): %v", err)
	}
	if got := strings.Count(unkeyedSQL, "CROSS JOIN"); got != 2 {
		t.Errorf("empty GroupBy must CROSS JOIN both the aggregate and raw-remainder terms, found %d\nSQL: %s", got, unkeyedSQL)
	}
	if strings.Contains(unkeyedSQL, "LEFT JOIN") {
		t.Errorf("empty GroupBy must not LEFT JOIN\nSQL: %s", unkeyedSQL)
	}
}

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
