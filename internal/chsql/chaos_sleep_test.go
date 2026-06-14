//go:build chaos_sleep

package chsql

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestChaosSleep_SplicedWhenSet asserts that, in the chaos_sleep build,
// Emit wraps the plan SQL in an outer SELECT whose WHERE blocks on a
// server-side ClickHouse sleep — and that the inner plan's own SQL is
// preserved verbatim inside the wrap (so the result set is unchanged, only
// the latency grows). The per-call sleep magnitude must stay STRICTLY
// under CH's per-block max_sleep_in_seconds cap (chMaxSleepSeconds, 3s)
// while the cumulative block time (perCall × rows) exceeds the chaos
// build's max_execution_time, so CH aborts the query with code 159 (503
// timeout) rather than rejecting it up front with code 160 (502).
func TestChaosSleep_SplicedWhenSet(t *testing.T) {
	const sleepSeconds = 7

	ctx := WithChaosSleepSeconds(context.Background(), sleepSeconds)
	sql, _, err := Emit(ctx, &chplan.OneRow{})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	// The inner plan (OneRow -> "SELECT 1") survives inside the wrap.
	if !strings.Contains(sql, "SELECT 1") {
		t.Fatalf("inner plan SQL not preserved in wrap: %q", sql)
	}
	// A genuinely server-side, per-row sleep over numbers(<rows>) at the
	// fixed per-call magnitude — NOT the raw ctx seconds (that was the
	// code-160 bug: a 10s per-block request > CH's 3s cap).
	wantSleep := fmt.Sprintf("sleepEachRow(%d)", chaosSleepPerCallSeconds)
	if !strings.Contains(sql, wantSleep) {
		t.Fatalf("expected %q splice, got: %q", wantSleep, sql)
	}
	wantNumbers := fmt.Sprintf("numbers(%d)", chaosSleepRows)
	if !strings.Contains(sql, wantNumbers) {
		t.Fatalf("expected %q (fixed cumulative sleep, NOT ctx seconds), got: %q", wantNumbers, sql)
	}
	// The per-call sleep must be strictly under CH's per-block cap, else a
	// single block trips code 160 (502) instead of letting
	// max_execution_time abort with code 159 (503).
	if chaosSleepPerCallSeconds >= chMaxSleepSeconds {
		t.Fatalf("per-call sleep %ds must be < CH per-block cap %ds (else code 160)",
			chaosSleepPerCallSeconds, chMaxSleepSeconds)
	}
	// The cumulative block time must exceed the chaos build's
	// max_execution_time (chaosCHExecutionCap, 3s) so CH aborts mid-scan.
	const chaosMaxExecutionSeconds = 3
	if cumulative := chaosSleepPerCallSeconds * chaosSleepRows; cumulative <= chaosMaxExecutionSeconds {
		t.Fatalf("cumulative sleep %ds must exceed max_execution_time %ds (else no 159 abort)",
			cumulative, chaosMaxExecutionSeconds)
	}
	// The blocking predicate must be a non-filtering scalar comparison
	// so the outer result set is unchanged.
	if !strings.Contains(sql, ">= 0") {
		t.Fatalf("expected non-filtering '>= 0' sleep predicate, got: %q", sql)
	}
}

// TestChaosSleep_AbsentWhenUnset asserts that a ctx with NO chaos sleep
// value (the normal request path) emits exactly the bare plan SQL — no
// sleep, no wrap. This pins the "a normal query is never slowed" property
// even inside the chaos_sleep build.
func TestChaosSleep_AbsentWhenUnset(t *testing.T) {
	bare, _, err := Emit(context.Background(), &chplan.OneRow{})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if strings.Contains(bare, "sleepEachRow") || strings.Contains(bare, "numbers(") {
		t.Fatalf("sleep spliced without a ctx value: %q", bare)
	}
	if bare != "SELECT 1" {
		t.Fatalf("expected bare plan SQL unchanged, got: %q", bare)
	}
}

// TestChaosSleep_NonPositiveInert asserts a zero/negative sleep duration
// is inert (no wrap), matching the handler's guard.
func TestChaosSleep_NonPositiveInert(t *testing.T) {
	for _, secs := range []int{0, -3} {
		ctx := WithChaosSleepSeconds(context.Background(), secs)
		sql, _, err := Emit(ctx, &chplan.OneRow{})
		if err != nil {
			t.Fatalf("Emit(%d): %v", secs, err)
		}
		if sql != "SELECT 1" {
			t.Fatalf("Emit(%d): expected inert bare SQL, got: %q", secs, sql)
		}
	}
}
