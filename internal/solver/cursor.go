package solver

import (
	"context"
	"errors"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/tsouza/cerberus/internal/api/format"
	"github.com/tsouza/cerberus/internal/chclient"
)

// shardCursor concatenates the K per-shard sample streams behind the
// chclient.Cursor interface (docs/solver.md §"Execution and cursor model").
// Composition is CONCATENATION, not evaluation: anchors are disjoint across
// slices by construction, so cerberus computes nothing — zero arithmetic,
// zero merge-by-key. The composer drains channel 0 (oldest slice) to
// exhaustion, then channel 1, ..., keeping per-series timestamps nearly
// ascending so the handler's insertion sort stays ~O(n).
//
// All per-request state — child cursors, interned labels, the errgroup, the
// gate / admit releases — is born with the request and dies at Close. There
// is no cross-request cache: the no-caching invariant is untouched.
type shardCursor struct {
	k   int
	cfg Config

	// client opens the per-shard cursors (set by the Executor).
	client CursorQuerier

	// gctx is the cause-carrying group context. Err() returns its cause
	// when non-Canceled.
	gctx        context.Context
	cancelCause context.CancelCauseFunc
	timer       *time.Timer
	g           *errgroup.Group

	// chans[i] carries shard i's samples, oldest-first. The composer drains
	// them in index order. Each is closed by its producer.
	chans []chan chclient.Sample

	// launched is closed once every shard has been handed to the errgroup.
	// Admission is bounded by SetLimit(P_eff), so with K > P_eff the launcher
	// blocks until a running shard finishes — which is why it runs off the
	// Execute goroutine (see the launch loop) and why teardown must join it
	// before g.Wait(): calling Wait while a Go is still pending would let it
	// return on a momentarily-zero counter and miss the unlaunched shards.
	launched chan struct{}

	// stop is the STOP-STREAMING signal, closed by Close. It is deliberately
	// distinct from cancelling gctx: a producer that observes stop unwinds
	// while its ClickHouse query context is still LIVE, so it can hand the
	// pooled connection back instead of having the socket destroyed (see
	// chclient.CloseCursor for why the ordering is the whole invariant).
	// Cancelling gctx is the ABORT signal — it belongs to a real failure (a
	// sibling's error, the wall-clock timeout, the client walking away), and it
	// is also the bounded fallback for a producer parked in cur.Next() that
	// cannot observe stop until its next send.
	stop chan struct{}

	// closeMu guards closeErr, which producers latch from their own teardown.
	closeMu sync.Mutex

	// releaseGate / releaseAdmit free the global gate slots and the admit
	// top-up. Each is idempotent; Close invokes them exactly once.
	releaseGate  func()
	releaseAdmit func()

	// interned re-interns labels ACROSS shards keyed by the canonical label
	// key, so the same series arriving from K shards holds ONE label-map
	// copy during the drain (not K). It ALSO stamps each distinct series a
	// stable cross-shard [chclient.Sample.SeriesID] — the child rowsCursors
	// each number their series independently from 1, so without this
	// re-numbering the composed stream would hand two genuinely-distinct
	// series the same per-shard ordinal, and a SeriesID-keyed consumer (the
	// prom matrix/vector label memo) would alias them. Per-request state.
	interned map[string]internedSeries
	// internSeq assigns each newly-seen canonical label key its 1-based
	// cross-shard SeriesID; it advances on first sight of a key, so the
	// composed cursor presents ONE consistent SeriesID namespace regardless
	// of which shard a row arrived on.
	internSeq uint32

	// drainIdx is the index of the channel the composer is currently
	// draining (0..k). Only Next touches it (single-consumer), so no lock.
	drainIdx int

	// cur is the sample the last successful Next landed on.
	cur chclient.Sample

	// emitted counts samples handed out via Next, enforced against
	// cfg.MaxOutputRows.
	emitted int64

	// err latches the first terminal error (output-cap or a shard error
	// surfaced via the group). Next returns false once set.
	err error

	closeOnce sync.Once
	closeErr  error
}

// latchCloseErr records a producer's cursor-teardown error. Close reports the
// first non-nil one, so a caller still sees the diagnostics the driver
// produced when a child could not be torn down cleanly.
func (sc *shardCursor) latchCloseErr(err error) {
	if err == nil {
		return
	}
	sc.closeMu.Lock()
	defer sc.closeMu.Unlock()
	if sc.closeErr == nil {
		sc.closeErr = err
	}
}

