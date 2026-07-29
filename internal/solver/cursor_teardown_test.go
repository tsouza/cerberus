package solver

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/tsouza/cerberus/internal/chclient"
)

// teardownRecorder captures, per shard, the ctx.Err() observed at the instant
// that shard's cursor Close() was entered. A nil recording means the cursor was
// torn down while its query context was still LIVE — the driver could drain the
// remaining rows to EndOfStream and hand the pooled connection back. A non-nil
// recording means cerberus had already cancelled, so clickhouse-go's
// connect.cancel() destroyed the socket instead of releasing it.
type teardownRecorder struct {
	mu      sync.Mutex
	ctxErrs map[int]error
	seen    map[int]bool
}

func newTeardownRecorder() *teardownRecorder {
	return &teardownRecorder{ctxErrs: map[int]error{}, seen: map[int]bool{}}
}

func (r *teardownRecorder) record(shard int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen[shard] {
		return
	}
	r.seen[shard] = true
	r.ctxErrs[shard] = err
}

// liveAtClose reports the shards whose Close() ran on an already-cancelled ctx.
func (r *teardownRecorder) cancelledAtClose() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []int
	for shard, err := range r.ctxErrs {
		if err != nil {
			out = append(out, shard)
		}
	}
	return out
}

func (r *teardownRecorder) closedCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.seen)
}

// teardownCursor is a chclient.Cursor that remembers the ctx its query was
// opened on and records that ctx's error at Close entry.
type teardownCursor struct {
	ctx   context.Context
	rec   *teardownRecorder
	shard int
	rows  int
	i     int
}

func (c *teardownCursor) Next() bool {
	if c.i >= c.rows {
		return false
	}
	c.i++
	return true
}

func (c *teardownCursor) Sample() chclient.Sample {
	return chclient.Sample{
		Labels: map[string]string{"shard": fmt.Sprint(c.shard)},
		Value:  float64(c.shard*1000 + c.i),
	}
}

func (c *teardownCursor) Err() error { return nil }

func (c *teardownCursor) Close() error {
	c.rec.record(c.shard, c.ctx.Err())
	return nil
}

func (c *teardownCursor) Inspected() int64 { return int64(c.i) }

// teardownQuerier hands out teardownCursors, optionally failing one shard's
// open so the sibling-error teardown shape can be exercised.
type teardownQuerier struct {
	rec       *teardownRecorder
	rows      int
	openErrAt int // -1 = never
	openErr   error
}

func (q *teardownQuerier) MaxQueryMemoryBytes() int64 { return 0 }

func (q *teardownQuerier) QueryCursor(ctx context.Context, sql string, _ ...any) (chclient.Cursor, error) {
	shard := shardOf(sql)
	if q.openErrAt >= 0 && shard == q.openErrAt {
		return nil, q.openErr
	}
	return &teardownCursor{ctx: ctx, rec: q.rec, shard: shard, rows: q.rows}, nil
}

