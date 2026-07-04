package autotune

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/tsouza/cerberus/internal/routerrules"
	"github.com/tsouza/cerberus/internal/solver"
)

// fakeCorpus answers the autotuner's OOM-floor aggregates from a fixed scenario.
type fakeCorpus struct {
	oomMinFanout  float64
	oomMinAnchors float64
	hasOOM        bool
}

func (f fakeCorpus) Aggregate(_ context.Context, spec routerrules.AggSpec) (routerrules.Value, error) {
	oom := spec.Scope["route"] == "A" && spec.Scope["exit_status"] == "oom"
	if oom && spec.Agg == routerrules.AggMin && spec.Column == "fanout" {
		if !f.hasOOM {
			return routerrules.Value{NoSignal: true}, nil
		}
		return routerrules.Value{Scalar: f.oomMinFanout}, nil
	}
	if oom && spec.Agg == routerrules.AggMin && spec.Column == "n_anchors" {
		if !f.hasOOM {
			return routerrules.Value{NoSignal: true}, nil
		}
		return routerrules.Value{Scalar: f.oomMinAnchors}, nil
	}
	return routerrules.Value{NoSignal: true}, nil
}

func (f fakeCorpus) EvalRule(_ context.Context, _ routerrules.RuleQuery) ([]routerrules.GroupResult, error) {
	return nil, nil
}

func newPlanner() *solver.Planner {
	cfg := solver.DefaultConfig()
	cfg.Mode = solver.ModeAuto
	return &solver.Planner{Cfg: cfg}
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestLoop_Tick_Applies drives one tick with a corpus that shows route A OOMing
// below the default fan-out and asserts the Planner's live thresholds dropped.
func TestLoop_Tick_Applies(t *testing.T) {
	p := newPlanner()
	f0, p0 := p.CurrentThresholds()
	if f0 != 16 || p0 != 4000 {
		t.Fatalf("unexpected default thresholds: fanout=%d pairs=%d", f0, p0)
	}

	corpus := fakeCorpus{hasOOM: true, oomMinFanout: 9, oomMinAnchors: 241}
	l := New(p, routerrules.NewAutotuner(corpus, routerrules.DefaultAutotuneOptions()), time.Hour, quietLogger())

	l.tick(context.Background())

	gotF, gotP := p.CurrentThresholds()
	if gotF != 9 || gotP != 2169 {
		t.Errorf("after tick: fanout=%d pairs=%d, want 9 / 2169", gotF, gotP)
	}
}

// TestLoop_Tick_ColdStartNoOp asserts an empty (no-OOM) corpus leaves the
// configured thresholds untouched — the default-on safety guarantee.
func TestLoop_Tick_ColdStartNoOp(t *testing.T) {
	p := newPlanner()
	l := New(p, routerrules.NewAutotuner(fakeCorpus{hasOOM: false}, routerrules.DefaultAutotuneOptions()), time.Hour, quietLogger())

	l.tick(context.Background())

	gotF, gotP := p.CurrentThresholds()
	if gotF != 16 || gotP != 4000 {
		t.Errorf("cold start changed thresholds: fanout=%d pairs=%d, want 16 / 4000", gotF, gotP)
	}
}

// TestLoop_Run_StopsOnCancel asserts Run returns promptly on ctx cancel and
// leaves no goroutine behind.
func TestLoop_Run_StopsOnCancel(t *testing.T) {
	defer goleak.VerifyNone(t)

	p := newPlanner()
	l := New(p, routerrules.NewAutotuner(fakeCorpus{hasOOM: false}, routerrules.DefaultAutotuneOptions()),
		time.Millisecond, quietLogger())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()

	// let a few ticks fire, then cancel.
	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of cancel")
	}
}

// TestLoop_Tick_RaceFree exercises the atomic threshold swap concurrently with
// reads (the Plan hot path reads through the same overlay). Run under -race.
func TestLoop_Tick_RaceFree(t *testing.T) {
	p := newPlanner()
	l := New(p, routerrules.NewAutotuner(fakeCorpus{hasOOM: true, oomMinFanout: 9, oomMinAnchors: 241},
		routerrules.DefaultAutotuneOptions()), time.Hour, quietLogger())

	var wg sync.WaitGroup
	const iters = 1000
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			l.tick(context.Background())
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			_, _ = p.CurrentThresholds()
		}
	}()
	wg.Wait()
}
