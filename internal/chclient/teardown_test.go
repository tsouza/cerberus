package chclient

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// recordingCursor captures ctx.Err() at the instant Close() is entered. A nil
// recording is the whole point of the teardown contract: clickhouse-go returns
// a connection to the idle pool only when the query reaches EndOfStream with
// its context still live, so a Close that runs on an already-cancelled ctx
// destroys the socket instead of releasing it.
type recordingCursor struct {
	ctx context.Context

	ctxErrAtClose atomic.Value // error, boxed; nil errors are boxed as errBox{}

	// blockUntilCancelled makes Close hang until its ctx is cancelled, which
	// is how a real driver behaves when the remaining result set is larger
	// than the caller is willing to wait for.
	blockUntilCancelled bool

	closeErr error
}

// errBox lets an atomic.Value carry a nil error unambiguously.
type errBox struct{ err error }

func (c *recordingCursor) Next() bool       { return false }
func (c *recordingCursor) Sample() Sample   { return Sample{} }
func (c *recordingCursor) Err() error       { return nil }
func (c *recordingCursor) Inspected() int64 { return 0 }

func (c *recordingCursor) Close() error {
	c.ctxErrAtClose.Store(errBox{err: c.ctx.Err()})
	if c.blockUntilCancelled {
		<-c.ctx.Done()
	}
	return c.closeErr
}

// ctxErrAt reports the ctx error observed at Close entry, and whether Close ran
// at all.
func (c *recordingCursor) ctxErrAt() (error, bool) {
	v := c.ctxErrAtClose.Load()
	if v == nil {
		return nil, false
	}
	return v.(errBox).err, true
}

// TestCloseCursor_DrainsBeforeCancel is the ordering gate. Restore a
// cancel-then-close teardown anywhere in CloseCursor and the recorded ctx error
// becomes context.Canceled, which is precisely the driver state that destroys a
// pooled connection instead of releasing it.
func TestCloseCursor_DrainsBeforeCancel(t *testing.T) {
	defer goleak.VerifyNone(t)

	m, reader := newTestConnMetrics(t)
	ctx, cancel := context.WithCancel(context.Background())
	cur := &recordingCursor{ctx: ctx}

	if err := closeCursorWith(cur, cancel, m); err != nil {
		t.Fatalf("CloseCursor: %v", err)
	}

	ctxErr, ran := cur.ctxErrAt()
	if !ran {
		t.Fatal("Close was never called — the teardown must actually drain the cursor")
	}
	if ctxErr != nil {
		t.Fatalf("ctx.Err() at Close entry = %v; want nil. Cancelling before the close is what "+
			"makes clickhouse-go destroy the socket rather than release the connection", ctxErr)
	}
	// The ctx must still be cancelled by the time CloseCursor returns, or the
	// caller's request-scoped resources outlive the request.
	if ctx.Err() == nil {
		t.Fatal("ctx was still live after CloseCursor returned; the cancel must fire after the drain")
	}

	sum, exported, _ := counterSum(t, reader, "cerberus_ch_cursor_teardown_total",
		map[string]string{"outcome": teardownDrained})
	if !exported {
		t.Fatal("cerberus_ch_cursor_teardown_total was not exported")
	}
	if sum != 1 {
		t.Fatalf("teardown_total{outcome=drained} = %d; want 1", sum)
	}
	abandoned, _, _ := counterSum(t, reader, "cerberus_ch_cursor_teardown_total",
		map[string]string{"outcome": teardownAbandoned})
	if abandoned != 0 {
		t.Fatalf("teardown_total{outcome=abandoned} = %d; want 0 for a clean drain", abandoned)
	}
}

// TestCloseCursor_BudgetExpiry_CancelsAndJoins is the bound gate. A caller who
// walked away must not be able to pin a pooled connection for the length of a
// result set nobody will read — so the drain races a budget, and on expiry the
// cancel fires and the drain goroutine is JOINED (goleak proves the join).
func TestCloseCursor_BudgetExpiry_CancelsAndJoins(t *testing.T) {
	defer goleak.VerifyNone(t)

	m, reader := newTestConnMetrics(t)
	ctx, cancel := context.WithCancel(context.Background())
	var cancelled atomic.Bool
	instrumented := func() {
		cancelled.Store(true)
		cancel()
	}
	cur := &recordingCursor{ctx: ctx, blockUntilCancelled: true}

	start := time.Now()
	if err := closeCursorWith(cur, instrumented, m); err != nil {
		t.Fatalf("CloseCursor: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed < CursorDrainBudget {
		t.Fatalf("CloseCursor returned in %s, before the %s drain budget elapsed — "+
			"the clean-drain arm must be given its full budget", elapsed, CursorDrainBudget)
	}
	// A blocked drain must not become an unbounded wait.
	if elapsed > closeCursorBudgetCeiling {
		t.Fatalf("CloseCursor took %s; the budget arm must bound it well under %s", elapsed, closeCursorBudgetCeiling)
	}
	if !cancelled.Load() {
		t.Fatal("cancel was never invoked; nothing would unblock the driver's drain")
	}

	sum, _, _ := counterSum(t, reader, "cerberus_ch_cursor_teardown_total",
		map[string]string{"outcome": teardownAbandoned})
	if sum != 1 {
		t.Fatalf("teardown_total{outcome=abandoned} = %d; want 1", sum)
	}
	drained, _, _ := counterSum(t, reader, "cerberus_ch_cursor_teardown_total",
		map[string]string{"outcome": teardownDrained})
	if drained != 0 {
		t.Fatalf("teardown_total{outcome=drained} = %d; want 0 when the budget expired", drained)
	}
}

// closeCursorBudgetCeiling is the slack a loaded CI runner gets over the drain
// budget before a bounded teardown counts as unbounded.
const closeCursorBudgetCeiling = 20 * CursorDrainBudget

// TestCloseCursor_PropagatesCloseError confirms the cursor's own diagnostics
// survive the teardown wrapper — a caller must still see why a close failed.
func TestCloseCursor_PropagatesCloseError(t *testing.T) {
	defer goleak.VerifyNone(t)

	m, _ := newTestConnMetrics(t)
	sentinel := errors.New("synthetic driver close failure")
	ctx, cancel := context.WithCancel(context.Background())
	cur := &recordingCursor{ctx: ctx, closeErr: sentinel}

	if err := closeCursorWith(cur, cancel, m); !errors.Is(err, sentinel) {
		t.Fatalf("CloseCursor err = %v; want the cursor's own %v", err, sentinel)
	}
}

// TestCloseCursor_NilCursorStillCancels pins the degenerate arm: there is no
// cursor to drain, so the only obligation left is releasing the context.
func TestCloseCursor_NilCursorStillCancels(t *testing.T) {
	defer goleak.VerifyNone(t)

	m, reader := newTestConnMetrics(t)
	ctx, cancel := context.WithCancel(context.Background())

	if err := closeCursorWith(nil, cancel, m); err != nil {
		t.Fatalf("CloseCursor(nil): %v", err)
	}
	if ctx.Err() == nil {
		t.Fatal("ctx was still live after CloseCursor(nil); the cancel must always fire")
	}
	drained, _, _ := counterSum(t, reader, "cerberus_ch_cursor_teardown_total",
		map[string]string{"outcome": teardownDrained})
	abandoned, _, _ := counterSum(t, reader, "cerberus_ch_cursor_teardown_total",
		map[string]string{"outcome": teardownAbandoned})
	if drained != 0 || abandoned != 0 {
		t.Fatalf("teardown_total drained=%d abandoned=%d; a nil cursor is not a teardown", drained, abandoned)
	}
}