// TestShardCursorClose_DrainsChildrenBeforeCancel pins the ordering invariant
// on the routed teardown path: every child cursor must be closed while the
// context its query runs on is still live, so clickhouse-go releases the
// connection back to the idle pool instead of destroying the socket.
//
// The two shapes cerberus itself controls are covered here: a clean full drain,
// and the composed output-row cap trip (which stops the stream mid-flight while
// the request context is still perfectly healthy). The shapes where the context
// is ALREADY dead when teardown begins — a sibling shard's error cancelling the
// errgroup, the solver wall-clock timeout, an HTTP client disconnect — are the
// upstream-owned destruction class and are covered by
// TestShardCursorClose_SiblingError_NoDeadlock for termination and error
// classification only.
func TestShardCursorClose_DrainsChildrenBeforeCancel(t *testing.T) {
	defer goleak.VerifyNone(t)

	const shards = 4

	// rowsPerShard MUST exceed shardChanCap, and the two subtests need it for
	// different reasons.
	//
	// "output cap trip" stops the composer mid-stream, so above the cap every
	// producer is PARKED on a send when Close() is entered. That parked state is
	// the only one in which the stop-before-cancel ordering is observable, and
	// the only one that reaches the `case <-sc.stop:` send arm in runShard.
	// Sized under the cap it passes against cancel-before-close and against a
	// deleted stop arm alike — it would be asserting on a teardown that had
	// already happened.
	//
	// "clean full drain" can never observe that ordering at any size: draining
	// to exhaustion unparks every producer, and each runs its own deferred
	// teardown before it closes its channel, so the recorder is complete and
	// live-ctx-only before Close() is even called. What the size buys THERE is
	// the fan-out liveness precondition — shards=4 exceeds the default P_eff of
	// 3, so a producer must park for a slot to be contended at all, which is
	// what makes the subtest red against an inline or reverse-order launch.
	const parkedProducerBacklog = 8
	const rowsPerShard = shardChanCap + parkedProducerBacklog

	t.Run("clean full drain", func(t *testing.T) {
		rec := newTeardownRecorder()
		q := &teardownQuerier{rec: rec, rows: rowsPerShard, openErrAt: -1}
		x := newExec(q, newFakeEmitter(), testCfg(), 32, newFakeBreaker(BreakerClosed), nil)

		cur, _, err := x.Execute(context.Background(), "promql", makeDecision(shards), nil)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		n, derr := drainAll(cur)
		if derr != nil {
			t.Fatalf("drain: %v", derr)
		}
		if n != shards*rowsPerShard {
			t.Fatalf("drained %d rows; want %d", n, shards*rowsPerShard)
		}
		if cerr := cur.Close(); cerr != nil {
			t.Fatalf("Close: %v", cerr)
		}
		if got := rec.closedCount(); got != shards {
			t.Fatalf("closed %d child cursors; want %d", got, shards)
		}
		if bad := rec.cancelledAtClose(); len(bad) != 0 {
			t.Fatalf("shards %v were closed on an already-cancelled ctx; every child must be "+
				"torn down while its query ctx is live so the driver releases the connection", bad)
		}
	})

	t.Run("output cap trip mid-stream", func(t *testing.T) {
		rec := newTeardownRecorder()
		q := &teardownQuerier{rec: rec, rows: rowsPerShard, openErrAt: -1}
		cfg := testCfg()
		// Trip the composed cap partway through shard 0 so producers are still
		// streaming when the composer stops pulling.
		cfg.MaxOutputRows = 2
		x := newExec(q, newFakeEmitter(), cfg, 32, newFakeBreaker(BreakerClosed), nil)

		cur, _, err := x.Execute(context.Background(), "promql", makeDecision(shards), nil)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		_, derr := drainAll(cur)
		var capErr *OutputCapError
		if !errors.As(derr, &capErr) {
			t.Fatalf("drain err = %v (%T); want *OutputCapError", derr, derr)
		}
		if cerr := cur.Close(); cerr != nil {
			t.Fatalf("Close: %v", cerr)
		}
		// Err() must still classify as the typed 422 after teardown — the
		// cause-threading must not be reclassified by the teardown signal.
		if !errors.As(cur.Err(), &capErr) {
			t.Fatalf("Err() after Close = %v (%T); want *OutputCapError", cur.Err(), cur.Err())
		}
		if got := rec.closedCount(); got != shards {
			t.Fatalf("closed %d child cursors; want %d", got, shards)
		}
		if bad := rec.cancelledAtClose(); len(bad) != 0 {
			t.Fatalf("shards %v were closed on an already-cancelled ctx after an output-cap trip; "+
				"the cap trip must stop streaming without cancelling the ClickHouse query ctx", bad)
		}
	})
}

