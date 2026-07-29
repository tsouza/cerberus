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
	const rowsPerShard = 5

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
