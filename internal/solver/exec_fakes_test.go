package solver

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tsouza/cerberus/internal/chclient"
	"github.com/tsouza/cerberus/internal/chplan"
)

// ---- fake SQLEmitter ----------------------------------------------------

// fakeEmitter emits a deterministic SQL string per call. emitErr (if set)
// fails on shard `failAt`; now64At injects a now64( into shard `now64At`'s
// SQL to exercise the belt-and-braces assertion.
type fakeEmitter struct {
	failAt  int // -1 = never
	now64At int // -1 = never
	calls   atomic.Int64
}

func newFakeEmitter() *fakeEmitter { return &fakeEmitter{failAt: -1, now64At: -1} }

func (e *fakeEmitter) Emit(_ context.Context, _ chplan.Node) (string, []any, error) {
	n := int(e.calls.Add(1)) - 1
	if e.failAt >= 0 && n == e.failAt {
		return "", nil, fmt.Errorf("synthetic emit failure on shard %d", n)
	}
	if e.now64At >= 0 && n == e.now64At {
		return fmt.Sprintf("SELECT now64(9) /* shard %d */", n), []any{n}, nil
	}
	return fmt.Sprintf("SELECT 1 /* shard %d */", n), []any{n}, nil
}

// ---- fake CursorQuerier -------------------------------------------------

// fakeQuerier hands out a fakeCursor per QueryCursor call. Behaviour is
// driven by a per-call plan keyed by an incrementing call index, so a test
// can make a specific shard error / block / stream.
type fakeQuerier struct {
	mu sync.Mutex
	// plan keyed by shard index parsed from the SQL comment; defaults to a
	// clean N-row stream.
	openErrAt   map[int]error // open-time error per shard
	iterErrAt   map[int]error // mid-drain Err() per shard
	rowsPerShrd map[int]int   // row count per shard (default rows)
	rows        int           // default rows per shard
	delay       time.Duration // per-row send delay (for blocking)
	labelsFn    func(shard, i int) map[string]string
	opened      atomic.Int64
	closed      atomic.Int64
	// live tracks open-but-not-closed cursors so tests can assert no leak.
	live atomic.Int64
}

func newFakeQuerier(rows int) *fakeQuerier {
	return &fakeQuerier{
		openErrAt:   map[int]error{},
		iterErrAt:   map[int]error{},
		rowsPerShrd: map[int]int{},
		rows:        rows,
	}
}

// shardOf parses the trailing "shard N" from the synthetic SQL comment.
func shardOf(sql string) int {
	// Find the last "shard " and read the integer that follows.
	idx := -1
	for i := 0; i+6 <= len(sql); i++ {
		if sql[i:i+6] == "shard " {
			idx = i + 6
		}
	}
	if idx < 0 {
		return 0
	}
	v := 0
	for idx < len(sql) && sql[idx] >= '0' && sql[idx] <= '9' {
		v = v*10 + int(sql[idx]-'0')
		idx++
	}
	return v
}

func (q *fakeQuerier) QueryCursor(ctx context.Context, sql string, _ ...any) (chclient.Cursor, error) {
	q.opened.Add(1)
	shard := shardOf(sql)
	q.mu.Lock()
	openErr := q.openErrAt[shard]
	iterErr := q.iterErrAt[shard]
	n, ok := q.rowsPerShrd[shard]
	if !ok {
		n = q.rows
	}
	q.mu.Unlock()
	if openErr != nil {
		return nil, openErr
	}
	q.live.Add(1)
	return &fakeCursor{
		q:        q,
		ctx:      ctx,
		shard:    shard,
		total:    n,
		iterErr:  iterErr,
		delay:    q.delay,
		labelsFn: q.labelsFn,
	}, nil
}

// fakeCursor streams `total` rows then optionally surfaces iterErr.
type fakeCursor struct {
	q         *fakeQuerier
	ctx       context.Context
	shard     int
	total     int
	i         int
	iterErr   error
	delay     time.Duration
	labelsFn  func(shard, i int) map[string]string
	cur       chclient.Sample
	err       error
	closeOnce sync.Once
}

func (c *fakeCursor) Next() bool {
	if c.err != nil {
		return false
	}
	if c.i >= c.total {
		// stream exhausted; surface the configured iteration error (if any).
		if c.iterErr != nil {
			c.err = c.iterErr
		}
		return false
	}
	if c.delay > 0 {
		select {
		case <-time.After(c.delay):
		case <-c.ctx.Done():
			c.err = c.ctx.Err()
			return false
		}
	}
	var labels map[string]string
	if c.labelsFn != nil {
		labels = c.labelsFn(c.shard, c.i)
	} else {
		labels = map[string]string{"shard": fmt.Sprintf("%d", c.shard)}
	}
	c.cur = chclient.Sample{
		MetricName: "m",
		Labels:     labels,
		Timestamp:  time.Unix(int64(c.shard*1000+c.i), 0),
		Value:      float64(c.shard*1000 + c.i),
	}
	c.i++
	return true
}

func (c *fakeCursor) Sample() chclient.Sample { return c.cur }
func (c *fakeCursor) Err() error              { return c.err }

func (c *fakeCursor) Close() error {
	c.closeOnce.Do(func() {
		c.q.closed.Add(1)
		c.q.live.Add(-1)
	})
	return nil
}

// ---- fake breakerPeeker -------------------------------------------------

type fakeBreaker struct {
	state atomic.Value // string
}

func newFakeBreaker(state string) *fakeBreaker {
	b := &fakeBreaker{}
	b.state.Store(state)
	return b
}

func (b *fakeBreaker) PeekBreakerState() string { return b.state.Load().(string) }

// ---- fake admitTopUp ----------------------------------------------------

// fakeAdmit grants min(want, avail) units. denyTopUp forces a zero grant to
// exercise the degrade path.
type fakeAdmit struct {
	avail     int
	denyTopUp bool
	released  atomic.Int64
	granted   atomic.Int64
}

func (a *fakeAdmit) TryAcquireTopUp(_ context.Context, want int) (int, func()) {
	if a.denyTopUp || want <= 0 {
		return 0, func() {}
	}
	g := want
	if a.avail < g {
		g = a.avail
	}
	if g < 0 {
		g = 0
	}
	a.granted.Add(int64(g))
	var once sync.Once
	return g, func() { once.Do(func() { a.released.Add(int64(g)) }) }
}

// ---- decision helpers ---------------------------------------------------

// makeDecision builds a routed Decision with k slices. The Plan in each
// slice is a trivial non-nil node (the fake emitter ignores it).
func makeDecision(k int) *Decision {
	d := &Decision{Strategy: StrategyShardedTimeslice, K: k, Reason: ReasonRouted}
	for i := 0; i < k; i++ {
		d.Slices = append(d.Slices, Slice{Index: i, Plan: &chplan.OneRow{}})
	}
	return d
}