// TestShardCursorClose_SiblingError_NoDeadlock covers the teardown shape where
// the group context is already dead before teardown begins: one shard fails its
// open, errgroup cancels, and the surviving producers unwind. The connection
// destruction there is clickhouse-go policy (connect.cancel closes the socket
// unconditionally), so the assertions are the ones cerberus does own —
// termination without deadlock, no goroutine leak, and the deterministic
// first-error-wins classification surviving the two-signal teardown.
func TestShardCursorClose_SiblingError_NoDeadlock(t *testing.T) {
	defer goleak.VerifyNone(t)

	shardErr := errors.New("synthetic shard 2 open failure")
	rec := newTeardownRecorder()
	q := &teardownQuerier{rec: rec, rows: 5, openErrAt: 2, openErr: shardErr}
	x := newExec(q, newFakeEmitter(), testCfg(), 32, newFakeBreaker(BreakerClosed), nil)

	cur, _, err := x.Execute(context.Background(), "promql", makeDecision(4), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	_, derr := drainAll(cur)
	if !errors.Is(derr, shardErr) {
		t.Fatalf("drain err = %v; want the deterministic shard error %v", derr, shardErr)
	}

	done := make(chan error, 1)
	go func() { done <- cur.Close() }()
	select {
	case cerr := <-done:
		if cerr != nil {
			t.Fatalf("Close: %v", cerr)
		}
	case <-time.After(closeDeadlockBudget):
		t.Fatalf("Close did not return within %s — the two-signal teardown deadlocked", closeDeadlockBudget)
	}
	if !errors.Is(cur.Err(), shardErr) {
		t.Fatalf("Err() after Close = %v; want the deterministic shard error %v", cur.Err(), shardErr)
	}
}

// closeDeadlockBudget is how long the test waits for a teardown that should
// complete promptly before declaring it deadlocked. Generous relative to the
// production drain budget so a loaded CI runner cannot flake it.
const closeDeadlockBudget = 30 * time.Second

// TestExecute_ShardsExceedingParallelism_DoNotWedge pins the liveness rule that
// makes a routed fan-out terminate at all: the shard the composer is draining
// must already hold one of the P_eff admission slots.
//
// A producer parks once it has buffered shardChanCap samples and only the
// composer can unpark it, while the composer cannot start until Execute has
// returned. With K > P_eff — the default shape, since defaultParallel is 3 and
// a decision routinely slices into more shards than that — launching the shards
// inline, or in the reverse of the drain order, left shard 0 waiting behind
// P_eff already-parked producers that could never finish. The request then
// yielded a wall-clock timeout instead of its rows, for every routed query
// whose shards exceed shardChanCap samples.
//
// The row count is what makes this real: at or below shardChanCap every
// producer completes its sends into the buffer and nothing ever parks, so the
// wedge is invisible.
func TestExecute_ShardsExceedingParallelism_DoNotWedge(t *testing.T) {
	defer goleak.VerifyNone(t)

	cfg := testCfg()
	// K strictly greater than P_eff is the precondition; assert it rather than
	// assume it, so a change to defaultParallel retunes the test instead of
	// silently making it vacuous.
	const shards = defaultParallel + 1
	if cfg.Parallel >= shards {
		t.Fatalf("Parallel=%d >= shards=%d: this test needs K > P_eff to exercise "+
			"admission back-pressure at all", cfg.Parallel, shards)
	}
	const rowsPerShard = shardChanCap + 1

	rec := newTeardownRecorder()
	q := &teardownQuerier{rec: rec, rows: rowsPerShard, openErrAt: -1}
	x := newExec(q, newFakeEmitter(), cfg, 32, newFakeBreaker(BreakerClosed), nil)

	cur, _, err := x.Execute(context.Background(), "promql", makeDecision(shards), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	n, derr := drainAll(cur)
	if derr != nil {
		t.Fatalf("drain: %v (a wall-clock timeout here means the fan-out wedged: "+
			"the composer was waiting on a shard that never got an admission slot)", derr)
	}
	if want := shards * rowsPerShard; n != want {
		t.Fatalf("drained %d rows; want %d — every shard must reach the composer", n, want)
	}
	if cerr := cur.Close(); cerr != nil {
		t.Fatalf("Close: %v", cerr)
	}
	if got := rec.closedCount(); got != shards {
		t.Fatalf("closed %d child cursors; want %d", got, shards)
	}
}

// stalledCursor blocks in Next() until its query context dies. That is the one
// producer state sc.stop cannot reach — the producer is inside the driver call,
// not on a channel send — so Close is forced through its cancel-and-join
// fallback rather than the clean bounded join.
type stalledCursor struct {
	ctx   context.Context
	rec   *teardownRecorder
	shard int
}

func (c *stalledCursor) Next() bool {
	<-c.ctx.Done()
	return false
}
func (c *stalledCursor) Sample() chclient.Sample { return chclient.Sample{} }
func (c *stalledCursor) Err() error              { return nil }
func (c *stalledCursor) Close() error            { c.rec.record(c.shard, c.ctx.Err()); return nil }
func (c *stalledCursor) Inspected() int64        { return 0 }

type stalledQuerier struct{ rec *teardownRecorder }

func (q *stalledQuerier) MaxQueryMemoryBytes() int64 { return 0 }

func (q *stalledQuerier) QueryCursor(ctx context.Context, sql string, _ ...any) (chclient.Cursor, error) {
	return &stalledCursor{ctx: ctx, rec: q.rec, shard: shardOf(sql)}, nil
}

// TestShardCursorClose_StalledShards_JoinsTheLauncher pins that Close is TOTAL
// over all K shards, not over the prefix the launcher had managed to admit.
//
// Shards are handed to the errgroup from a launcher goroutine, so with K > P_eff
// the launcher is parked in g.Go on the admission semaphore for most of the
// request's life. errgroup's counter is incremented by g.Go, not at construction,
// so a g.Wait() issued while that Go is still pending observes a counter that is
// only momentarily correct: it can return on a prefix — or, when Close wins the
// race outright, before a single shard has been admitted — and the request's
// gate slots are then released while the launcher goes on opening ClickHouse
// queries behind it. Joining sc.launched first is what makes the wait total.
//
// Both of Close's waits need the join, and this shape exercises both: the clean
// bounded join in waitProducers, and — because a producer stalled inside the
// driver never observes sc.stop — the cancel-and-join fallback that follows it.
// Without the join in waitProducers this test fails instantly with shards still
// unlaunched; without the join in the fallback it panics outright with "sync:
// WaitGroup is reused before previous Wait has returned".
func TestShardCursorClose_StalledShards_JoinsTheLauncher(t *testing.T) {
	defer goleak.VerifyNone(t)

	// K strictly greater than P_eff is the precondition: it is what leaves the
	// launcher parked mid-loop when Close arrives.
	const shards = defaultParallel + 1
	cfg := testCfg()
	if cfg.Parallel >= shards {
		t.Fatalf("Parallel=%d >= shards=%d: this test needs K > P_eff to leave the "+
			"launcher parked at teardown", cfg.Parallel, shards)
	}

	rec := newTeardownRecorder()
	x := newExec(&stalledQuerier{rec: rec}, newFakeEmitter(), cfg, 32, newFakeBreaker(BreakerClosed), nil)

	cur, _, err := x.Execute(context.Background(), "promql", makeDecision(shards), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	sc, ok := cur.(*shardCursor)
	if !ok {
		t.Fatalf("Execute returned %T; want *shardCursor", cur)
	}

	// Close without draining a single row: every producer is stalled inside
	// Next(), so only the fallback cancellation can unwind them.
	done := make(chan error, 1)
	go func() { done <- cur.Close() }()
	select {
	case cerr := <-done:
		if cerr != nil {
			t.Fatalf("Close: %v", cerr)
		}
	case <-time.After(closeDeadlockBudget):
		t.Fatalf("Close did not return within %s — the stalled fan-out deadlocked", closeDeadlockBudget)
	}

	select {
	case <-sc.launched:
	default:
		t.Fatalf("Close returned while shards were still being handed to the errgroup: " +
			"the gate slots are released here, so the wait must cover every shard")
	}
	if got := rec.closedCount(); got != shards {
		t.Fatalf("closed %d child cursors; want %d — Close must tear down every shard, "+
			"including the ones the launcher had not yet admitted", got, shards)
	}
}