// Next advances to the next concatenated sample. It drains channel 0 to
// exhaustion, then channel 1, and so on — oldest-first. It returns false at
// end-of-stream or on a terminal error (check Err()). The per-request
// output-row cap (cfg.MaxOutputRows) is enforced here with a distinct typed
// 422 (OutputCapError) — NOT the upstream max-samples message.
func (sc *shardCursor) Next() bool {
	if sc.err != nil {
		return false
	}
	for sc.drainIdx < sc.k {
		select {
		case s, ok := <-sc.chans[sc.drainIdx]:
			if !ok {
				// This shard is exhausted; advance to the next oldest.
				sc.drainIdx++
				continue
			}
			// Enforce the composed output-row cap BEFORE handing the row
			// out. A high-cardinality success must not OOM the shared
			// gateway heap: this is a distinct 422, never the upstream
			// max-samples text.
			if sc.cfg.MaxOutputRows > 0 && sc.emitted >= sc.cfg.MaxOutputRows {
				// Latch the typed 422 and stop pulling. The group context is
				// deliberately left LIVE: the cap is a cerberus-side decision
				// about how many rows to HAND OUT, not a reason to abort the
				// ClickHouse queries dirtily. Close releases the producers via
				// the stop signal, so each tears its cursor down on a live ctx
				// and the connections go back to the pool.
				sc.err = &OutputCapError{Limit: sc.cfg.MaxOutputRows}
				return false
			}
			s.Labels, s.SeriesID = sc.reintern(s.Labels)
			sc.cur = s
			sc.emitted++
			return true
		case <-sc.gctx.Done():
			// A shard failed (or the request was cancelled / timed out).
			// Surface the cause as the terminal error; Err() classifies it.
			sc.err = sc.causeErr()
			return false
		}
	}
	// All channels drained without a cancel. But a producer may have
	// returned an error WITHOUT cancelling fast enough to beat the drain
	// (e.g. an open-time error on the last shard while earlier shards
	// streamed clean). Reap the group to surface a first-error-wins result.
	if werr := sc.g.Wait(); werr != nil {
		sc.err = sc.normalizeGroupErr(werr)
		return false
	}
	return false
}

// causeErr maps the group's cancel cause onto the terminal error Err()
// returns. The solver-timeout sentinel and real shard errors pass through;
// a plain context.Canceled (client gone) passes through too so the handler
// maps it breaker-neutrally.
func (sc *shardCursor) causeErr() error {
	cause := context.Cause(sc.gctx)
	if cause == nil {
		return sc.gctx.Err()
	}
	if errors.Is(cause, errSolverTimeout) {
		return &SolverTimeoutError{Timeout: sc.cfg.Timeout.String()}
	}
	return cause
}

// normalizeGroupErr maps an errgroup.Wait() result onto the terminal error.
// It preserves the deterministic first error and only translates the
// timeout sentinel into the typed SolverTimeoutError.
func (sc *shardCursor) normalizeGroupErr(werr error) error {
	if errors.Is(werr, errSolverTimeout) {
		return &SolverTimeoutError{Timeout: sc.cfg.Timeout.String()}
	}
	return werr
}

// internedSeries pairs the canonical shared label-map instance with the
// 1-based cross-shard ordinal the composer assigned it on first sight, so
// [shardCursor.reintern] can hand both back: the shared map (memory dedup)
// and a composed-stream-stable series identity (so a SeriesID-keyed
// consumer can memoise per-series work without aliasing two shards' rows).
type internedSeries struct {
	labels map[string]string
	id     uint32
}

// reintern returns the canonical shared map instance for a label set across
// ALL shards (so the same series from K shards holds one map during the
// drain) plus a stable cross-shard SeriesID. The child rowsCursors restart
// their per-shard numbering at 1, so the composer re-assigns the ordinal
// here against its OWN first-seen order — turning the concatenated stream
// into one consistent SeriesID namespace. A nil label set returns id 0
// (the "not interned" sentinel; a SeriesID-keyed memo recomputes for it).
// Labels stay read-only.
func (sc *shardCursor) reintern(labels map[string]string) (map[string]string, uint32) {
	if labels == nil {
		return nil, 0
	}
	key := format.CanonicalKey(labels)
	if cached, ok := sc.interned[key]; ok {
		return cached.labels, cached.id
	}
	sc.internSeq++
	sc.interned[key] = internedSeries{labels: labels, id: sc.internSeq}
	return labels, sc.internSeq
}

// Sample returns the row the most recent successful Next landed on.
func (sc *shardCursor) Sample() chclient.Sample { return sc.cur }

// Inspected returns the number of composed rows handed out via Next — the
// emitted counter the output-row cap is enforced against. It is the routed
// path's drain count, the same per-request quantity the single-cursor row
// path reports via seen.
func (sc *shardCursor) Inspected() int64 { return sc.emitted }

