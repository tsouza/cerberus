package solver

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/tsouza/cerberus/internal/chclient"
)

func testCfg() Config {
	c := DefaultConfig()
	c.Mode = ModeSharded
	c.Timeout = 5 * time.Second
	c.MaxOutputRows = 1_000_000
	return c
}

// drainAll iterates the cursor to exhaustion and returns the row count plus
// the terminal error.
func drainAll(c chclient.Cursor) (int, error) {
	n := 0
	for c.Next() {
		_ = c.Sample()
		n++
	}
	return n, c.Err()
}

// newExec builds an Executor over the supplied fakes with a gate of the
// given size.
func newExec(q CursorQuerier, em SQLEmitter, cfg Config, gateCap int64, br breakerPeeker, ad admitTopUp) *Executor {
	x := &Executor{
		Client:  q,
		Emitter: em,
		Cfg:     cfg,
		Breaker: br,
		Admit:   ad,
	}
	if gateCap > 0 {
		x.Gate = semaphore.NewWeighted(gateCap)
		x.GateCap = gateCap
	}
	return x
}

// TestExecute_HappyPath_Concatenates verifies oldest-first concatenation:
// K shards × R rows each, in index order.
func TestExecute_HappyPath_Concatenates(t *testing.T) {
	q := newFakeQuerier(5)
	x := newExec(q, newFakeEmitter(), testCfg(), 32, newFakeBreaker(BreakerClosed), nil)
	cur, info, err := x.Execute(context.Background(), "promql", makeDecision(4), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	defer cur.Close()
	if len(info.SQLs) != 4 {
		t.Fatalf("want 4 SQLs, got %d", len(info.SQLs))
	}
	// Drain and confirm oldest-first: shard 0 rows come before shard 1, ...
	var seenShards []int
	for cur.Next() {
		seenShards = append(seenShards, int(cur.Sample().Value)/1000)
	}
	if err := cur.Err(); err != nil {
		t.Fatalf("drain err: %v", err)
	}
	if len(seenShards) != 20 {
		t.Fatalf("want 20 rows, got %d", len(seenShards))
	}
	last := -1
	for _, s := range seenShards {
		if s < last {
			t.Fatalf("not oldest-first: shard %d after %d", s, last)
		}
		last = s
	}
	if err := cur.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if q.live.Load() != 0 {
		t.Fatalf("cursors leaked: %d live", q.live.Load())
	}
}

// TestExecute_BreakerHalfOpen_FailsFast asserts a non-CLOSED breaker fails
// fast with ErrCircuitOpen and opens ZERO cursors (probe preserved).
func TestExecute_BreakerHalfOpen_FailsFast(t *testing.T) {
	for _, state := range []string{BreakerOpen, BreakerHalfOpen} {
		t.Run(state, func(t *testing.T) {
			q := newFakeQuerier(5)
			x := newExec(q, newFakeEmitter(), testCfg(), 32, newFakeBreaker(state), nil)
			_, _, err := x.Execute(context.Background(), "promql", makeDecision(4), nil)
			if !errors.Is(err, chclient.ErrCircuitOpen) {
				t.Fatalf("want ErrCircuitOpen, got %v", err)
			}
			if q.opened.Load() != 0 {
				t.Fatalf("opened %d cursors on %s breaker — probe burned", q.opened.Load(), state)
			}
		})
	}
}

// TestExecute_EmitFailure_ZeroCHWork asserts an emit failure aborts before
// any cursor opens.
func TestExecute_EmitFailure_ZeroCHWork(t *testing.T) {
	q := newFakeQuerier(5)
	em := newFakeEmitter()
	em.failAt = 2
	x := newExec(q, em, testCfg(), 32, newFakeBreaker(BreakerClosed), nil)
	_, _, err := x.Execute(context.Background(), "promql", makeDecision(4), nil)
	if !errors.Is(err, ErrSolverEmit) {
		t.Fatalf("want ErrSolverEmit, got %v", err)
	}
	if q.opened.Load() != 0 {
		t.Fatalf("opened %d cursors after emit failure", q.opened.Load())
	}
}

// TestExecute_Now64InShardSQL asserts the belt-and-braces now64( assertion
// fires and aborts with zero CH work.
func TestExecute_Now64InShardSQL(t *testing.T) {
	q := newFakeQuerier(5)
	em := newFakeEmitter()
	em.now64At = 1
	x := newExec(q, em, testCfg(), 32, newFakeBreaker(BreakerClosed), nil)
	_, _, err := x.Execute(context.Background(), "promql", makeDecision(4), nil)
	if !errors.Is(err, errNow64InShardSQL) {
		t.Fatalf("want errNow64InShardSQL, got %v", err)
	}
	if q.opened.Load() != 0 {
		t.Fatalf("opened %d cursors despite now64 in SQL", q.opened.Load())
	}
}

// TestExecute_AdmissionDegrade asserts a top-up denial clamps parallelism
// but returns an IDENTICAL response (same rows), never a 503.
func TestExecute_AdmissionDegrade(t *testing.T) {
	q := newFakeQuerier(5)
	cfg := testCfg()
	cfg.Parallel = 4
	ad := &fakeAdmit{denyTopUp: true}
	x := newExec(q, newFakeEmitter(), cfg, 32, newFakeBreaker(BreakerClosed), ad)
	cur, info, err := x.Execute(context.Background(), "promql", makeDecision(4), nil)
	if err != nil {
		t.Fatalf("degrade must not error, got %v", err)
	}
	defer cur.Close()
	if info.Parallelism != 1 {
		t.Fatalf("denied top-up should clamp P to 1, got %d", info.Parallelism)
	}
	n, derr := drainAll(cur)
	if derr != nil {
		t.Fatalf("drain err: %v", derr)
	}
	if n != 20 {
		t.Fatalf("clamped response must still be complete: want 20 rows, got %d", n)
	}
}

// TestExecute_AdmissionPartial asserts a partial top-up grant clamps P to
// 1+granted and releases exactly the granted units at Close.
func TestExecute_AdmissionPartial(t *testing.T) {
	q := newFakeQuerier(3)
	cfg := testCfg()
	cfg.Parallel = 4
	ad := &fakeAdmit{avail: 1} // grant 1 of the requested 3
	x := newExec(q, newFakeEmitter(), cfg, 32, newFakeBreaker(BreakerClosed), ad)
	cur, info, err := x.Execute(context.Background(), "promql", makeDecision(4), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if info.Parallelism != 2 {
		t.Fatalf("want P=2 (1+1 granted), got %d", info.Parallelism)
	}
	if _, derr := drainAll(cur); derr != nil {
		t.Fatalf("drain: %v", derr)
	}
	if err := cur.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if ad.granted.Load() != 1 {
		t.Fatalf("want 1 unit granted, got %d", ad.granted.Load())
	}
	if ad.released.Load() != 1 {
		t.Fatalf("top-up not released exactly once: released=%d", ad.released.Load())
	}
}

// TestExecute_OutputCap asserts the composed output-row cap fires a DISTINCT
// 422 whose message is NOT the upstream max-samples text.
func TestExecute_OutputCap(t *testing.T) {
	q := newFakeQuerier(100) // 4 shards × 100 = 400 rows
	cfg := testCfg()
	cfg.MaxOutputRows = 250
	x := newExec(q, newFakeEmitter(), cfg, 32, newFakeBreaker(BreakerClosed), nil)
	cur, _, err := x.Execute(context.Background(), "promql", makeDecision(4), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	defer cur.Close()
	_, derr := drainAll(cur)
	var oc *OutputCapError
	if !errors.As(derr, &oc) {
		t.Fatalf("want *OutputCapError, got %v", derr)
	}
	if !errors.Is(derr, errOutputCapExceeded) {
		t.Fatalf("want errOutputCapExceeded sentinel, got %v", derr)
	}
	// The output-cap message must NOT collide with the upstream max-samples
	// parity message.
	ups := (&chclient.TooManySamplesError{Limit: 250}).Error()
	if oc.Error() == ups {
		t.Fatalf("output-cap message reuses upstream max-samples text: %q", oc.Error())
	}
}

// TestExecute_FirstErrorWins runs the cancellation/cause matrix: for each
// shard index × error class, inject and assert the EXACT typed error
// surfaces (first-error-wins), never flipped to context.Canceled by an
// induced sibling cancel. Run under GOMAXPROCS variation.
func TestExecute_FirstErrorWins(t *testing.T) {
	memErr := &chclient.MemoryLimitError{Limit: 1 << 30, Cause: errors.New("code: 241")}
	budgetErr := &chclient.TooManySamplesError{Limit: 50}
	transportErr := errors.New("read: connection reset by peer")
	chExcErr := errors.New("clickhouse exception: code 159 timeout")

	classes := []struct {
		name   string
		open   bool // open-time vs iteration error
		err    error
		expect func(error) bool
	}{
		{"memory-241-iter", false, memErr, func(e error) bool { var m *chclient.MemoryLimitError; return errors.As(e, &m) }},
		{"sample-budget-iter", false, budgetErr, func(e error) bool { var s *chclient.TooManySamplesError; return errors.As(e, &s) }},
		{"transport-open", true, transportErr, func(e error) bool { return errors.Is(e, transportErr) }},
		{"transport-iter", false, transportErr, func(e error) bool { return errors.Is(e, transportErr) }},
		{"ch-exception-open", true, chExcErr, func(e error) bool { return errors.Is(e, chExcErr) }},
	}

	for _, gomax := range []int{1, 4} {
		runtime.GOMAXPROCS(gomax)
		for _, cls := range classes {
			for shard := 0; shard < 4; shard++ {
				name := fmt.Sprintf("%s/shard%d/gomax%d", cls.name, shard, gomax)
				t.Run(name, func(t *testing.T) {
					q := newFakeQuerier(8)
					if cls.open {
						q.openErrAt[shard] = cls.err
					} else {
						q.iterErrAt[shard] = cls.err
					}
					x := newExec(q, newFakeEmitter(), testCfg(), 32, newFakeBreaker(BreakerClosed), nil)
					cur, _, err := x.Execute(context.Background(), "promql", makeDecision(4), nil)
					if err != nil {
						t.Fatalf("Execute returned setup error: %v", err)
					}
					_, derr := drainAll(cur)
					_ = cur.Close()
					if derr == nil {
						t.Fatalf("want %s error, got nil", cls.name)
					}
					if errors.Is(derr, context.Canceled) {
						t.Fatalf("deterministic error flipped to context.Canceled: %v", derr)
					}
					if !cls.expect(derr) {
						t.Fatalf("want %s typed error, got %T: %v", cls.name, derr, derr)
					}
					if q.live.Load() != 0 {
						t.Fatalf("cursors leaked: %d", q.live.Load())
					}
				})
			}
		}
	}
	runtime.GOMAXPROCS(runtime.NumCPU())
}

// TestExecute_BreakerDedup_FirstErrorWins asserts that when all P opens fail
// concurrently, exactly ONE error surfaces as the terminal error (the dedup
// contract at the solver boundary: one logical failure, siblings
// cause-cancelled — never K records).
func TestExecute_BreakerDedup_FirstErrorWins(t *testing.T) {
	shardErr := errors.New("shard open failed: CH down")
	q := newFakeQuerier(5)
	for i := 0; i < 4; i++ {
		q.openErrAt[i] = shardErr
	}
	cfg := testCfg()
	cfg.Parallel = 4
	x := newExec(q, newFakeEmitter(), cfg, 32, newFakeBreaker(BreakerClosed), nil)
	cur, _, err := x.Execute(context.Background(), "promql", makeDecision(4), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	_, derr := drainAll(cur)
	_ = cur.Close()
	if !errors.Is(derr, shardErr) {
		t.Fatalf("want shardErr, got %v", derr)
	}
	if q.live.Load() != 0 {
		t.Fatalf("cursors leaked: %d", q.live.Load())
	}
}

// TestExecute_SolverTimeout asserts the wall-clock deadline fires a typed
// SolverTimeoutError (breaker-neutral 504) distinct from context.Canceled /
// DeadlineExceeded.
func TestExecute_SolverTimeout(t *testing.T) {
	q := newFakeQuerier(1_000_000)
	q.delay = 2 * time.Millisecond // slow enough to outlast the timeout
	cfg := testCfg()
	cfg.Timeout = 30 * time.Millisecond
	x := newExec(q, newFakeEmitter(), cfg, 32, newFakeBreaker(BreakerClosed), nil)
	cur, _, err := x.Execute(context.Background(), "promql", makeDecision(4), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	_, derr := drainAll(cur)
	_ = cur.Close()
	var st *SolverTimeoutError
	if !errors.As(derr, &st) {
		t.Fatalf("want *SolverTimeoutError, got %T: %v", derr, derr)
	}
	if !errors.Is(derr, errSolverTimeout) {
		t.Fatalf("want errSolverTimeout sentinel, got %v", derr)
	}
	if errors.Is(derr, context.DeadlineExceeded) {
		t.Fatalf("solver-timeout must be distinct from DeadlineExceeded")
	}
	if q.live.Load() != 0 {
		t.Fatalf("cursors leaked after timeout: %d", q.live.Load())
	}
}

// TestExecute_GateHalfCap asserts K_eff is capped at gate/2 so >=2 routed
// requests can progress. With a gate of 4, K_eff <= 2 even when K=8.
func TestExecute_GateHalfCap(t *testing.T) {
	q := newFakeQuerier(2)
	cfg := testCfg()
	cfg.Parallel = 8
	x := newExec(q, newFakeEmitter(), cfg, 4, newFakeBreaker(BreakerClosed), nil)
	cur, info, err := x.Execute(context.Background(), "promql", makeDecision(8), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	defer cur.Close()
	if info.Parallelism > 2 {
		t.Fatalf("gate/2 cap violated: P=%d with gate=4", info.Parallelism)
	}
	// Two concurrent routed requests must both make progress (gate=4, each
	// takes <=2). Launch a second while the first holds its slots.
	cur2, _, err := x.Execute(context.Background(), "promql", makeDecision(8), nil)
	if err != nil {
		t.Fatalf("second routed Execute blocked/failed: %v", err)
	}
	if _, derr := drainAll(cur2); derr != nil {
		t.Fatalf("second drain: %v", derr)
	}
	_ = cur2.Close()
	if _, derr := drainAll(cur); derr != nil {
		t.Fatalf("first drain: %v", derr)
	}
}

// TestExecute_ShardKillMidDrain — one shard errors while siblings stream;
// assert typed error, zero leaks, all conns returned.
func TestExecute_ShardKillMidDrain(t *testing.T) {
	q := newFakeQuerier(50)
	q.delay = 100 * time.Microsecond
	killErr := errors.New("shard 2 transport drop mid-stream")
	q.iterErrAt[2] = killErr
	x := newExec(q, newFakeEmitter(), testCfg(), 32, newFakeBreaker(BreakerClosed), nil)
	cur, _, err := x.Execute(context.Background(), "promql", makeDecision(4), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	_, derr := drainAll(cur)
	_ = cur.Close()
	if !errors.Is(derr, killErr) {
		t.Fatalf("want killErr, got %v", derr)
	}
	if q.live.Load() != 0 {
		t.Fatalf("cursors leaked mid-drain: %d", q.live.Load())
	}
	if q.opened.Load() != q.closed.Load() {
		t.Fatalf("opened (%d) != closed (%d)", q.opened.Load(), q.closed.Load())
	}
}

// TestExecute_CrossShardReintern asserts the same series arriving from K
// shards shares ONE label-map instance after composition.
func TestExecute_CrossShardReintern(t *testing.T) {
	q := newFakeQuerier(3)
	// every shard emits the SAME logical series (identical labels).
	q.labelsFn = func(_, _ int) map[string]string {
		return map[string]string{"job": "api", "inst": "0"}
	}
	x := newExec(q, newFakeEmitter(), testCfg(), 32, newFakeBreaker(BreakerClosed), nil)
	cur, _, err := x.Execute(context.Background(), "promql", makeDecision(4), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	defer cur.Close()
	var first map[string]string
	mismatched := 0
	for cur.Next() {
		l := cur.Sample().Labels
		if first == nil {
			first = l
			continue
		}
		// Same underlying map instance => pointer-equal via fmt of address.
		if fmt.Sprintf("%p", l) != fmt.Sprintf("%p", first) {
			mismatched++
		}
	}
	if err := cur.Err(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if mismatched != 0 {
		t.Fatalf("cross-shard reintern failed: %d rows held a distinct map", mismatched)
	}
}

// TestExecute_SharedSampleBudget asserts the per-request budget is shared
// across K shard cursors — the fan-out enforces ONE max-samples limit.
func TestExecute_SharedSampleBudget(t *testing.T) {
	// 4 shards × 10 rows = 40 total; budget 25 => the 26th sample trips.
	q := newFakeQuerier(10)
	budget := chclient.NewSampleBudget(25)
	// Use the real chclient cursor budget path: route shard cursors through
	// a querier that honours WithSampleBudget. The fake cursor does NOT, so
	// instead assert the budget is consumed: we wrap with a budget-aware
	// fake.
	bq := &budgetQuerier{inner: q}
	x := newExec(bq, newFakeEmitter(), testCfg(), 32, newFakeBreaker(BreakerClosed), nil)
	cur, _, err := x.Execute(context.Background(), "promql", makeDecision(4), budget)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	defer cur.Close()
	n, derr := drainAll(cur)
	var tms *chclient.TooManySamplesError
	if !errors.As(derr, &tms) {
		t.Fatalf("want *TooManySamplesError from shared budget, got %T: %v", derr, derr)
	}
	if n > 25 {
		t.Fatalf("shared budget over-served: %d rows past 25-sample limit", n)
	}
}

// budgetQuerier wraps the fake querier and enforces the shared ctx budget on
// each yielded sample — modelling chclient's rowsCursor budget consult.
type budgetQuerier struct{ inner *fakeQuerier }

func (b *budgetQuerier) QueryCursor(ctx context.Context, sql string, args ...any) (chclient.Cursor, error) {
	cur, err := b.inner.QueryCursor(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return &budgetCursor{Cursor: cur, budget: chclient.SampleBudgetFromContext(ctx)}, nil
}

type budgetCursor struct {
	chclient.Cursor
	budget *chclient.SampleBudget
	err    error
}

func (c *budgetCursor) Next() bool {
	if c.err != nil {
		return false
	}
	if !c.Cursor.Next() {
		return false
	}
	if c.budget != nil && !c.budget.Consume(1) {
		c.err = &chclient.TooManySamplesError{Limit: c.budget.Limit()}
		return false
	}
	return true
}

func (c *budgetCursor) Err() error {
	if c.err != nil {
		return c.err
	}
	return c.Cursor.Err()
}

// TestExecute_NilGateAndBreaker asserts the Executor runs without a gate /
// breaker / admit (the degenerate / disabled wiring).
func TestExecute_NilGateAndBreaker(t *testing.T) {
	q := newFakeQuerier(4)
	x := &Executor{Client: q, Emitter: newFakeEmitter(), Cfg: testCfg()}
	cur, info, err := x.Execute(context.Background(), "promql", makeDecision(3), nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	defer cur.Close()
	if info.Parallelism < 1 {
		t.Fatalf("parallelism must be >=1, got %d", info.Parallelism)
	}
	n, derr := drainAll(cur)
	if derr != nil {
		t.Fatalf("drain: %v", derr)
	}
	if n != 12 {
		t.Fatalf("want 12 rows, got %d", n)
	}
}

// guard against unused import of sync in this file if refactored.
var _ = sync.Once{}
