package solver

import (
	"context"
	"sync"
	"testing"
	"time"

	"golang.org/x/sync/semaphore"

	"go.uber.org/goleak"

	"github.com/tsouza/cerberus/internal/chclient"
)

// TestMain wraps the package's tests in a goleak verifier so every producer
// goroutine the Executor spawns is proven to terminate — including the
// shard-kill-mid-drain and timeout scenarios. A leaked producer (e.g. one
// blocked forever on a channel send because it didn't select on gctx.Done)
// fails the whole package here.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(
		m,
		// The OTel global meter provider may keep background goroutines; the
		// solver itself spawns none beyond its per-request producers.
		goleak.IgnoreTopFunction("go.opentelemetry.io/otel/sdk/metric.(*PeriodicReader).run"),
	)
}

// TestDeadlockHammer is the required deadlock-hammer stress lane (docs
// §Parallel #3 / #10): 64 concurrent routed Execute calls against a fake
// CursorQuerier + a Gate sized 32, PLUS 64 concurrent route-A gate
// acquisitions contending for the same gate. Asserts zero deadlock (the
// whole run completes within a generous bound) and that the gate/2 progress
// guarantee holds — routed requests cap at gate/2 slots each so the pool
// never wedges.
func TestDeadlockHammer(t *testing.T) {
	const gateCap = 32
	gate := semaphore.NewWeighted(gateCap)

	cfg := testCfg()
	cfg.Parallel = 8
	cfg.Timeout = 10 * time.Second

	// Shared querier across all routed requests — every cursor returned must
	// be closed, asserted at the end via live==0.
	q := newFakeQuerier(20)
	q.delay = 50 * time.Microsecond

	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup

		// 64 routed fan-outs sharing the one global gate.
		for i := 0; i < 64; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				x := &Executor{
					Client:  q,
					Emitter: newFakeEmitter(),
					Cfg:     cfg,
					Gate:    gate,
					GateCap: gateCap,
					Breaker: newFakeBreaker(BreakerClosed),
				}
				cur, info, err := x.Execute(context.Background(), "promql", makeDecision(8), nil)
				if err != nil {
					t.Errorf("routed Execute: %v", err)
					return
				}
				// gate/2 progress guarantee: never more than 16 slots.
				if info.Parallelism > gateCap/2 {
					t.Errorf("gate/2 cap violated: P=%d", info.Parallelism)
				}
				if _, derr := drainAll(cur); derr != nil {
					t.Errorf("routed drain: %v", derr)
				}
				_ = cur.Close()
			}()
		}

		// 64 concurrent route-A acquisitions: each grabs one slot, does a
		// little work, releases. These contend with the routed fan-outs for
		// the SAME gate, exactly the mixed-load shape the design pins.
		for i := 0; i < 64; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := gate.Acquire(ctx, 1); err != nil {
					t.Errorf("route-A acquire: %v", err)
					return
				}
				time.Sleep(100 * time.Microsecond)
				gate.Release(1)
			}()
		}

		wg.Wait()
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("deadlock hammer did not complete in 30s — wedge suspected")
	}

	if q.live.Load() != 0 {
		t.Fatalf("cursors leaked after hammer: %d live", q.live.Load())
	}
	if q.opened.Load() != q.closed.Load() {
		t.Fatalf("opened (%d) != closed (%d)", q.opened.Load(), q.closed.Load())
	}
}

// TestMixedLoadStress is the required mixed-load pin (docs §Parallel #10):
// a smaller gate (8) with routed + route-A contention, plus a slice of
// requests whose admit top-up is denied (degrade path) and a slice whose
// breaker is open (fail-fast). Asserts every request terminates with a
// consistent outcome, no goroutine leaks, all conns returned.
func TestMixedLoadStress(t *testing.T) {
	const gateCap = 8
	gate := semaphore.NewWeighted(gateCap)
	cfg := testCfg()
	cfg.Parallel = 4
	cfg.Timeout = 8 * time.Second

	q := newFakeQuerier(10)
	q.delay = 30 * time.Microsecond

	var wg sync.WaitGroup
	for i := 0; i < 48; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			br := newFakeBreaker(BreakerClosed)
			var ad admitTopUp
			switch i % 3 {
			case 0:
				// healthy routed
			case 1:
				ad = &fakeAdmit{denyTopUp: true} // degrade to sequential
			case 2:
				br = newFakeBreaker(BreakerOpen) // fail-fast
			}
			x := &Executor{
				Client:  q,
				Emitter: newFakeEmitter(),
				Cfg:     cfg,
				Gate:    gate,
				GateCap: gateCap,
				Breaker: br,
				Admit:   ad,
			}
			cur, _, err := x.Execute(context.Background(), "promql", makeDecision(4), nil)
			if i%3 == 2 {
				// breaker open => fail-fast, no cursor.
				if err == nil {
					t.Errorf("open breaker should fail-fast")
					if cur != nil {
						_ = cur.Close()
					}
				}
				return
			}
			if err != nil {
				t.Errorf("Execute: %v", err)
				return
			}
			if _, derr := drainAll(cur); derr != nil {
				t.Errorf("drain: %v", derr)
			}
			_ = cur.Close()
		}()
	}
	wg.Wait()

	if q.live.Load() != 0 {
		t.Fatalf("cursors leaked: %d", q.live.Load())
	}
}

// guard against an accidental unused-import if a fake type changes.
var _ = chclient.Sample{}
