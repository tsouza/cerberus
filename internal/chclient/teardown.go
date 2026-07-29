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

// ComposedCursor is a cursor that owns no ClickHouse connection of its own: it
// fans out over child cursors, each of which routes ITS OWN teardown through
// CloseCursor. The solver's sharded-timeslice cursor is the one implementor.
//
// Two consequences follow, and together they are why the interface exists:
//
//   - Its teardown is already bounded, by TeardownBudget, and that bound is
//     LONGER than a single connection's drain budget (it covers K of them,
//     concurrently). CloseCursor must therefore not race it against the
//     per-connection budget: the outer cancel is an ANCESTOR of every child's
//     query context, so firing it early destroys exactly the sockets the inner
//     budget exists to release cleanly.
//   - Its teardown is not itself a connection teardown, so CloseCursor does not
//     count it — the children already counted themselves, and double-counting
//     would break the churn decomposition the outcome label exists for.
type ComposedCursor interface {
	Cursor

	// TeardownBudget is the ceiling this cursor's own Close observes: the
	// point past which it has cancelled and joined everything it owns.
	TeardownBudget() time.Duration
}

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
//  2. That drain races the cursor's teardown budget. A caller who walked away
//     must not be able to pin a pooled connection for the length of a result
//     set nobody will read.
//  3. On expiry cancel() fires — the driver's ClientCancel unblocks the drain —
//     and the drain goroutine is JOINED before returning, so teardown never
//     leaves a goroutine behind.
//
// ctx is the context the cursor's query runs on, and cancel is its CancelFunc.
// ctx is read to classify the outcome: a teardown that begins on an ALREADY
// cancelled context cannot release the connection no matter how fast Close
// returns, so it is counted as such rather than as a clean drain. Passing the
// wrong cancel (a parent whose cancellation the cursor does not observe, or a
// child that does not reach the query) makes the budget arm unable to unblock
// the drain. Passing a nil cancel is legitimate only when the cursor's Close is
// guaranteed to terminate on its own, because the join in step 3 is
// unconditional.
//
// It returns the cursor's own Close error, unchanged, so callers keep whatever
// diagnostics the driver produced.
func CloseCursor(ctx context.Context, cur Cursor, cancel context.CancelFunc) error {
	return closeCursorWith(ctx, cur, cancel, connTelemetry())
}

// closeCursorWith is CloseCursor with the telemetry set injected, so the
// ordering and budget arms can be asserted against a manual reader without
// touching process-wide state.
func closeCursorWith(ctx context.Context, cur Cursor, cancel context.CancelFunc, m *connMetrics) error {
	if cur == nil {
		if cancel != nil {
			cancel()
		}
		return nil
	}

	// A composed cursor bounds its own teardown and accounts for its own
	// children's connections; the outer arms here are only its backstop.
	composed, isComposed := cur.(ComposedCursor)
	budget := CursorDrainBudget
	if isComposed {
		if own := composed.TeardownBudget(); own > budget {
			budget = own
		}
	}
	record := func(outcome string) {
		if isComposed {
			return
		}
		m.recordTeardown(outcome)
	}

	// Sampled BEFORE the drain starts: a cursor whose query context is
	// already dead has had its socket destroyed by the driver, so however
	// fast Close returns it was never a pool release. This is the dominant
	// teardown class on a gateway — a Grafana client disconnecting, a
	// wall-clock timeout, a sibling shard failing — and counting it as
	// "drained" would hide cerberus's own cancellations inside the residual
	// the operator attributes to age eviction.
	cancelledOnEntry := ctx.Err() != nil

	done := make(chan error, 1)
	go func() { done <- cur.Close() }()

	timer := time.NewTimer(budget)
	defer timer.Stop()

	select {
	case err := <-done:
		// Sample again before firing our own cancel: the context may have
		// died DURING the drain (the client walked away mid-teardown), which
		// is the same destroyed-socket outcome.
		cancelledDuringDrain := ctx.Err() != nil
		if cancel != nil {
			cancel()
		}
		switch {
		case cancelledOnEntry || cancelledDuringDrain:
			record(teardownCancelled)
		default:
			// Close returned inside the budget on a live context, so teardown
			// ran on cerberus's terms rather than being aborted. That is the
			// whole claim: it does NOT assert the driver reached EndOfStream,
			// because an early Close on a partially-read cursor lands here too
			// and the driver destroys that socket instead of pooling it.
			record(teardownDrained)
		}
		return err
	case <-timer.C:
		// Budget spent. Cancel to unblock the drain, then join it — an
		// unjoined drain goroutine would outlive the request and the goleak
		// detectors would (correctly) call it a leak.
		if cancel != nil {
			cancel()
		}
		err := <-done
		if cancelledOnEntry {
			record(teardownCancelled)
		} else {
			record(teardownAbandoned)
		}
		return err
	}
}