// Err returns the terminal error that stopped iteration, or nil at a clean
// end-of-stream. First-error-wins, cause-threaded: a sibling's induced
// cancel never masks the deterministic shard error.
func (sc *shardCursor) Err() error {
	if sc.err != nil {
		return sc.err
	}
	// No terminal error latched in Next. If a producer errored after the
	// composer already drained every channel, the cause is on gctx.
	if cause := context.Cause(sc.gctx); cause != nil && !errors.Is(cause, context.Canceled) {
		if errors.Is(cause, errSolverTimeout) {
			return &SolverTimeoutError{Timeout: sc.cfg.Timeout.String()}
		}
		return cause
	}
	return nil
}

// Close tears the routed request down exactly once (docs/solver.md §"Failure
// and cancellation contract", "Lifecycle"): signal stop → wait for every
// producer to tear its own cursor down → cancel gctx → release the gate slots
// + admit top-up → return the first non-nil child close error. Safe to call
// from multiple goroutines; the teardown runs under sync.Once.
//
// The two signals are ordered, not interchangeable. stop tells the producers to
// stop streaming while their ClickHouse query contexts are still LIVE, so each
// child cursor is torn down by the goroutine that owns it, on a live ctx, and
// clickhouse-go hands the connection back to the idle pool. Cancellation is the
// ABORT signal and the bounded fallback: the one shape stop cannot reach is a
// producer parked in cur.Next() on a stalled shard, and cancelling is what makes
// teardown terminate unconditionally there.
func (sc *shardCursor) Close() error {
	sc.closeOnce.Do(func() {
		// 1. STOP STREAMING. Producers blocked on a send unblock here and run
		// their own bounded cursor teardown on a still-live ctx.
		close(sc.stop)

		// 2. Bounded join. The budget is the ceiling on how long teardown may
		// hold pooled connections; past it, cancelling gctx unblocks a producer
		// that stop could not reach.
		if !sc.waitProducers(cursorTeardownBudget) {
			sc.cancelCause(context.Canceled)
			// Cancelling frees every slot the launcher is queued behind, so
			// joining it here terminates and makes the Wait below total over
			// all K shards rather than the launched prefix.
			<-sc.launched
			_ = sc.g.Wait()
		}

		// 3. Cancel unconditionally so nothing derived from gctx outlives the
		// request. If a real cause already latched (shard error / timeout),
		// CancelCause keeps the FIRST cause — this call only ensures
		// cancellation, it never reclassifies.
		sc.cancelCause(context.Canceled)

		if sc.timer != nil {
			sc.timer.Stop()
		}

		// Release the gate slots and the admit top-up — exactly once each.
		if sc.releaseGate != nil {
			sc.releaseGate()
		}
		if sc.releaseAdmit != nil {
			sc.releaseAdmit()
		}
	})
	sc.closeMu.Lock()
	defer sc.closeMu.Unlock()
	return sc.closeErr
}

// cursorTeardownBudget bounds the clean-teardown window for a routed request:
// how long Close waits for the K producers to drain and release their
// connections before falling back to cancellation. The producers tear down
// CONCURRENTLY, so the multiple over chclient's per-cursor budget covers
// scheduling skew, not K serial drains.
const cursorTeardownBudget = 4 * chclient.CursorDrainBudget

// TeardownBudget implements chclient.ComposedCursor: the ceiling Close itself
// observes. It is the clean-teardown window plus the one per-connection drain
// budget a producer still gets AFTER the fallback cancellation — a producer
// parked in cur.Next() only starts its own bounded teardown once gctx dies.
//
// Reporting it is what keeps chclient.CloseCursor from racing this cursor
// against a single connection's budget: the cancel CloseCursor holds is an
// ANCESTOR of every child query context, so firing it at 250ms would destroy
// exactly the K sockets the two-signal teardown exists to release cleanly.
func (sc *shardCursor) TeardownBudget() time.Duration {
	return cursorTeardownBudget + chclient.CursorDrainBudget
}

// waitProducers waits up to budget for every producer to return, reporting
// whether they all did. The waiter goroutine is joined by the caller's
// fallback g.Wait(), so no goroutine outlives Close.
func (sc *shardCursor) waitProducers(budget time.Duration) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Join the launcher first. Close has already closed sc.stop, so any
		// producer parked on a send unwinds and frees the slot the launcher is
		// waiting for; waiting here is what makes g.Wait() below observe every
		// shard rather than a prefix of them.
		<-sc.launched
		_ = sc.g.Wait()
	}()
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}
