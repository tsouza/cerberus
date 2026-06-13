package solver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"

	"github.com/tsouza/cerberus/internal/chclient"
)

// ExecInfo is the per-request execution metadata the engine adapter stamps
// onto X-Cerberus-* response headers / tracing spans. It carries no
// row data — composition flows through the returned Cursor.
type ExecInfo struct {
	// SQLs is one emitted SQL string per shard, oldest-first. Surfaced on
	// the X-Cerberus-Shards / tracing path.
	SQLs []string

	// ShardArgs is the positional-arg list per shard, parallel to SQLs.
	ShardArgs [][]any

	// Parallelism is the effective P after the admission clamp — equal to
	// Cfg.Parallel when the top-up granted everything, lower (down to 1)
	// when it degraded. Reported so capacity dashboards can see the clamp.
	Parallelism int
}

// Executor is the bounded-parallel shard dispatcher (docs §"Parallel
// execution"). It emits all K shard SQLs, performs two-stage weighted
// admission, atomically acquires the global connection gate, opens one
// cursor per shard under a cause-carrying errgroup, and concatenates the
// streams behind a shardCursor. It owns no per-request state itself — every
// routed request gets a fresh shardCursor that holds the gate / admit
// releases and dies with the request (the no-caching invariant).
type Executor struct {
	// Client opens the per-shard cursors. *chclient.Client in production.
	Client CursorQuerier

	// Emitter lowers each re-anchored shard plan to SQL. internal/chsql in
	// production. Required — a nil Emitter is a wiring bug, surfaced as an
	// emit error rather than a panic.
	Emitter SQLEmitter

	// Cfg is the solver configuration (timeout, max output rows, P, ...).
	Cfg Config

	// Gate is the GLOBAL connection semaphore, sized MaxOpenConns - reserve
	// and injected by the engine adapter so every head shares one gate. The
	// Executor acquires K_eff = min(K, P_eff, gate/2) slots ATOMICALLY
	// before opening any cursor (no hold-and-wait) and releases them all at
	// shardCursor.Close. A nil Gate means "no gate" (test / disabled).
	Gate *semaphore.Weighted

	// gateCap is the Gate's total size, used to compute the gate/2 cap that
	// guarantees >=2 routed requests can always progress. Injected
	// alongside Gate (semaphore.Weighted does not expose its size).
	GateCap int64

	// Breaker peeks the circuit state pre-flight so a routed fan-out never
	// burns the single half-open recovery probe. *chclient.Client in
	// production. Nil disables the pre-flight (test / no breaker).
	Breaker breakerPeeker

	// Admit is the two-stage weighted-admission hook for the (P-1) top-up.
	// *admit.Limiter in production. Nil means admission disabled — the
	// Executor runs at full P.
	Admit admitTopUp
}

