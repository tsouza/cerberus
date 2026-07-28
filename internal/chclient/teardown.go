package chclient

import (
	"context"
	"time"
)

// CursorDrainBudget is the ceiling on how long a teardown may hold a pooled
// connection reading rows the caller will never look at. Past it, cerberus
// cancels: the connection is destroyed, but the pool slot is freed immediately
// rather than being occupied for the length of an arbitrarily large remainder.
const CursorDrainBudget = 250 * time.Millisecond

// CloseCursor is the single implementation of cerberus's cursor-teardown
// contract: CLOSE BEFORE CANCEL, bounded.
//
// clickhouse-go returns a connection to the idle pool only when the query
// reaches EndOfStream with its context still live — `rows.Close()` drains the
// stream and errors channels to closure, which lets the driver's process
// goroutine run `release(conn, nil)`. Cancel first and the driver takes the
// other branch instead: `connect.cancel()` writes ClientCancel and then tears
// the socket down outright, because a cancelled query leaves undrained bytes on
// the wire. There is no third option in this driver — a clean release requires
// reading the remainder, and abandoning early requires destroying the socket.
//
// So the ordering is the whole invariant, and it is structural here rather than
// an emergent property of some caller's defer order:
//
//  1. Close() runs on its own goroutine while the query context is STILL LIVE,
//     so the driver can drain to EndOfStream and release the connection.
//  2. That drain races CursorDrainBudget. A caller who walked away must not be
//     able to pin a pooled connection for the length of a result set nobody
//     will read.
//  3. On expiry cancel() fires — the driver's ClientCancel unblocks the drain —
//     and the drain goroutine is JOINED before returning, so teardown never
//     leaves a goroutine behind.
//
// cancel is the CancelFunc of the context the cursor's query runs on; passing
// the wrong one (a parent whose cancellation the cursor does not observe, or a
// child that does not reach the query) makes the budget arm unable to unblock
// the drain. Passing nil is legitimate only when the cursor's Close is
// guaranteed to terminate on its own — a composed cursor that owns its own
// bounded teardown, say — because the join in step 3 is unconditional.
//
// It returns the cursor's own Close error, unchanged, so callers keep whatever
// diagnostics the driver produced.
func CloseCursor(cur Cursor, cancel context.CancelFunc) error {
	return closeCursorWith(cur, cancel, connTelemetry())
}

// closeCursorWith is CloseCursor with the telemetry set injected, so the
// ordering and budget arms can be asserted against a manual reader without
// touching process-wide state.
func closeCursorWith(cur Cursor, cancel context.CancelFunc, m *connMetrics) error {
	if cur == nil {
		if cancel != nil {
			cancel()
		}
		return nil
	}

	done := make(chan error, 1)
	go func() { done <- cur.Close() }()

	timer := time.NewTimer(CursorDrainBudget)
	defer timer.Stop()

	select {
	case err := <-done:
		// The driver reached its own terminal state while the context was
		// live, so release(conn, nil) put the connection back in the pool.
		if cancel != nil {
			cancel()
		}
		m.recordTeardown(teardownDrained)
		return err
	case <-timer.C:
		// Budget spent. Cancel to unblock the drain, then join it — an
		// unjoined drain goroutine would outlive the request and the goleak
		// detectors would (correctly) call it a leak.
		if cancel != nil {
			cancel()
		}
		err := <-done
		m.recordTeardown(teardownAbandoned)
		return err
	}
}
