package chclient

import (
	"context"
	"errors"
	"testing"
)

// drainCursor iterates a cursor to completion and returns the number of
// rows it yielded plus its terminal Err().
func drainCursor(c Cursor) (int, error) {
	n := 0
	for c.Next() {
		n++
	}
	return n, c.Err()
}

// TestSampleBudget_SharedAcrossCursorsTripsAtTotal pins the per-request
// budget contract the sharded-pushdown solver depends on: two cursors
// sharing ONE SampleBudget collectively trip the 422 at the shared
// total, not per cursor. Without the shared budget each cursor would
// enforce its own per-cursor limit and a fan-out request could drain N
// times the configured cap in aggregate.
func TestSampleBudget_SharedAcrossCursorsTripsAtTotal(t *testing.T) {
	t.Parallel()

	// Budget of 5 total. Each cursor has 10 rows available and NO
	// per-cursor maxSamples — the only cap is the shared budget.
	budget := NewSampleBudget(5)

	c1 := &rowsCursor{rows: newGenRows(10), budget: budget}
	c2 := &rowsCursor{rows: newGenRows(10), budget: budget}
	defer func() { _ = c1.Close() }()
	defer func() { _ = c2.Close() }()

	// First cursor drains 4 rows, stopping short of the budget so the
	// second cursor inherits the remaining headroom.
	got1 := 0
	for got1 < 4 && c1.Next() {
		got1++
	}
	if got1 != 4 {
		t.Fatalf("first cursor drained %d rows of its first 4, want 4", got1)
	}
	if err := c1.Err(); err != nil {
		t.Fatalf("first cursor errored early: %v", err)
	}

	// Second cursor now drains: only 1 sample of headroom remains (5
	// total - 4 already taken), so it yields exactly 1 row then trips
	// the shared budget.
	got2, err2 := drainCursor(c2)
	if got2 != 1 {
		t.Fatalf("second cursor drained %d rows, want 1 (the shared headroom)", got2)
	}
	assertTooManySamples(t, err2, 5)

	// The budget is now exhausted, so resuming the FIRST cursor trips
	// immediately on its next row — the cap is request-global, not
	// per-cursor.
	if c1.Next() {
		t.Fatal("first cursor advanced past the exhausted shared budget")
	}
	assertTooManySamples(t, c1.Err(), 5)
}

// assertTooManySamples asserts err is the verbatim max-samples
// rejection: errors.Is ErrTooManySamples, a *TooManySamplesError, and
// reporting the expected configured limit. The message and behaviour
// must be IDENTICAL to the per-cursor path so the upstream 422 stays
// byte-for-byte the same regardless of which limit fired.
func assertTooManySamples(t *testing.T, err error, wantLimit int64) {
	t.Helper()
	if !errors.Is(err, ErrTooManySamples) {
		t.Fatalf("err: got %v, want errors.Is(_, ErrTooManySamples)", err)
	}
	var tooMany *TooManySamplesError
	if !errors.As(err, &tooMany) {
		t.Fatalf("err: got %T, want *TooManySamplesError", err)
	}
	if tooMany.Limit != wantLimit {
		t.Fatalf("Limit: got %d, want %d", tooMany.Limit, wantLimit)
	}
	// The rendered message is the same shape the per-cursor budget
	// produces — a *TooManySamplesError{Limit} with no per-source
	// divergence.
	if want := (&TooManySamplesError{Limit: wantLimit}).Error(); tooMany.Error() != want {
		t.Fatalf("message: got %q, want %q", tooMany.Error(), want)
	}
}

// TestSampleBudget_FallsBackToPerCursorLimit pins the fallback: a cursor
// opened WITHOUT a shared budget enforces its own per-cursor maxSamples
// exactly as before — the budget is purely additive substrate.
func TestSampleBudget_FallsBackToPerCursorLimit(t *testing.T) {
	t.Parallel()

	cursor := &rowsCursor{rows: newGenRows(10), maxSamples: 3}
	defer func() { _ = cursor.Close() }()

	got, err := drainCursor(cursor)
	if got != 3 {
		t.Fatalf("drained %d rows, want the per-cursor limit of 3", got)
	}
	assertTooManySamples(t, err, 3)
}

// TestSampleBudget_ContextRoundTrip pins WithSampleBudget /
// budgetFromContext: a positive budget attached to a ctx is recovered,
// an absent one yields nil (per-cursor fallback), and an inert
// (non-positive) budget is treated as absent.
func TestSampleBudget_ContextRoundTrip(t *testing.T) {
	t.Parallel()

	b := NewSampleBudget(7)
	ctx := WithSampleBudget(context.Background(), b)
	if got := budgetFromContext(ctx); got != b {
		t.Fatalf("budgetFromContext: got %v, want the attached budget", got)
	}

	if got := budgetFromContext(context.Background()); got != nil {
		t.Fatalf("budgetFromContext on a bare ctx: got %v, want nil", got)
	}

	inert := WithSampleBudget(context.Background(), NewSampleBudget(0))
	if got := budgetFromContext(inert); got != nil {
		t.Fatalf("budgetFromContext on an inert budget: got %v, want nil", got)
	}
}