// Execute emits, admits, gates, and dispatches a routed Decision, returning
// a Cursor that concatenates the K shard streams oldest-first. The returned
// Cursor owns the gate + admit releases and frees them on Close; callers
// MUST Close it exactly once (the handler's `defer cursor.Close()`).
//
// langName is the head identifier ("promql") threaded into each shard's
// progress recorder. budget is the shared per-request SampleBudget carried
// into every shard ctx so the max-samples 422 parity stays per-request.
//
// Failure modes, all before any cursor is returned:
//   - breaker not CLOSED  => ErrCircuitOpen (probe preserved)
//   - emit failure        => ErrSolverEmit-wrapped error (zero CH work)
//   - now64 in shard SQL  => errNow64InShardSQL (belt-and-braces)
//   - gate acquire denied  => the ctx error (typically the timeout cause)
//
// On the happy path the returned error is nil and the Cursor surfaces any
// shard error via Err() under the first-error-wins / cause-threaded
// contract.
func (x *Executor) Execute(
	ctx context.Context,
	langName string,
	d *Decision,
	budget *chclient.SampleBudget,
) (chclient.Cursor, *ExecInfo, error) {
	if d == nil || len(d.Slices) == 0 {
		return nil, nil, fmt.Errorf("%w: decision has no slices", ErrSolverEmit)
	}
	k := len(d.Slices)

	// 6. HALF-OPEN PRE-FLIGHT — peek before emitting. A non-CLOSED breaker
	// fails fast WITHOUT consuming the half-open probe (PeekBreakerState is
	// read-only): a K-shard fan-out must never burn the single recovery
	// probe on a doomed request; recovery probing is left to route-A
	// traffic.
	if x.Breaker != nil {
		if st := x.Breaker.PeekBreakerState(); st != BreakerClosed {
			return nil, nil, fmt.Errorf("solver: pre-flight: %w", chclient.ErrCircuitOpen)
		}
	}

	// 1. EMIT FIRST — all K shard SQLs before any cursor opens. An emit
	// failure aborts with ZERO CH work. The now64 string assertion runs
	// here (belt-and-braces over the Planner's static gate).
	if x.Emitter == nil {
		return nil, nil, fmt.Errorf("%w: nil emitter", ErrSolverEmit)
	}
	info := &ExecInfo{
		SQLs:      make([]string, k),
		ShardArgs: make([][]any, k),
	}
	for i := range d.Slices {
		sql, args, err := x.Emitter.Emit(ctx, d.Slices[i].Plan)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: shard %d: %w", ErrSolverEmit, i, err)
		}
		if strings.Contains(sql, "now64(") {
			return nil, nil, fmt.Errorf("%w: shard %d", errNow64InShardSQL, i)
		}
		info.SQLs[i] = sql
		info.ShardArgs[i] = args
	}

	// 2. TWO-STAGE WEIGHTED ADMISSION (degrade-don't-reject). The handler
	// already charged weight 1; ask for (P-1) extra units. On a partial /
	// zero grant we clamp effective P to 1+granted — down to sequential —
	// and run. We NEVER 503 and NEVER proceed at full P.
	pCfg := x.Cfg.Parallel
	if pCfg < 1 {
		pCfg = 1
	}
	pEff := pCfg
	var admitRelease func()
	if x.Admit != nil && pCfg > 1 {
		granted, release := x.Admit.TryAcquireTopUp(ctx, pCfg-1)
		admitRelease = release
		pEff = 1 + granted
		if pEff < pCfg {
			recordParallelismClamped(ctx)
		}
	}
	// admitRelease must run exactly once. If anything below fails before we
	// hand ownership to the shardCursor, release here.
	releaseAdmit := func() {
		if admitRelease != nil {
			admitRelease()
			admitRelease = nil
		}
	}

	// 3. ATOMIC GATE ACQUISITION — no hold-and-wait. K_eff = min(K, P_eff,
	// gate/2). The gate/2 cap guarantees >=2 routed requests can always make
	// progress. Acquire ALL K_eff slots in one call before opening any
	// cursor; release them all at Close.
	kEff := k
	if pEff < kEff {
		kEff = pEff
	}
	if x.Gate != nil && x.GateCap > 0 {
		half := int(x.GateCap / 2)
		if half < 1 {
			half = 1
		}
		if half < kEff {
			kEff = half
		}
	}
	if kEff < 1 {
		kEff = 1
	}

	var gateReleased atomic.Bool
	releaseGate := func() {
		if x.Gate != nil && gateReleased.CompareAndSwap(false, true) {
			x.Gate.Release(int64(kEff))
		}
	}
	if x.Gate != nil {
		if err := x.Gate.Acquire(ctx, int64(kEff)); err != nil {
			// Gate acquire honoured the request ctx (timeout / client
			// cancel). No cursors opened; release the admit top-up and
			// surface the ctx error. This is breaker-neutral: the Executor
			// never opened a CH connection.
			releaseAdmit()
			return nil, nil, fmt.Errorf("solver: gate acquire: %w", err)
		}
	}

	// effective concurrency cannot exceed the slots we hold.
	if pEff > kEff {
		pEff = kEff
	}
	info.Parallelism = pEff

	// 4. WALL-CLOCK DEADLINE — a dedicated cancel cause so a solver-timeout
	// is breaker-NEUTRAL (504) and distinct from a real DeadlineExceeded.
	timeout := x.Cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	causeCtx, cancelCause := context.WithCancelCause(ctx)
	timer := time.AfterFunc(timeout, func() {
		cancelCause(errSolverTimeout)
	})

	// 5. BREAKER FAILURE DEDUP — install ONE per-request latch on the SHARED
	// cause-carrying ctx so every shard's QueryCursor → breaker.record consults
	// the same latch. Under P_eff concurrent shard opens against a degraded
	// ClickHouse, each open can fail with a real (non-Canceled, non-241) error
	// before the first failure's cancel propagates; without the latch each
	// would advance the shared breaker counter, tripping the threshold-5
	// breaker by up to gate/2 in ONE logical request and 503-ing all three
	// heads. The latch makes the first real failure count and treats siblings
	// as breaker-neutral, enforcing the docs §"Parallel execution" #6 contract
	// (at most one breaker failure per logical request). The latch is
	// request-scoped (born/dies with causeCtx, no cross-request state).
	causeCtx = chclient.WithBreakerDedup(causeCtx)

	// 7. PER-SHARD EXECUTION. errgroup under the cause-carrying ctx;
	// SetLimit(P_eff). Each producer drains its cursor into a bounded chan
	// and selects on gctx.Done() while sending (provably terminating).
	g, gctx := errgroup.WithContext(causeCtx)
	g.SetLimit(pEff)

	sc := &shardCursor{
		k:            k,
		cfg:          x.Cfg,
		client:       x.Client,
		gctx:         gctx,
		cancelCause:  cancelCause,
		timer:        timer,
		g:            g,
		releaseGate:  releaseGate,
		releaseAdmit: releaseAdmit,
		chans:        make([]chan chclient.Sample, k),
		childCursors: make([]chclient.Cursor, k),
		interned:     make(map[string]map[string]string),
	}

	for i := range sc.chans {
		sc.chans[i] = make(chan chclient.Sample, shardChanCap)
	}

	// LAUNCH newest-slice-first (minimizes live-edge snapshot skew);
	// composition order is oldest-first regardless because the channels
	// buffer and the shardCursor drains them in index order.
	for i := k - 1; i >= 0; i-- {
		shardIdx := i
		sql := info.SQLs[shardIdx]
		args := info.ShardArgs[shardIdx]
		out := sc.chans[shardIdx]
		g.Go(func() error {
			return sc.runShard(gctx, langName, shardIdx, sql, args, budget, out)
		})
	}

	return sc, info, nil
}

