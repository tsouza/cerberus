//go:build chaos_sleep

package chsql

import (
	"context"
	"strings"
	"testing"

	"github.com/tsouza/cerberus/internal/chplan"
)

// TestChaosSleep_SplicedWhenSet asserts that, in the chaos_sleep build,
// Emit wraps the plan SQL in an outer SELECT whose WHERE blocks on a
// server-side ClickHouse sleep scaled to the ctx-carried duration — and
// that the inner plan's own SQL is preserved verbatim inside the wrap (so
// the result set is unchanged, only the latency grows).
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
	// A genuinely server-side, per-row sleep over numbers(<seconds>).
	if !strings.Contains(sql, "sleepEachRow") {
		t.Fatalf("expected sleepEachRow splice, got: %q", sql)
	}
	if !strings.Contains(sql, "numbers(7)") {
		t.Fatalf("expected numbers(7) (sleep scaled to ctx seconds), got: %q", sql)
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