// shardChanCap bounds each per-shard producer→composer channel (docs
// §Parallel #7). The producer blocks when the composer falls behind, so the
// gateway never buffers more than P*cap samples beyond what the composer
// has drained — the new, fixed solver-overhead term in the memory model.
const shardChanCap = 4096

// runShard is one producer goroutine. It derives its own progress ctx (one
// recorder per ctx key — sharing would corrupt the rows/bytes histograms),
// opens a cursor, drains it into out, and closes the cursor. It selects on
// gctx.Done() while sending so it terminates the instant the group is
// cancelled (provably leak-free under goleak).
//
// First-error-wins is enforced by errgroup.WithContext: the first non-nil
// return cancels gctx; siblings observe gctx.Done() and exit with the
// cause sentinel (breaker-neutral), never re-recording. A producer that
// hits an induced cancel returns the cause (not its own context.Canceled)
// so the deterministic error never flips to context.Canceled under a race.
func (sc *shardCursor) runShard(
	gctx context.Context,
	langName string,
	idx int,
	sql string,
	args []any,
	budget *chclient.SampleBudget,
	out chan<- chclient.Sample,
) (err error) {
	// Always close this shard's channel so the composer's range loop over it
	// terminates, regardless of how this producer exits.
	defer close(out)

	// Per-shard progress recorder (one per ctx key).
	pctx := chclient.WithProgressFor(gctx, langName)
	// Carry the shared per-request sample budget so the max-samples 422
	// parity stays per-REQUEST across all shards.
	if budget != nil {
		pctx = chclient.WithSampleBudget(pctx, budget)
	}

	cur, err := sc.client.QueryCursor(pctx, sql, args...)
	if err != nil {
		// Open-time error. If the group is already cancelled, prefer the
		// cause (a sibling's real error or the timeout) so a racing
		// induced-cancel never masquerades as this shard's failure and a
		// deterministic error never flips to context.Canceled.
		if cause := context.Cause(gctx); cause != nil && !errors.Is(cause, context.Canceled) {
			return cause
		}
		return err
	}

	// Register the child cursor so Close can tear it down even if this
	// producer is mid-drain when the group cancels.
	sc.registerChild(idx, cur)

	for cur.Next() {
		s := cur.Sample()
		select {
		case out <- s:
		case <-gctx.Done():
			// Group cancelled (sibling error, timeout, or client gone).
			// Stop draining and report the cause so the error class is
			// preserved deterministically.
			if cause := context.Cause(gctx); cause != nil {
				return cause
			}
			return gctx.Err()
		}
	}
	if cerr := cur.Err(); cerr != nil {
		// A deterministic iteration error (memory-241, sample-budget,
		// transport mid-drain). First-error-wins: return it verbatim so the
		// handler maps the exact typed wire status. If the group was
		// already cancelled by a sibling's real error, prefer that cause.
		if cause := context.Cause(gctx); cause != nil && !errors.Is(cause, context.Canceled) && !errors.Is(cause, cerr) {
			return cause
		}
		return cerr
	}
	return nil
}
